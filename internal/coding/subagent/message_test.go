//nolint:wsl_v5 // Delivery fixtures keep setup beside the assertion they serve.
package subagent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type admittedMessage struct {
	id       string
	delivery MessageDelivery
	text     string
	release  func()
}

// sinkRunner is a stubRunner that also accepts child messages. hold leaves each
// admission's release pending so the tree-wide in-flight accounting stays
// observable, and blockCtx makes the admission wait for its deadline.
type sinkRunner struct {
	*stubRunner
	mu       sync.Mutex
	admitted []admittedMessage
	hold     bool
	blockCtx bool
}

func (r *sinkRunner) AdmitMessage(
	ctx context.Context,
	delivery MessageDelivery,
	messageID string,
	text string,
	onRelease func(),
) error {
	if r.blockCtx {
		<-ctx.Done()

		return ctx.Err()
	}
	r.mu.Lock()
	r.admitted = append(r.admitted, admittedMessage{
		id: messageID, delivery: delivery, text: text, release: onRelease,
	})
	r.mu.Unlock()

	if !r.hold {
		onRelease()
	}

	return nil
}

func (r *sinkRunner) messages() []admittedMessage {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]admittedMessage(nil), r.admitted...)
}

// sinkDispatcher compiles builtin plans and opens one shared sinkRunner.
type sinkDispatcher struct {
	limits Limits
	runner *sinkRunner
}

func (d sinkDispatcher) AgentIDs() []string { return nil }

func (d sinkDispatcher) Compile(ctx context.Context, request Request) (ExecutionPlan, error) {
	return stubDispatcher{limits: d.limits}.Compile(ctx, request)
}

func (d sinkDispatcher) Open(_ context.Context, input DispatchInput) (Runner, error) {
	d.runner.child = input.Child
	d.runner.onEvent = input.OnEvent

	return d.runner, nil
}

// blockingOpenDispatcher holds Open until the test releases it, which is the
// only way to observe a child that has a durable record but no live execution.
type blockingOpenDispatcher struct {
	limits  Limits
	release chan struct{}
	runner  *sinkRunner
}

func (d blockingOpenDispatcher) AgentIDs() []string { return nil }

func (d blockingOpenDispatcher) Compile(ctx context.Context, request Request) (ExecutionPlan, error) {
	return stubDispatcher{limits: d.limits}.Compile(ctx, request)
}

func (d blockingOpenDispatcher) Open(ctx context.Context, input DispatchInput) (Runner, error) {
	select {
	case <-d.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	d.runner.child = input.Child
	d.runner.onEvent = input.OnEvent

	return d.runner, nil
}

// wakeRecorder records each wake a sender binding starts.
type wakeRecorder struct {
	mu      sync.Mutex
	targets []WakeTarget
	err     error
}

func (r *wakeRecorder) port() WakePort {
	return func(_ context.Context, target WakeTarget) error {
		r.mu.Lock()
		r.targets = append(r.targets, target)
		err := r.err
		r.mu.Unlock()

		return err
	}
}

func (r *wakeRecorder) recorded() []WakeTarget {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]WakeTarget(nil), r.targets...)
}

func newMessageFixture(t *testing.T) *managerFixture {
	t.Helper()

	return newManagerFixtureWithConfig(t, &testModel{}, ExecutionOptions{
		MaxConcurrent:                8,
		MaxSpawnedPerRootInteraction: 128,
	}, func(config *Config) {
		config.ActiveMessages = &MessagePorts{}
	})
}

func (f *managerFixture) ownership() Ownership {
	return Ownership{
		ParentInteractionID: "interaction-1",
		ParentRunID:         "run-1",
		ParentToolCallID:    "call-1",
		RootInteractionID:   "interaction-1",
	}
}

// messageCall is the Tool call every fixture sender is attributed to.
func messageCall() MessageCall {
	return MessageCall{RunID: "run-1", ID: "call-1"}
}

// rootSender is the messaging binding of the root conversation.
func (f *managerFixture) rootSender() MessageSender {
	return NewMessageBinding(MessageSenderIdentity{
		SessionID: f.parent.Metadata().ID,
		AttemptID: "interaction-1",
	}, nil).Sender(messageCall())
}

// childSender is the messaging binding of one child of the fixture's tree.
func (f *managerFixture) childSender(sessionID string, wake WakePort) MessageSender {
	return NewMessageBinding(MessageSenderIdentity{
		SessionID: sessionID,
		AttemptID: sessionID,
		Child:     true,
		Ownership: Ownership{
			ParentSessionID:     f.parent.Metadata().ID,
			ParentInteractionID: "interaction-1",
			ParentRunID:         "run-1",
			ParentToolCallID:    "call-1",
			RootInteractionID:   "interaction-1",
		},
		AgentID: string(RoleExplore),
	}, wake).Sender(messageCall())
}

