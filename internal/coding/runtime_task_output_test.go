package coding

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/rsbin1178/pips/internal/coding/subagent"
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

// childReadFixture is one live background child of a real Runtime: the model
// that serves it, its session id, and the release that lets its held turn end.
type childReadFixture struct {
	runtime *Runtime
	model   *messagingTestModel
	childID string
	release func()
}

// runningChildFixture opens a Runtime with one background child that stays live
// until release is called. The child makes one successful Tool call first, so a
// read of it has a real activity to report.
func runningChildFixture(t *testing.T) childReadFixture {
	t.Helper()

	var (
		started     = make(chan struct{})
		released    = make(chan struct{})
		startOnce   sync.Once
		releaseOnce sync.Once
	)

	model := &messagingTestModel{onRequest: func(request ai.Request) *ai.Response {
		if !isChildSystemText(requestSystemText(request)) {
			if spawnedChildID(request, "spawn-1") != "" {
				return runtimeTextResponse("the parent answer")
			}

			return runtimeToolResponse("spawn-1", subagent.ToolName,
				`{"prompt":"Inspect the target.","description":"inspect the target",`+
					`"subagent_type":"general-purpose","run_in_background":true}`)
		}

		if requestToolResultText(request, "read-1") == "" {
			return runtimeToolResponse("read-1", "read", `{"path":"target.txt"}`)
		}

		startOnce.Do(func() { close(started) })
		<-released

		return runtimeTextResponse("the child answer")
	}}

	base := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(base, "workspace"), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(base, "workspace", "target.txt"), []byte("hello\n"), 0o600,
	))

	runtime := openMessagingTestRuntime(t, base, model, false)
	release := func() { releaseOnce.Do(func() { close(released) }) }
	t.Cleanup(release)

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("delegate")))

	select {
	case <-started:
	case <-time.After(30 * time.Second):
		require.FailNow(t, "the child never reached its held turn")
	}

	summaries, err := runtime.ListSubagents(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	require.Equal(t, subagent.StateRunning, summaries[0].State)

	return childReadFixture{
		runtime: runtime, model: model, childID: summaries[0].ChildSessionID, release: release,
	}
}

// TestRuntimeTaskOutputResolvesAChildBesideABackgroundTask pins the resolution
// order and the one record shape: a registry id and a child's session id both
// resolve through the same read, in the order asked, and an id of neither shape
// reads as the same not-found record. A child maps onto the record the registry
// already produces, so it names no output file.
func TestRuntimeTaskOutputResolvesAChildBesideABackgroundTask(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeToolResponse("delegate-1", subagent.ToolName,
			`{"prompt":"Inspect the target.","description":"inspect the target",`+
				`"subagent_type":"explore","run_in_background":false}`),
		runtimeTextResponse("the child answer"),
		runtimeTextResponse("the parent answer"),
	)
	runtime := openTestRuntime(t, model)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("delegate")))
	require.Contains(t, eventTypes(events), EventSubagentCompleted)

	summaries, err := runtime.ListSubagents(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	childID := summaries[0].ChildSessionID

	snapshot, err := runtime.backgroundTasks.Launch(t.Context(), background.Request{
		Kind: background.KindMonitor, Command: "watch once", Timeout: time.Minute,
		Start: func(context.Context, execution.Sink) (execution.Result, error) {
			return execution.Result{Status: execution.StatusExited, ExitCode: 0}, nil
		},
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		stored, ok := runtime.backgroundTasks.Snapshot(snapshot.ID)

		return ok && stored.Completed
	}, 30*time.Second, 10*time.Millisecond)

	unknown := "s-00000000-0000-4000-8000-000000000000"
	records, outcome := runtime.readBackgroundTasks(
		t.Context(), []string{childID, snapshot.ID, unknown}, tools.TaskOutputModePoll, 0,
	)
	require.Equal(t, background.WaitOutcomeCompleted, outcome)
	require.Len(t, records, 3)

	child := records[0]
	assert.Equal(t, childID, child.TaskID)
	assert.Equal(t, background.SubjectSubagent, child.Subject)
	assert.Equal(t, background.StatusCompleted, child.Status)
	require.NotNil(t, child.ExitCode)
	assert.Equal(t, 0, *child.ExitCode)
	assert.Equal(t, "[subagent:explore] Inspect the target.", child.Command)
	assert.Empty(t, child.OutputFile, "a child has no log file")
	assert.False(t, child.Ended.IsZero())
	assert.False(t, child.Truncated)
	assert.Contains(t, child.Output, "the child answer")
	assert.Contains(t, child.Output, "<subagent_meta>id="+childID)
	assert.Contains(t, child.Output, "tool_calls=0")
	assert.Contains(t, child.Output, `resume_from="`+childID+`"`)
	assert.Contains(t, background.RenderTaskOutputBody(child), "=== Task "+childID+" ===")

	task := records[1]
	assert.Equal(t, snapshot.ID, task.TaskID)
	assert.Equal(t, background.SubjectTask, task.Subject)
	assert.Equal(t, background.StatusCompleted, task.Status)

	assert.Equal(t, unknown, records[2].TaskID)
	assert.False(t, records[2].Found)
	assert.Equal(t, background.StatusNotFound, records[2].Status)
}

