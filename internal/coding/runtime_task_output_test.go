package coding

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRuntimeBlockingWaitSuppressesTheCompletion drives the whole suppression
// path with a real Runtime and Registry: a blocking read takes the terminal
// state, so the completion the registry produced for the same task becomes no
// queue record at all.
func TestRuntimeBlockingWaitSuppressesTheCompletion(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel())
	require.NoError(t, runtime.stopNotificationCoordinator(t.Context()))

	snapshot, err := runtime.backgroundTasks.Launch(t.Context(), background.Request{
		Kind: background.KindMonitor, Command: "watch once", Timeout: time.Minute,
		Start: func(ctx context.Context, _ execution.Sink) (execution.Result, error) {
			select {
			case <-time.After(100 * time.Millisecond):
				return execution.Result{Status: execution.StatusExited, ExitCode: 0}, nil
			case <-ctx.Done():
				return execution.Result{Status: execution.StatusCanceled, ExitCode: -1}, ctx.Err()
			}
		},
	})
	require.NoError(t, err)

	records, outcome := runtime.readBackgroundTasks(
		t.Context(), []string{snapshot.ID}, tools.TaskOutputModeWaitAll, 10*time.Second,
	)
	require.Equal(t, background.WaitOutcomeCompleted, outcome)
	require.Len(t, records, 1)
	assert.Equal(t, background.StatusCompleted, records[0].Status)

	require.Eventually(t, func() bool {
		return runtime.backgroundOutput.observedBackground() >= 1
	}, 30*time.Second, 10*time.Millisecond)

	assert.True(t, runtime.backgroundTasks.ResultTaken(snapshot.ID),
		"the wait that returned the terminal record took it")

	events, completions := runtime.backgroundOutput.takeBackground()
	require.Len(t, completions, 1)
	assert.Empty(t, runtime.backgroundOutput.backgroundMessages(events, completions),
		"a result the waiter already has must not wake the model again")
}

// TestRuntimeElapsedWaitStillLetsTheCompletionWake pins the release rule end to
// end: a wait that ran out while the task was still running stops being a
// reason to suppress, so the task's own later completion still produces a queue
// record.
func TestRuntimeElapsedWaitStillLetsTheCompletionWake(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel())
	require.NoError(t, runtime.stopNotificationCoordinator(t.Context()))

	release := make(chan struct{})

	snapshot, err := runtime.backgroundTasks.Launch(t.Context(), background.Request{
		Kind: background.KindMonitor, Command: "watch once", Timeout: time.Minute,
		Start: func(ctx context.Context, _ execution.Sink) (execution.Result, error) {
			select {
			case <-release:
				return execution.Result{Status: execution.StatusExited, ExitCode: 0}, nil
			case <-ctx.Done():
				return execution.Result{Status: execution.StatusCanceled, ExitCode: -1}, ctx.Err()
			}
		},
	})
	require.NoError(t, err)

	records, outcome := runtime.readBackgroundTasks(
		t.Context(), []string{snapshot.ID}, tools.TaskOutputModeWaitAll, 20*time.Millisecond,
	)
	require.Equal(t, background.WaitOutcomeDeadline, outcome)
	require.Len(t, records, 1)
	require.Equal(t, background.StatusRunning, records[0].Status)
	assert.False(t, runtime.backgroundTasks.ResultTaken(snapshot.ID),
		"an elapsed wait must release its registration")

	close(release)

	require.Eventually(t, func() bool {
		return runtime.backgroundOutput.observedBackground() >= 1
	}, 30*time.Second, 10*time.Millisecond)

	events, completions := runtime.backgroundOutput.takeBackground()
	require.Len(t, completions, 1)

	delivered := runtime.backgroundOutput.backgroundMessages(events, completions)
	require.Len(t, delivered, 1, "the task's own completion must still wake the model")
	assert.Contains(t, delivered[0].Body, "for full output.")
}

