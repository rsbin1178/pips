package coding

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRuntimeStopsALiveChildByItsSubagentID drives the whole stop path with a
// real Runtime and Manager: the id the spawn returned resolves as a child
// rather than as a registry task, the cancellation is initiated through the
// manager, and the child reaches a cancelled terminal state.
func TestRuntimeStopsALiveChildByItsSubagentID(t *testing.T) {
	t.Parallel()

	fixture := runningChildFixture(t)

	outcome, subject, err := fixture.runtime.KillBackgroundTask(
		t.Context(), fixture.childID, background.KillModel,
	)
	require.NoError(t, err)
	assert.Equal(t, background.OutcomeKilled, outcome)
	assert.Equal(t, background.SubjectSubagent, subject,
		"the answer must name the child, because its cancellation is only initiated")

	fixture.release()

	require.Eventually(t, func() bool {
		summaries, listErr := fixture.runtime.ListSubagents(t.Context())

		return listErr == nil && len(summaries) == 1 && summaries[0].State == subagent.StateCanceled
	}, 30*time.Second, 10*time.Millisecond, "the stopped child must wind down")
}

// TestRuntimeStopsAFinishedChildAsAlreadyExited pins the terminal half of the
// child branch: a child that already ended is not cancelled again, and its
// completion stays the surface that carries its result.
func TestRuntimeStopsAFinishedChildAsAlreadyExited(t *testing.T) {
	t.Parallel()

	fixture := runningChildFixture(t)
	fixture.release()

	require.Eventually(t, func() bool {
		summaries, listErr := fixture.runtime.ListSubagents(t.Context())

		return listErr == nil && len(summaries) == 1 && terminalSubagentState(summaries[0].State)
	}, 30*time.Second, 10*time.Millisecond)

	outcome, subject, err := fixture.runtime.KillBackgroundTask(
		t.Context(), fixture.childID, background.KillModel,
	)
	require.NoError(t, err)
	assert.Equal(t, background.OutcomeAlreadyExited, outcome)
	assert.Equal(t, background.SubjectSubagent, subject)
	assert.False(t, fixture.runtime.childResultTaken(fixture.childID),
		"a child that had already finished marks nothing")
}

// TestRuntimeStopAnswersAnUnknownIDAsNotFound pins the shared not-found outcome
// for an id of either shape. The subject is the same for both, which is what
// makes the shared sentence the answer whichever shape the caller guessed: only
// a known id names the producer that answered.
func TestRuntimeStopAnswersAnUnknownIDAsNotFound(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel())

	for _, id := range []string{
		"s-00000000-0000-4000-8000-0000000000ff", // a child-shaped session id
		"monitor-999-deadbeef",                   // a registry-shaped task id
	} {
		outcome, subject, err := runtime.KillBackgroundTask(
			t.Context(), id, background.KillModel,
		)
		require.NoError(t, err)
		assert.Equal(t, background.OutcomeNotFound, outcome)
		assert.Equal(t, background.SubjectSubagent, subject)
	}
}

// TestRuntimeStopStillAnswersAWatchsOwnOutcomes pins the registry half: a
// watch's stop keeps the outcomes and the subject the registry path always
// reported, and it is resolved before the child branch is reached.
func TestRuntimeStopStillAnswersAWatchsOwnOutcomes(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel())
	require.NoError(t, runtime.stopNotificationCoordinator(t.Context()))

	snapshot, err := runtime.backgroundTasks.Launch(t.Context(), background.Request{
		Kind: background.KindMonitor, Command: "watch", Timeout: time.Minute,
		Start: func(ctx context.Context, _ execution.Sink) (execution.Result, error) {
			<-ctx.Done()

			return execution.Result{Status: execution.StatusCanceled, ExitCode: -1}, ctx.Err()
		},
	})
	require.NoError(t, err)

	outcome, subject, err := runtime.KillBackgroundTask(
		t.Context(), snapshot.ID, background.KillModel,
	)
	require.NoError(t, err)
	assert.Equal(t, background.OutcomeKilled, outcome)
	assert.Equal(t, background.SubjectTask, subject)

	require.Eventually(t, func() bool {
		stored, found := runtime.backgroundTasks.Snapshot(snapshot.ID)

		return found && stored.Completed
	}, 30*time.Second, 10*time.Millisecond)

	outcome, subject, err = runtime.KillBackgroundTask(
		t.Context(), snapshot.ID, background.KillModel,
	)
	require.NoError(t, err)
	assert.Equal(t, background.OutcomeAlreadyExited, outcome)
	assert.Equal(t, background.SubjectTask, subject)
}