// TestRuntimeTaskOutputReadsARunningChild pins the progress body: a live child
// reports its own progress rather than an output dump, and the record still
// names no file.
func TestRuntimeTaskOutputReadsARunningChild(t *testing.T) {
	t.Parallel()

	fixture := runningChildFixture(t)

	records, outcome := fixture.runtime.readBackgroundTasks(
		t.Context(), []string{fixture.childID}, tools.TaskOutputModePoll, 0,
	)
	require.Equal(t, background.WaitOutcomeCompleted, outcome)
	require.Len(t, records, 1)

	record := records[0]
	require.True(t, record.Found)
	assert.Equal(t, background.SubjectSubagent, record.Subject)
	assert.Equal(t, background.StatusRunning, record.Status)
	assert.Nil(t, record.ExitCode)
	assert.True(t, record.Ended.IsZero(), "a live child has not ended")
	assert.Empty(t, record.OutputFile)

	assert.Contains(t, record.Output, "Subagent is still running.")
	assert.Contains(t, record.Output, "Type: general-purpose")
	assert.Contains(t, record.Output, "Description: Inspect the target.")
	assert.Contains(t, record.Output, "Elapsed: ")
	assert.Contains(t, record.Output, "Progress: turn ")
	assert.Contains(t, record.Output, ", 1 tool calls, ")
	assert.Contains(t, record.Output, " tokens (")
	assert.Contains(t, record.Output, "% context)")
	assert.Contains(t, record.Output, "Tools used: read")
	assert.Contains(t, record.Output, "Errors: 0")

	// A poll is not a wait, so the child's own still-running words are absent.
	assert.NotContains(t, record.Output, "do not kill this subagent")
}

// TestRuntimeTaskOutputMapsEveryChildState pins the status vocabulary, the exit
// codes, the body a state selects, and the dump bound, all through the one
// projection the runtime builds a child record with.
func TestRuntimeTaskOutputMapsEveryChildState(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel())

	summary := func(state subagent.State, code string) subagent.Summary {
		return subagent.Summary{
			ChildSessionID: "s-00000000-0000-4000-8000-000000000001",
			Identity:       subagent.AgentIdentity{ID: "explore"},
			Role:           subagent.RoleExplore,
			State:          state,
			TaskPreview:    "Inspect the target.",
			WorktreePath:   "/tmp/child-worktree",
			CreatedAt:      time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC),
			Duration:       1500 * time.Millisecond,
			Turns:          3,
			ToolCalls:      5,
			Usage:          ai.Usage{InputTokens: 12_000, OutputTokens: 3_000},
			Code:           code,
		}
	}
	detail := func(state subagent.State, code string) subagent.Detail {
		value := subagent.Detail{
			Summary: summary(state, code),
			Plan:    subagent.ExecutionPlan{Model: "openai/runtime-test"},
		}
		// Only a succeeded child has a validated result; every other terminal
		// state reports its code where a completed one reports its answer.
		if state == subagent.StateSucceeded {
			value.Result = "the child answer"
		}

		return value
	}

	cases := []struct {
		name     string
		state    subagent.State
		code     string
		status   string
		exitCode *int
		contains []string
		endedSet bool
	}{
		{
			name: "created reads initializing", state: subagent.StateCreated,
			status: background.StatusInitializing,
			contains: []string{
				"Subagent is initializing (creating worktree, resolving config).",
				"Type: explore", "Description: Inspect the target.",
			},
		},
		{
			name: "running reads running", state: subagent.StateRunning,
			status:   background.StatusRunning,
			contains: []string{"Subagent is still running.", "Progress: turn 3, 5 tool calls, 15K/"},
		},
		{
			name: "succeeded reads completed", state: subagent.StateSucceeded,
			status: background.StatusCompleted, exitCode: new(0), endedSet: true,
			contains: []string{
				"the child answer",
				"<subagent_meta>id=s-00000000-0000-4000-8000-000000000001, " +
					"tool_calls=5, turns=3, duration_ms=1500</subagent_meta>",
				"<worktree_path>/tmp/child-worktree</worktree_path>",
				`resume_from="s-00000000-0000-4000-8000-000000000001"`,
			},
		},
		{
			name: "failed reads failed", state: subagent.StateFailed,
			code: "invalid_result", status: background.StatusFailed,
			exitCode: new(1), endedSet: true,
			contains: []string{"invalid_result", "<subagent_meta>id="},
		},
		{
			name: "canceled reads cancelled", state: subagent.StateCanceled,
			code: "canceled", status: background.StatusCancelled, endedSet: true,
			contains: []string{"canceled", "<subagent_meta>id="},
		},
		{
			name: "interrupted reads cancelled with its code", state: subagent.StateInterrupted,
			code: "process_interrupted", status: background.StatusCancelled, endedSet: true,
			contains: []string{"process_interrupted", "<subagent_meta>id="},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			record := runtime.childOutputRecord(detail(test.state, test.code))

			assert.Equal(t, test.status, record.Status)
			assert.Equal(t, test.status == background.StatusRunning ||
				test.status == background.StatusInitializing, record.Live())
			assert.Equal(t, test.exitCode, record.ExitCode)
			assert.Equal(t, test.endedSet, !record.Ended.IsZero())

			for _, value := range test.contains {
				assert.Contains(t, record.Output, value)
			}
		})
	}

	// A result past the bound is cut in place, with the body's real length and
	// the footer that names no file.
	long := detail(subagent.StateSucceeded, "")
	long.Result = strings.Repeat("x", 40_001)

	record := runtime.childOutputRecord(long)
	require.True(t, record.Truncated)
	assert.Equal(t, int64(len(childTerminalOutputBody(long))), record.RawOutputBytes)
	assert.Contains(t, record.Output,
		"[Output truncated - "+strconv.FormatInt(record.RawOutputBytes, 10)+" bytes total]")
	assert.NotContains(t, record.Output, "Use read on")
	assert.Equal(t, "[truncated - "+strconv.FormatInt(record.RawOutputBytes, 10)+" bytes total]",
		record.TruncationHint, "a child has no file, so its hint names no path")
}