// TestCancelledInteractionCancelsItsOwnWatches pins the per-interaction cancel:
// the watch an interaction started is stamped with that interaction's id, and
// cancelling the interaction cancels exactly that watch.
func TestCancelledInteractionCancelsItsOwnWatches(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeToolResponse("call-monitor", tools.MonitorName,
			`{"command":"sleep 30","description":"mine","persistent":true}`),
		runtimeToolResponse("call-hold", "shell", `{"command":"sleep 30"}`),
		runtimeTextResponse("done"),
	)
	runtime := openTestRuntime(t, model)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})

	go func() {
		defer close(done)

		for range runtime.Prompt(ctx, ai.UserText("start a watch")) {
		}
	}()

	var monitorID string

	require.Eventually(t, func() bool {
		tasks := runtime.backgroundTasks.List()
		if len(tasks) != 1 || tasks[0].Completed {
			return false
		}

		monitorID = tasks[0].ID

		return true
	}, 30*time.Second, 10*time.Millisecond)

	// The interaction that spawned the watch is on the snapshot every surface
	// reads, which is what the cancel addresses.
	snapshot, ok := runtime.backgroundTasks.Snapshot(monitorID)
	require.True(t, ok)
	assert.NotEmpty(t, snapshot.Owner, "the spawning interaction's id must be on the snapshot")

	cancel()
	<-done

	require.Eventually(t, func() bool {
		stored, ok := runtime.backgroundTasks.Snapshot(monitorID)

		return ok && stored.Completed && stored.Killed
	}, 30*time.Second, 10*time.Millisecond)

	records := runtime.backgroundTasks.TaskOutputs([]string{monitorID})
	require.Len(t, records, 1)
	assert.Equal(t, background.StatusCancelled, records[0].Status)
}

// TestCancelledInteractionKeepsAnEarlierWatches pins the boundary: only the
// cancelled interaction's own watches stop, so a watch an earlier interaction
// started keeps running.
func TestCancelledInteractionKeepsAnEarlierWatches(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeToolResponse("call-early", tools.MonitorName,
			`{"command":"sleep 30","description":"early","persistent":true}`),
		runtimeTextResponse("started"),
		runtimeToolResponse("call-late", tools.MonitorName,
			`{"command":"sleep 30","description":"late","persistent":true}`),
		runtimeToolResponse("call-hold", "shell", `{"command":"sleep 30"}`),
		runtimeTextResponse("done"),
	)
	runtime := openTestRuntime(t, model)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("early watch")))
	require.Contains(t, eventTypes(events), EventInteractionCompleted)

	require.Eventually(t, func() bool {
		return len(runtime.backgroundTasks.List()) == 1
	}, 30*time.Second, 10*time.Millisecond)

	early := runtime.backgroundTasks.List()[0]
	require.False(t, early.Completed, "a normally completed interaction leaves its watch running")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})

	go func() {
		defer close(done)

		for range runtime.Prompt(ctx, ai.UserText("late watch")) {
		}
	}()

	require.Eventually(t, func() bool {
		return len(runtime.backgroundTasks.List()) == 2
	}, 30*time.Second, 10*time.Millisecond)

	cancel()
	<-done

	// The late watch is cancelled; the early one keeps running.
	require.Eventually(t, func() bool {
		for _, task := range runtime.backgroundTasks.List() {
			if task.ID != early.ID && task.Completed && task.Killed {
				return true
			}
		}

		return false
	}, 30*time.Second, 10*time.Millisecond)

	stored, ok := runtime.backgroundTasks.Snapshot(early.ID)
	require.True(t, ok)
	assert.False(t, stored.Completed, "an earlier interaction's watch must keep running")
	assert.Equal(t, background.StatusRunning, runtime.backgroundTasks.TaskOutputs([]string{early.ID})[0].Status)
}

// TestRuntimeSuppressesTheCompletionInBothOrderings pins the determinism the
// live predicate buys: a completion the delivery renders while a wait still
// stands by is suppressed by the registration, and the same completion rendered
// after the waiter took the result is suppressed by the taken mark.
func TestRuntimeSuppressesTheCompletionInBothOrderings(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel())
	require.NoError(t, runtime.stopNotificationCoordinator(t.Context()))

	release := make(chan struct{})

	snapshot, err := runtime.backgroundTasks.Launch(t.Context(), background.Request{
		Kind: background.KindMonitor, Command: "watch once", Timeout: time.Minute,
		Start: func(ctx context.Context, _ execution.Sink) (execution.Result, error) {
			select {
			case <-release:
				return execution.Result{Status: execution.StatusExited, ExitCode: 0}, nil
			case <-ctx.Done():
				return execution.Result{Status: execution.StatusCanceled, ExitCode: -1}, ctx.Err()
			}
		},
	})
	require.NoError(t, err)

	waited := make(chan background.WaitOutcome, 1)

	go func() {
		_, outcome := runtime.readBackgroundTasks(
			context.Background(), []string{snapshot.ID}, tools.TaskOutputModeWaitAll, time.Minute,
		)
		waited <- outcome
	}()

	require.Eventually(t, func() bool {
		return runtime.backgroundTasks.ResultTaken(snapshot.ID)
	}, 30*time.Second, 10*time.Millisecond)

	completion := background.Completion{
		Snapshot: background.Snapshot{
			ID: snapshot.ID, Completed: true, ExitCode: 0, Kind: background.KindMonitor,
		},
	}
	observer := runtime.backgroundOutput

	// Ordering A: the delivery renders while the waiter is still inside its
	// call, so only the live registration can suppress it.
	assert.Empty(t, observer.backgroundMessages(nil, []background.Completion{completion}),
		"a completion rendered while a wait stands by must not wake the model")

	close(release)

	require.Equal(t, background.WaitOutcomeCompleted, <-waited)

	// Ordering B: the waiter has exited and taken the result, so the same
	// completion stays suppressed.
	assert.Empty(t, observer.backgroundMessages(nil, []background.Completion{completion}),
		"a completion rendered after the waiter took the result must not wake the model")
}