// startExplore admits one builtin explore child and returns its execution.
func (f *managerFixture) startExplore(
	t *testing.T,
	dispatcher Dispatcher,
	ownership Ownership,
) *Execution {
	t.Helper()
	if dispatcher == nil {
		dispatcher = f.dispatcher
	}
	execution, err := f.manager.StartWithDispatcher(t.Context(), Request{
		AgentID: string(RoleExplore), Task: "explore the code",
		Ownership: ownership, Delivery: DeliveryForeground,
	}, nil, dispatcher)
	require.NoError(t, err)

	return execution
}

func (f *managerFixture) coordinator(t *testing.T) *MessageCoordinator {
	t.Helper()
	require.NotNil(t, f.manager.MessageCoordinator())

	return f.manager.MessageCoordinator()
}

func TestMessageOutcomeVocabularyIsClosed(t *testing.T) {
	t.Parallel()

	assert.Empty(t, MessageOutcome{}.Kind)
	assert.False(t, MessageOutcome{}.Accepted())
	assert.Equal(t, MessageDispositionRejected, MessageOutcome{}.Disposition())
	assert.Equal(t, "Message was not accepted.", MessageOutcome{}.Text())

	accepted := MessageOutcome{Kind: MessageOutcomeAccepted, MessageID: "msg-1"}
	assert.True(t, accepted.Accepted())
	assert.Equal(t, MessageDispositionAccepted, accepted.Disposition())
	assert.Equal(t, "Message accepted (message_id: msg-1).", accepted.Text())

	tests := []struct {
		outcome     MessageOutcome
		disposition MessageDisposition
		message     string
	}{
		{
			outcome:     MessageOutcome{Kind: MessageOutcomeNotFoundOrNotOwned},
			disposition: MessageDispositionRejected,
			message:     "Subagent not found or not owned by this session.",
		},
		{
			outcome:     MessageOutcome{Kind: MessageOutcomeNotActiveOrFinalizing},
			disposition: MessageDispositionRejected,
			message:     "Subagent is not active or is finalizing.",
		},
		{
			outcome:     MessageOutcome{Kind: MessageOutcomeSaturated, MaxInFlight: 8},
			disposition: MessageDispositionRejected,
			message:     "Message admission is saturated (maximum 8 in flight).",
		},
		{
			outcome: MessageOutcome{
				Kind: MessageOutcomeQuotaExceeded, Quota: MessageQuotaAttemptOutbound, Limit: 32,
			},
			disposition: MessageDispositionRejected,
			message:     "Agent-message quota exceeded (attempt_outbound, limit 32).",
		},
		{
			outcome:     MessageOutcome{Kind: MessageOutcomeAdmissionUncertain},
			disposition: MessageDispositionUnconfirmed,
			message:     "Message admission could not be confirmed; the message may or may not have been accepted.",
		},
		{
			outcome:     MessageOutcome{Kind: MessageOutcomeNotAcceptedBeforeDeadline},
			disposition: MessageDispositionRejected,
			message:     "Message was not accepted before the delivery deadline.",
		},
		{
			outcome:     MessageOutcome{Kind: MessageOutcomeUnsupported},
			disposition: MessageDispositionRejected,
			message:     "Active agent messages are unsupported in this context.",
		},
		{
			outcome: MessageOutcome{
				Kind: MessageOutcomeLimit, MaxBytes: MaxMessageBytes, ObservedBytes: 3,
			},
			disposition: MessageDispositionRejected,
			message:     "Message size is invalid: observed 3 bytes; maximum is 32768 bytes.",
		},
		{
			outcome:     MessageOutcome{Kind: MessageOutcomeChannelClosed},
			disposition: MessageDispositionRejected,
			message:     "Message was not accepted because the subagent channel closed.",
		},
	}
	for _, test := range tests {
		assert.Equal(t, test.disposition, test.outcome.Disposition())
		assert.Equal(t, test.message, test.outcome.Text())
	}
}

func TestMessageSendRejectsTextOutsideByteBound(t *testing.T) {
	t.Parallel()

	fixture := newMessageFixture(t)
	coordinator := fixture.coordinator(t)
	sender := fixture.rootSender()

	empty := MessageOutcome{
		Kind: MessageOutcomeLimit, MaxBytes: MaxMessageBytes, ObservedBytes: 0,
	}
	outcome, err := coordinator.Send(t.Context(), sender, "s-child", "", MessageDeliverySteer)
	require.NoError(t, err)
	assert.Equal(t, empty, outcome)

	oversized := strings.Repeat("a", MaxMessageBytes+1)
	outcome, err = coordinator.Send(t.Context(), sender, "s-child", oversized, MessageDeliverySteer)
	require.NoError(t, err)
	assert.Equal(t, MessageOutcome{
		Kind: MessageOutcomeLimit, MaxBytes: MaxMessageBytes, ObservedBytes: MaxMessageBytes + 1,
	}, outcome)

	// The bound is inclusive: a message of exactly the limit reaches the sink.
	runner := &sinkRunner{stubRunner: &stubRunner{block: true}}
	child := fixture.startExplore(t, sinkDispatcher{
		limits: fixture.manager.limits, runner: runner,
	}, fixture.ownership())
	atLimit := strings.Repeat("a", MaxMessageBytes)
	outcome, err = coordinator.Send(
		t.Context(), sender, child.child.Metadata().ID, atLimit, MessageDeliverySteer,
	)
	require.NoError(t, err)
	assert.Equal(t, MessageOutcomeAccepted, outcome.Kind)
	admitted := runner.messages()
	require.Len(t, admitted, 1)
	assert.Len(t, admitted[0].text, MaxMessageBytes)

	child.Cancel()
	_, _ = child.Wait(t.Context())
}