// TestRuntimeTaskOutputNamesAChildWithoutAnIdentity pins the command line's
// fallback: the identity id, else the legacy role, else the generic name, so a
// record that carries no identity still names something.
func TestRuntimeTaskOutputNamesAChildWithoutAnIdentity(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel())

	base := subagent.Summary{
		ChildSessionID: "s-00000000-0000-4000-8000-000000000002",
		State:          subagent.StateRunning,
		TaskPreview:    "Inspect the target.",
	}

	withIdentity := base
	withIdentity.Identity = subagent.AgentIdentity{ID: "explore"}
	assert.Equal(t, "[subagent:explore] Inspect the target.",
		runtime.childOutputRecord(subagent.Detail{Summary: withIdentity}).Command)

	withRole := base
	withRole.Role = subagent.RoleExplore
	assert.Equal(t, "[subagent:explore] Inspect the target.",
		runtime.childOutputRecord(subagent.Detail{Summary: withRole}).Command,
		"a legacy record with no identity falls back to its role")

	assert.Equal(t, "[subagent:subagent] Inspect the target.",
		runtime.childOutputRecord(subagent.Detail{Summary: base}).Command,
		"a record with neither identity nor role still names something")
}

// TestRuntimeTaskOutputReadsAnInterruptedChildAsCancelled pins the interrupted
// mapping end to end, on a store a restart left behind: the closed status
// vocabulary reads `cancelled`, and the interruption's own code stays in the
// body so nothing is hidden.
func TestRuntimeTaskOutputReadsAnInterruptedChildAsCancelled(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	first := openDynamicTestRuntimeAt(t, base, SessionTarget{RetainEmpty: true}, newRuntimeModel())
	parentMeta := first.handle.Metadata()
	require.NoError(t, first.Close(t.Context()))

	repository, err := session.NewRepository(first.paths.SessionsDir())
	require.NoError(t, err)

	parent, err := repository.Open(t.Context(), session.OpenOptions{
		ID: parentMeta.ID, WorkspaceID: parentMeta.WorkspaceID,
	})
	require.NoError(t, err)

	child, err := repository.Create(t.Context(), session.CreateOptions{
		WorkspaceID: parentMeta.WorkspaceID, WorkspacePath: parentMeta.WorkspacePath,
		Kind: session.KindSubagent, ParentSessionID: parentMeta.ID,
		ParentRunID: "parent-run", Agent: string(subagent.RoleExplore),
	})
	require.NoError(t, err)

	childID := child.Metadata().ID
	appendInterruptedChildLifecycle(t, parent.Session(), child.Session(), childID, parentMeta.ID)
	require.NoError(t, child.Close())
	require.NoError(t, parent.Close())

	reopened := openDynamicTestRuntimeAt(t, base, SessionTarget{ID: parentMeta.ID}, newRuntimeModel())

	records, outcome := reopened.readBackgroundTasks(
		t.Context(), []string{childID}, tools.TaskOutputModePoll, 0,
	)
	require.Equal(t, background.WaitOutcomeCompleted, outcome)
	require.Len(t, records, 1)
	require.True(t, records[0].Found)
	assert.Equal(t, background.StatusCancelled, records[0].Status)
	assert.Nil(t, records[0].ExitCode, "a cancelled child has no exit code")
	assert.Contains(t, records[0].Output, "process_interrupted")
	assert.Contains(t, records[0].Output, "<subagent_meta>id="+childID)
	assert.NotContains(t, records[0].Output, "resume_from",
		"an interrupted child cannot be resumed, so the record promises no resume")
}

