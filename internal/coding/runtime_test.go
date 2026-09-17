//nolint:wsl_v5 // Integration tests group setup, stream execution, and state assertions.
package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/agent/extension"
	"github.com/rsbin1178/pips/agent/harness"
	agentobservability "github.com/rsbin1178/pips/agent/observability"
	agentotel "github.com/rsbin1178/pips/agent/observability/otel"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/approval"
	"github.com/rsbin1178/pips/internal/coding/changes"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/coding/modelcatalog"
	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfiguredSubagentOptionsUseConfigAndPreserveExplicitOverrides(t *testing.T) {
	t.Parallel()

	configured := config.DefaultSubagentConfig()
	configured.MaxDepth = 2
	configured.MaxConcurrent = 6
	configured.MaxTurns = 80
	configured.MaxDurationMinutes = 45
	options := configuredSubagentOptions(configured, subagent.ExecutionOptions{})
	assert.Equal(t, 6, options.MaxConcurrent)
	assert.Equal(t, 2, options.MaxDepth)
	assert.Equal(t, 80, options.Limits.MaxTurns)
	assert.Equal(t, 2, options.Limits.FinalizationTurns)
	assert.Equal(t, 45*time.Minute, options.Limits.MaxDuration)

	unbounded := subagent.DefaultLimits()
	overridden := configuredSubagentOptions(configured, subagent.ExecutionOptions{
		Limits: unbounded, MaxDepth: 1, MaxConcurrent: 1,
	})
	assert.Equal(t, 1, overridden.MaxConcurrent)
	assert.Equal(t, 1, overridden.MaxDepth)
	assert.Equal(t, unbounded, overridden.Limits)
	assert.Equal(t, configured.MaxSpawnedPerRootInteraction, overridden.MaxSpawnedPerRootInteraction)
	assert.Equal(
		t,
		subagent.ProductionLimits(),
		configuredSubagentOptions(config.DefaultSubagentConfig(), subagent.ExecutionOptions{}).Limits,
	)
}

func TestRuntimePromptStreamsAndPersistsOneInteraction(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel(runtimeTextResponse("done")))

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("hello")))
	require.NotEmpty(t, events)
	assertEventSequence(t, events)
	assert.Contains(t, eventTypes(events), EventInteractionStarted)
	assert.Contains(t, eventTypes(events), EventRunCompleted)
	assert.Contains(t, eventTypes(events), EventInteractionCompleted)
	assert.NotContains(t, eventTypes(events), EventWorkspaceChanged)
	assert.Zero(t, countDiagnostic(events, "changes", "report_failed"))
	assert.Zero(t, countDiagnostic(events, "changes", "not_repository"))

	snapshot := runtime.Snapshot()
	assert.Equal(t, PhaseIdle, snapshot.Phase)
	assert.False(t, snapshot.Interaction.Active)
	assert.Equal(t, InteractionSucceeded, snapshot.Interaction.Outcome)
	require.Len(t, snapshot.Transcript, 2)
	assert.IsType(t, ai.UserMessage{}, snapshot.Transcript[0])
	assert.IsType(t, ai.AssistantMessage{}, snapshot.Transcript[1])

	require.NoError(t, runtime.Close(t.Context()))
	assert.Equal(t, PhaseClosed, runtime.Snapshot().Phase)
	require.NoError(t, runtime.Close(t.Context()))
}

func TestEffectiveCompactionSettingsUseOneResolvedPolicy(t *testing.T) {
	t.Parallel()

	configured := config.CompactionConfig{
		Enabled: true, ReserveTokens: 100, KeepRecentTokens: 900, SummaryMaxTokens: 64,
	}
	resolved := modelcatalog.ResolvedModel{
		Limits:  modelcatalog.Limits{ContextWindow: 1000},
		Options: config.ModelOptions{MaxOutputTokens: ai.Ptr(400)},
	}

	settings, disabled := effectiveCompactionSettings(configured, resolved)
	assert.Empty(t, disabled)
	assert.Equal(t, 1000, settings.ContextTokens)
	assert.Equal(t, 400, settings.ReserveTokens)
	assert.Equal(t, 450, settings.KeepRecentTokens)
	assert.Equal(t, 64, settings.SummaryTokens)

	configured.Enabled = false
	_, disabled = effectiveCompactionSettings(configured, resolved)
	assert.Equal(t, "automatic and manual compaction are disabled", disabled)

	configured.Enabled = true
	resolved.Limits.ContextWindow = 0
	_, disabled = effectiveCompactionSettings(configured, resolved)
	assert.Equal(t, "the selected model has no context_window metadata", disabled)
}

func TestRuntimeMainAgentRunsPastGenericTurnDefault(t *testing.T) {
	t.Parallel()

	responses := make([]*ai.Response, 0, agent.DefaultMaxTurns+2)
	for turn := 1; turn <= agent.DefaultMaxTurns+1; turn++ {
		responses = append(responses, runtimeToolResponse(
			fmt.Sprintf("call-%d", turn), "ls", `{"path":"."}`,
		))
	}
	responses = append(responses, runtimeTextResponse("done after the default boundary"))

	model := newRuntimeModel(responses...)
	runtime := openTestRuntime(t, model)
	configureRuntimeCompaction(runtime, 1_000_000, 100, 100, 64)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("inspect repeatedly")))
	assert.Equal(t, 1, countEventType(events, EventRunStarted))
	assert.Equal(t, 1, countEventType(events, EventRunCompleted))
	assert.Zero(t, countEventType(events, EventCompactionStarted))
	assert.Len(t, model.Requests(), agent.DefaultMaxTurns+2)

	state := runtime.Snapshot()
	assert.Equal(t, InteractionSucceeded, state.Interaction.Outcome)
	assert.Equal(t, agent.StopEndTurn, state.Interaction.Stop)
	assert.Equal(t, agent.DefaultMaxTurns+2, state.Runs[len(state.Runs)-1].Turn)
}

func TestRuntimeCompactsUnlimitedRunOnlyAfterContextThreshold(t *testing.T) {
	t.Parallel()

	toolResponse := runtimeToolResponse("call-read", "read", `{"path":"large.txt"}`)
	toolResponse.Usage.InputTokens = 1_800
	model := newRuntimeModel(
		toolResponse,
		runtimeTextResponse("## Goal\nPreserve the earlier work."),
		runtimeTextResponse("done after compaction"),
	)
	runtime := openTestRuntime(t, model)
	appendRuntimeHistory(t, runtime, 425, 425, 425, 425)
	require.NoError(t, os.WriteFile(
		filepath.Join(runtime.workspace.Root(), "large.txt"),
		[]byte("small file"),
		0o600,
	))
	configureRuntimeCompaction(runtime, 2_000, 300, 2_100, 64)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("read the file")))
	types := eventTypes(events)
	compactIndex := slices.Index(types, EventCompactionStarted)
	require.Greater(t, compactIndex, slices.Index(types, EventTurnCompleted))
	nextTurnOffset := slices.Index(types[compactIndex+1:], EventTurnStarted)
	require.GreaterOrEqual(t, nextTurnOffset, 0)
	assert.Equal(t, 1, countEventType(events, EventCompactionStarted))
	assert.Equal(t, 1, countEventType(events, EventRunStarted))
	assert.Equal(t, 1, countEventType(events, EventRunCompleted))
	assert.Equal(t, 1, countHarnessKind(runtime.session.Path(), harness.KindCompaction))
	assert.Len(t, model.Requests(), 3)
	assert.Equal(t, InteractionSucceeded, runtime.Snapshot().Interaction.Outcome)
}

func TestTerminalInteractionOutcomePreservesNonNaturalStops(t *testing.T) {
	t.Parallel()

	tests := []struct {
		stop     agent.StopReason
		outcome  InteractionOutcome
		terminal bool
	}{
		{stop: agent.StopEndTurn, outcome: InteractionSucceeded, terminal: true},
		{stop: agent.StopTerminated, outcome: InteractionSucceeded, terminal: true},
		{stop: agent.StopPaused},
		{stop: agent.StopMaxTurns, outcome: InteractionIncomplete, terminal: true},
		{stop: agent.StopBudget, outcome: InteractionIncomplete, terminal: true},
		{stop: agent.StopWhen, outcome: InteractionIncomplete, terminal: true},
	}

	for _, test := range tests {
		outcome, terminal := terminalInteractionOutcome(test.stop)
		assert.Equal(t, test.outcome, outcome, test.stop)
		assert.Equal(t, test.terminal, terminal, test.stop)
	}
}

func TestRuntimeObservationMatchesOperationIterator(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel(runtimeTextResponse("done")))
	observation, err := runtime.ObserveEvents()
	require.NoError(t, err)
	t.Cleanup(observation.Subscription.Close)
	assert.Equal(t, runtime.Snapshot(), observation.State)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("hello")))
	records := make([]EventRecord, 0, len(events))
	for range events {
		select {
		case record := <-observation.Subscription.Events():
			records = append(records, record)
		case <-time.After(time.Second):
			require.FailNow(t, "timed out waiting for observed event")
		}
	}

	require.Len(t, records, len(events))
	for index, event := range events {
		assert.Equal(t, event, records[index].Event)
		assert.Equal(t, observation.Cursor+uint64(index)+1, records[index].Cursor)
	}
}

func TestRuntimeSlowObservationDoesNotStopPrompt(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel(runtimeTextResponse("done")))
	runtime.publisher.mu.Lock()
	runtime.publisher.hub.close()
	runtime.publisher.hub = newEventHub(8, 1)
	runtime.publisher.mu.Unlock()

	observation, err := runtime.ObserveEvents()
	require.NoError(t, err)
	t.Cleanup(observation.Subscription.Close)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("hello")))
	assert.Contains(t, eventTypes(events), EventInteractionCompleted)
	assert.Equal(t, InteractionSucceeded, runtime.Snapshot().Interaction.Outcome)

	for range observation.Subscription.Events() {
		continue
	}
	assert.ErrorIs(t, observation.Subscription.Err(), ErrEventGap)
}