// TestRuntimeStopKeepsItsOwnErrors pins the boundary the child branch must not
// soften: an unopened runtime, a runtime with no registry, an empty id, and a
// cancelled context all keep the errors they already reported.
func TestRuntimeStopKeepsItsOwnErrors(t *testing.T) {
	t.Parallel()

	var unopened *Runtime

	outcome, subject, err := unopened.KillBackgroundTask(
		t.Context(), "monitor-1", background.KillModel,
	)
	assert.Equal(t, background.OutcomeNotFound, outcome)
	assert.Equal(t, background.SubjectTask, subject)
	require.ErrorIs(t, err, ErrRuntimeClosed)

	nilRegistry := &Runtime{}

	outcome, subject, err = nilRegistry.KillBackgroundTask(
		t.Context(), "monitor-1", background.KillModel,
	)
	assert.Equal(t, background.OutcomeNotFound, outcome)
	assert.Equal(t, background.SubjectTask, subject)
	require.ErrorIs(t, err, ErrRuntimeClosed)

	runtime := openTestRuntime(t, newRuntimeModel())

	outcome, subject, err = runtime.KillBackgroundTask(t.Context(), "", background.KillModel)
	assert.Equal(t, background.OutcomeNotFound, outcome)
	assert.Equal(t, background.SubjectTask, subject)
	require.ErrorIs(t, err, ErrRuntimeInvalid)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	outcome, subject, err = runtime.KillBackgroundTask(ctx, "monitor-1", background.KillModel)
	assert.Equal(t, background.OutcomeNotFound, outcome)
	assert.Equal(t, background.SubjectTask, subject)
	require.ErrorIs(t, err, context.Canceled)
}

// TestRuntimeModelStopSuppressesTheChildCompletion pins the suppression rule for
// a model stop: the Tool result already carries the outcome, so the child's
// completion is dropped and acknowledged rather than waking the model again.
func TestRuntimeModelStopSuppressesTheChildCompletion(t *testing.T) {
	t.Parallel()

	fixture := runningChildFixture(t)
	require.NoError(t, fixture.runtime.stopNotificationCoordinator(t.Context()))

	outcome, subject, err := fixture.runtime.KillBackgroundTask(
		t.Context(), fixture.childID, background.KillModel,
	)
	require.NoError(t, err)
	require.Equal(t, background.OutcomeKilled, outcome)
	require.Equal(t, background.SubjectSubagent, subject)
	require.True(t, fixture.runtime.childResultTaken(fixture.childID))

	fixture.release()

	require.Eventually(t, func() bool {
		pending, pendingErr := fixture.runtime.notifications.Pending()

		return pendingErr == nil && len(pending) == 1
	}, 30*time.Second, 10*time.Millisecond)

	requests := len(fixture.model.Requests())

	fixture.runtime.deliverNotificationBatch(t.Context())

	require.Eventually(t, func() bool {
		pending, pendingErr := fixture.runtime.notifications.Pending()

		return pendingErr == nil && len(pending) == 0
	}, 30*time.Second, 10*time.Millisecond, "the suppressed completion must be acknowledged")

	assert.Len(t, fixture.model.Requests(), requests,
		"a stop the Tool result already answers must not wake the model")
}

// TestRuntimeUserStopLetsTheChildCompletionWake pins the other half of the
// source rule: a stop the pane requested carries no Tool result, so the model
// must still learn that the child was stopped.
func TestRuntimeUserStopLetsTheChildCompletionWake(t *testing.T) {
	t.Parallel()

	fixture := runningChildFixture(t)
	require.NoError(t, fixture.runtime.stopNotificationCoordinator(t.Context()))

	outcome, subject, err := fixture.runtime.KillBackgroundTask(
		t.Context(), fixture.childID, background.KillUser,
	)
	require.NoError(t, err)
	require.Equal(t, background.OutcomeKilled, outcome)
	require.Equal(t, background.SubjectSubagent, subject)
	assert.False(t, fixture.runtime.childResultTaken(fixture.childID),
		"a user stop must leave the completion to arrive")

	fixture.release()

	require.Eventually(t, func() bool {
		pending, pendingErr := fixture.runtime.notifications.Pending()

		return pendingErr == nil && len(pending) == 1
	}, 30*time.Second, 10*time.Millisecond)

	requests := len(fixture.model.Requests())

	fixture.runtime.deliverNotificationBatch(t.Context())

	require.Eventually(t, func() bool {
		return len(fixture.model.Requests()) > requests
	}, 30*time.Second, 10*time.Millisecond, "the pane's stop must still reach the model")
}