// childWaitResult is one blocking read's outcome, so a test can wait for the
// call itself without blocking the test goroutine.
type childWaitResult struct {
	records []background.TaskOutput
	outcome background.WaitOutcome
}

// TestRuntimeTaskOutputWaitsOnAChild pins the child wait end to end: a blocking
// read returns the child's terminal record as soon as the child ends, and the
// registration it held is what suppressed the completion in the meantime.
func TestRuntimeTaskOutputWaitsOnAChild(t *testing.T) {
	t.Parallel()

	fixture := runningChildFixture(t)

	waited := make(chan childWaitResult, 1)

	go func() {
		records, outcome := fixture.runtime.readBackgroundTasks(
			context.Background(), []string{fixture.childID}, tools.TaskOutputModeWaitAll, time.Minute,
		)
		waited <- childWaitResult{records: records, outcome: outcome}
	}()

	require.Eventually(t, func() bool {
		return fixture.runtime.childResultTaken(fixture.childID)
	}, 30*time.Second, 10*time.Millisecond)

	fixture.release()

	select {
	case value := <-waited:
		assert.Equal(t, background.WaitOutcomeCompleted, value.outcome)
		require.Len(t, value.records, 1)
		assert.Equal(t, background.StatusCompleted, value.records[0].Status)
		assert.Contains(t, value.records[0].Output, "the child answer")
		assert.True(t, fixture.runtime.childResultTaken(fixture.childID))
	case <-time.After(30 * time.Second):
		t.Fatal("the child wait did not return when the child ended")
	}
}

// TestRuntimeElapsedChildWaitReturnsTheRunningRecord pins the elapsed case: the
// running record comes back in the child's own wait words, and the registration
// is released so the child's later completion still wakes.
func TestRuntimeElapsedChildWaitReturnsTheRunningRecord(t *testing.T) {
	t.Parallel()

	fixture := runningChildFixture(t)

	records, outcome := fixture.runtime.readBackgroundTasks(
		t.Context(), []string{fixture.childID}, tools.TaskOutputModeWaitAll, 20*time.Millisecond,
	)
	assert.Equal(t, background.WaitOutcomeDeadline, outcome)
	require.Len(t, records, 1)
	assert.Equal(t, background.StatusRunning, records[0].Status)
	assert.Equal(t, background.SubjectSubagent, records[0].Subject)
	assert.False(t, fixture.runtime.childResultTaken(fixture.childID),
		"an elapsed wait must release its registration")

	// The Tool layer appends the wait words to the record the runtime produced,
	// so the child's own subject reaches the model.
	tool := tools.NewTaskOutputTool(
		fixture.runtime.readBackgroundTasks, fixture.runtime.KnownBackgroundTaskIDs,
	)

	parts, err := tool.Exec(t.Context(), agent.ToolCall{
		ID: "call-wait", Name: tools.TaskOutputName,
		Args: ai.JSON(`{"task_ids":["` + fixture.childID + `"],"timeout_ms":20}`),
	})
	require.NoError(t, err)
	require.Len(t, parts, 1)

	text, ok := parts[0].(ai.TextPart)
	require.True(t, ok)

	header, body, err := tools.ParseResult(text.Text)
	require.NoError(t, err)
	require.True(t, header.OK)
	require.NotNil(t, header.TaskOutput)
	assert.Equal(t, fixture.childID, header.TaskOutput.TaskID)
	assert.Contains(t, body, "do not kill this subagent and do not tell it to stop")
	assert.NotContains(t, body, "do not kill this task")
}