func TestRuntimeClosingSubscriptionDoesNotCancelPrompt(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel(runtimeTextResponse("done")))
	observation, err := runtime.ObserveEvents()
	require.NoError(t, err)
	observation.Subscription.Close()

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("hello")))
	assert.Contains(t, eventTypes(events), EventInteractionCompleted)
	assert.Equal(t, InteractionSucceeded, runtime.Snapshot().Interaction.Outcome)
}

func TestRuntimeClosePublishesTerminalEventsBeforeClosingSubscription(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel())
	observation, err := runtime.ObserveEvents()
	require.NoError(t, err)

	require.NoError(t, runtime.Close(t.Context()))
	var types []EventType
	for record := range observation.Subscription.Events() {
		types = append(types, record.Event.Type)
	}

	require.NoError(t, observation.Subscription.Err())
	require.GreaterOrEqual(t, len(types), 2)
	assert.Equal(t, []EventType{EventStatusChanged, EventSessionClosed}, types[len(types)-2:])
}

func TestRuntimeCloseTimeoutKeepsOneCleanupOwnerRunning(t *testing.T) {
	t.Parallel()

	model := newDelayedCancelRuntimeModel()
	runtime := openTestRuntime(t, model)
	promptDone := make(chan struct{})
	go func() {
		defer close(promptDone)
		for range runtime.Prompt(context.Background(), ai.UserText("wait for shutdown")) {
		}
	}()
	select {
	case <-model.started:
	case <-time.After(3 * time.Second):
		t.Fatal("model did not start")
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	assert.ErrorIs(t, runtime.Close(closeCtx), context.DeadlineExceeded)

	results := make(chan error, 2)
	go func() { results <- runtime.Close(context.Background()) }()
	go func() { results <- runtime.Close(context.Background()) }()
	close(model.release)
	for range 2 {
		require.NoError(t, <-results)
	}
	select {
	case <-promptDone:
	case <-time.After(3 * time.Second):
		t.Fatal("prompt did not stop")
	}
	assert.Equal(t, PhaseClosed, runtime.Snapshot().Phase)
}

func TestRuntimeDeliversIdleAgentNotificationAsSyntheticInteraction(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(runtimeTextResponse("completion handled"))
	runtime := openTestRuntime(t, model)
	notification := testAgentNotification(runtime, "s-child-idle", "root-idle")
	require.NoError(t, runtime.notifications.Enqueue(notification))
	runtime.signalNotifications()

	require.Eventually(t, func() bool {
		pending, err := runtime.notifications.Pending()
		return err == nil && len(pending) == 0 && runtime.Snapshot().Phase == PhaseIdle &&
			len(model.Requests()) == 1
	}, 30*time.Second, 20*time.Millisecond)

	state := runtime.Snapshot()
	assert.Equal(t, InteractionSourceAgentNotification, state.Interaction.Source)
	assert.Equal(t, "root-idle", state.Interaction.RootInteractionID)
	require.Len(t, state.Transcript, 2)
	assert.Equal(t, []int{0}, state.SyntheticMessages)
	assert.IsType(t, ai.UserMessage{}, state.Transcript[0])
	assert.IsType(t, ai.AssistantMessage{}, state.Transcript[1])

	requests := model.Requests()
	require.Len(t, requests, 1)
	assert.True(t, requestContainsText(requests[0], agentNotificationSchema))
}

func TestRuntimeHoldsAgentNotificationWhilePaused(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(runtimeTextResponse("completion handled"))
	runtime := openTestRuntime(t, model)
	runtime.mu.Lock()
	runtime.state.Phase = PhasePaused
	runtime.mu.Unlock()

	notification := testAgentNotification(runtime, "s-child-paused", "root-paused")
	require.NoError(t, runtime.notifications.Enqueue(notification))
	runtime.signalNotifications()
	time.Sleep(50 * time.Millisecond)
	pending, err := runtime.notifications.Pending()
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Empty(t, model.Requests())

	runtime.mu.Lock()
	runtime.state.Phase = PhaseIdle
	runtime.mu.Unlock()
	runtime.signalNotifications()
	require.Eventually(t, func() bool {
		pending, pendingErr := runtime.notifications.Pending()
		return pendingErr == nil && len(pending) == 0 && runtime.Snapshot().Phase == PhaseIdle &&
			len(model.Requests()) == 1
	}, 30*time.Second, 20*time.Millisecond)
	assert.Len(t, model.Requests(), 1)
}

func TestRuntimeBatchesAgentNotificationsForSameRoot(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(runtimeTextResponse("both completions handled"))
	runtime := openTestRuntime(t, model)

	// The coordinator wakes on every signal, including the one its own startup
	// raises. A wakeup landing between the two Enqueues delivers the first
	// notification alone and the second in a later batch: correct delivery, but
	// not the batching this test is about. Park the coordinator, make the
	// pending set atomic, then let one wakeup claim it.
	require.NoError(t, runtime.stopNotificationCoordinator(t.Context()))

	first := testAgentNotification(runtime, "s-child-batch-1", "root-batch")
	second := testAgentNotification(runtime, "s-child-batch-2", "root-batch")
	require.NoError(t, runtime.notifications.Enqueue(first))
	require.NoError(t, runtime.notifications.Enqueue(second))
	runtime.startNotificationCoordinator(t.Context())

	require.Eventually(t, func() bool {
		pending, err := runtime.notifications.Pending()
		return err == nil && len(pending) == 0 && runtime.Snapshot().Phase == PhaseIdle &&
			len(model.Requests()) == 1
	}, 30*time.Second, 20*time.Millisecond)
	requests := model.Requests()
	require.Len(t, requests, 1)
	assert.True(t, requestContainsText(requests[0], first.AgentID))
	assert.True(t, requestContainsText(requests[0], second.AgentID))
	assert.Equal(t, []int{0}, runtime.Snapshot().SyntheticMessages)
}

// TestNextNotificationBatchGroupsOneRoot pins the batching rule itself, with no
// dependence on when the coordinator happens to wake: one claim takes every
// pending notification of the first root, in order, and leaves other roots for
// a later claim.
func TestNextNotificationBatchGroupsOneRoot(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(runtimeTextResponse("unused"))
	runtime := openTestRuntime(t, model)
	first := testAgentNotification(runtime, "s-batch-1", "root-a")
	second := testAgentNotification(runtime, "s-batch-2", "root-a")
	other := testAgentNotification(runtime, "s-batch-3", "root-b")

	batch := runtime.nextNotificationBatch([]subagent.Notification{first, second, other})

	assert.Equal(t, []string{"s-batch-1", "s-batch-2"}, notificationIDs(batch))

	// The other root is claimed on its own once the first batch is delivered.
	assert.Equal(t, []string{"s-batch-3"}, notificationIDs(
		runtime.nextNotificationBatch([]subagent.Notification{other}),
	))
}

func TestRuntimeQueuesAgentNotificationIntoMatchingActiveInteraction(t *testing.T) {
	t.Parallel()

	model := newNotificationFollowUpModel()
	runtime := openTestRuntime(t, model)
	type promptResult struct {
		events []Event
		err    error
	}
	done := make(chan promptResult, 1)
	go func() {
		var result promptResult
		for event, err := range runtime.Prompt(t.Context(), ai.UserText("start")) {
			if err != nil {
				result.err = err
				break
			}
			result.events = append(result.events, event)
		}
		done <- result
	}()
	select {
	case <-model.started:
	case <-time.After(time.Second):
		require.FailNow(t, "parent model did not start")
	}

	runtime.mu.Lock()
	require.NotNil(t, runtime.interaction)
	rootID := runtime.interaction.rootInteractionID
	runtime.mu.Unlock()
	notification := testAgentNotification(runtime, "s-child-active", rootID)
	require.NoError(t, runtime.notifications.Enqueue(notification))
	runtime.signalNotifications()
	require.Eventually(t, func() bool {
		runtime.notificationMu.Lock()
		defer runtime.notificationMu.Unlock()
		_, ok := runtime.notificationInflight[notification.ID]
		return ok
	}, time.Second, 5*time.Millisecond)
	close(model.release)

	select {
	case result := <-done:
		require.NoError(t, result.err)
		assert.Contains(t, eventTypes(result.events), EventInteractionCompleted)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "active notification interaction did not complete")
	}
	require.Eventually(t, func() bool {
		pending, err := runtime.notifications.Pending()
		return err == nil && len(pending) == 0
	}, time.Second, 5*time.Millisecond)

	state := runtime.Snapshot()
	require.Len(t, state.Transcript, 4)
	assert.Equal(t, []int{2}, state.SyntheticMessages)
	assert.Equal(t, "initial answer", runtimeMessageText(state.Transcript[1]))
	assert.Equal(t, "completion handled", runtimeMessageText(state.Transcript[3]))
}

func TestRuntimeSpawnAgentCompletesAfterParentAndAutomaticallyContinues(t *testing.T) {
	t.Parallel()

	model := newBackgroundSpawnRuntimeModel()
	runtime := openTestRuntime(t, model)
	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("delegate")))
	assert.Contains(t, eventTypes(events), EventInteractionCompleted)
	select {
	case <-model.childStarted:
	case <-time.After(time.Second):
		require.FailNow(t, "background child did not start")
	}
	assert.Equal(t, PhaseIdle, runtime.Snapshot().Phase)

	close(model.releaseChild)
	require.Eventually(t, func() bool {
		state := runtime.Snapshot()
		pending, err := runtime.notifications.Pending()
		return err == nil && len(pending) == 0 && state.Phase == PhaseIdle &&
			len(state.Subagents) == 1 && state.Subagents[0].State == subagent.StateSucceeded &&
			len(state.Transcript) == 6
	}, 3*time.Second, 10*time.Millisecond)

	state := runtime.Snapshot()
	assert.Equal(t, subagent.DeliveryBackground, state.Subagents[0].Delivery)
	assert.Equal(t, []int{4}, state.SyntheticMessages)
	assert.Equal(t, "background completion handled", runtimeMessageText(state.Transcript[5]))
	assert.Equal(t, 3, model.MainCalls())
}