// TestRuntimePromptEndStopSuppressesTheChildCompletion pins the third source: a
// prompt-end stop leaves no turn to report the child, so its completion is
// dropped and acknowledged like a model stop's.
func TestRuntimePromptEndStopSuppressesTheChildCompletion(t *testing.T) {
	t.Parallel()

	fixture := runningChildFixture(t)
	require.NoError(t, fixture.runtime.stopNotificationCoordinator(t.Context()))

	outcome, subject, err := fixture.runtime.KillBackgroundTask(
		t.Context(), fixture.childID, background.KillPromptEnd,
	)
	require.NoError(t, err)
	require.Equal(t, background.OutcomeKilled, outcome)
	require.Equal(t, background.SubjectSubagent, subject)
	require.True(t, fixture.runtime.childResultTaken(fixture.childID))

	fixture.release()

	require.Eventually(t, func() bool {
		pending, pendingErr := fixture.runtime.notifications.Pending()

		return pendingErr == nil && len(pending) == 1
	}, 30*time.Second, 10*time.Millisecond)

	requests := len(fixture.model.Requests())

	fixture.runtime.deliverNotificationBatch(t.Context())

	require.Eventually(t, func() bool {
		pending, pendingErr := fixture.runtime.notifications.Pending()

		return pendingErr == nil && len(pending) == 0
	}, 30*time.Second, 10*time.Millisecond)

	assert.Len(t, fixture.model.Requests(), requests,
		"no turn is left to report a prompt-end stop")
}

// TestRuntimeStopOfAFinishedChildLeavesItsCompletionAlone pins the
// already-finished rule end to end: the model asked about a child that had
// already ended, and its completion is the surface that carries the result.
func TestRuntimeStopOfAFinishedChildLeavesItsCompletionAlone(t *testing.T) {
	t.Parallel()

	fixture := runningChildFixture(t)
	require.NoError(t, fixture.runtime.stopNotificationCoordinator(t.Context()))

	fixture.release()

	require.Eventually(t, func() bool {
		summaries, listErr := fixture.runtime.ListSubagents(t.Context())

		return listErr == nil && len(summaries) == 1 && terminalSubagentState(summaries[0].State)
	}, 30*time.Second, 10*time.Millisecond)

	outcome, _, err := fixture.runtime.KillBackgroundTask(
		t.Context(), fixture.childID, background.KillModel,
	)
	require.NoError(t, err)
	require.Equal(t, background.OutcomeAlreadyExited, outcome)

	require.Eventually(t, func() bool {
		pending, pendingErr := fixture.runtime.notifications.Pending()

		return pendingErr == nil && len(pending) == 1
	}, 30*time.Second, 10*time.Millisecond)

	requests := len(fixture.model.Requests())

	fixture.runtime.deliverNotificationBatch(t.Context())

	require.Eventually(t, func() bool {
		return len(fixture.model.Requests()) > requests
	}, 30*time.Second, 10*time.Millisecond,
		"a finished child's completion must still wake the model")
}

