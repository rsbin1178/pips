package coding

import (
	"context"
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

// TestCancelledInteractionCancelsItsOwnBackgroundCommand pins the shell kind's
// identity and lifetime: a backgrounded command is stamped with the interaction
// that started it, so cancelling that interaction stops exactly its command
// while an earlier interaction's keeps running.
func TestCancelledInteractionCancelsItsOwnBackgroundCommand(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeToolResponse("call-early", "shell", `{"command":"sleep 30","block_until_ms":0}`),
		runtimeTextResponse("started"),
		runtimeToolResponse("call-late", "shell", `{"command":"sleep 30","block_until_ms":0}`),
		runtimeTextResponse("done"),
	)
	runtime := openTestRuntime(t, model)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("early command")))
	require.Contains(t, eventTypes(events), EventInteractionCompleted)

	require.Eventually(t, func() bool {
		return len(runtime.backgroundTasks.List()) == 1
	}, 30*time.Second, 10*time.Millisecond)

	early := runtime.backgroundTasks.List()[0]
	require.False(t, early.Completed, "a normally completed interaction leaves its command running")
	assert.Equal(t, background.KindShell, early.Kind)
	assert.NotEmpty(t, early.Owner, "the spawning interaction's id must be on the snapshot")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})

	go func() {
		defer close(done)

		for range runtime.Prompt(ctx, ai.UserText("late command")) {
		}
	}()

	require.Eventually(t, func() bool {
		return len(runtime.backgroundTasks.List()) == 2
	}, 30*time.Second, 10*time.Millisecond)

	cancel()
	<-done

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
	assert.False(t, stored.Completed, "an earlier interaction's command must keep running")
	assert.Equal(t, background.StatusRunning, runtime.backgroundTasks.TaskOutputs([]string{early.ID})[0].Status)
}

// TestPromptEndKillProducesNoCompletionRecordForABackgroundCommand pins the
// shell kind's terminal-report rule: a command killed by the cancelled
// interaction leaves no queue record, because no turn remains to report it.
func TestPromptEndKillProducesNoCompletionRecordForABackgroundCommand(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel())
	require.NoError(t, runtime.stopNotificationCoordinator(t.Context()))

	_, err := runtime.backgroundTasks.Launch(t.Context(), background.Request{
		Kind: background.KindShell, Command: "sleep 30", Timeout: time.Minute, Owner: "interaction-1",
		Start: func(ctx context.Context, _ execution.Sink) (execution.Result, error) {
			<-ctx.Done()

			return execution.Result{Status: execution.StatusCanceled, ExitCode: -1}, ctx.Err()
		},
	})
	require.NoError(t, err)

	assert.Equal(t, 1, runtime.backgroundTasks.CancelOwned("interaction-1", background.KillPromptEnd))

	require.Eventually(t, func() bool {
		return runtime.backgroundOutput.observedBackground() >= 1
	}, 30*time.Second, 10*time.Millisecond)

	events, completions := runtime.backgroundOutput.takeBackground()
	require.Len(t, completions, 1)
	assert.True(t, completions[0].Killed)
	assert.Equal(t, background.KillPromptEnd, completions[0].Source)
	assert.Empty(t, runtime.backgroundOutput.backgroundMessages(events, completions),
		"a prompt-end kill leaves no queue record")
}

// TestBackgroundCommandSnapshotCarriesTheSpawningInteraction pins the identity
// every surface reads: the pane's inspection and the stop Tool's known ids both
// see the same owner-stamped command.
func TestBackgroundCommandSnapshotCarriesTheSpawningInteraction(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel())
	require.NoError(t, runtime.stopNotificationCoordinator(t.Context()))

	snapshot, err := runtime.backgroundTasks.Launch(t.Context(), background.Request{
		Kind: background.KindShell, Command: "sleep 30", Timeout: time.Minute, Owner: "interaction-7",
		Start: func(ctx context.Context, _ execution.Sink) (execution.Result, error) {
			<-ctx.Done()

			return execution.Result{Status: execution.StatusCanceled, ExitCode: -1}, ctx.Err()
		},
	})
	require.NoError(t, err)

	inspection, err := runtime.InspectBackgroundTask(t.Context(), snapshot.ID)
	require.NoError(t, err)
	assert.Equal(t, "interaction-7", inspection.Task.Owner)
	assert.Equal(t, background.KindShell, inspection.Task.Kind)
	assert.Contains(t, runtime.KnownBackgroundTaskIDs(t.Context()), snapshot.ID)
}