func TestMessageRevokeSettlesItsOwnReservationOnly(t *testing.T) {
	t.Parallel()

	fixture := newMessageFixture(t)
	runnerA := &sinkRunner{stubRunner: &stubRunner{block: true}, hold: true}
	runnerB := &sinkRunner{stubRunner: &stubRunner{block: true}, hold: true}
	childA := fixture.startExplore(t, sinkDispatcher{
		limits: fixture.manager.limits, runner: runnerA,
	}, fixture.ownership())
	childB := fixture.startExplore(t, sinkDispatcher{
		limits: fixture.manager.limits, runner: runnerB,
	}, fixture.ownership())
	targetA := childA.child.Metadata().ID
	targetB := childB.child.Metadata().ID

	coordinator := fixture.coordinator(t)
	coordinator.limits.Finalization = 20 * time.Millisecond
	sender := fixture.childSender("s-emitter", nil)
	for _, target := range []string{targetA, targetB} {
		outcome, err := coordinator.Send(t.Context(), sender, target, "note", MessageDeliverySteer)
		require.NoError(t, err)
		require.Equal(t, MessageOutcomeAccepted, outcome.Kind)
	}
	require.Equal(t, 2, coordinator.admissions)

	require.Equal(t, 1, coordinator.finalizeTarget(targetA))
	require.Equal(t, 1, coordinator.admissions, "the surviving admission keeps its slot")

	// The revoked reservation's own release must not free the survivor's slot.
	for _, message := range runnerA.messages() {
		message.release()
	}
	assert.Equal(t, 1, coordinator.admissions)
	assert.Equal(t, 1, coordinator.targetInFlight(targetB))

	// A later admission still sees an accurate count.
	outcome, err := coordinator.Send(t.Context(), sender, targetA, "note", MessageDeliverySteer)
	require.NoError(t, err)
	require.Equal(t, MessageOutcomeAccepted, outcome.Kind)
	assert.Equal(t, 2, coordinator.admissions)

	for _, message := range runnerB.messages() {
		message.release()
	}
	for _, message := range runnerA.messages() {
		message.release()
	}
	assert.Equal(t, 0, coordinator.admissions)
	assert.Equal(t, 0, coordinator.targetInFlight(targetA))
	assert.Equal(t, 0, coordinator.targetInFlight(targetB))

	childA.Cancel()
	childB.Cancel()
	_, _ = childA.Wait(t.Context())
	_, _ = childB.Wait(t.Context())
}

func TestMessageSendRejectsInvalidDelivery(t *testing.T) {
	t.Parallel()

	fixture := newMessageFixture(t)
	outcome, err := fixture.coordinator(t).Send(
		t.Context(), fixture.rootSender(), "s-child", "hello", MessageDelivery("teleport"),
	)
	require.NoError(t, err)
	assert.Equal(t, MessageOutcomeUnsupported, outcome.Kind)
}

func TestMessageSendRootCannotTargetParent(t *testing.T) {
	t.Parallel()

	fixture := newMessageFixture(t)
	outcome, err := fixture.coordinator(t).Send(
		t.Context(), fixture.rootSender(), ParentMessageTarget, "hello", MessageDeliverySteer,
	)
	require.NoError(t, err)
	assert.Equal(t, MessageOutcomeNotFoundOrNotOwned, outcome.Kind)
}

func TestMessageSendRejectsSelfAndForeignTargets(t *testing.T) {
	t.Parallel()

	fixture := newMessageFixture(t)
	coordinator := fixture.coordinator(t)
	sender := fixture.childSender("s-self", nil)

	outcome, err := coordinator.Send(t.Context(), sender, "s-self", "hello", MessageDeliverySteer)
	require.NoError(t, err)
	assert.Equal(t, MessageOutcomeNotFoundOrNotOwned, outcome.Kind)

	outcome, err = coordinator.Send(t.Context(), sender, "s-absent", "hello", MessageDeliverySteer)
	require.NoError(t, err)
	assert.Equal(t, MessageOutcomeNotFoundOrNotOwned, outcome.Kind)

	// A child of another tree is not reachable from this one.
	other := newMessageFixture(t)
	otherChild := other.startExplore(t, nil, other.ownership())
	_, err = otherChild.Wait(t.Context())
	require.NoError(t, err)

	outcome, err = coordinator.Send(
		t.Context(), sender, otherChild.child.Metadata().ID, "hello", MessageDeliverySteer,
	)
	require.NoError(t, err)
	assert.Equal(t, MessageOutcomeNotFoundOrNotOwned, outcome.Kind)
}