// TestRuntimeChildStopLeavesASiblingsCompletionAlone pins the boundary: the mark
// is keyed by the child that was stopped, so a sibling's completion still
// wakes.
func TestRuntimeChildStopLeavesASiblingsCompletionAlone(t *testing.T) {
	t.Parallel()

	var (
		startedFirst = make(chan struct{})
		releaseFirst = make(chan struct{})
		firstOnce    sync.Once
		releaseOnce  sync.Once
	)

	model := &messagingTestModel{onRequest: func(request ai.Request) *ai.Response {
		if isChildSystemText(requestSystemText(request)) {
			// The second child answers at once; the first holds its turn open
			// until the test releases it.
			if strings.Contains(requestAllText(request), "Second task.") {
				return runtimeTextResponse("the second child answer")
			}

			firstOnce.Do(func() { close(startedFirst) })
			<-releaseFirst

			return runtimeTextResponse("the first child answer")
		}

		switch {
		case spawnedChildID(request, "spawn-2") != "":
			return runtimeTextResponse("the parent answer")
		case spawnedChildID(request, "spawn-1") != "":
			return runtimeToolResponse("spawn-2", subagent.ToolName,
				`{"prompt":"Second task.","description":"second",`+
					`"subagent_type":"explore","run_in_background":true}`)
		default:
			return runtimeToolResponse("spawn-1", subagent.ToolName,
				`{"prompt":"First task.","description":"first",`+
					`"subagent_type":"explore","run_in_background":true}`)
		}
	}}
	runtime := openMessagingTestRuntime(t, t.TempDir(), model, false)
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	t.Cleanup(release)

	require.NoError(t, runtime.stopNotificationCoordinator(t.Context()))
	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("delegate twice")))

	select {
	case <-startedFirst:
	case <-time.After(30 * time.Second):
		require.FailNow(t, "the first child never reached its held turn")
	}

	summaries, err := runtime.ListSubagents(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 2)

	var firstID, secondID string

	for _, summary := range summaries {
		switch summary.TaskPreview {
		case "First task.":
			firstID = summary.ChildSessionID
		case "Second task.":
			secondID = summary.ChildSessionID
		}
	}

	require.NotEmpty(t, firstID)
	require.NotEmpty(t, secondID)

	require.Eventually(t, func() bool {
		pending, pendingErr := runtime.notifications.Pending()

		return pendingErr == nil && len(pending) == 1 && pending[0].AgentID == secondID
	}, 30*time.Second, 10*time.Millisecond)

	outcome, subject, err := runtime.KillBackgroundTask(t.Context(), firstID, background.KillModel)
	require.NoError(t, err)
	require.Equal(t, background.OutcomeKilled, outcome)
	require.Equal(t, background.SubjectSubagent, subject)
	assert.True(t, runtime.childResultTaken(firstID))
	assert.False(t, runtime.childResultTaken(secondID),
		"a stop marks only the child it resolved")

	release()

	require.Eventually(t, func() bool {
		pending, pendingErr := runtime.notifications.Pending()

		return pendingErr == nil && len(pending) == 1
	}, 30*time.Second, 10*time.Millisecond)

	requests := len(model.Requests())

	runtime.deliverNotificationBatch(t.Context())

	require.Eventually(t, func() bool {
		return len(model.Requests()) > requests
	}, 30*time.Second, 10*time.Millisecond)

	assert.True(t, requestContainsText(model.Requests()[requests], secondID),
		"the sibling's completion must survive a stop of another child")
}