// TestRuntimeTaskOutputWaitsOnAMixedList pins the mixed wait: wait_any returns at
// the first resolution of either kind and cancels the wait it left behind, and a
// wait-all over the same two resolves both.
func TestRuntimeTaskOutputWaitsOnAMixedList(t *testing.T) {
	t.Parallel()

	fixture := runningChildFixture(t)

	snapshot, err := fixture.runtime.backgroundTasks.Launch(t.Context(), background.Request{
		Kind: background.KindMonitor, Command: "watch", Timeout: time.Minute,
		Start: func(ctx context.Context, _ execution.Sink) (execution.Result, error) {
			<-ctx.Done()

			return execution.Result{Status: execution.StatusCanceled, ExitCode: -1}, ctx.Err()
		},
	})
	require.NoError(t, err)

	waited := make(chan childWaitResult, 1)

	go func() {
		records, outcome := fixture.runtime.readBackgroundTasks(
			context.Background(), []string{snapshot.ID, fixture.childID},
			tools.TaskOutputModeWaitAny, time.Minute,
		)
		waited <- childWaitResult{records: records, outcome: outcome}
	}()

	require.Eventually(t, func() bool {
		return fixture.runtime.childResultTaken(fixture.childID)
	}, 30*time.Second, 10*time.Millisecond)

	fixture.release()

	select {
	case value := <-waited:
		assert.Equal(t, background.WaitOutcomeCompleted, value.outcome)
		require.Len(t, value.records, 2)
		assert.Equal(t, background.StatusRunning, value.records[0].Status,
			"the watch keeps running; the wait-any cancelled its own wait")
		assert.Equal(t, background.StatusCompleted, value.records[1].Status)
	case <-time.After(30 * time.Second):
		t.Fatal("the mixed wait-any did not return at the child's resolution")
	}

	// The wait the wait-any left behind is cancelled, and its registration on
	// the watch is released when it returns, so an early return leaks neither a
	// goroutine nor a waiter.
	require.Eventually(t, func() bool {
		return !fixture.runtime.backgroundTasks.ResultTaken(snapshot.ID)
	}, 30*time.Second, 10*time.Millisecond)

	// The watch's own completion resolves the same mixed list under wait-all.
	require.Equal(t, background.OutcomeKilled,
		fixture.runtime.backgroundTasks.Kill(snapshot.ID, background.KillUser))

	records, outcome := fixture.runtime.readBackgroundTasks(
		t.Context(), []string{snapshot.ID, fixture.childID}, tools.TaskOutputModeWaitAll, time.Minute,
	)
	assert.Equal(t, background.WaitOutcomeCompleted, outcome)
	require.Len(t, records, 2)
	assert.Equal(t, background.StatusCancelled, records[0].Status)
	assert.Equal(t, background.StatusCompleted, records[1].Status)
}

// TestRuntimeTaskOutputWaitAnyReturnsAtTheWatchResolution is the mirror of the
// child-first mixed wait: the watch resolves first, so wait_any returns at the
// watch while the child keeps running.
func TestRuntimeTaskOutputWaitAnyReturnsAtTheWatchResolution(t *testing.T) {
	t.Parallel()

	fixture := runningChildFixture(t)

	snapshot, err := fixture.runtime.backgroundTasks.Launch(t.Context(), background.Request{
		Kind: background.KindMonitor, Command: "watch", Timeout: time.Minute,
		Start: func(ctx context.Context, _ execution.Sink) (execution.Result, error) {
			<-ctx.Done()

			return execution.Result{Status: execution.StatusCanceled, ExitCode: -1}, ctx.Err()
		},
	})
	require.NoError(t, err)

	waited := make(chan childWaitResult, 1)

	go func() {
		records, outcome := fixture.runtime.readBackgroundTasks(
			context.Background(), []string{fixture.childID, snapshot.ID},
			tools.TaskOutputModeWaitAny, time.Minute,
		)
		waited <- childWaitResult{records: records, outcome: outcome}
	}()

	// The watch's own wait is standing by before it is stopped, so the
	// resolution the wait-any returns at is the watch's, not an early return.
	require.Eventually(t, func() bool {
		return fixture.runtime.backgroundTasks.ResultTaken(snapshot.ID)
	}, 30*time.Second, 10*time.Millisecond)

	require.Equal(t, background.OutcomeKilled,
		fixture.runtime.backgroundTasks.Kill(snapshot.ID, background.KillUser))

	select {
	case value := <-waited:
		assert.Equal(t, background.WaitOutcomeCompleted, value.outcome)
		require.Len(t, value.records, 2)
		assert.Equal(t, background.StatusRunning, value.records[0].Status,
			"the child keeps running; the wait-any cancelled only its wait")
		assert.Equal(t, background.StatusCancelled, value.records[1].Status,
			"the watch resolved first and the wait-any returned at it")
	case <-time.After(30 * time.Second):
		t.Fatal("the mixed wait-any did not return at the watch's resolution")
	}
}