func testAgentNotification(
	runtime *Runtime,
	agentID string,
	rootID string,
) subagent.Notification {
	return subagent.Notification{
		ID: agentID, AgentID: agentID,
		Ownership: subagent.Ownership{
			ParentSessionID:     runtime.handle.Metadata().ID,
			ParentInteractionID: rootID,
			ParentRunID:         "run-parent", ParentToolCallID: "call-parent",
			RootInteractionID: rootID,
		},
		Role: subagent.RoleExplore, Outcome: subagent.OutcomeSucceeded,
		Code: "ok", TaskPreview: "inspect runtime",
		Result: ai.JSON(`{"summary":"done","evidence":[],"unknowns":[]}`),
		Usage:  ai.Usage{InputTokens: 12, OutputTokens: 4}, TerminalAt: time.Now().UTC(),
	}
}

func TestRuntimeRunsReadOnlySubagentWithoutProjectingChildTranscript(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeToolResponse(
			"delegate-1",
			"run_subagent",
			`{"role":"explore","task":"Locate the runtime composition root."}`,
		),
		runtimeTextResponse(`{"summary":"located","evidence":[],"unknowns":[]}`),
		runtimeTextResponse("parent answer"),
	)
	telemetry := make([]TelemetryEvent, 0)
	runtime := openTestRuntimeWithTelemetry(
		t,
		model,
		TelemetryObserverFunc(func(_ context.Context, event TelemetryEvent) error {
			telemetry = append(telemetry, event)

			return nil
		}),
	)
	observation, err := runtime.ObserveEvents()
	require.NoError(t, err)
	t.Cleanup(observation.Subscription.Close)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("inspect")))
	types := eventTypes(events)
	assert.Contains(t, types, EventSubagentCreated)
	assert.Contains(t, types, EventSubagentStarted)
	assert.Contains(t, types, EventSubagentProgress)
	assert.Contains(t, types, EventSubagentCompleted)
	for _, event := range events {
		if delta, ok := event.Payload.(MessageDelta); ok {
			assert.NotContains(t, delta.Text, `"summary":"located"`)
		}
	}

	snapshot := runtime.Snapshot()
	require.Len(t, snapshot.Subagents, 1)
	child := snapshot.Subagents[0]
	assert.Equal(t, subagent.RoleExplore, child.Role)
	assert.Equal(t, subagent.StateSucceeded, child.State)
	assert.NotEmpty(t, child.ChildSessionID)
	assert.NotEmpty(t, child.ParentRunID)
	assert.NotEmpty(t, child.ChildRunID)
	assert.Equal(t, snapshot.Interaction.ID, child.ParentInteractionID)
	assert.Equal(t, "delegate-1", child.ParentToolCallID)
	assert.Equal(t, snapshot.Interaction.ID, child.RootInteractionID)
	assert.Equal(t, subagent.DeliveryForeground, child.Delivery)
	assert.Equal(t, TokenUsage{
		InputTokens: 30, OutputTokens: 6,
	}, snapshot.Interaction.Usage)

	requests := model.Requests()
	require.Len(t, requests, 3)
	assert.Contains(t, toolNamesFromRequest(requests[0]), "run_subagent")
	assert.Equal(t, []string{"read", "ls", "glob", "grep"}, toolNamesFromRequest(requests[1]))
	assert.Contains(t, toolNamesFromRequest(requests[2]), "run_subagent")

	summaries, err := runtime.ListSubagents(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	detail, err := runtime.InspectSubagent(t.Context(), summaries[0].ChildSessionID)
	require.NoError(t, err)
	assert.IsType(t, subagent.ExploreResult{}, detail.Result)
	require.Len(t, detail.Transcript, 2)
	assert.IsType(t, ai.UserMessage{}, detail.Transcript[0])
	assert.IsType(t, ai.AssistantMessage{}, detail.Transcript[1])
	childState, err := runtime.InspectSubagentState(t.Context(), child.ChildSessionID)
	require.NoError(t, err)
	waited, err := runtime.WaitSubagent(t.Context(), child.ChildSessionID)
	require.NoError(t, err)
	assert.Equal(t, child.ChildSessionID, waited.ChildSessionID)
	assert.Equal(t, subagent.OutcomeSucceeded, waited.Outcome)
	require.NoError(t, runtime.CancelSubagent(t.Context(), child.ChildSessionID))
	assert.Equal(t, child.ChildSessionID, childState.SessionID)
	assert.Equal(t, InteractionSucceeded, childState.Interaction.Outcome)
	assert.False(t, childState.Interaction.Active)
	require.Len(t, childState.Transcript, 2)
	assert.IsType(t, ai.UserMessage{}, childState.Transcript[0])
	assert.IsType(t, ai.AssistantMessage{}, childState.Transcript[1])
	require.Len(t, childState.Runs, 1)
	assert.False(t, childState.Runs[0].Active)

	after, err := runtime.ObserveEvents()
	require.NoError(t, err)
	after.Subscription.Close()
	// The in-memory test hub bounds this delta well below the platform int limit.
	count := int(after.Cursor - observation.Cursor) //nolint:gosec // Test hub capacity bounds the delta.
	records := receiveRuntimeRecords(t, observation.Subscription, count)
	childEventTypes := make([]EventType, 0)
	for _, record := range records {
		if record.Event.SessionID == child.ChildSessionID {
			childEventTypes = append(childEventTypes, record.Event.Type)
		}
	}
	assert.Contains(t, childEventTypes, EventSessionOpened)
	assert.Contains(t, childEventTypes, EventMessageCommitted)
	assert.Contains(t, childEventTypes, EventInteractionCompleted)

	for _, event := range telemetry {
		encoded := fmt.Sprintf("%#v", event)
		assert.NotContains(t, encoded, "Locate the runtime")
		assert.NotContains(t, encoded, child.ChildSessionID)
		assert.NotContains(t, encoded, child.ChildRunID)
	}
}

func receiveRuntimeRecords(
	t *testing.T,
	subscription *EventSubscription,
	count int,
) []EventRecord {
	t.Helper()

	records := make([]EventRecord, 0, count)
	for range count {
		select {
		case record, ok := <-subscription.Events():
			require.True(t, ok)
			records = append(records, record)
		case <-time.After(time.Second):
			require.FailNow(t, "timed out waiting for Runtime event record")
		}
	}

	return records
}

func TestRuntimeOpensCustomProviderMetadata(t *testing.T) {
	t.Parallel()

	provider := ai.Provider("opencode-go")
	runtime := openTestRuntime(
		t,
		newRuntimeModelFor(provider, "deepseek-v4.1-flash"),
	)

	snapshot := runtime.Snapshot()
	assert.Equal(t, provider, snapshot.Provider)
	assert.Equal(t, "deepseek-v4.1-flash", snapshot.ModelID)
}

func TestValidateOpenOptionsRejectsNilObservers(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	workspacePath := base + "/workspace"
	require.NoError(t, mkdirPrivate(workspacePath))
	ws, err := workspace.Open(workspacePath)
	require.NoError(t, err)
	layout, err := paths.New(base + "/home")
	require.NoError(t, err)
	cfg := config.Defaults()
	cfg.Model.Provider = ai.ProviderOpenAI
	cfg.Model.Model = "runtime-test"
	options := OpenOptions{Workspace: ws, Config: cfg, Paths: layout}

	options.AgentObservers = []func(context.Context, agent.Event){nil}
	require.ErrorIs(t, validateOpenOptions(options), ErrRuntimeInvalid)
	options.AgentObservers = nil
	options.TelemetryObservers = []TelemetryObserver{nil}
	require.ErrorIs(t, validateOpenOptions(options), ErrRuntimeInvalid)
}

func TestRuntimeTelemetryObserverFailuresAreIsolated(t *testing.T) {
	t.Parallel()

	const privatePayload = "telemetry-observer-private-payload"

	var mu sync.Mutex
	errorCalls, panicCalls := 0, 0
	observed := make([]TelemetryEvent, 0, 16)
	runtime := openTestRuntimeWithTelemetry(
		t,
		newRuntimeModel(runtimeTextResponse("done")),
		TelemetryObserverFunc(func(_ context.Context, event TelemetryEvent) error {
			if event.Type != EventInteractionStarted {
				return nil
			}

			mu.Lock()
			errorCalls++
			mu.Unlock()

			return errors.New(privatePayload)
		}),
		TelemetryObserverFunc(func(_ context.Context, event TelemetryEvent) error {
			if event.Type != EventInteractionStarted {
				return nil
			}

			mu.Lock()
			panicCalls++
			mu.Unlock()
			panic(privatePayload)
		}),
		TelemetryObserverFunc(func(_ context.Context, event TelemetryEvent) error {
			mu.Lock()
			observed = append(observed, event)
			mu.Unlock()

			return nil
		}),
	)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("hello")))
	assert.Equal(t, InteractionSucceeded, runtime.Snapshot().Interaction.Outcome)
	assert.Equal(t, 2, countDiagnostic(events, componentTelemetry, "observer_disabled"))
	for _, event := range events {
		diagnostic, ok := event.Payload.(IntegrationDiagnostic)
		if ok {
			assert.NotContains(t, diagnostic.Message, privatePayload)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, errorCalls)
	assert.Equal(t, 1, panicCalls)
	assert.Contains(t, telemetryTypes(observed), EventSessionOpened)
	assert.Contains(t, telemetryTypes(observed), EventInteractionCompleted)
}