func TestMessageSendRejectsDirectInvocationTarget(t *testing.T) {
	t.Parallel()

	fixture := newMessageFixture(t)
	ownership := fixture.ownership()
	ownership.ParentRunID = DirectAgentRunPrefix + "interaction-1"
	ownership.ParentToolCallID = DirectAgentRunPrefix + "interaction-1"

	child := fixture.startExplore(t, nil, ownership)
	_, err := child.Wait(t.Context())
	require.NoError(t, err)

	outcome, err := fixture.coordinator(t).Send(
		t.Context(), fixture.rootSender(), child.child.Metadata().ID, "hello", MessageDeliverySteer,
	)
	require.NoError(t, err)
	assert.Equal(t, MessageOutcomeNotFoundOrNotOwned, outcome.Kind)
}

func TestMessageSendDeliversToLiveChild(t *testing.T) {
	t.Parallel()

	fixture := newMessageFixture(t)
	runner := &sinkRunner{stubRunner: &stubRunner{block: true}}
	dispatcher := sinkDispatcher{limits: fixture.manager.limits, runner: runner}
	child := fixture.startExplore(t, dispatcher, fixture.ownership())
	childID := child.child.Metadata().ID

	coordinator := fixture.coordinator(t)
	sender := fixture.rootSender()
	for _, delivery := range []MessageDelivery{
		MessageDeliverySteer, MessageDeliveryQueue, MessageDeliveryInterject,
	} {
		outcome, err := coordinator.Send(t.Context(), sender, childID, "note "+string(delivery), delivery)
		require.NoError(t, err)
		require.Equal(t, MessageOutcomeAccepted, outcome.Kind)
		assert.NotEmpty(t, outcome.MessageID)
	}

	admitted := runner.messages()
	require.Len(t, admitted, 3)
	assert.Equal(t, MessageDeliverySteer, admitted[0].delivery)
	assert.Equal(t, "note steer", admitted[0].text)
	assert.Equal(t, MessageDeliveryQueue, admitted[1].delivery)
	assert.Equal(t, MessageDeliveryInterject, admitted[2].delivery)
	for _, message := range admitted {
		assert.True(t, strings.HasPrefix(message.id, messageIDPrefix))
	}

	child.Cancel()
	_, err := child.Wait(t.Context())
	require.Error(t, err)
}

func TestMessageSendRejectsLiveTargetWithoutSink(t *testing.T) {
	t.Parallel()

	fixture := newMessageFixture(t)
	fixture.blockChildren()
	child := fixture.startExplore(t, nil, fixture.ownership())
	<-fixture.started

	outcome, err := fixture.coordinator(t).Send(
		t.Context(), fixture.rootSender(), child.child.Metadata().ID, "hello", MessageDeliverySteer,
	)
	require.NoError(t, err)
	assert.Equal(t, MessageOutcomeUnsupported, outcome.Kind)
}

func TestMessageSendParksUntilSpawningChildBecomesLive(t *testing.T) {
	t.Parallel()

	fixture := newMessageFixture(t)
	runner := &sinkRunner{stubRunner: &stubRunner{}}
	dispatcher := blockingOpenDispatcher{
		limits: fixture.manager.limits, release: make(chan struct{}), runner: runner,
	}

	var child *Execution
	var startErr error
	started := make(chan struct{})
	go func() {
		defer close(started)
		child, startErr = fixture.manager.StartWithDispatcher(t.Context(), Request{
			AgentID: string(RoleExplore), Task: "explore the code",
			Ownership: fixture.ownership(), Delivery: DeliveryForeground,
		}, nil, dispatcher)
	}()
	childID := waitForSpawningChild(t, fixture)

	coordinator := fixture.coordinator(t)
	outcome := make(chan MessageOutcome, 1)
	deliverErr := make(chan error, 1)
	go func() {
		value, err := coordinator.Send(
			t.Context(), fixture.rootSender(), childID, "wake up", MessageDeliverySteer,
		)
		deliverErr <- err
		outcome <- value
	}()

	close(dispatcher.release)
	<-started
	require.NoError(t, startErr)
	require.NotNil(t, child)

	select {
	case value := <-outcome:
		require.NoError(t, <-deliverErr)
		require.Equal(t, MessageOutcomeAccepted, value.Kind)
	case <-time.After(30 * time.Second):
		t.Fatal("parked message was never admitted")
	}
	require.Eventually(t, func() bool {
		return len(runner.messages()) == 1
	}, 30*time.Second, 10*time.Millisecond)

	child.Cancel()
	_, _ = child.Wait(t.Context())
}

// waitForSpawningChild returns the child session id of the one durable child
// record the fixture's parent already indexes.
func waitForSpawningChild(t *testing.T, fixture *managerFixture) string {
	t.Helper()

	var id string
	require.Eventually(t, func() bool {
		metas, err := fixture.repository.ListSubagents(t.Context(), fixture.parent.Metadata())
		if err != nil || len(metas) != 1 {
			return false
		}
		if fixture.manager.spawningMessageTarget(metas[0].ID) == nil {
			return false
		}
		id = metas[0].ID

		return true
	}, 30*time.Second, 10*time.Millisecond)

	return id
}