// TestChildMonitorIsOwnedByItsInteraction pins the child half of the owner rule:
// a watch a child scope starts is stamped with the interaction that owns the
// child, so the same interaction-boundary cancel reaches it.
func TestChildMonitorIsOwnedByItsInteraction(t *testing.T) {
	t.Parallel()

	const owner = "interaction-child-owner"

	fixture := newChildCompactionFixture(t, newRuntimeModel(runtimeTextResponse("done")), false)
	fixture.factory.backgroundTasks = fixture.runtime.backgroundTasks
	fixture.factory.monitorOwner = func() string { return owner }

	scope := fixture.open(t)

	monitor, ok := scope.controller.Tool(tools.MonitorName)
	require.True(t, ok)

	call := agent.ToolCall{
		ID: "child-monitor", Name: tools.MonitorName,
		Args: ai.JSON(`{"command":"sleep 30","description":"child watch","persistent":true}`),
	}
	decision := scope.controller.BeforeTool(
		t.Context(), agent.ToolCallInfo{ToolCall: call, Turn: 1, BatchSize: 1},
	)
	require.Equal(t, agent.ToolDecisionAllow, decision.Action)

	_, err := monitor.Exec(t.Context(), call)
	require.NoError(t, err)

	tasks := fixture.runtime.backgroundTasks.List()
	require.Len(t, tasks, 1)
	assert.Equal(t, owner, tasks[0].Owner,
		"a child's watch must carry the interaction that owns the child")

	// The interaction boundary cancels exactly the watches carrying that owner.
	assert.Equal(t, 1, fixture.runtime.backgroundTasks.CancelOwned(owner, background.KillPromptEnd))
}

// TestRuntimeAdmitsOnlyItsOwnLiveTaskLogs pins the read admission's scope at the
// runtime boundary: exactly the log path of a task this session's registry still
// knows, and nothing a model could spell around it.
func TestRuntimeAdmitsOnlyItsOwnLiveTaskLogs(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel())
	other := openTestRuntime(t, newRuntimeModel())

	launch := func(target *Runtime) string {
		t.Helper()

		snapshot, err := target.backgroundTasks.Launch(t.Context(), background.Request{
			Kind: background.KindMonitor, Command: "watch", Timeout: time.Minute,
			Start: func(ctx context.Context, _ execution.Sink) (execution.Result, error) {
				<-ctx.Done()

				return execution.Result{Status: execution.StatusCanceled, ExitCode: -1}, ctx.Err()
			},
		})
		require.NoError(t, err)

		return snapshot.ID
	}

	logPath, ok := runtime.backgroundTasks.OutputPath(launch(runtime))
	require.True(t, ok)

	otherPath, ok := other.backgroundTasks.OutputPath(launch(other))
	require.True(t, ok)

	admitted, ok := runtime.admitBackgroundLogPath(logPath)
	require.True(t, ok)
	assert.Equal(t, logPath, admitted)

	directory := filepath.Dir(logPath)

	cases := map[string]string{
		"a sibling log":           filepath.Join(directory, "monitor-other-token.log"),
		"another directory":       filepath.Join(t.TempDir(), filepath.Base(logPath)),
		"a traversed spelling":    directory + "/../terminal/" + filepath.Base(logPath),
		"a trailing slash":        logPath + "/",
		"a case variant":          strings.ToUpper(logPath),
		"an unknown id's log":     filepath.Join(directory, "monitor-999999-ffffffff.log"),
		"the log's own directory": directory,
		"another runtime's log":   otherPath,
		"the workspace root":      runtime.workspace.Root(),
	}

	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, ok := runtime.admitBackgroundLogPath(value)
			assert.False(t, ok)
		})
	}
}