// TestRuntimeChildReadSuppressesTheCompletion pins the child half of the
// suppression rule: a blocking read that returns a child's terminal record
// carries it, so the completion the same child produced becomes no message and
// its queue record is acknowledged instead of retried.
func TestRuntimeChildReadSuppressesTheCompletion(t *testing.T) {
	t.Parallel()

	fixture := runningChildFixture(t)
	require.NoError(t, fixture.runtime.stopNotificationCoordinator(t.Context()))

	waited := make(chan childWaitResult, 1)

	go func() {
		records, outcome := fixture.runtime.readBackgroundTasks(
			context.Background(), []string{fixture.childID}, tools.TaskOutputModeWaitAll, time.Minute,
		)
		waited <- childWaitResult{records: records, outcome: outcome}
	}()

	require.Eventually(t, func() bool {
		return fixture.runtime.childResultTaken(fixture.childID)
	}, 30*time.Second, 10*time.Millisecond)

	fixture.release()

	select {
	case value := <-waited:
		require.Equal(t, background.WaitOutcomeCompleted, value.outcome)
		require.Len(t, value.records, 1)
		require.Equal(t, background.StatusCompleted, value.records[0].Status)
	case <-time.After(30 * time.Second):
		t.Fatal("the child wait did not return when the child ended")
	}

	// The completion reached the durable queue, and the delivery drops it.
	require.Eventually(t, func() bool {
		pending, pendingErr := fixture.runtime.notifications.Pending()

		return pendingErr == nil && len(pending) == 1
	}, 30*time.Second, 10*time.Millisecond)

	requests := len(fixture.model.Requests())
	transcript := len(fixture.runtime.Snapshot().Transcript)

	fixture.runtime.deliverNotificationBatch(t.Context())

	require.Eventually(t, func() bool {
		pending, pendingErr := fixture.runtime.notifications.Pending()

		return pendingErr == nil && len(pending) == 0
	}, 30*time.Second, 10*time.Millisecond)

	assert.Len(t, fixture.model.Requests(), requests,
		"a completion a read already carried must not wake the model")
	assert.Len(t, fixture.runtime.Snapshot().Transcript, transcript)
}

// TestRuntimeReadOfATerminalChildSuppressesTheCompletion pins the other
// ordering: the completion is already on the queue, and a read of the finished
// child drops it when the delivery reaches the chooser.
func TestRuntimeReadOfATerminalChildSuppressesTheCompletion(t *testing.T) {
	t.Parallel()

	fixture := runningChildFixture(t)
	require.NoError(t, fixture.runtime.stopNotificationCoordinator(t.Context()))

	fixture.release()

	require.Eventually(t, func() bool {
		pending, err := fixture.runtime.notifications.Pending()

		return err == nil && len(pending) == 1
	}, 30*time.Second, 10*time.Millisecond)

	records, outcome := fixture.runtime.readBackgroundTasks(
		t.Context(), []string{fixture.childID}, tools.TaskOutputModePoll, 0,
	)
	require.Equal(t, background.WaitOutcomeCompleted, outcome)
	require.Len(t, records, 1)
	require.Equal(t, background.StatusCompleted, records[0].Status)
	assert.True(t, fixture.runtime.childResultTaken(fixture.childID))

	requests := len(fixture.model.Requests())

	fixture.runtime.deliverNotificationBatch(t.Context())

	require.Eventually(t, func() bool {
		pending, err := fixture.runtime.notifications.Pending()

		return err == nil && len(pending) == 0
	}, 30*time.Second, 10*time.Millisecond)

	assert.Len(t, fixture.model.Requests(), requests)

	// The acknowledgement is durable: a fresh inbox over the same session finds
	// no pending record, so a restart cannot resurrect the dropped completion.
	reopened, err := subagent.NewNotificationInbox(
		fixture.runtime.session,
		fixture.runtime.handle.Metadata().ID,
		subagent.DefaultLimits().MaxResultBytes,
	)
	require.NoError(t, err)

	replayed, err := reopened.Pending()
	require.NoError(t, err)
	assert.Empty(t, replayed, "a suppressed completion must not return after a restart")
}

// TestRuntimeElapsedChildWaitLeavesTheCompletion pins the release rule: a wait
// that ran out while the child was still running stops being a reason to
// suppress, so the child's own later completion still produces a message.
func TestRuntimeElapsedChildWaitLeavesTheCompletion(t *testing.T) {
	t.Parallel()

	fixture := runningChildFixture(t)
	require.NoError(t, fixture.runtime.stopNotificationCoordinator(t.Context()))

	records, outcome := fixture.runtime.readBackgroundTasks(
		t.Context(), []string{fixture.childID}, tools.TaskOutputModeWaitAll, 20*time.Millisecond,
	)
	require.Equal(t, background.WaitOutcomeDeadline, outcome)
	require.Len(t, records, 1)
	require.Equal(t, background.StatusRunning, records[0].Status)
	require.False(t, fixture.runtime.childResultTaken(fixture.childID))

	fixture.release()

	require.Eventually(t, func() bool {
		pending, err := fixture.runtime.notifications.Pending()

		return err == nil && len(pending) == 1
	}, 30*time.Second, 10*time.Millisecond)

	requests := len(fixture.model.Requests())

	fixture.runtime.deliverNotificationBatch(t.Context())

	require.Eventually(t, func() bool {
		return len(fixture.model.Requests()) > requests
	}, 30*time.Second, 10*time.Millisecond)
}