func TestMessageSendParksUntilDeadline(t *testing.T) {
	t.Parallel()

	fixture := newMessageFixture(t)
	runner := &sinkRunner{stubRunner: &stubRunner{}}
	dispatcher := blockingOpenDispatcher{
		limits: fixture.manager.limits, release: make(chan struct{}), runner: runner,
	}

	started := make(chan *Execution, 1)
	go func() {
		execution, err := fixture.manager.StartWithDispatcher(t.Context(), Request{
			AgentID: string(RoleExplore), Task: "explore the code",
			Ownership: fixture.ownership(), Delivery: DeliveryForeground,
		}, nil, dispatcher)
		if err != nil {
			started <- nil

			return
		}
		started <- execution
	}()
	childID := waitForSpawningChild(t, fixture)

	coordinator := fixture.coordinator(t)
	coordinator.limits.SpawnReady = 50 * time.Millisecond

	outcome, err := coordinator.Send(
		t.Context(), fixture.rootSender(), childID, "too late", MessageDeliverySteer,
	)
	require.NoError(t, err)
	assert.Equal(t, MessageOutcomeNotAcceptedBeforeDeadline, outcome.Kind)
	assert.Empty(t, runner.messages())

	close(dispatcher.release)
	child := <-started
	require.NotNil(t, child)
	child.Cancel()
	_, _ = child.Wait(t.Context())
}

func TestMessageSendWakesCompletedChild(t *testing.T) {
	t.Parallel()

	fixture := newMessageFixture(t)
	child := fixture.startExplore(t, nil, fixture.ownership())
	result, err := child.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, OutcomeSucceeded, result.Outcome)
	childID := child.child.Metadata().ID

	recorder := &wakeRecorder{}
	sender := NewMessageBinding(MessageSenderIdentity{
		SessionID: fixture.parent.Metadata().ID,
		AttemptID: "interaction-1",
	}, recorder.port()).Sender(messageCall())

	outcome, err := fixture.coordinator(t).Send(
		t.Context(), sender, childID, "one more fact", MessageDeliverySteer,
	)
	require.NoError(t, err)
	assert.Equal(t, MessageOutcomeAccepted, outcome.Kind)
	assert.True(t, strings.HasPrefix(outcome.MessageID, "agent-message-"))

	recorded := recorder.recorded()
	require.Len(t, recorded, 1)
	assert.Equal(t, childID, recorded[0].ChildSessionID)
	assert.Equal(t, "one more fact", recorded[0].Task)
	assert.Equal(t, string(RoleExplore), recorded[0].AgentID)
	assert.Equal(t, "openai/subagent-test", recorded[0].Model)
	assert.Equal(t, messageCall(), recorded[0].Call)
}

func TestMessageSendRefusesWakeWithoutPort(t *testing.T) {
	t.Parallel()

	fixture := newMessageFixture(t)
	child := fixture.startExplore(t, nil, fixture.ownership())
	_, err := child.Wait(t.Context())
	require.NoError(t, err)

	outcome, err := fixture.coordinator(t).Send(
		t.Context(), fixture.rootSender(), child.child.Metadata().ID,
		"one more fact", MessageDeliverySteer,
	)
	require.NoError(t, err)
	assert.Equal(t, MessageOutcomeUnsupported, outcome.Kind)
}

func TestMessageSendRefusesWakeFailure(t *testing.T) {
	t.Parallel()

	fixture := newMessageFixture(t)
	child := fixture.startExplore(t, nil, fixture.ownership())
	_, err := child.Wait(t.Context())
	require.NoError(t, err)

	recorder := &wakeRecorder{err: errors.New("wake refused")}
	sender := NewMessageBinding(MessageSenderIdentity{
		SessionID: fixture.parent.Metadata().ID, AttemptID: "interaction-1",
	}, recorder.port()).Sender(messageCall())

	outcome, err := fixture.coordinator(t).Send(
		t.Context(), sender, child.child.Metadata().ID, "one more fact", MessageDeliverySteer,
	)
	require.NoError(t, err)
	assert.Equal(t, MessageOutcomeNotActiveOrFinalizing, outcome.Kind)
}

func TestMessageSendOutboundQuotaAppliesToChildSenders(t *testing.T) {
	t.Parallel()

	fixture := newMessageFixture(t)
	runner := &sinkRunner{stubRunner: &stubRunner{block: true}}
	dispatcher := sinkDispatcher{limits: fixture.manager.limits, runner: runner}
	child := fixture.startExplore(t, dispatcher, fixture.ownership())
	childID := child.child.Metadata().ID

	coordinator := fixture.coordinator(t)
	sender := fixture.childSender("s-emitter", nil)
	for index := range MaxSenderAttemptOutbound {
		outcome, err := coordinator.Send(
			t.Context(), sender, childID, "note", MessageDeliverySteer,
		)
		require.NoError(t, err)
		require.Equalf(t, MessageOutcomeAccepted, outcome.Kind, "attempt %d", index)
	}
	outcome, err := coordinator.Send(t.Context(), sender, childID, "note", MessageDeliverySteer)
	require.NoError(t, err)
	assert.Equal(t, MessageOutcome{
		Kind: MessageOutcomeQuotaExceeded, Quota: MessageQuotaAttemptOutbound,
		Limit: MaxSenderAttemptOutbound,
	}, outcome)

	child.Cancel()
	_, _ = child.Wait(t.Context())
}