func TestRuntimeProjectsContentFreeSubagentAdmissionTelemetry(t *testing.T) {
	t.Parallel()

	observed := make([]TelemetryEvent, 0, 1)
	runtime := openTestRuntimeWithTelemetry(
		t,
		newRuntimeModel(runtimeTextResponse("unused")),
		TelemetryObserverFunc(func(_ context.Context, event TelemetryEvent) error {
			if event.Signal == TelemetrySignalSubagentAdmission {
				observed = append(observed, event)
			}

			return nil
		}),
	)
	runtime.observeSubagentAdmission(t.Context(), subagent.AdmissionEvent{
		Outcome:         subagent.AdmissionOutcomeRejected,
		Reason:          subagent.AdmissionReasonCapacity,
		Delivery:        subagent.DeliveryBackground,
		DelegationDepth: 2,
	})

	require.Len(t, observed, 1)
	assert.Equal(t, TelemetrySignalSubagentAdmission, observed[0].Signal)
	assert.Equal(t, subagent.AdmissionOutcomeRejected, observed[0].AdmissionOutcome)
	assert.Equal(t, subagent.AdmissionReasonCapacity, observed[0].AdmissionReason)
	assert.Equal(t, subagent.DeliveryBackground, observed[0].SubagentDelivery)
	assert.Equal(t, 2, observed[0].SubagentDepth)
	encoded, err := json.Marshal(observed[0])
	require.NoError(t, err)
	for _, forbidden := range []string{"session_id", "run_id", "agent_id", "task", "path", "command"} {
		assert.NotContains(t, string(encoded), forbidden)
	}
}

func TestRuntimeTelemetrySessionOpenFailureBecomesSnapshotDiagnostic(t *testing.T) {
	t.Parallel()

	calls := 0
	runtime := openTestRuntimeWithTelemetry(
		t,
		newRuntimeModel(runtimeTextResponse("done")),
		TelemetryObserverFunc(func(context.Context, TelemetryEvent) error {
			calls++

			return errors.New("session exporter unavailable")
		}),
	)

	snapshot := runtime.Snapshot()
	require.Len(t, snapshot.Diagnostics, 1)
	assert.Equal(t, componentTelemetry, snapshot.Diagnostics[0].Component)
	assert.Equal(t, "observer_disabled", snapshot.Diagnostics[0].Code)

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("hello")))
	assert.Equal(t, 1, calls)
	assert.Equal(t, InteractionSucceeded, runtime.Snapshot().Interaction.Outcome)
}

func TestRuntimeTelemetryObserverBackpressuresSynchronously(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	release := make(chan struct{})
	observer := TelemetryObserverFunc(func(_ context.Context, event TelemetryEvent) error {
		if event.Type == EventInteractionStarted {
			close(entered)
			<-release
		}

		return nil
	})
	runtime := openTestRuntimeWithTelemetry(
		t,
		newRuntimeModel(runtimeTextResponse("done")),
		observer,
	)

	type result struct {
		events []Event
		err    error
	}
	done := make(chan result, 1)
	go func() {
		value := result{}
		runtime.Prompt(t.Context(), ai.UserText("hello"))(func(event Event, err error) bool {
			if err != nil {
				value.err = err

				return false
			}
			value.events = append(value.events, event)

			return true
		})
		done <- value
	}()

	<-entered
	select {
	case <-done:
		t.Fatal("prompt completed while the synchronous observer was blocked")
	default:
	}
	close(release)

	value := <-done
	require.NoError(t, value.err)
	assert.Contains(t, eventTypes(value.events), EventInteractionCompleted)
	assert.Equal(t, InteractionSucceeded, runtime.Snapshot().Interaction.Outcome)
}

func TestRuntimeComposesRawAgentObserversAtHarnessBoundary(t *testing.T) {
	t.Parallel()

	recorder := agentobservability.NewRecorder()
	otelObserver, err := agentotel.New(agentotel.Config{})
	require.NoError(t, err)
	runtime := openTestRuntimeWithAgentObservers(
		t,
		newRuntimeModel(runtimeTextResponse("done")),
		recorder.Observe,
		otelObserver.Observe,
	)

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("hello")))
	metrics := recorder.Metrics()
	assert.Equal(t, uint64(1), metrics.RunsStarted)
	assert.Equal(t, uint64(1), metrics.RunsCompleted)
	assert.Equal(t, InteractionSucceeded, runtime.Snapshot().Interaction.Outcome)
}

func TestRuntimeApprovalPauseAndDenyContinuation(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeToolResponse(
			"call-shell",
			"shell",
			`{"command":"printf ok","permissions":{"write_paths":[],"network":true},"justification":"test"}`,
		),
		runtimeTextResponse("continued"),
	)
	runtime := openTestRuntime(t, model)

	promptEvents := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("run")))
	assert.Contains(t, eventTypes(promptEvents), EventApprovalRequired)

	paused := runtime.Snapshot()
	assert.Equal(t, PhasePaused, paused.Phase)
	require.NotNil(t, paused.Approval.Required)
	require.ErrorIs(t, runtime.Reload(t.Context()), ErrRuntimeBusy)
	requestID := paused.Approval.Required.RequestID

	resolveEvents := collectRuntimeEvents(t, runtime.Resolve(t.Context(), approval.Resolution{
		RequestID: requestID,
		Choice:    approval.ChoiceDeny,
	}))
	assert.Contains(t, eventTypes(resolveEvents), EventApprovalResolved)
	assert.Contains(t, eventTypes(resolveEvents), EventRunCompleted)

	settled := runtime.Snapshot()
	assert.Equal(t, PhaseIdle, settled.Phase)
	assert.Equal(t, ApprovalNone, settled.Approval.Kind)
	assert.Equal(t, InteractionSucceeded, settled.Interaction.Outcome)
	requests := model.Requests()
	assert.Len(t, requests, 2)
	assert.Equal(t, 1, countUserMessages(requests[1].Messages))

	require.NoError(t, runtime.Close(t.Context()))
}

func TestRuntimeEarlyIteratorBreakFinalizesInteraction(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel(runtimeTextResponse("unused")))

	for range runtime.Prompt(t.Context(), ai.UserText("stop early")) {
		break
	}

	snapshot := runtime.Snapshot()
	assert.Equal(t, PhaseIdle, snapshot.Phase)
	assert.False(t, snapshot.Interaction.Active)
	assert.Equal(t, InteractionCanceled, snapshot.Interaction.Outcome)

	recovery, err := runtime.journal.replay()
	require.NoError(t, err)
	assert.Empty(t, recovery.PendingID)
	assert.Equal(t, InteractionCanceled, recovery.LastOutcome)

	require.NoError(t, runtime.Close(t.Context()))
}

func TestRuntimeContinueRestoresPendingInteraction(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	firstModel := newRuntimeModel(runtimeToolResponse(
		"call-shell",
		"shell",
		`{"command":"printf ok","permissions":{"write_paths":[],"network":true},"justification":"test"}`,
	))
	first := openTestRuntimeAt(t, base, SessionTarget{}, firstModel)
	collectRuntimeEvents(t, first.Prompt(t.Context(), ai.UserText("run")))
	require.Equal(t, PhasePaused, first.Snapshot().Phase)

	sessionID := first.handle.Metadata().ID
	abruptRuntimeStop(t, first)

	secondModel := newRuntimeModel(runtimeTextResponse("continued after reopen"))
	second := openTestRuntimeAt(t, base, SessionTarget{ID: sessionID}, secondModel)
	assert.Equal(t, PhasePaused, second.Snapshot().Phase)

	continued := collectRuntimeEvents(t, second.Continue(t.Context()))
	assert.Contains(t, eventTypes(continued), EventApprovalRequired)
	changeDiagnostics := countDiagnostic(continued, "changes", "not_repository") +
		countDiagnostic(continued, "changes", "capture_git_failed")
	assert.Equal(t, 1, changeDiagnostics)
	assert.Less(
		t,
		slices.Index(eventTypes(continued), EventIntegrationDiagnostic),
		slices.Index(eventTypes(continued), EventApprovalRequired),
	)
	requestID := second.Snapshot().Approval.Required.RequestID

	collectRuntimeEvents(t, second.Resolve(t.Context(), approval.Resolution{
		RequestID: requestID,
		Choice:    approval.ChoiceDeny,
	}))
	assert.Equal(t, PhaseIdle, second.Snapshot().Phase)
	assert.Equal(t, InteractionSucceeded, second.Snapshot().Interaction.Outcome)

	require.NoError(t, second.Close(t.Context()))
}

func TestRuntimeReconcilesUncontrolledPendingSuffixWithFrozenHooks(t *testing.T) {
	t.Parallel()

	var gateCalls, toolCalls int
	ext, err := extension.NewDefinition(extension.Descriptor{
		ID: "pause-once", Version: "1.0.0",
	}, func(context.Context) (extension.Contribution, error) {
		tool := agent.NewTool("extension_echo", "Echo a value.", func(
			_ context.Context,
			args struct {
				Value string `json:"value"`
			},
		) (string, error) {
			toolCalls++

			return args.Value, nil
		})

		return extension.Contribution{
			Tools: []extension.Tool{{Value: tool, Risk: catalog.RiskRead}},
			Hooks: extension.Hooks{BeforeTool: func(
				_ context.Context,
				info agent.ToolCallInfo,
			) agent.ToolDecision {
				if info.Name == "extension_echo" {
					gateCalls++
					if gateCalls == 1 {
						return agent.ToolDecision{Action: agent.ToolDecisionPause}
					}
				}

				return agent.ToolDecision{}
			}},
		}, nil
	})
	require.NoError(t, err)

	model := newRuntimeModel(
		runtimeToolResponse("call-extension", "extension_echo", `{"value":"ok"}`),
		runtimeTextResponse("continued"),
	)
	runtime := openTestRuntimeWithExtensions(t, model, ext)
	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("echo")))

	assert.Equal(t, 2, gateCalls)
	assert.Equal(t, 1, toolCalls)
	assert.Equal(t, 2, countEventType(events, EventRunCompleted))
	assert.NotContains(t, eventTypes(events), EventApprovalRequired)
	assert.Equal(t, PhaseIdle, runtime.Snapshot().Phase)
	require.NoError(t, runtime.Close(t.Context()))
}