// TestRuntimeSuppressedChildCompletionDoesNotResurrectOnRestart pins the
// durability of the suppression: the completion a model stop answered is
// acknowledged in the durable queue, so a Runtime opened over the same session
// again neither re-queues the child's completion nor wakes the model with it.
func TestRuntimeSuppressedChildCompletionDoesNotResurrectOnRestart(t *testing.T) {
	t.Parallel()

	fixture := runningChildFixture(t)
	require.NoError(t, fixture.runtime.stopNotificationCoordinator(t.Context()))

	outcome, subject, err := fixture.runtime.KillBackgroundTask(
		t.Context(), fixture.childID, background.KillModel,
	)
	require.NoError(t, err)
	require.Equal(t, background.OutcomeKilled, outcome)
	require.Equal(t, background.SubjectSubagent, subject)

	fixture.release()

	require.Eventually(t, func() bool {
		pending, pendingErr := fixture.runtime.notifications.Pending()

		return pendingErr == nil && len(pending) == 1
	}, 30*time.Second, 10*time.Millisecond)

	requests := len(fixture.model.Requests())

	fixture.runtime.deliverNotificationBatch(t.Context())

	require.Eventually(t, func() bool {
		pending, pendingErr := fixture.runtime.notifications.Pending()

		return pendingErr == nil && len(pending) == 0
	}, 30*time.Second, 10*time.Millisecond, "the suppressed completion must be acknowledged")
	assert.Len(t, fixture.model.Requests(), requests,
		"the suppressed completion must not wake the model")

	base := filepath.Dir(fixture.runtime.workspace.Root())
	sessionID := fixture.runtime.handle.Metadata().ID
	require.NoError(t, fixture.runtime.Close(t.Context()))

	reopenedModel := newRuntimeModel(runtimeTextResponse("unused"))
	reopened := openTestRuntimeAt(t, base, SessionTarget{ID: sessionID}, reopenedModel)

	// Recovery reads the child as terminal background work, so only the
	// acknowledged completion keeps it from being queued again.
	summaries, err := reopened.ListSubagents(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	require.Equal(t, fixture.childID, summaries[0].ChildSessionID)

	known, err := reopened.notifications.Contains(fixture.childID)
	require.NoError(t, err)
	assert.True(t, known, "the suppressed completion stays known across a restart")

	pending, err := reopened.notifications.Pending()
	require.NoError(t, err)
	assert.Empty(t, pending, "a restart must not resurrect the suppressed completion")
	assert.Empty(t, reopenedModel.Requests(), "a restart must not wake the model with it")
}

// TestRuntimeUserChildCancelDeliversTheCompletion pins the pane's own child
// control: its child rows cancel through CancelSubagent, which marks nothing, so
// the completion still reaches the model.
func TestRuntimeUserChildCancelDeliversTheCompletion(t *testing.T) {
	t.Parallel()

	fixture := runningChildFixture(t)
	require.NoError(t, fixture.runtime.stopNotificationCoordinator(t.Context()))

	require.NoError(t, fixture.runtime.CancelSubagent(t.Context(), fixture.childID))
	assert.False(t, fixture.runtime.childResultTaken(fixture.childID),
		"the pane's child control leaves the completion to arrive")

	fixture.release()

	require.Eventually(t, func() bool {
		pending, pendingErr := fixture.runtime.notifications.Pending()

		return pendingErr == nil && len(pending) == 1
	}, 30*time.Second, 10*time.Millisecond)

	requests := len(fixture.model.Requests())

	fixture.runtime.deliverNotificationBatch(t.Context())

	require.Eventually(t, func() bool {
		return len(fixture.model.Requests()) > requests
	}, 30*time.Second, 10*time.Millisecond, "the pane's child cancel must still reach the model")
}

// TestRuntimeModelStopsAChildThroughTheRealTool pins the end-to-end flow the
// model experiences: a background child spawned through the spawn Tool is
// stopped by the real kill_background_task, whose result carries the child
// sentence, and the completion that stop already answered never wakes the model.
func TestRuntimeModelStopsAChildThroughTheRealTool(t *testing.T) {
	t.Parallel()

	var (
		started     = make(chan struct{})
		released    = make(chan struct{})
		startOnce   sync.Once
		releaseOnce sync.Once
	)

	model := &messagingTestModel{onRequest: func(request ai.Request) *ai.Response {
		if isChildSystemText(requestSystemText(request)) {
			startOnce.Do(func() { close(started) })
			<-released

			return runtimeTextResponse("the child answer")
		}

		if requestToolResultText(request, "call-stop") != "" {
			return runtimeTextResponse("the parent answer")
		}

		if childID := spawnedChildID(request, "call-spawn"); childID != "" {
			return runtimeToolResponse("call-stop", tools.KillBackgroundName,
				`{"task_id":"`+childID+`"}`)
		}

		return runtimeToolResponse("call-spawn", subagent.ToolName,
			`{"prompt":"Inspect the target.","description":"inspect the target",`+
				`"subagent_type":"general-purpose","run_in_background":true}`)
	}}

	runtime := openMessagingTestRuntime(t, t.TempDir(), model, false)
	release := func() { releaseOnce.Do(func() { close(released) }) }
	t.Cleanup(release)

	require.NoError(t, runtime.stopNotificationCoordinator(t.Context()))
	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("delegate and stop it")))

	select {
	case <-started:
	case <-time.After(30 * time.Second):
		require.FailNow(t, "the child never reached its held turn")
	}

	summaries, err := runtime.ListSubagents(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	childID := summaries[0].ChildSessionID

	// The stop's own Tool result is the surface that answers the model.
	killBody := ""

	for _, request := range model.Requests() {
		if body := requestToolResultText(request, "call-stop"); body != "" {
			killBody = body
		}
	}

	require.NotEmpty(t, killBody)
	assert.Contains(t, killBody, `"outcome":"killed"`)
	assert.Contains(t, killBody, `Subagent "`+childID+`" cancellation was initiated.`)

	release()

	require.Eventually(t, func() bool {
		pending, pendingErr := runtime.notifications.Pending()

		return pendingErr == nil && len(pending) == 1
	}, 30*time.Second, 10*time.Millisecond)

	requests := len(model.Requests())

	runtime.deliverNotificationBatch(t.Context())

	require.Eventually(t, func() bool {
		pending, pendingErr := runtime.notifications.Pending()

		return pendingErr == nil && len(pending) == 0
	}, 30*time.Second, 10*time.Millisecond, "the suppressed completion must be acknowledged")

	assert.Len(t, model.Requests(), requests,
		"the stop the Tool result already answered must not wake the model")
}