func TestMessageSendSenderTargetInFlightQuota(t *testing.T) {
	t.Parallel()

	fixture := newMessageFixture(t)
	runner := &sinkRunner{stubRunner: &stubRunner{block: true}, hold: true}
	dispatcher := sinkDispatcher{limits: fixture.manager.limits, runner: runner}
	child := fixture.startExplore(t, dispatcher, fixture.ownership())
	childID := child.child.Metadata().ID

	coordinator := fixture.coordinator(t)
	sender := fixture.childSender("s-emitter", nil)
	for index := range MaxSenderTargetInFlight {
		outcome, err := coordinator.Send(t.Context(), sender, childID, "note", MessageDeliverySteer)
		require.NoError(t, err)
		require.Equalf(t, MessageOutcomeAccepted, outcome.Kind, "in flight %d", index)
	}
	outcome, err := coordinator.Send(t.Context(), sender, childID, "note", MessageDeliverySteer)
	require.NoError(t, err)
	assert.Equal(t, MessageOutcome{
		Kind: MessageOutcomeQuotaExceeded, Quota: MessageQuotaSenderTargetInFlight,
		Limit: MaxSenderTargetInFlight,
	}, outcome)

	// Consuming one message frees exactly one in-flight slot.
	admitted := runner.messages()
	require.Len(t, admitted, MaxSenderTargetInFlight)
	admitted[0].release()

	outcome, err = coordinator.Send(t.Context(), sender, childID, "note", MessageDeliverySteer)
	require.NoError(t, err)
	assert.Equal(t, MessageOutcomeAccepted, outcome.Kind)

	for _, message := range runner.messages() {
		message.release()
	}
	child.Cancel()
	_, _ = child.Wait(t.Context())
}

func TestMessageSendSaturatedAtChild(t *testing.T) {
	t.Parallel()

	fixture := newMessageFixture(t)
	saturated := errors.New("saturated")
	runner := &saturatedSinkRunner{
		stubRunner: &stubRunner{block: true}, err: saturated,
	}
	dispatcher := saturatedDispatcher{limits: fixture.manager.limits, runner: runner}
	child := fixture.startExplore(t, dispatcher, fixture.ownership())

	outcome, err := fixture.coordinator(t).Send(
		t.Context(), fixture.rootSender(), child.child.Metadata().ID,
		"hello", MessageDeliverySteer,
	)
	require.NoError(t, err)
	assert.Equal(t, MessageOutcomeAdmissionUncertain, outcome.Kind)

	runner.err = ErrMessageSaturated
	outcome, err = fixture.coordinator(t).Send(
		t.Context(), fixture.rootSender(), child.child.Metadata().ID,
		"hello", MessageDeliverySteer,
	)
	require.NoError(t, err)
	assert.Equal(t, MessageOutcomeSaturated, outcome.Kind)
	assert.Equal(t, MaxChildMessageAdmissions, outcome.MaxInFlight)

	runner.err = ErrMessageFinalizing
	outcome, err = fixture.coordinator(t).Send(
		t.Context(), fixture.rootSender(), child.child.Metadata().ID,
		"hello", MessageDeliverySteer,
	)
	require.NoError(t, err)
	assert.Equal(t, MessageOutcomeNotActiveOrFinalizing, outcome.Kind)

	child.Cancel()
	_, _ = child.Wait(t.Context())
}

// saturatedSinkRunner reports one configured admission error.
type saturatedSinkRunner struct {
	*stubRunner
	err error
}

func (r *saturatedSinkRunner) AdmitMessage(
	context.Context,
	MessageDelivery,
	string,
	string,
	func(),
) error {
	return r.err
}

type saturatedDispatcher struct {
	limits Limits
	runner *saturatedSinkRunner
}

func (d saturatedDispatcher) AgentIDs() []string { return nil }

func (d saturatedDispatcher) Compile(ctx context.Context, request Request) (ExecutionPlan, error) {
	return stubDispatcher{limits: d.limits}.Compile(ctx, request)
}

func (d saturatedDispatcher) Open(_ context.Context, input DispatchInput) (Runner, error) {
	d.runner.child = input.Child
	d.runner.onEvent = input.OnEvent

	return d.runner, nil
}

func TestMessageSendSaturatedTreeWide(t *testing.T) {
	t.Parallel()

	fixture := newMessageFixture(t)
	runner := &sinkRunner{stubRunner: &stubRunner{block: true}, hold: true}
	dispatcher := sinkDispatcher{limits: fixture.manager.limits, runner: runner}
	child := fixture.startExplore(t, dispatcher, fixture.ownership())
	childID := child.child.Metadata().ID

	coordinator := fixture.coordinator(t)
	coordinator.limits.Admissions = 2
	for index := range 2 {
		outcome, err := coordinator.Send(
			t.Context(), fixture.childSender("s-emitter", nil), childID, "note", MessageDeliverySteer,
		)
		require.NoError(t, err)
		require.Equalf(t, MessageOutcomeAccepted, outcome.Kind, "admission %d", index)
	}
	outcome, err := coordinator.Send(
		t.Context(), fixture.childSender("s-other", nil), childID, "note", MessageDeliverySteer,
	)
	require.NoError(t, err)
	assert.Equal(t, MessageOutcomeSaturated, outcome.Kind)
	assert.Equal(t, 2, outcome.MaxInFlight)

	for _, message := range runner.messages() {
		message.release()
	}
	child.Cancel()
	_, _ = child.Wait(t.Context())
}

