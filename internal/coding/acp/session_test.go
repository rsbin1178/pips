package acp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"iter"
	"sync"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/modelcatalog"
	"github.com/rsbin/pips/internal/coding/planreview"
	"github.com/rsbin/pips/internal/coding/question"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeController struct {
	mu sync.Mutex

	state       coding.State
	prompt      func(context.Context, ...ai.Message) runtimeSequence
	continued   runtimeSequence
	resolved    approval.Resolution
	resolveSeq  runtimeSequence
	question    question.Resolution
	questionSeq runtimeSequence
	rejected    bool
	rejectSeq   runtimeSequence
	plan        planreview.Resolution
	planSeq     runtimeSequence
	cancelCount int
	closeCount  int
	closed      bool
	mode        coding.OperatingMode
	models      []modelcatalog.Entry
	selection   modelcatalog.Selection
	switchErr   error
}

func (f *fakeController) Prompt(ctx context.Context, messages ...ai.Message) runtimeSequence {
	if f.prompt != nil {
		return f.prompt(ctx, messages...)
	}

	return eventSequence(completedEvent())
}

func (f *fakeController) Continue(context.Context) runtimeSequence { return f.continued }

func (f *fakeController) Resolve(_ context.Context, value approval.Resolution) runtimeSequence {
	f.mu.Lock()
	f.resolved = value
	f.mu.Unlock()

	return f.resolveSeq
}

func (f *fakeController) ResolveQuestion(_ context.Context, value question.Resolution) runtimeSequence {
	f.mu.Lock()
	f.question = value
	f.mu.Unlock()

	return f.questionSeq
}

func (f *fakeController) ResolvePlanReview(_ context.Context, value planreview.Resolution) runtimeSequence {
	f.mu.Lock()
	f.plan = value
	f.mu.Unlock()

	return f.planSeq
}

func (f *fakeController) RejectQuestion(context.Context, string, string) runtimeSequence {
	f.mu.Lock()
	f.rejected = true
	f.mu.Unlock()

	return f.rejectSeq
}

func (f *fakeController) Cancel() error {
	f.mu.Lock()
	f.cancelCount++
	f.mu.Unlock()

	return nil
}

func (f *fakeController) Snapshot() coding.State {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.state.Clone()
}

func (f *fakeController) SetMode(_ context.Context, mode coding.OperatingMode) error {
	f.mu.Lock()
	f.mode = mode
	f.state.Mode = mode
	f.mu.Unlock()

	return nil
}

func (f *fakeController) Models() []modelcatalog.Entry {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]modelcatalog.Entry{}, f.models...)
}

func (f *fakeController) SwitchSessionModel(
	_ context.Context,
	selection modelcatalog.Selection,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.switchErr != nil {
		return f.switchErr
	}

	f.selection = selection
	f.state.Provider = selection.Ref.Provider
	f.state.ModelID = selection.Ref.Model

	return nil
}

func (f *fakeController) Close(context.Context) error {
	f.mu.Lock()
	f.closeCount++
	f.closed = true
	f.mu.Unlock()

	return nil
}

func TestSessionRequestContextCancellationCancelsRuntime(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	controller := &fakeController{prompt: func(ctx context.Context, _ ...ai.Message) runtimeSequence {
		return func(yield func(coding.Event, error) bool) {
			close(started)
			<-ctx.Done()
			yield(coding.Event{}, ctx.Err())
		}
	}}
	active, err := newSession("session-1", controller, &fakeOutbound{}, false)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())

	type promptResult struct {
		stop acpsdk.StopReason
		err  error
	}

	result := make(chan promptResult, 1)

	go func() {
		stop, promptErr := active.prompt(ctx, ai.UserText("wait"))
		result <- promptResult{stop: stop, err: promptErr}
	}()

	<-started
	cancel()

	prompted := <-result
	require.NoError(t, prompted.err)
	assert.Equal(t, acpsdk.StopReasonCancelled, prompted.stop)
	assert.Eventually(t, func() bool {
		controller.mu.Lock()
		defer controller.mu.Unlock()

		return controller.cancelCount > 0
	}, time.Second, time.Millisecond)
}