func TestRuntimeModelFailureClosesReducerRun(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, runtimeFailModel{})
	var runErr error
	for _, err := range runtime.Prompt(t.Context(), ai.UserText("fail")) {
		if err != nil {
			runErr = err
		}
	}

	require.ErrorIs(t, runErr, errRuntimeModelFailure)
	snapshot := runtime.Snapshot()
	assert.Equal(t, PhaseIdle, snapshot.Phase)
	assert.Equal(t, InteractionFailed, snapshot.Interaction.Outcome)
	require.NotNil(t, snapshot.LastError)
	assert.Equal(t, "run_failed", snapshot.LastError.Code)
	for _, run := range snapshot.Runs {
		assert.False(t, run.Active)
		assert.False(t, run.TurnOpen)
	}

	require.NoError(t, runtime.Close(t.Context()))
}

func TestRuntimeDisablesPanickingExtensionObserver(t *testing.T) {
	t.Parallel()

	observerCalls := 0
	ext, err := extension.NewDefinition(extension.Descriptor{
		ID: "bad-observer", Version: "1.0.0",
	}, func(context.Context) (extension.Contribution, error) {
		return extension.Contribution{Hooks: extension.Hooks{
			Observe: func(context.Context, agent.Event) {
				observerCalls++
				panic("observer failure")
			},
		}}, nil
	})
	require.NoError(t, err)

	runtime := openTestRuntimeWithExtensions(
		t,
		newRuntimeModel(runtimeTextResponse("done")),
		ext,
	)
	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("hello")))

	assert.Equal(t, 1, observerCalls)
	assert.True(t, hasDiagnostic(events, "extension_observer_disabled"))
	assert.Equal(t, InteractionSucceeded, runtime.Snapshot().Interaction.Outcome)
	require.NoError(t, runtime.Close(t.Context()))
}

func TestWorkspaceChangedBoundsEventPayload(t *testing.T) {
	t.Parallel()

	entries := make([]changes.Entry, maxEventItems+5)
	for index := range entries {
		entries[index] = changes.Entry{
			Path: fmt.Sprintf("file-%04d.go", index), Kind: changes.KindModified,
		}
	}

	report, err := changes.NewReport(
		entries,
		strings.Repeat("x", maxEventTextBytes+5),
		false,
	)
	require.NoError(t, err)

	projected := workspaceChanged(report)
	assert.Len(t, projected.Entries, maxEventItems)
	assert.Len(t, projected.Diff, maxEventTextBytes)
	assert.Equal(t, maxEventItems+5, projected.Files)
	assert.True(t, projected.Truncated)
	require.NoError(t, validateWorkspaceChanged(projected))
}

func TestRuntimeCloseCancelsAndWaitsForActivePrompt(t *testing.T) {
	t.Parallel()

	model := newBlockingRuntimeModel()
	runtime := openTestRuntime(t, model)
	promptDone := make(chan error, 1)
	go func() {
		var promptErr error
		for _, err := range runtime.Prompt(t.Context(), ai.UserText("wait")) {
			if err != nil {
				promptErr = err
			}
		}

		promptDone <- promptErr
	}()

	select {
	case <-model.started:
	case <-t.Context().Done():
		t.Fatal("blocking model did not start")
	}

	require.NoError(t, runtime.Close(t.Context()))
	require.ErrorIs(t, <-promptDone, context.Canceled)
	assert.Equal(t, PhaseClosed, runtime.Snapshot().Phase)
	require.NoError(t, runtime.Close(t.Context()))
}

func TestRuntimeCloseAfterCanceledForegroundSubagentDoesNotReuseStoppedIterator(t *testing.T) {
	t.Parallel()

	model := newCanceledForegroundSubagentRuntimeModel()
	runtime := openTestRuntime(t, model)
	promptCtx, cancelPrompt := context.WithCancel(t.Context())
	promptDone := make(chan error, 1)
	go func() {
		var promptErr error
		for _, err := range runtime.Prompt(promptCtx, ai.UserText("delegate")) {
			if err != nil {
				promptErr = errors.Join(promptErr, err)
			}
		}
		promptDone <- promptErr
	}()

	select {
	case <-model.childStarted:
	case <-t.Context().Done():
		t.Fatal("foreground child did not start")
	}
	cancelPrompt()

	select {
	case promptErr := <-promptDone:
		require.ErrorIs(t, promptErr, context.Canceled)
	case <-t.Context().Done():
		t.Fatal("parent prompt did not stop after cancellation")
	}

	close(model.releaseChild)
	require.NoError(t, runtime.Close(t.Context()))
	snapshot := runtime.Snapshot()
	assert.Equal(t, PhaseClosed, snapshot.Phase)
	require.Len(t, snapshot.Subagents, 1)
	assert.Equal(t, subagent.StateCanceled, snapshot.Subagents[0].State)
}

func TestCleanupStackClosesResourcesInReverseAndJoinsErrors(t *testing.T) {
	t.Parallel()

	firstErr := errors.New("inspector close failed")
	secondErr := errors.New("extension close failed")
	var order []string
	stack := &cleanupStack{}
	for _, resource := range []struct {
		name string
		err  error
	}{
		{name: "tree"},
		{name: "session"},
		{name: "inspector", err: firstErr},
		{name: "mcp"},
		{name: "extension", err: secondErr},
	} {
		stack.add(func(context.Context) error {
			order = append(order, resource.name)

			return resource.err
		})
	}

	err := stack.close(t.Context())
	require.ErrorIs(t, err, firstErr)
	require.ErrorIs(t, err, secondErr)
	assert.Equal(t, []string{"extension", "mcp", "inspector", "session", "tree"}, order)
}

func TestRuntimeReloadInstallsAtIdleBoundary(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel(runtimeTextResponse("after reload")))
	require.NoError(t, runtime.Reload(t.Context()))

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("hello")))
	assert.Contains(t, eventTypes(events), EventRunCompleted)
	assert.Equal(t, InteractionSucceeded, runtime.Snapshot().Interaction.Outcome)
	require.NoError(t, runtime.Close(t.Context()))
}

func TestRuntimeReloadPublishesImmutableAgentProfileRegistry(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	profileDir := filepath.Join(base, "home", "agents")
	profilePath := filepath.Join(profileDir, "go-helper.md")
	require.NoError(t, os.MkdirAll(profileDir, 0o700))
	require.NoError(t, os.WriteFile(profilePath, []byte(`---
schema: pips.agent/v1alpha1
name: Go helper
description: Inspect Go code.
---
initial instructions
`), 0o600))

	runtime := openTestRuntimeAt(t, base, SessionTarget{}, newRuntimeModel(runtimeTextResponse("unused")))
	before := runtime.integration.agentProfilesSnapshot()
	definition, found := before.Lookup("go-helper")
	require.True(t, found)
	assert.Equal(t, "initial instructions", definition.Instructions)

	require.NoError(t, os.WriteFile(profilePath, []byte(`---
schema: pips.agent/v1alpha1
name: Go helper
description: Inspect Go code.
---
updated instructions
`), 0o600))
	require.NoError(t, runtime.Reload(t.Context()))

	after := runtime.integration.agentProfilesSnapshot()
	updated, found := after.Lookup("go-helper")
	require.True(t, found)
	assert.Equal(t, "updated instructions", updated.Instructions)
	assert.Equal(t, "initial instructions", definition.Instructions)
	assert.NotEqual(t, definition.Digest, updated.Digest)
}

func TestRuntimeReadPatchControlledShellAndAnswer(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeToolResponse("call-read", "read", `{"path":"main.txt"}`),
		runtimeToolResponse(
			"call-patch",
			"apply_patch",
			`{"patch":"*** Begin Patch\n*** Update File: main.txt\n@@\n-old\n+new\n*** End Patch\n"}`,
		),
		runtimeToolResponse(
			"call-shell",
			"shell",
			`{"command":"printf checked","permissions":{"write_paths":[],"network":true},"justification":"test"}`,
		),
		runtimeTextResponse("done"),
	)
	runtime := openTestRuntime(t, model)
	filePath := runtime.workspace.Root() + "/main.txt"
	require.NoError(t, os.WriteFile(filePath, []byte("old\n"), 0o600))

	promptEvents := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("update")))
	assert.Equal(t, 2, countEventType(promptEvents, EventToolCompleted))
	assert.Contains(t, eventTypes(promptEvents), EventApprovalRequired)
	updated, err := os.ReadFile(filePath) //nolint:gosec // Test path is below t.TempDir.
	require.NoError(t, err)
	assert.Equal(t, "new\n", string(updated))

	requestID := runtime.Snapshot().Approval.Required.RequestID
	resolveEvents := collectRuntimeEvents(t, runtime.Resolve(t.Context(), approval.Resolution{
		RequestID: requestID,
		Choice:    approval.ChoiceDeny,
	}))
	assert.Contains(t, eventTypes(resolveEvents), EventInteractionCompleted)
	assert.Equal(t, InteractionSucceeded, runtime.Snapshot().Interaction.Outcome)
	assert.Len(t, model.Requests(), 4)
	require.NoError(t, runtime.Close(t.Context()))
}

func TestRuntimeLiveDurableStateMatchesReopenBootstrap(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	first := openTestRuntimeAt(
		t,
		base,
		SessionTarget{},
		newRuntimeModel(runtimeTextResponse("done")),
	)
	collectRuntimeEvents(t, first.Prompt(t.Context(), ai.UserText("hello")))
	want := first.Snapshot().Durable()
	sessionID := first.handle.Metadata().ID
	require.NoError(t, first.Close(t.Context()))

	second := openTestRuntimeAt(
		t,
		base,
		SessionTarget{ID: sessionID},
		newRuntimeModel(runtimeTextResponse("unused")),
	)
	assert.Equal(t, want, second.Snapshot().Durable())
	require.NoError(t, second.Close(t.Context()))
}

func TestRuntimeRetainedEmptySessionCanReopenBeforeFirstPrompt(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	first := openTestRuntimeAt(
		t,
		base,
		SessionTarget{RetainEmpty: true},
		newRuntimeModel(runtimeTextResponse("unused")),
	)
	sessionID := first.Snapshot().SessionID
	assert.True(t, first.Snapshot().IsSessionProvisional())
	require.NoError(t, first.Close(t.Context()))

	reopened := openTestRuntimeAt(
		t,
		base,
		SessionTarget{ID: sessionID, RetainEmpty: true},
		newRuntimeModel(runtimeTextResponse("unused")),
	)
	assert.Equal(t, sessionID, reopened.Snapshot().SessionID)
	assert.True(t, reopened.Snapshot().IsSessionProvisional())
	require.NoError(t, reopened.Close(t.Context()))
}