func TestMessageSendAdmissionUncertainOnDeadline(t *testing.T) {
	t.Parallel()

	fixture := newMessageFixture(t)
	runner := &sinkRunner{stubRunner: &stubRunner{block: true}, blockCtx: true}
	dispatcher := sinkDispatcher{limits: fixture.manager.limits, runner: runner}
	child := fixture.startExplore(t, dispatcher, fixture.ownership())

	coordinator := fixture.coordinator(t)
	coordinator.limits.SinkWait = 20 * time.Millisecond

	outcome, err := coordinator.Send(
		t.Context(), fixture.rootSender(), child.child.Metadata().ID,
		"no answer", MessageDeliverySteer,
	)
	require.NoError(t, err)
	assert.Equal(t, MessageOutcomeAdmissionUncertain, outcome.Kind)

	child.Cancel()
	_, _ = child.Wait(t.Context())
}

func TestMessageFinalizeTargetRevokesUnsettledAdmissions(t *testing.T) {
	t.Parallel()

	fixture := newMessageFixture(t)
	runner := &sinkRunner{stubRunner: &stubRunner{block: true}, hold: true}
	dispatcher := sinkDispatcher{limits: fixture.manager.limits, runner: runner}
	child := fixture.startExplore(t, dispatcher, fixture.ownership())
	childID := child.child.Metadata().ID

	coordinator := fixture.coordinator(t)
	outcome, err := coordinator.Send(
		t.Context(), fixture.childSender("s-emitter", nil), childID, "note", MessageDeliverySteer,
	)
	require.NoError(t, err)
	require.Equal(t, MessageOutcomeAccepted, outcome.Kind)

	coordinator.limits.Finalization = 20 * time.Millisecond
	assert.Equal(t, 1, coordinator.finalizeTarget(childID))
	assert.Equal(t, 0, coordinator.targetInFlight(childID))

	// A message consumed before finalization is not revoked.
	outcome, err = coordinator.Send(
		t.Context(), fixture.childSender("s-emitter", nil), childID, "note", MessageDeliverySteer,
	)
	require.NoError(t, err)
	require.Equal(t, MessageOutcomeAccepted, outcome.Kind)
	admitted := runner.messages()
	require.NotEmpty(t, admitted)
	admitted[len(admitted)-1].release()
	assert.Equal(t, 0, coordinator.finalizeTarget(childID))

	child.Cancel()
	_, _ = child.Wait(t.Context())
}

func TestMessageSendToParentUsesDeliverParentPort(t *testing.T) {
	t.Parallel()

	fixture := newMessageFixture(t)
	var (
		deliveries []MessageDelivery
		texts      []string
		fail       error
	)
	fixture.manager.shared.messages.ports.DeliverParent = func(
		_ context.Context,
		delivery MessageDelivery,
		text string,
	) error {
		deliveries = append(deliveries, delivery)
		texts = append(texts, text)

		return fail
	}

	coordinator := fixture.coordinator(t)
	sender := fixture.childSender("s-child", nil)
	for _, delivery := range []MessageDelivery{
		MessageDeliverySteer, MessageDeliveryQueue, MessageDeliveryInterject,
	} {
		outcome, err := coordinator.Send(
			t.Context(), sender, ParentMessageTarget, "for the parent", delivery,
		)
		require.NoError(t, err)
		require.Equal(t, MessageOutcomeAccepted, outcome.Kind)
	}
	assert.Equal(t, []MessageDelivery{
		MessageDeliverySteer, MessageDeliveryQueue, MessageDeliveryInterject,
	}, deliveries)
	assert.Equal(t, []string{"for the parent", "for the parent", "for the parent"}, texts)

	fail = errors.New("parent is busy")
	outcome, err := coordinator.Send(
		t.Context(), sender, ParentMessageTarget, "for the parent", MessageDeliverySteer,
	)
	require.NoError(t, err)
	assert.Equal(t, MessageOutcomeNotActiveOrFinalizing, outcome.Kind)

	// A root has no lineage, so the same literal cannot resolve for it.
	outcome, err = coordinator.Send(
		t.Context(), fixture.rootSender(), ParentMessageTarget, "hello", MessageDeliverySteer,
	)
	require.NoError(t, err)
	assert.Equal(t, MessageOutcomeNotFoundOrNotOwned, outcome.Kind)
}

func TestMessageSendToParentWithoutPortIsUnsupported(t *testing.T) {
	t.Parallel()

	fixture := newMessageFixture(t)
	fixture.manager.shared.messages.ports.DeliverParent = nil

	outcome, err := fixture.coordinator(t).Send(
		t.Context(), fixture.childSender("s-child", nil), ParentMessageTarget,
		"for the parent", MessageDeliverySteer,
	)
	require.NoError(t, err)
	assert.Equal(t, MessageOutcomeUnsupported, outcome.Kind)
}