func TestSessionModeControlSerializesAfterActivePrompt(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	controller := &fakeController{
		state: coding.State{SessionID: "session-1", Mode: coding.ModeAgent},
		prompt: func(context.Context, ...ai.Message) runtimeSequence {
			return func(yield func(coding.Event, error) bool) {
				close(started)
				<-release
				yield(completedEvent(), nil)
			}
		},
	}
	active, err := newSession("session-1", controller, &fakeOutbound{}, false)
	require.NoError(t, err)

	prompted := make(chan error, 1)

	go func() {
		_, promptErr := active.prompt(context.Background(), ai.UserText("work"))
		prompted <- promptErr
	}()

	<-started

	configured := make(chan error, 1)
	go func() { configured <- active.setMode(context.Background(), coding.ModePlan) }()

	select {
	case err := <-configured:
		t.Fatalf("mode control completed before prompt: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	require.NoError(t, <-prompted)
	require.NoError(t, <-configured)
	assert.Equal(t, coding.ModePlan, controller.Snapshot().Mode)
}

type fakeOutbound struct {
	mu sync.Mutex

	updates              []acpsdk.SessionUpdate
	permissionResponses  []acpsdk.RequestPermissionResponse
	permissionRequests   []acpsdk.RequestPermissionRequest
	elicitationResponses []createElicitationResponse
	elicitationRequests  []createElicitationRequest
}

func (f *fakeOutbound) update(
	_ context.Context,
	_ acpsdk.SessionId,
	update acpsdk.SessionUpdate,
) error {
	f.mu.Lock()
	f.updates = append(f.updates, update)
	f.mu.Unlock()

	return nil
}

func (f *fakeOutbound) requestPermission(
	_ context.Context,
	request acpsdk.RequestPermissionRequest,
) (acpsdk.RequestPermissionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.permissionRequests = append(f.permissionRequests, request)
	response := f.permissionResponses[0]
	f.permissionResponses = f.permissionResponses[1:]

	return response, nil
}

func (f *fakeOutbound) elicit(
	_ context.Context,
	request createElicitationRequest,
) (createElicitationResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.elicitationRequests = append(f.elicitationRequests, request)
	response := f.elicitationResponses[0]
	f.elicitationResponses = f.elicitationResponses[1:]

	return response, nil
}

func TestSessionProjectsEventsAndReturnsStopReason(t *testing.T) {
	t.Parallel()

	controller := &fakeController{prompt: func(context.Context, ...ai.Message) runtimeSequence {
		return eventSequence(
			coding.Event{Payload: coding.MessageDelta{Kind: ai.StreamTextDelta, Text: "done"}},
			completedEvent(),
		)
	}}
	out := &fakeOutbound{}
	active, err := newSession("session-1", controller, out, false)
	require.NoError(t, err)

	stop, err := active.prompt(t.Context(), ai.UserText("work"))
	require.NoError(t, err)
	assert.Equal(t, acpsdk.StopReasonEndTurn, stop)
	require.Len(t, out.updates, 1)
	assert.Equal(t, "done", out.updates[0].AgentMessageChunk.Content.Text.Text)
}

func TestSessionResolvesApprovalWithExactChoice(t *testing.T) {
	t.Parallel()

	controller := &fakeController{
		prompt: func(context.Context, ...ai.Message) runtimeSequence {
			return eventSequence(coding.Event{Payload: coding.ApprovalRequired{
				RequestID: "request-1", CallID: "call-1", Tool: "exec",
				Command: []string{"go", "test", "./..."}, Choices: []approval.Choice{
					approval.ChoiceAllowOnce, approval.ChoiceDeny,
				},
			}})
		},
		resolveSeq: eventSequence(completedEvent()),
	}
	out := &fakeOutbound{permissionResponses: []acpsdk.RequestPermissionResponse{{
		Outcome: acpsdk.RequestPermissionOutcome{Selected: &acpsdk.RequestPermissionOutcomeSelected{
			OptionId: acpsdk.PermissionOptionId(approval.ChoiceAllowOnce),
		}},
	}}}
	active, err := newSession("session-1", controller, out, false)
	require.NoError(t, err)

	stop, err := active.prompt(t.Context(), ai.UserText("test"))
	require.NoError(t, err)
	assert.Equal(t, acpsdk.StopReasonEndTurn, stop)
	assert.Equal(t, approval.Resolution{
		RequestID: "request-1", Choice: approval.ChoiceAllowOnce,
	}, controller.resolved)
	require.Len(t, out.permissionRequests, 1)
	rawInput := requireType[map[string]any](t, out.permissionRequests[0].ToolCall.RawInput)
	assert.Equal(t, []string{"go", "test", "./..."},
		rawInput["command"])
}

func TestSessionRejectsUnknownPermissionChoiceAndHandlesCancellation(t *testing.T) {
	t.Parallel()

	newController := func() *fakeController {
		return &fakeController{prompt: func(context.Context, ...ai.Message) runtimeSequence {
			return eventSequence(coding.Event{Payload: coding.ApprovalRequired{
				RequestID: "request-1", CallID: "call-1", Tool: "exec",
				Choices: []approval.Choice{approval.ChoiceAllowOnce, approval.ChoiceDeny},
			}})
		}}
	}

	t.Run("unknown choice", func(t *testing.T) {
		t.Parallel()

		controller := newController()
		out := &fakeOutbound{permissionResponses: []acpsdk.RequestPermissionResponse{{
			Outcome: acpsdk.RequestPermissionOutcome{Selected: &acpsdk.RequestPermissionOutcomeSelected{
				OptionId: "forged",
			}},
		}}}
		active, err := newSession("session-1", controller, out, false)
		require.NoError(t, err)

		_, err = active.prompt(t.Context(), ai.UserText("test"))
		require.ErrorIs(t, err, ErrInvalid)
		assert.Empty(t, controller.resolved.RequestID)
	})

	t.Run("cancelled", func(t *testing.T) {
		t.Parallel()

		controller := newController()
		out := &fakeOutbound{permissionResponses: []acpsdk.RequestPermissionResponse{{
			Outcome: acpsdk.RequestPermissionOutcome{
				Cancelled: &acpsdk.RequestPermissionOutcomeCancelled{},
			},
		}}}
		active, err := newSession("session-1", controller, out, false)
		require.NoError(t, err)

		stop, err := active.prompt(t.Context(), ai.UserText("test"))
		require.NoError(t, err)
		assert.Equal(t, acpsdk.StopReasonCancelled, stop)
		controller.mu.Lock()
		defer controller.mu.Unlock()

		assert.Positive(t, controller.cancelCount)
		assert.Empty(t, controller.resolved.RequestID)
	})
}

func TestSessionResolvesStructuredQuestionThroughStableElicitation(t *testing.T) {
	t.Parallel()

	request, err := question.NewRequest("request-1", "call-1", question.Spec{Questions: []question.Question{{
		Header: "Scope", Question: "Which?", Options: []question.Option{
			{Label: "One", Description: "One package"},
			{Label: "All", Description: "All packages"},
		},
	}}})
	require.NoError(t, err)

	controller := &fakeController{
		prompt: func(context.Context, ...ai.Message) runtimeSequence {
			return eventSequence(coding.Event{Payload: coding.QuestionRequired{Request: request}})
		},
		questionSeq: eventSequence(completedEvent()),
	}
	out := &fakeOutbound{elicitationResponses: []createElicitationResponse{{
		Action: "accept", Content: map[string]json.RawMessage{
			"question_1": json.RawMessage(`"One"`),
		},
	}}}
	active, err := newSession("session-1", controller, out, true)
	require.NoError(t, err)

	stop, err := active.prompt(t.Context(), ai.UserText("ask"))
	require.NoError(t, err)
	assert.Equal(t, acpsdk.StopReasonEndTurn, stop)
	assert.Equal(t, []string{"One"}, controller.question.Answers[0].Selections)
	require.Len(t, out.elicitationRequests, 1)
	assert.Equal(t, "session-1", string(out.elicitationRequests[0].SessionID))
}

func TestSessionRejectsQuestionWhenClientHasNoFormCapability(t *testing.T) {
	t.Parallel()

	request, err := question.NewFreeformRequest("request-1", "call-1", "More detail")
	require.NoError(t, err)

	controller := &fakeController{
		prompt: func(context.Context, ...ai.Message) runtimeSequence {
			return eventSequence(coding.Event{Payload: coding.QuestionRequired{Request: request}})
		},
		rejectSeq: eventSequence(completedEvent()),
	}
	out := &fakeOutbound{}
	active, err := newSession("session-1", controller, out, false)
	require.NoError(t, err)

	stop, err := active.prompt(t.Context(), ai.UserText("ask"))
	require.NoError(t, err)
	assert.Equal(t, acpsdk.StopReasonRefusal, stop)
	assert.True(t, controller.rejected)
	require.Len(t, out.updates, 1)
	assert.Contains(t,
		out.updates[0].AgentMessageChunk.Content.Text.Text,
		"cannot present the form",
	)
}

func TestSessionRejectsQuestionWhenElicitationIsCancelled(t *testing.T) {
	t.Parallel()

	request, err := question.NewFreeformRequest("request-1", "call-1", "More detail")
	require.NoError(t, err)

	controller := &fakeController{
		prompt: func(context.Context, ...ai.Message) runtimeSequence {
			return eventSequence(coding.Event{Payload: coding.QuestionRequired{Request: request}})
		},
		rejectSeq: eventSequence(completedEvent()),
	}
	out := &fakeOutbound{elicitationResponses: []createElicitationResponse{{Action: "cancel"}}}
	active, err := newSession("session-1", controller, out, true)
	require.NoError(t, err)

	stop, err := active.prompt(t.Context(), ai.UserText("ask"))
	require.NoError(t, err)
	assert.Equal(t, acpsdk.StopReasonCancelled, stop)
	assert.True(t, controller.rejected)
}

func TestSessionResolvesExactPlanRevision(t *testing.T) {
	t.Parallel()

	content := "# Plan\n\nImplement it."
	sum := sha256.Sum256([]byte(content))
	request, err := planreview.NewProposal("call-plan", hex.EncodeToString(sum[:]), content)
	require.NoError(t, err)

	controller := &fakeController{
		prompt: func(context.Context, ...ai.Message) runtimeSequence {
			return eventSequence(coding.Event{Payload: coding.PlanReviewRequired{Request: request}})
		},
		planSeq: eventSequence(completedEvent()),
	}
	out := &fakeOutbound{permissionResponses: []acpsdk.RequestPermissionResponse{{
		Outcome: acpsdk.RequestPermissionOutcome{Selected: &acpsdk.RequestPermissionOutcomeSelected{
			OptionId: "approve_agent_mode",
		}},
	}}}
	active, err := newSession("session-1", controller, out, false)
	require.NoError(t, err)

	stop, err := active.prompt(t.Context(), ai.UserText("plan"))
	require.NoError(t, err)
	assert.Equal(t, acpsdk.StopReasonEndTurn, stop)
	assert.Equal(t, planreview.Resolution{
		RequestID: request.ID, Revision: request.Revision,
		Decision: planreview.DecisionApprove,
	}, controller.plan)
	require.Len(t, out.permissionRequests, 1)
	rawInput := requireType[map[string]any](t, out.permissionRequests[0].ToolCall.RawInput)
	assert.Equal(t, request.Content,
		rawInput["plan"])
}

func TestSessionContinuesPlanningAtExactRevision(t *testing.T) {
	t.Parallel()

	content := "# Plan\n\nRevise it."
	sum := sha256.Sum256([]byte(content))
	request, err := planreview.NewProposal("call-plan", hex.EncodeToString(sum[:]), content)
	require.NoError(t, err)

	controller := &fakeController{
		prompt: func(context.Context, ...ai.Message) runtimeSequence {
			return eventSequence(coding.Event{Payload: coding.PlanReviewRequired{Request: request}})
		},
		planSeq: eventSequence(completedEvent()),
	}
	out := &fakeOutbound{permissionResponses: []acpsdk.RequestPermissionResponse{{
		Outcome: acpsdk.RequestPermissionOutcome{Selected: &acpsdk.RequestPermissionOutcomeSelected{
			OptionId: "continue_planning",
		}},
	}}}
	active, err := newSession("session-1", controller, out, false)
	require.NoError(t, err)

	stop, err := active.prompt(t.Context(), ai.UserText("plan"))
	require.NoError(t, err)
	assert.Equal(t, acpsdk.StopReasonEndTurn, stop)
	assert.Equal(t, planreview.Resolution{
		RequestID: request.ID, Revision: request.Revision,
		Decision: planreview.DecisionContinue,
	}, controller.plan)
}

func TestSessionAllowsOnlyOnePromptAndCancellationEndsIt(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	controller := &fakeController{prompt: func(ctx context.Context, _ ...ai.Message) runtimeSequence {
		return func(yield func(coding.Event, error) bool) {
			close(started)
			<-ctx.Done()
			yield(coding.Event{}, ctx.Err())
		}
	}}
	active, err := newSession("session-1", controller, &fakeOutbound{}, false)
	require.NoError(t, err)

	type promptResult struct {
		stop acpsdk.StopReason
		err  error
	}

	result := make(chan promptResult, 1)

	go func() {
		stop, promptErr := active.prompt(t.Context(), ai.UserText("wait"))
		result <- promptResult{stop: stop, err: promptErr}
	}()

	<-started

	_, err = active.prompt(t.Context(), ai.UserText("second"))
	require.ErrorIs(t, err, ErrSessionBusy)
	require.NoError(t, active.cancel())

	first := <-result
	require.NoError(t, first.err)
	assert.Equal(t, acpsdk.StopReasonCancelled, first.stop)
	controller.mu.Lock()
	cancelCount := controller.cancelCount
	controller.mu.Unlock()
	assert.Positive(t, cancelCount)
}

func eventSequence(events ...coding.Event) iter.Seq2[coding.Event, error] {
	return func(yield func(coding.Event, error) bool) {
		for _, event := range events {
			if !yield(event, nil) {
				return
			}
		}
	}
}

func completedEvent() coding.Event {
	return coding.Event{Payload: coding.InteractionCompleted{
		Outcome: coding.InteractionSucceeded, Stop: agent.StopEndTurn,
	}}
}

var (
	_ Controller = (*fakeController)(nil)
	_ outbound   = (*fakeOutbound)(nil)
)