func TestRuntimeResumeUsesConfigurationInsteadOfLegacyModelChange(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	first := openTestRuntimeAt(
		t,
		base,
		SessionTarget{},
		newRuntimeModel(runtimeTextResponse("first")),
	)
	_, err := first.session.AppendModelChange(ai.ProviderAnthropic, "legacy-model")
	require.NoError(t, err)
	sessionID := first.handle.Metadata().ID
	require.NoError(t, first.Close(t.Context()))

	secondModel := newRuntimeModel(runtimeTextResponse("configured"))
	second := openTestRuntimeAt(
		t,
		base,
		SessionTarget{ID: sessionID},
		secondModel,
	)
	assert.Equal(t, ai.ProviderOpenAI, second.Snapshot().Provider)
	assert.Equal(t, "runtime-test", second.Snapshot().ModelID)
	assert.Equal(t, 1, countHarnessKind(second.session.Path(), harness.KindModelChange))

	collectRuntimeEvents(t, second.Prompt(t.Context(), ai.UserText("continue")))
	require.Len(t, secondModel.Requests(), 1)
	require.NoError(t, second.Close(t.Context()))
}

func TestRuntimeManualCompactionRejectsStalePreviewAndCommitsFreshPlan(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(runtimeTextResponse("## Goal\nContinue the implementation."))
	runtime := openTestRuntime(t, model)
	configureRuntimeCompaction(runtime, 2000, 300, 1000, 64)
	runtime.requestPolicy = func(request *ai.Request) {
		if request.Temperature == nil {
			request.Temperature = ai.Ptr(0.25)
		}
	}
	appendRuntimeHistory(t, runtime, 800, 800, 600, 600)

	preview, err := runtime.PreviewCompaction(t.Context())
	require.NoError(t, err)
	require.True(t, preview.Available)
	assert.NotEmpty(t, preview.Token)
	assert.Equal(t, 1700, preview.ThresholdTokens)

	_, err = runtime.session.AppendMessage(ai.UserText("changed after preview"), nil)
	require.NoError(t, err)
	before := countHarnessKind(runtime.session.Path(), harness.KindCompaction)
	_, staleErr := collectRuntimeResult(runtime.Compact(t.Context(), CompactionRequest{
		PreviewToken: preview.Token,
	}))
	require.ErrorIs(t, staleErr, ErrCompactionStale)
	assert.Equal(t, before, countHarnessKind(runtime.session.Path(), harness.KindCompaction))

	fresh, err := runtime.PreviewCompaction(t.Context())
	require.NoError(t, err)
	require.True(t, fresh.Available)
	events := collectRuntimeEvents(t, runtime.Compact(t.Context(), CompactionRequest{
		PreviewToken: fresh.Token,
		Instructions: "Preserve implementation decisions.",
	}))
	assert.Contains(t, eventTypes(events), EventCompactionStarted)
	assert.Contains(t, eventTypes(events), EventCompactionCompleted)
	assert.Contains(t, eventTypes(events), EventSessionTreeChanged)
	assert.Equal(t, before+1, countHarnessKind(runtime.session.Path(), harness.KindCompaction))
	requests := model.Requests()
	require.Len(t, requests, 1)
	require.NotNil(t, requests[0].MaxTokens)
	assert.Equal(t, 64, *requests[0].MaxTokens)
	require.NotNil(t, requests[0].Temperature)
	assert.InDelta(t, 0.25, *requests[0].Temperature, 1e-9)
}

func TestRuntimeAutomaticCompactionPrecedesInteractionCommit(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeTextResponse("## Goal\nRetain earlier context."),
		runtimeTextResponse("done"),
	)
	runtime := openTestRuntime(t, model)
	configureRuntimeCompaction(runtime, 1800, 300, 1000, 64)
	appendRuntimeHistory(t, runtime, 700, 700, 700, 700)
	before := len(runtime.session.Path())

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("new goal")))
	types := eventTypes(events)
	compactIndex := slices.Index(types, EventCompactionStarted)
	interactionIndex := slices.Index(types, EventInteractionStarted)
	require.GreaterOrEqual(t, compactIndex, 0)
	require.GreaterOrEqual(t, interactionIndex, 0)
	assert.Less(t, compactIndex, interactionIndex)

	path := runtime.session.Path()
	require.Greater(t, len(path), before)
	assert.Equal(t, harness.KindCompaction, path[before].Kind,
		"automatic compaction must commit before the interaction journal and user goal")
	requests := model.Requests()
	require.Len(t, requests, 2)
	require.NotNil(t, requests[0].MaxTokens)
	assert.Equal(t, 64, *requests[0].MaxTokens)
	assert.True(t, requestContainsText(requests[1], "new goal"))
}

func TestRuntimeAutomaticCompactionSuppressesRetryOnUnchangedLeaf(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel()
	runtime := openTestRuntime(t, model)
	configureRuntimeCompaction(runtime, 1800, 300, 1000, 64)
	appendRuntimeHistory(t, runtime, 700, 700, 700, 700)
	before := runtime.session.Entries()

	_, firstErr := collectRuntimeResult(runtime.Prompt(t.Context(), ai.UserText("new goal")))
	require.Error(t, firstErr)
	assert.Equal(t, before, runtime.session.Entries(), "failed automatic compaction must not commit the goal")
	require.Len(t, model.Requests(), 1)

	_, secondErr := collectRuntimeResult(runtime.Prompt(t.Context(), ai.UserText("new goal")))
	require.ErrorIs(t, secondErr, ErrCompactionRetrySuppressed)
	assert.Equal(t, before, runtime.session.Entries())
	assert.Len(t, model.Requests(), 1, "unchanged history must not call the failing summarizer again")
}

func TestRuntimeNavigateRebuildsTranscriptAndSurvivesReopen(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	first := openTestRuntimeAt(t, base, SessionTarget{}, newRuntimeModel())
	root, err := first.session.AppendMessage(ai.UserText("root goal"), nil)
	require.NoError(t, err)
	abandoned, err := first.session.AppendMessage(ai.AssistantText("abandoned answer"), nil)
	require.NoError(t, err)

	events := collectRuntimeEvents(t, first.Navigate(t.Context(), root, false))
	assert.Contains(t, eventTypes(events), EventSessionNavigated)
	assert.Contains(t, eventTypes(events), EventSessionTreeChanged)
	state := first.Snapshot()
	require.Len(t, state.Transcript, 1)
	assert.Equal(t, ai.UserText("root goal"), state.Transcript[0])
	node, ok := findSessionNode(state.Tree, abandoned)
	require.True(t, ok)
	assert.False(t, node.OnActivePath)
	expectedTree := state.Tree.Clone()
	sessionID := first.handle.Metadata().ID
	require.NoError(t, first.Close(t.Context()))

	reopened := openTestRuntimeAt(t, base, SessionTarget{ID: sessionID}, newRuntimeModel())
	reopenedState := reopened.Snapshot()
	assert.Equal(t, expectedTree, reopenedState.Tree)
	assert.Equal(t, state.Transcript, reopenedState.Transcript)
}

func countHarnessKind(entries []harness.Entry, kind harness.Kind) int {
	count := 0
	for _, entry := range entries {
		if entry.Kind == kind {
			count++
		}
	}

	return count
}

func configureRuntimeCompaction(
	runtime *Runtime,
	contextWindow int,
	reserve int,
	keepRecent int,
	summaryMax int,
) {
	runtime.resolved.Limits.ContextWindow = contextWindow
	runtime.config.Compaction = config.CompactionConfig{
		Enabled: true, ReserveTokens: reserve, KeepRecentTokens: keepRecent,
		SummaryMaxTokens: summaryMax,
	}
}

func appendRuntimeHistory(t *testing.T, runtime *Runtime, tokenSizes ...int) {
	t.Helper()

	for index, tokens := range tokenSizes {
		var message ai.Message = ai.UserText(strings.Repeat("word", tokens))
		if index%2 == 1 {
			message = ai.AssistantText(strings.Repeat("word", tokens))
		}
		_, err := runtime.session.AppendMessage(message, nil)
		require.NoError(t, err)
	}
}

func collectRuntimeResult(sequence iter.Seq2[Event, error]) ([]Event, error) {
	var events []Event
	var resultErr error
	sequence(func(event Event, err error) bool {
		if err != nil {
			resultErr = errors.Join(resultErr, err)

			return true
		}
		events = append(events, event)

		return true
	})

	return events, resultErr
}

func requestContainsText(request ai.Request, expected string) bool {
	for _, message := range request.Messages {
		parts, err := ai.MessageParts(message)
		if err != nil {
			continue
		}
		for _, part := range parts {
			if value, ok := part.(ai.TextPart); ok && strings.Contains(value.Text, expected) {
				return true
			}
		}
	}

	return false
}

func runtimeMessageText(message ai.Message) string {
	var value strings.Builder
	parts, err := ai.MessageParts(message)
	if err != nil {
		return ""
	}
	for _, part := range parts {
		if text, ok := part.(ai.TextPart); ok {
			value.WriteString(text.Text)
		}
	}

	return value.String()
}

func findSessionNode(tree SessionTree, id string) (SessionNode, bool) {
	for _, node := range tree.Nodes {
		if node.ID == id {
			return node, true
		}
	}

	return SessionNode{}, false
}

func openTestRuntime(t *testing.T, model ai.LanguageModel) *Runtime {
	t.Helper()

	return openTestRuntimeAt(t, t.TempDir(), SessionTarget{}, model)
}

func openTestRuntimeWithTelemetry(
	t *testing.T,
	model ai.LanguageModel,
	observers ...TelemetryObserver,
) *Runtime {
	t.Helper()

	return openTestRuntimeConfigured(
		t,
		t.TempDir(),
		SessionTarget{},
		model,
		nil,
		nil,
		observers,
	)
}