func TestMessageSendToParentThatIsItsOwnChildIsRefused(t *testing.T) {
	t.Parallel()

	fixture := newMessageFixture(t)
	sender := NewMessageBinding(MessageSenderIdentity{
		SessionID: "s-child",
		AttemptID: "s-child",
		Child:     true,
		Ownership: Ownership{ParentSessionID: "s-child"},
	}, nil).Sender(messageCall())

	outcome, err := fixture.coordinator(t).Send(
		t.Context(), sender, ParentMessageTarget, "hello", MessageDeliverySteer,
	)
	require.NoError(t, err)
	assert.Equal(t, MessageOutcomeNotFoundOrNotOwned, outcome.Kind)
}

func TestManagerWakeRequestValidatesItsSource(t *testing.T) {
	t.Parallel()

	fixture := newMessageFixture(t)
	completed := fixture.startExplore(t, nil, fixture.ownership())
	_, err := completed.Wait(t.Context())
	require.NoError(t, err)
	completedID := completed.child.Metadata().ID

	// A live child is not a wake source. It runs on its own blocking dispatcher
	// so the completed source above stays completed.
	blocking := stubDispatcher{
		limits: fixture.manager.limits,
		newRunner: func() *stubRunner {
			return &stubRunner{block: true, onStart: func() {
				fixture.startedOne.Do(func() { close(fixture.started) })
			}}
		},
	}
	live := fixture.startExplore(t, blocking, fixture.ownership())
	<-fixture.started
	_, err = fixture.manager.StartWithDispatcher(t.Context(), Request{
		AgentID: string(RoleExplore), Task: "continue",
		Ownership: fixture.ownership(), WakeOf: live.child.Metadata().ID,
	}, nil, fixture.dispatcher)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "not completed")

	_, err = fixture.manager.StartWithDispatcher(t.Context(), Request{
		AgentID: string(RoleExplore), Task: "continue",
		Ownership: fixture.ownership(), WakeOf: "s-absent",
	}, nil, fixture.dispatcher)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "no completed child")

	// A completed child continues in the background with the message as its
	// prompt and the source's own type and model.
	execution, err := fixture.manager.StartWithDispatcher(t.Context(), Request{
		AgentID: "ignored-by-the-wake", Task: "one more fact",
		Ownership: fixture.ownership(), WakeOf: completedID, Delivery: DeliveryForeground,
	}, nil, fixture.dispatcher)
	require.NoError(t, err)
	assert.Equal(t, DeliveryBackground, execution.request.Delivery)
	assert.Equal(t, completedID, execution.request.ResumeFrom)

	woken, err := execution.Wait(t.Context())
	require.NoError(t, err)
	assert.Equal(t, OutcomeSucceeded, woken.Outcome)
	assert.NotEqual(t, completedID, woken.ChildSessionID)

	detail, err := fixture.manager.Inspect(t.Context(), woken.ChildSessionID)
	require.NoError(t, err)
	texts := make([]string, 0, len(detail.Transcript))
	for _, message := range detail.Transcript {
		texts = append(texts, messageText(message))
	}
	assert.Contains(t, texts, "explore the code")
	assert.Contains(t, texts, "one more fact")
}

func TestManagerMessageCoordinatorIsAttachedToTheTree(t *testing.T) {
	t.Parallel()

	fixture := newMessageFixture(t)
	require.NotNil(t, fixture.manager.MessageCoordinator())
	assert.Equal(t, fixture.parent.Metadata().ID, fixture.manager.rootSessionID())
}

func TestMessageAttemptBudgetIsSpentOnce(t *testing.T) {
	t.Parallel()

	budget := &messageAttemptBudget{}
	for index := range MaxSenderAttemptOutbound {
		require.Truef(t, budget.Admit(), "attempt %d", index)
	}
	assert.False(t, budget.Admit())
}

func TestParseMessageDeliveryAcceptsTheThreeVerbs(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"steer", "queue", "interject", " Steer ", " Queue "} {
		parsed, ok := ParseMessageDelivery(value)
		require.True(t, ok, value)
		assert.Equal(t, MessageDelivery(strings.ToLower(strings.TrimSpace(value))), parsed)
	}
	for _, value := range []string{"", "null", "none", "interrupt", "steering"} {
		_, ok := ParseMessageDelivery(value)
		assert.False(t, ok, value)
	}
}

func TestEligibleMessageOwnershipExcludesDirectInvocations(t *testing.T) {
	t.Parallel()

	assert.True(t, eligibleMessageOwnership(Ownership{ParentRunID: "run-1"}))
	assert.False(t, eligibleMessageOwnership(Ownership{
		ParentRunID: DirectAgentRunPrefix + "i-1",
	}))
	assert.True(t, eligibleMessageOwnership(Ownership{}))
}

var _ MessageSink = (*sinkRunner)(nil)

var _ MessageSender = (*messageSenderBinding)(nil)