// TestBackgroundCompletionTextSplitsByKind pins the delivery dispatch: a
// command's terminal report uses the command wording and a watch keeps its own.
func TestBackgroundCompletionTextSplitsByKind(t *testing.T) {
	t.Parallel()

	shell := background.Completion{Snapshot: background.Snapshot{
		ID: "shell-1", Kind: background.KindShell, Completed: true, ExitCode: 0,
	}}
	assert.Contains(t, backgroundCompletionText(shell), "Background task \"shell-1\" completed (exit code: 0).")

	monitor := background.Completion{Snapshot: background.Snapshot{
		ID: "monitor-1", Kind: background.KindMonitor, Completed: true, ExitCode: 0,
	}}
	assert.Contains(t, backgroundCompletionText(monitor), "Monitor \"monitor-1\" ended")
}

// TestShellBackgroundLifecycleEventIsAccepted pins the pane's data path: a
// shell task's lifecycle event validates and round-trips, so the pane's
// projection carries the command row rather than dropping it.
func TestShellBackgroundLifecycleEventIsAccepted(t *testing.T) {
	t.Parallel()

	event := Event{
		Schema: EventSchema, Sequence: 1, Time: time.Unix(1, 0).UTC(), SessionID: "session-1",
		Type: EventBackgroundStarted,
		Payload: BackgroundTaskLifecycle{
			ID: "shell-1", Kind: string(background.KindShell), Command: "cargo build",
			StartedAt: time.Unix(2, 0).UTC(),
		},
	}

	require.NoError(t, ValidateEvent(event))

	data, err := MarshalEvent(event)
	require.NoError(t, err)

	decoded, err := UnmarshalEvent(data)
	require.NoError(t, err)
	assert.Equal(t, event.Payload, decoded.Payload)
}

// TestShellPositiveBlockTakesTheResultSoNoCompletionRecord pins the positive
// block's suppression end to end: a command that ends inside the wait is
// rendered by the shell's own result, and the wait that returned it is the
// surface that carries the outcome, so the delivery path produces no completion
// record for the same task.
func TestShellPositiveBlockTakesTheResultSoNoCompletionRecord(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeToolResponse("call-block", "shell", `{"command":"printf done","block_until_ms":30000}`),
		runtimeTextResponse("done"),
	)
	runtime := openTestRuntime(t, model)
	require.NoError(t, runtime.stopNotificationCoordinator(t.Context()))

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("run it")))

	require.Eventually(t, func() bool {
		return runtime.backgroundOutput.observedBackground() >= 1
	}, 30*time.Second, 10*time.Millisecond)

	events, completions := runtime.backgroundOutput.takeBackground()
	require.Len(t, completions, 1)
	assert.True(t, completions[0].Snapshot.Completed)
	assert.True(t, runtime.backgroundTasks.ResultTaken(completions[0].Snapshot.ID),
		"the wait that rendered the result took it")
	assert.Empty(t, runtime.backgroundOutput.backgroundMessages(events, completions),
		"the completion the wait already took must not wake the model again")
}

// TestShellElapsedBlockStillLetsTheCompletionArrive pins the other half of the
// positive block: a command still running when the block elapses is not killed,
// the call returns its receipt, and the task's own later completion is the
// surface that reports it.
func TestShellElapsedBlockStillLetsTheCompletionArrive(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeToolResponse("call-outlive", "shell", `{"command":"sleep 1; printf done","block_until_ms":100}`),
		runtimeTextResponse("started"),
	)
	runtime := openTestRuntime(t, model)
	require.NoError(t, runtime.stopNotificationCoordinator(t.Context()))

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("run it")))

	require.Eventually(t, func() bool {
		return runtime.backgroundOutput.observedBackground() >= 1
	}, 30*time.Second, 10*time.Millisecond)

	events, completions := runtime.backgroundOutput.takeBackground()
	require.Len(t, completions, 1)
	assert.True(t, completions[0].Snapshot.Completed)
	assert.False(t, runtime.backgroundTasks.ResultTaken(completions[0].Snapshot.ID),
		"a block that elapsed must release its registration")

	delivered := runtime.backgroundOutput.backgroundMessages(events, completions)
	require.Len(t, delivered, 1, "the command's own completion must still wake the model")
	assert.Contains(t, delivered[0].Body, "Background task \"")
}