func openTestRuntimeWithAgentObservers(
	t *testing.T,
	model ai.LanguageModel,
	observers ...func(context.Context, agent.Event),
) *Runtime {
	t.Helper()

	return openTestRuntimeConfigured(
		t,
		t.TempDir(),
		SessionTarget{},
		model,
		nil,
		observers,
		nil,
	)
}

func openTestRuntimeWithExtensions(
	t *testing.T,
	model ai.LanguageModel,
	extensions ...extension.Extension,
) *Runtime {
	t.Helper()

	return openTestRuntimeAtWithExtensions(
		t,
		t.TempDir(),
		SessionTarget{},
		model,
		extensions,
	)
}

func openTestRuntimeAt(
	t *testing.T,
	base string,
	target SessionTarget,
	model ai.LanguageModel,
) *Runtime {
	t.Helper()

	return openTestRuntimeAtWithExtensions(t, base, target, model, nil)
}

func openTestRuntimeAtWithExtensions(
	t *testing.T,
	base string,
	target SessionTarget,
	model ai.LanguageModel,
	extensions []extension.Extension,
) *Runtime {
	t.Helper()

	return openTestRuntimeConfigured(t, base, target, model, extensions, nil, nil)
}

func openTestRuntimeConfigured(
	t *testing.T,
	base string,
	target SessionTarget,
	model ai.LanguageModel,
	extensions []extension.Extension,
	agentObservers []func(context.Context, agent.Event),
	telemetry []TelemetryObserver,
) *Runtime {
	t.Helper()

	return openTestRuntimeConfiguredWithTrust(
		t,
		base,
		target,
		model,
		extensions,
		agentObservers,
		telemetry,
		false,
	)
}

func openTestRuntimeConfiguredWithTrust(
	t *testing.T,
	base string,
	target SessionTarget,
	model ai.LanguageModel,
	extensions []extension.Extension,
	agentObservers []func(context.Context, agent.Event),
	telemetry []TelemetryObserver,
	trusted bool,
) *Runtime {
	t.Helper()

	return openTestRuntimeConfiguredWithSandbox(
		t,
		base,
		target,
		model,
		extensions,
		agentObservers,
		telemetry,
		trusted,
		config.SandboxWorkspaceWrite,
	)
}

func openFullAccessTestRuntimeConfiguredWithTrust(
	t *testing.T,
	base string,
	target SessionTarget,
	model ai.LanguageModel,
	extensions []extension.Extension,
	agentObservers []func(context.Context, agent.Event),
	telemetry []TelemetryObserver,
	trusted bool,
) *Runtime {
	t.Helper()

	return openTestRuntimeConfiguredWithSandbox(
		t,
		base,
		target,
		model,
		extensions,
		agentObservers,
		telemetry,
		trusted,
		config.SandboxFullAccess,
	)
}

func openTestRuntimeConfiguredWithSandbox(
	t *testing.T,
	base string,
	target SessionTarget,
	model ai.LanguageModel,
	extensions []extension.Extension,
	agentObservers []func(context.Context, agent.Event),
	telemetry []TelemetryObserver,
	trusted bool,
	sandbox config.SandboxMode,
) *Runtime {
	t.Helper()

	return openTestRuntimeConfiguredWithSandboxAndConfig(
		t, base, target, model, extensions, agentObservers, telemetry, trusted, sandbox, nil,
	)
}