// TestRuntimeChildReadLeavesASiblingsCompletionAlone pins the boundary: only the
// child a read took is suppressed, so a sibling's completion still wakes.
func TestRuntimeChildReadLeavesASiblingsCompletionAlone(t *testing.T) {
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

	release()

	records, _ := runtime.readBackgroundTasks(
		t.Context(), []string{firstID}, tools.TaskOutputModeWaitAll, time.Minute,
	)
	require.Len(t, records, 1)
	require.Equal(t, background.StatusCompleted, records[0].Status)
	assert.True(t, runtime.childResultTaken(firstID))
	assert.False(t, runtime.childResultTaken(secondID))

	requests := len(model.Requests())

	runtime.deliverNotificationBatch(t.Context())

	require.Eventually(t, func() bool {
		return len(model.Requests()) > requests
	}, 30*time.Second, 10*time.Millisecond)

	assert.True(t, requestContainsText(model.Requests()[requests], secondID),
		"the sibling's completion must survive a read of another child")
}

// TestRuntimeTaskOutputRejectsAForeignChildID pins the ownership boundary: a
// child of another conversation in the same store is not this session's child,
// so its id reads the same not-found record an unknown id does.
func TestRuntimeTaskOutputRejectsAForeignChildID(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	other := openTestRuntimeAt(t, base, SessionTarget{}, newRuntimeModel(
		runtimeToolResponse("delegate-1", subagent.ToolName,
			`{"prompt":"Inspect the target.","description":"inspect the target",`+
				`"subagent_type":"explore","run_in_background":false}`),
		runtimeTextResponse("the child answer"),
		runtimeTextResponse("the parent answer"),
	))

	events := collectRuntimeEvents(t, other.Prompt(t.Context(), ai.UserText("delegate")))
	require.Contains(t, eventTypes(events), EventSubagentCompleted)

	summaries, err := other.ListSubagents(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 1)

	runtime := openTestRuntimeAt(t, base, SessionTarget{}, newRuntimeModel())

	records, outcome := runtime.readBackgroundTasks(
		t.Context(), []string{summaries[0].ChildSessionID}, tools.TaskOutputModePoll, 0,
	)
	require.Equal(t, background.WaitOutcomeCompleted, outcome)
	require.Len(t, records, 1)
	assert.False(t, records[0].Found)
	assert.Equal(t, background.StatusNotFound, records[0].Status)
}

// TestRuntimeTaskOutputWaitIgnoresAnUnknownID pins the wait's subject set: an id
// of neither shape reads as not-found and must not resolve a wait_any the way a
// real completion does, exactly as the registry's own wait skips an id it does
// not know.
func TestRuntimeTaskOutputWaitIgnoresAnUnknownID(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel())

	snapshot, err := runtime.backgroundTasks.Launch(t.Context(), background.Request{
		Kind: background.KindMonitor, Command: "watch", Timeout: time.Minute,
		Start: func(ctx context.Context, _ execution.Sink) (execution.Result, error) {
			<-ctx.Done()

			return execution.Result{Status: execution.StatusCanceled, ExitCode: -1}, ctx.Err()
		},
	})
	require.NoError(t, err)

	unknown := "s-00000000-0000-4000-8000-000000000000"

	records, outcome := runtime.readBackgroundTasks(
		t.Context(), []string{snapshot.ID, unknown}, tools.TaskOutputModeWaitAny, 20*time.Millisecond,
	)
	assert.Equal(t, background.WaitOutcomeDeadline, outcome,
		"an unknown id must not resolve the wait")
	require.Len(t, records, 2)
	assert.Equal(t, background.StatusRunning, records[0].Status)
	assert.False(t, records[1].Found)
}