// TestChildBackgroundCommandIsOwnedByItsInteraction pins the child half of the
// owner rule for the new producer: a backgrounded command a child scope starts
// lands in the session's shared registry stamped with the interaction that owns
// the child, so the same interaction-boundary cancel reaches it.
func TestChildBackgroundCommandIsOwnedByItsInteraction(t *testing.T) {
	t.Parallel()

	const owner = "interaction-child-owner"

	fixture := newChildCompactionFixture(t, newRuntimeModel(runtimeTextResponse("done")), false)
	fixture.factory.backgroundTasks = fixture.runtime.backgroundTasks
	fixture.factory.monitorOwner = func() string { return owner }

	scope := fixture.open(t)

	shell, ok := scope.controller.Tool("shell")
	require.True(t, ok)

	call := agent.ToolCall{
		ID: "child-shell", Name: "shell",
		Args: ai.JSON(`{"command":"sleep 30","block_until_ms":0}`),
	}
	decision := scope.controller.BeforeTool(
		t.Context(), agent.ToolCallInfo{ToolCall: call, Turn: 1, BatchSize: 1},
	)
	require.Equal(t, agent.ToolDecisionAllow, decision.Action, decision.Reason)

	_, err := shell.Exec(t.Context(), call)
	require.NoError(t, err)

	tasks := fixture.runtime.backgroundTasks.List()
	require.Len(t, tasks, 1)
	assert.Equal(t, background.KindShell, tasks[0].Kind)
	assert.Equal(t, owner, tasks[0].Owner,
		"a child's command must carry the interaction that owns the child")

	assert.Equal(t, 1, fixture.runtime.backgroundTasks.CancelOwned(owner, background.KillPromptEnd))
}

// TestBackgroundLogReadCoversTheWidestKind pins the read admission's bound: it
// is the widest kind's ceiling, so both a watch's log and a backgrounded
// command's log are readable, while a watch's own log cap stays at 10 MiB.
func TestBackgroundLogReadCoversTheWidestKind(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel())

	launch := func(kind background.Kind) string {
		t.Helper()

		snapshot, err := runtime.backgroundTasks.Launch(t.Context(), background.Request{
			Kind: kind, Command: "run", NoDeadline: true,
			Start: func(ctx context.Context, _ execution.Sink) (execution.Result, error) {
				<-ctx.Done()

				return execution.Result{Status: execution.StatusCanceled}, nil
			},
		})
		require.NoError(t, err)

		return snapshot.ID
	}

	commandPath, ok := runtime.backgroundTasks.OutputPath(launch(background.KindShell))
	require.True(t, ok)
	watchPath, ok := runtime.backgroundTasks.OutputPath(launch(background.KindMonitor))
	require.True(t, ok)

	admitted, ok := runtime.admitBackgroundLogPath(commandPath)
	require.True(t, ok, "a backgrounded command's log must be readable")
	assert.Equal(t, commandPath, admitted)

	admitted, ok = runtime.admitBackgroundLogPath(watchPath)
	require.True(t, ok, "a watch's log must stay readable")
	assert.Equal(t, watchPath, admitted)

	assert.Equal(t, int64(tools.ShellMaximumOutput), int64(shellBackgroundOutputBytes))
	assert.Equal(t, int64(shellBackgroundOutputBytes), int64(backgroundLogReadBytes),
		"the read bound covers the widest kind")
	assert.Equal(t, int64(10<<20), int64(backgroundOutputBytes), "a watch's own log cap did not move")
}

// TestRuntimeTeardownReapsARunningCommand pins that a backgrounded command is
// reaped like a watch: closing the Runtime leaves it terminal.
func TestRuntimeTeardownReapsARunningCommand(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel())

	shell, ok := runtime.controller.Tool("shell")
	require.True(t, ok)

	call := agent.ToolCall{
		ID: "call-reap", Name: "shell",
		Args: ai.JSON(`{"command":"sleep 30","block_until_ms":0}`),
	}
	decision := runtime.controller.BeforeTool(t.Context(), agent.ToolCallInfo{ToolCall: call})
	require.Equal(t, agent.ToolDecisionAllow, decision.Action, decision.Reason)

	parts, err := shell.Exec(t.Context(), call)
	require.NoError(t, err)
	require.Len(t, parts, 1)

	text, ok := parts[0].(ai.TextPart)
	require.True(t, ok)

	header, _, err := tools.ParseResult(text.Text)
	require.NoError(t, err)
	require.NotNil(t, header.Background)

	closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	require.NoError(t, runtime.Close(closeCtx))

	stored, found := runtime.backgroundTasks.Snapshot(header.Background.TaskID)
	require.True(t, found)
	assert.True(t, stored.Completed)
}