func openTestRuntimeConfiguredWithSandboxAndConfig(
	t *testing.T,
	base string,
	target SessionTarget,
	model ai.LanguageModel,
	extensions []extension.Extension,
	agentObservers []func(context.Context, agent.Event),
	telemetry []TelemetryObserver,
	trusted bool,
	sandbox config.SandboxMode,
	mutateConfig func(*config.Config),
) *Runtime {
	t.Helper()

	workspacePath := base + "/workspace"
	require.NoError(t, mkdirPrivate(workspacePath))

	ws, err := workspace.Open(workspacePath)
	require.NoError(t, err)

	layout, err := paths.New(base + "/home")
	require.NoError(t, err)

	cfg := config.Defaults()
	if sandbox == config.SandboxFullAccess {
		loaded, loadErr := config.Load(config.LoadOptions{
			ConfigFile: filepath.Join(base, "absent-test-config.toml"),
			FlagOverrides: config.Patch{
				Sandbox: &sandbox,
			},
		})
		require.NoError(t, loadErr)
		cfg = loaded.Config
	} else {
		cfg.Sandbox = sandbox
	}
	if mutateConfig != nil {
		mutateConfig(&cfg)
	}
	cfg.Model.Provider = model.Provider()
	cfg.Model.Model = model.ModelID()
	if model.Provider() == "opencode-go" {
		cfg.Providers[model.Provider()] = config.ProviderConfig{
			BaseURL:  "https://opencode.ai/zen/go/v1",
			Protocol: config.ProtocolOpenAIChatCompletions,
		}
	}

	runtime, err := Open(t.Context(), OpenOptions{
		Workspace:          ws,
		Trusted:            trusted,
		Config:             cfg,
		Paths:              layout,
		Session:            target,
		Model:              model,
		Extensions:         extensions,
		AgentObservers:     agentObservers,
		TelemetryObservers: telemetry,
		Execution: ExecutionOptions{
			SandboxProbe: func(context.Context, *execution.Executor) error { return nil },
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	return runtime
}

func abruptRuntimeStop(t *testing.T, runtime *Runtime) {
	t.Helper()

	runtime.mu.Lock()
	current := runtime.interaction
	runtime.interaction = nil
	runtime.mu.Unlock()

	runtime.pending.clear()
	runtime.resolver.set(nil)
	if current != nil && current.integration != nil {
		runtime.mu.Lock()
		integration := current.integration
		runtime.integration = nil
		runtime.mu.Unlock()
		require.NoError(t, integration.retire(t.Context()))
	}
	require.NoError(t, runtime.extensions.Shutdown(t.Context()))
	require.NoError(t, runtime.inspector.Close())
	require.NoError(t, runtime.handle.Close())
	require.NoError(t, runtime.tree.Close())

	runtime.mu.Lock()
	runtime.closed = true
	runtime.mu.Unlock()
}

func mkdirPrivate(path string) error {
	return os.MkdirAll(path, 0o700)
}

func collectRuntimeEvents(t *testing.T, sequence iter.Seq2[Event, error]) []Event {
	t.Helper()

	var events []Event
	sequence(func(event Event, err error) bool {
		require.NoError(t, err)
		events = append(events, event)

		return true
	})

	return events
}

func assertEventSequence(t *testing.T, events []Event) {
	t.Helper()

	for index := 1; index < len(events); index++ {
		assert.Equal(t, events[index-1].Sequence+1, events[index].Sequence)
	}
}

func eventTypes(events []Event) []EventType {
	values := make([]EventType, len(events))
	for index, event := range events {
		values[index] = event.Type
	}

	return values
}

func countEventType(events []Event, eventType EventType) int {
	count := 0
	for _, event := range events {
		if event.Type == eventType {
			count++
		}
	}

	return count
}

func hasDiagnostic(events []Event, code string) bool {
	for _, event := range events {
		diagnostic, ok := event.Payload.(IntegrationDiagnostic)
		if ok && diagnostic.Code == code {
			return true
		}
	}

	return false
}

func countDiagnostic(events []Event, component, code string) int {
	count := 0
	for _, event := range events {
		diagnostic, ok := event.Payload.(IntegrationDiagnostic)
		if ok && diagnostic.Component == component && diagnostic.Code == code {
			count++
		}
	}

	return count
}

func telemetryTypes(events []TelemetryEvent) []EventType {
	values := make([]EventType, len(events))
	for index, event := range events {
		values[index] = event.Type
	}

	return values
}

func countUserMessages(messages []ai.Message) int {
	count := 0
	for _, message := range messages {
		if _, ok := message.(ai.UserMessage); ok {
			count++
		}
	}

	return count
}

func requestSystemText(request ai.Request) string {
	system, _, err := request.Messages.SplitSystem()
	if err != nil {
		return ""
	}

	return ai.JoinSystemText(system)
}

func toolNamesFromRequest(request ai.Request) []string {
	values := make([]string, len(request.Tools))
	for index, tool := range request.Tools {
		values[index] = tool.Name
	}

	return values
}

type runtimeModel struct {
	mu        sync.Mutex
	provider  ai.Provider
	modelID   string
	responses []*ai.Response
	requests  []ai.Request
}

func newRuntimeModel(responses ...*ai.Response) *runtimeModel {
	return newRuntimeModelFor(ai.ProviderOpenAI, "runtime-test", responses...)
}

func newRuntimeModelFor(
	provider ai.Provider,
	modelID string,
	responses ...*ai.Response,
) *runtimeModel {
	return &runtimeModel{provider: provider, modelID: modelID, responses: responses}
}

func (m *runtimeModel) Generate(_ context.Context, request ai.Request) (*ai.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.requests = append(m.requests, request)
	if len(m.responses) == 0 {
		return nil, errors.New("runtime model script exhausted")
	}

	response := m.responses[0]
	m.responses = m.responses[1:]

	return response, nil
}

func (m *runtimeModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		response, err := m.Generate(ctx, request)
		if err != nil {
			yield(ai.StreamEvent{}, err)
			return
		}

		for _, event := range runtimeResponseEvents(response) {
			if !yield(event, nil) {
				return
			}
		}
	}
}

func (m *runtimeModel) Provider() ai.Provider { return m.provider }
func (m *runtimeModel) ModelID() string       { return m.modelID }
func (m *runtimeModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

func (m *runtimeModel) Requests() []ai.Request {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]ai.Request(nil), m.requests...)
}

func runtimeTextResponse(text string) *ai.Response {
	return &ai.Response{
		Provider:     ai.ProviderOpenAI,
		Model:        "runtime-test",
		Message:      ai.AssistantText(text),
		FinishReason: ai.FinishStop,
		Usage:        ai.Usage{InputTokens: 10, OutputTokens: 2},
	}
}

func runtimeToolResponse(id, name, args string) *ai.Response {
	return &ai.Response{
		Provider:     ai.ProviderOpenAI,
		Model:        "runtime-test",
		Message:      ai.Assistant(ai.ToolCallPart{ID: id, Name: name, Args: ai.JSON(args)}),
		FinishReason: ai.FinishToolCalls,
		Usage:        ai.Usage{InputTokens: 10, OutputTokens: 2},
	}
}

func runtimeResponseEvents(response *ai.Response) []ai.StreamEvent {
	events := []ai.StreamEvent{{
		Type:     ai.StreamMessageStart,
		Provider: response.Provider,
		Model:    response.Model,
	}}

	toolIndex := 0
	for _, part := range response.Message.Parts {
		switch value := part.(type) {
		case ai.TextPart:
			events = append(events, ai.StreamEvent{Type: ai.StreamTextDelta, Text: value.Text})
		case ai.ToolCallPart:
			events = append(events,
				ai.StreamEvent{
					Type: ai.StreamToolCallStart, ToolCallIndex: toolIndex,
					ToolCallID: value.ID, ToolCallName: value.Name,
				},
				ai.StreamEvent{
					Type: ai.StreamToolCallDelta, ToolCallIndex: toolIndex,
					ArgsDelta: string(value.Args),
				},
				ai.StreamEvent{Type: ai.StreamToolCallEnd, ToolCallIndex: toolIndex},
			)
			toolIndex++
		}
	}

	usage := response.Usage
	return append(events, ai.StreamEvent{
		Type: ai.StreamMessageEnd, FinishReason: response.FinishReason, Usage: &usage,
	})
}

var _ ai.LanguageModel = (*runtimeModel)(nil)

type notificationFollowUpModel struct {
	mu      sync.Mutex
	started chan struct{}
	release chan struct{}
	calls   int
}

func newNotificationFollowUpModel() *notificationFollowUpModel {
	return &notificationFollowUpModel{
		started: make(chan struct{}), release: make(chan struct{}),
	}
}

func (m *notificationFollowUpModel) Generate(
	ctx context.Context,
	request ai.Request,
) (*ai.Response, error) {
	m.mu.Lock()
	m.calls++
	call := m.calls
	m.mu.Unlock()

	switch call {
	case 1:
		close(m.started)
		select {
		case <-m.release:
			return runtimeTextResponse("initial answer"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case 2:
		if !requestContainsText(request, agentNotificationSchema) {
			return nil, errors.New("notification follow-up is missing from model context")
		}

		return runtimeTextResponse("completion handled"), nil
	default:
		return nil, errors.New("notification follow-up model script exhausted")
	}
}

func (m *notificationFollowUpModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		response, err := m.Generate(ctx, request)
		if err != nil {
			yield(ai.StreamEvent{}, err)
			return
		}
		for _, event := range runtimeResponseEvents(response) {
			if !yield(event, nil) {
				return
			}
		}
	}
}

func (*notificationFollowUpModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (*notificationFollowUpModel) ModelID() string       { return "runtime-test" }
func (*notificationFollowUpModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

var _ ai.LanguageModel = (*notificationFollowUpModel)(nil)

type backgroundSpawnRuntimeModel struct {
	mu           sync.Mutex
	mainCalls    int
	childOnce    sync.Once
	childStarted chan struct{}
	releaseChild chan struct{}
}

func newBackgroundSpawnRuntimeModel() *backgroundSpawnRuntimeModel {
	return &backgroundSpawnRuntimeModel{
		childStarted: make(chan struct{}), releaseChild: make(chan struct{}),
	}
}

func (m *backgroundSpawnRuntimeModel) Generate(
	ctx context.Context,
	request ai.Request,
) (*ai.Response, error) {
	if strings.Contains(requestSystemText(request), "You are a read-only specialist") {
		m.childOnce.Do(func() { close(m.childStarted) })
		select {
		case <-m.releaseChild:
			return runtimeTextResponse(
				`{"summary":"background done","evidence":[],"unknowns":[]}`,
			), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	m.mu.Lock()
	m.mainCalls++
	call := m.mainCalls
	m.mu.Unlock()
	switch call {
	case 1:
		return runtimeToolResponse(
			"spawn-1",
			"spawn_agent",
			`{"role":"explore","task":"Inspect the runtime in the background."}`,
		), nil
	case 2:
		return runtimeTextResponse("parent continued"), nil
	case 3:
		if !requestContainsText(request, agentNotificationSchema) {
			return nil, errors.New("background completion is missing from parent context")
		}

		return runtimeTextResponse("background completion handled"), nil
	default:
		return nil, errors.New("background spawn model script exhausted")
	}
}

func (m *backgroundSpawnRuntimeModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		response, err := m.Generate(ctx, request)
		if err != nil {
			yield(ai.StreamEvent{}, err)
			return
		}
		for _, event := range runtimeResponseEvents(response) {
			if !yield(event, nil) {
				return
			}
		}
	}
}

func (*backgroundSpawnRuntimeModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (*backgroundSpawnRuntimeModel) ModelID() string       { return "runtime-test" }
func (*backgroundSpawnRuntimeModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true, StructuredOutput: true}
}

func (m *backgroundSpawnRuntimeModel) MainCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.mainCalls
}

var _ ai.LanguageModel = (*backgroundSpawnRuntimeModel)(nil)

type canceledForegroundSubagentRuntimeModel struct {
	mu           sync.Mutex
	mainCalls    int
	childOnce    sync.Once
	childStarted chan struct{}
	releaseChild chan struct{}
}

func newCanceledForegroundSubagentRuntimeModel() *canceledForegroundSubagentRuntimeModel {
	return &canceledForegroundSubagentRuntimeModel{
		childStarted: make(chan struct{}), releaseChild: make(chan struct{}),
	}
}

func (m *canceledForegroundSubagentRuntimeModel) Generate(
	ctx context.Context,
	request ai.Request,
) (*ai.Response, error) {
	if strings.Contains(requestSystemText(request), "You are a read-only specialist") {
		m.childOnce.Do(func() { close(m.childStarted) })
		<-m.releaseChild

		return nil, ctx.Err()
	}

	m.mu.Lock()
	m.mainCalls++
	call := m.mainCalls
	m.mu.Unlock()
	if call != 1 {
		return nil, errors.New("foreground cancellation model script exhausted")
	}

	return runtimeToolResponse(
		"delegate-cancel",
		"run_subagent",
		`{"role":"explore","task":"Wait until the parent is canceled."}`,
	), nil
}

func (m *canceledForegroundSubagentRuntimeModel) Stream(
	ctx context.Context,
	request ai.Request,
) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		response, err := m.Generate(ctx, request)
		if err != nil {
			yield(ai.StreamEvent{}, err)

			return
		}
		for _, event := range runtimeResponseEvents(response) {
			if !yield(event, nil) {
				return
			}
		}
	}
}

func (*canceledForegroundSubagentRuntimeModel) Provider() ai.Provider {
	return ai.ProviderOpenAI
}

func (*canceledForegroundSubagentRuntimeModel) ModelID() string { return "runtime-test" }

func (*canceledForegroundSubagentRuntimeModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true, StructuredOutput: true}
}

var _ ai.LanguageModel = (*canceledForegroundSubagentRuntimeModel)(nil)

var errRuntimeModelFailure = errors.New("runtime model failed")

type runtimeFailModel struct{}

func (runtimeFailModel) Generate(context.Context, ai.Request) (*ai.Response, error) {
	return nil, errRuntimeModelFailure
}

func (runtimeFailModel) Stream(context.Context, ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		yield(ai.StreamEvent{}, errRuntimeModelFailure)
	}
}

func (runtimeFailModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (runtimeFailModel) ModelID() string       { return "runtime-test" }
func (runtimeFailModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

var _ ai.LanguageModel = runtimeFailModel{}

type blockingRuntimeModel struct {
	started chan struct{}
	once    sync.Once
}

func newBlockingRuntimeModel() *blockingRuntimeModel {
	return &blockingRuntimeModel{started: make(chan struct{})}
}

func (m *blockingRuntimeModel) Generate(ctx context.Context, _ ai.Request) (*ai.Response, error) {
	m.once.Do(func() { close(m.started) })
	<-ctx.Done()

	return nil, ctx.Err()
}

func (m *blockingRuntimeModel) Stream(ctx context.Context, _ ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		if !yield(ai.StreamEvent{
			Type: ai.StreamMessageStart, Provider: ai.ProviderOpenAI, Model: "runtime-test",
		}, nil) {
			return
		}

		m.once.Do(func() { close(m.started) })
		<-ctx.Done()
		yield(ai.StreamEvent{}, ctx.Err())
	}
}

func (*blockingRuntimeModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (*blockingRuntimeModel) ModelID() string       { return "runtime-test" }
func (*blockingRuntimeModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

var _ ai.LanguageModel = (*blockingRuntimeModel)(nil)

type delayedCancelRuntimeModel struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newDelayedCancelRuntimeModel() *delayedCancelRuntimeModel {
	return &delayedCancelRuntimeModel{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (m *delayedCancelRuntimeModel) Generate(ctx context.Context, _ ai.Request) (*ai.Response, error) {
	m.once.Do(func() { close(m.started) })
	<-ctx.Done()
	<-m.release

	return nil, ctx.Err()
}

func (m *delayedCancelRuntimeModel) Stream(ctx context.Context, _ ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		if !yield(ai.StreamEvent{
			Type: ai.StreamMessageStart, Provider: ai.ProviderOpenAI, Model: "runtime-test",
		}, nil) {
			return
		}
		m.once.Do(func() { close(m.started) })
		<-ctx.Done()
		<-m.release
		yield(ai.StreamEvent{}, ctx.Err())
	}
}

func (*delayedCancelRuntimeModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (*delayedCancelRuntimeModel) ModelID() string       { return "runtime-test" }
func (*delayedCancelRuntimeModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

var _ ai.LanguageModel = (*delayedCancelRuntimeModel)(nil)