// TestRuntimeTaskOutputToolReadsAndWaitsOnARealChild drives the flow the model
// actually experiences: a real background child spawned through the real spawn
// Tool, read by its subagent_id while it runs through the real task-output Tool,
// then waited on through the same Tool, with the completion that read already
// carried dropped from the delivery.
func TestRuntimeTaskOutputToolReadsAndWaitsOnARealChild(t *testing.T) {
	t.Parallel()

	fixture := runningChildFixture(t)
	require.NoError(t, fixture.runtime.stopNotificationCoordinator(t.Context()))

	tool := tools.NewTaskOutputTool(
		fixture.runtime.readBackgroundTasks, fixture.runtime.KnownBackgroundTaskIDs,
	)

	// A poll reads the live child: the record names it, carries its progress
	// body and the subagent's own wait words, and names no output file.
	parts, err := tool.Exec(t.Context(), agent.ToolCall{
		ID: "call-read", Name: tools.TaskOutputName,
		Args: ai.JSON(`{"task_ids":["` + fixture.childID + `"]}`),
	})
	require.NoError(t, err)
	require.Len(t, parts, 1)

	text, ok := parts[0].(ai.TextPart)
	require.True(t, ok)

	header, body, err := tools.ParseResult(text.Text)
	require.NoError(t, err)
	require.True(t, header.OK)
	require.NotNil(t, header.TaskOutput)
	assert.Equal(t, fixture.childID, header.TaskOutput.TaskID)
	assert.Equal(t, "[subagent:general-purpose] Inspect the target.", header.TaskOutput.Command)
	assert.Equal(t, background.StatusRunning, header.TaskOutput.Status)
	assert.Empty(t, header.TaskOutput.OutputFile, "a child has no log file")
	assert.Contains(t, body, "Subagent is still running.")
	assert.Contains(t, body, "Tools used: read")
	assert.Contains(t, body, "do not kill this subagent and do not tell it to stop")

	// A blocking read through the same Tool takes the child's terminal record
	// when the child ends inside the wait.
	type toolResult struct {
		parts []ai.Part
		err   error
	}

	waited := make(chan toolResult, 1)

	go func() {
		value, execErr := tool.Exec(context.Background(), agent.ToolCall{
			ID: "call-wait", Name: tools.TaskOutputName,
			Args: ai.JSON(`{"task_ids":["` + fixture.childID + `"],"timeout_ms":60000}`),
		})
		waited <- toolResult{parts: value, err: execErr}
	}()

	require.Eventually(t, func() bool {
		return fixture.runtime.childResultTaken(fixture.childID)
	}, 30*time.Second, 10*time.Millisecond)

	fixture.release()

	select {
	case value := <-waited:
		require.NoError(t, value.err)
		require.Len(t, value.parts, 1)

		waitedText, ok := value.parts[0].(ai.TextPart)
		require.True(t, ok)

		waitedHeader, waitedBody, parseErr := tools.ParseResult(waitedText.Text)
		require.NoError(t, parseErr)
		require.True(t, waitedHeader.OK)
		require.NotNil(t, waitedHeader.TaskOutput)
		assert.Equal(t, background.StatusCompleted, waitedHeader.TaskOutput.Status)
		assert.Contains(t, waitedBody, "the child answer")
		assert.Contains(t, waitedBody, "<subagent_meta>id="+fixture.childID)
	case <-time.After(30 * time.Second):
		t.Fatal("the blocking read did not return when the child ended")
	}

	// The read carried the result, so the completion the child produced is
	// dropped and acknowledged instead of waking the model again.
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
		"a completion a Tool read already carried must not wake the model")
}

// TestRuntimeWatchCompletionSurvivesAChildSuppression pins the boundary between
// the two parent-facing queues: the suppression filter sits on the child
// completion queue, so a watch's own completion still reaches the model while a
// child's result is already taken.
func TestRuntimeWatchCompletionSurvivesAChildSuppression(t *testing.T) {
	t.Parallel()

	fixture := runningChildFixture(t)
	require.NoError(t, fixture.runtime.stopNotificationCoordinator(t.Context()))

	fixture.release()

	require.Eventually(t, func() bool {
		pending, err := fixture.runtime.notifications.Pending()

		return err == nil && len(pending) == 1
	}, 30*time.Second, 10*time.Millisecond)

	records, outcome := fixture.runtime.readBackgroundTasks(
		t.Context(), []string{fixture.childID}, tools.TaskOutputModePoll, 0,
	)
	require.Equal(t, background.WaitOutcomeCompleted, outcome)
	require.Len(t, records, 1)
	require.Equal(t, background.StatusCompleted, records[0].Status)
	require.True(t, fixture.runtime.childResultTaken(fixture.childID))

	requests := len(fixture.model.Requests())

	fixture.runtime.deliverNotificationBatch(t.Context())

	require.Eventually(t, func() bool {
		pending, err := fixture.runtime.notifications.Pending()

		return err == nil && len(pending) == 0
	}, 30*time.Second, 10*time.Millisecond)

	assert.Len(t, fixture.model.Requests(), requests,
		"the taken child's completion must not wake the model")

	// A watch's completion rides the other queue and still renders its own
	// parent message.
	snapshot, err := fixture.runtime.backgroundTasks.Launch(t.Context(), background.Request{
		Kind: background.KindMonitor, Command: "watch once", Timeout: time.Minute,
		Start: func(context.Context, execution.Sink) (execution.Result, error) {
			return execution.Result{Status: execution.StatusExited, ExitCode: 0}, nil
		},
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		stored, ok := fixture.runtime.backgroundTasks.Snapshot(snapshot.ID)

		return ok && stored.Completed
	}, 30*time.Second, 10*time.Millisecond)

	require.Eventually(t, func() bool {
		return fixture.runtime.backgroundOutput.observedBackground() >= 1
	}, 30*time.Second, 10*time.Millisecond)

	events, completions := fixture.runtime.backgroundOutput.takeBackground()
	require.Len(t, completions, 1)
	assert.NotEmpty(t, fixture.runtime.backgroundOutput.backgroundMessages(events, completions),
		"a child's taken result must not suppress a watch's own completion")
}
