package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registryRead wires the Tool's injected read surface to a real Registry the way
// the Runtime does.
func registryRead(registry *background.Registry) ReadBackgroundTasks {
	return func(
		_ context.Context,
		ids []string,
		mode string,
		budget time.Duration,
	) ([]background.TaskOutput, background.WaitOutcome) {
		if mode == TaskOutputModePoll || budget <= 0 {
			return registry.TaskOutputs(ids), background.WaitOutcomeCompleted
		}

		return registry.Wait(context.Background(), ids, mode == TaskOutputModeWaitAny, budget)
	}
}

func newTaskOutputRegistry(t *testing.T) *background.Registry {
	t.Helper()

	registry, err := background.NewRegistry(background.Options{
		OutputDir:    t.TempDir(),
		KillToolName: background.KillTaskToolName,
		OutputBytes:  1 << 20,
		TailBytes:    1024,
	})
	require.NoError(t, err)

	return registry
}

func launchTaskOutputTask(
	t *testing.T,
	registry *background.Registry,
	start background.Start,
) background.Snapshot {
	t.Helper()

	snapshot, err := registry.Launch(t.Context(), background.Request{
		Kind: background.KindMonitor, Command: "deploy", Description: "deploy log",
		Timeout: time.Minute, Start: start,
	})
	require.NoError(t, err)

	return snapshot
}

func finishedStart(line string) background.Start {
	return func(ctx context.Context, sink execution.Sink) (execution.Result, error) {
		if err := sink.WriteOutput(ctx, execution.OutputChunk{
			Stream: execution.StreamStdout, Data: []byte(line + "\n"),
		}); err != nil {
			return execution.Result{Status: execution.StatusCanceled, ExitCode: -1}, err
		}

		return execution.Result{Status: execution.StatusExited, ExitCode: 0}, nil
	}
}

func runningStart() background.Start {
	return func(ctx context.Context, _ execution.Sink) (execution.Result, error) {
		<-ctx.Done()

		return execution.Result{Status: execution.StatusCanceled, ExitCode: -1}, ctx.Err()
	}
}

func waitTaskTerminal(t *testing.T, registry *background.Registry, id string) {
	t.Helper()

	require.Eventually(t, func() bool {
		records := registry.TaskOutputs([]string{id})

		return len(records) == 1 && records[0].Status != background.StatusRunning
	}, 30*time.Second, 10*time.Millisecond)
}

func execTaskOutputTool(t *testing.T, tool agent.Tool, args string) (ResultHeader, string) {
	t.Helper()

	header, body, err := execTaskOutputQuiet(tool, args)
	require.NoError(t, err)

	return header, body
}

// execTaskOutputQuiet runs one call without touching the test's assertions, so a
// goroutine can run it and hand the result back to the test's own goroutine.
func execTaskOutputQuiet(tool agent.Tool, args string) (ResultHeader, string, error) {
	parts, err := tool.Exec(context.Background(), agent.ToolCall{
		ID: "call-1", Name: tool.Decl().Name, Args: ai.JSON(args),
	})
	if err != nil {
		return ResultHeader{}, "", err
	}

	if len(parts) != 1 {
		return ResultHeader{}, "", fmt.Errorf("coding tools: got %d parts", len(parts))
	}

	text, ok := parts[0].(ai.TextPart)
	if !ok {
		return ResultHeader{}, "", errors.New("coding tools: got a non-text part")
	}

	return ParseResult(text.Text)
}

func TestTaskOutputToolDeclarationNamesBothAliasesAndTheCeiling(t *testing.T) {
	t.Parallel()

	tool := NewTaskOutputTool(registryRead(newTaskOutputRegistry(t)), nil)
	decl := tool.Decl()

	assert.Equal(t, TaskOutputName, decl.Name)
	assert.Equal(t, background.TaskOutputToolName, decl.Name)

	schema := string(decl.InputSchema.RawJSON)
	assert.Contains(t, schema, `"task_ids"`)
	assert.Contains(t, schema, `"task_id"`)
	assert.Contains(t, schema, `"maxItems":20`)
	assert.Contains(t, schema, `"maximum":3600000`)
	assert.Contains(t, decl.Description, "3600000")
	assert.Contains(t, decl.Description, "read")
}

// TestTaskOutputToolReadsOneTaskByIDAndByAlias pins the single-id shape and the
// `task_id` alias the model mirrors from the stop Tool.
func TestTaskOutputToolReadsOneTaskByIDAndByAlias(t *testing.T) {
	t.Parallel()

	registry := newTaskOutputRegistry(t)
	snapshot := launchTaskOutputTask(t, registry, finishedStart("line-1"))
	waitTaskTerminal(t, registry, snapshot.ID)

	tool := NewTaskOutputTool(registryRead(registry), nil)

	header, body := execTaskOutputTool(t, tool, `{"task_ids":["`+snapshot.ID+`"]}`)
	assert.True(t, header.OK)
	require.NotNil(t, header.TaskOutput)
	assert.Equal(t, snapshot.ID, header.TaskOutput.TaskID)
	assert.Equal(t, "completed", header.TaskOutput.Status)
	require.NotNil(t, header.TaskOutput.ExitCode)
	assert.Equal(t, 0, *header.TaskOutput.ExitCode)
	assert.Equal(t, "line-1\n", header.TaskOutput.Output)
	assert.Contains(t, header.TaskOutput.OutputFile, snapshot.ID+".log")
	assert.Equal(t, int64(len("line-1\n")), header.TaskOutput.RawOutputBytes)
	assert.NotEmpty(t, header.TaskOutput.Started)
	assert.NotEmpty(t, header.TaskOutput.Ended)
	assert.False(t, header.TaskOutput.Truncated)

	assert.Contains(t, body, "=== Task "+snapshot.ID+" ===")
	assert.Contains(t, body, "Command: deploy")
	assert.Contains(t, body, "Status: completed")
	assert.Contains(t, body, "Exit Code: 0")
	assert.Contains(t, body, "=== Output ===")
	assert.Contains(t, body, "line-1")

	aliased, _ := execTaskOutputTool(t, tool, `{"task_id":"`+snapshot.ID+`"}`)
	assert.True(t, aliased.OK)
	require.NotNil(t, aliased.TaskOutput)
	assert.Equal(t, snapshot.ID, aliased.TaskOutput.TaskID)
}

// TestTaskOutputToolReadsARunningTaskWithoutWaiting pins the poll semantics:
// timeout_ms 0 (and an omitted one) returns the running record with the wait
// hint instead of blocking.
func TestTaskOutputToolReadsARunningTaskWithoutWaiting(t *testing.T) {
	t.Parallel()

	registry := newTaskOutputRegistry(t)
	snapshot := launchTaskOutputTask(t, registry, runningStart())

	tool := NewTaskOutputTool(registryRead(registry), nil)

	for _, args := range []string{
		`{"task_id":"` + snapshot.ID + `"}`,
		`{"task_id":"` + snapshot.ID + `","timeout_ms":0}`,
	} {
		header, body := execTaskOutputTool(t, tool, args)
		assert.True(t, header.OK)
		require.NotNil(t, header.TaskOutput)
		assert.Equal(t, "running", header.TaskOutput.Status)
		assert.Nil(t, header.TaskOutput.ExitCode)
		assert.Empty(t, header.TaskOutput.Ended)
		assert.Contains(t, body, "Status: running")
		assert.Contains(t, body, "Use timeout_ms to wait for completion.")
		assert.Contains(t, body, "It is still working.")
	}
}

// TestTaskOutputToolWaitsUntilCompletion proves a positive timeout_ms blocks
// until the task ends and returns its terminal record.
func TestTaskOutputToolWaitsUntilCompletion(t *testing.T) {
	t.Parallel()

	registry := newTaskOutputRegistry(t)

	started := make(chan struct{})
	release := make(chan struct{})

	start := func(ctx context.Context, sink execution.Sink) (execution.Result, error) {
		close(started)

		select {
		case <-release:
			return finishedStart("done")(ctx, sink)
		case <-ctx.Done():
			return execution.Result{Status: execution.StatusCanceled, ExitCode: -1}, ctx.Err()
		}
	}

	snapshot := launchTaskOutputTask(t, registry, start)

	type outcome struct {
		header ResultHeader
		body   string
		err    error
	}

	done := make(chan outcome, 1)

	go func() {
		header, body, err := execTaskOutputQuiet(
			NewTaskOutputTool(registryRead(registry), nil),
			`{"task_id":"`+snapshot.ID+`","timeout_ms":30000}`,
		)
		done <- outcome{header: header, body: body, err: err}
	}()

	<-started
	close(release)

	value := <-done
	require.NoError(t, value.err)
	assert.True(t, value.header.OK)
	require.NotNil(t, value.header.TaskOutput)
	assert.Equal(t, "completed", value.header.TaskOutput.Status)
	assert.Contains(t, value.body, "done")
}

// TestTaskOutputToolReportsTheElapsedWait pins the wait that ran out: the
// running record carries the reference's elapsed wording.
func TestTaskOutputToolReportsTheElapsedWait(t *testing.T) {
	t.Parallel()

	registry := newTaskOutputRegistry(t)
	snapshot := launchTaskOutputTask(t, registry, runningStart())

	header, body := execTaskOutputTool(
		t, NewTaskOutputTool(registryRead(registry), nil),
		`{"task_id":"`+snapshot.ID+`","timeout_ms":100}`,
	)
	assert.True(t, header.OK)
	require.NotNil(t, header.TaskOutput)
	assert.Equal(t, "running", header.TaskOutput.Status)
	assert.Contains(t, body, "Waited the requested 100ms.")
	assert.Contains(t, body, "do not kill this task just because this wait returned")
}

// TestTaskOutputWaitClampsToTheCeiling pins the one-hour ceiling: the effective
// budget is the clamped one, and the hint says so.
func TestTaskOutputWaitClampsToTheCeiling(t *testing.T) {
	t.Parallel()

	oversized := int64(7_200_000)

	waiting, requested, budget := taskOutputWait(&oversized)
	assert.True(t, waiting)
	assert.Equal(t, 2*time.Hour, requested)
	assert.Equal(t, time.Hour, budget)

	record := background.TaskOutput{Found: true, Status: background.StatusRunning}
	hinted := background.ApplyWaitHint(record, background.WaitHintElapsed, requested, budget)
	assert.Contains(t, hinted.Output, "Waited 3600s, the per-call maximum, of the 7200s you requested.")

	zero := int64(0)
	waiting, requested, budget = taskOutputWait(&zero)
	assert.False(t, waiting)
	assert.Zero(t, requested)
	assert.Zero(t, budget)

	waiting, requested, budget = taskOutputWait(nil)
	assert.False(t, waiting)
	assert.Zero(t, requested)
	assert.Zero(t, budget)
}

// TestTaskOutputToolResolvesTheAliasAndDeduplicates pins the argument boundary:
// a plural list wins over the singular alias, and duplicate ids collapse to one
// record preserving first-seen order, exactly as the reference resolves them.
func TestTaskOutputToolResolvesTheAliasAndDeduplicates(t *testing.T) {
	t.Parallel()

	registry := newTaskOutputRegistry(t)
	snapshot := launchTaskOutputTask(t, registry, finishedStart("one"))
	waitTaskTerminal(t, registry, snapshot.ID)

	tool := NewTaskOutputTool(registryRead(registry), nil)

	header, _ := execTaskOutputTool(t, tool,
		`{"task_ids":["`+snapshot.ID+`"],"task_id":"monitor-missing"}`)
	assert.True(t, header.OK)
	require.NotNil(t, header.TaskOutput)
	assert.Equal(t, snapshot.ID, header.TaskOutput.TaskID)

	header, _ = execTaskOutputTool(t, tool,
		`{"task_ids":["`+snapshot.ID+`","`+snapshot.ID+`"]}`)
	assert.True(t, header.OK)
	require.NotNil(t, header.TaskOutput, "duplicate ids must collapse to a single-id read")
	assert.Equal(t, snapshot.ID, header.TaskOutput.TaskID)
}

// TestTaskOutputToolRefusesUnusableCalls pins the named limits: an empty list
// and more than twenty ids are invalid arguments, not empty reads.
func TestTaskOutputToolRefusesUnusableCalls(t *testing.T) {
	t.Parallel()

	tool := NewTaskOutputTool(registryRead(newTaskOutputRegistry(t)), nil)

	_, err := tool.Exec(t.Context(), agent.ToolCall{
		ID: "call-1", Name: TaskOutputName, Args: ai.JSON(`{"task_ids":[]}`),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one task id")

	ids := make([]string, 0, maxTaskOutputIDs+1)
	for index := range maxTaskOutputIDs + 1 {
		ids = append(ids, "monitor-"+string(rune('a'+index)))
	}

	args := `{"task_ids":["` + strings.Join(ids, `","`) + `"]}`
	_, err = tool.Exec(t.Context(), agent.ToolCall{
		ID: "call-2", Name: TaskOutputName, Args: ai.JSON(args),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "maximum of 20")

	// A missing read surface is reported, never swallowed into an empty success.
	unavailable := NewTaskOutputTool(nil, nil)
	_, err = unavailable.Exec(t.Context(), agent.ToolCall{
		ID: "call-3", Name: TaskOutputName, Args: ai.JSON(`{"task_id":"monitor-1"}`),
	})
	assert.Error(t, err)
}

// TestTaskOutputToolNamesKnownIDsOnNotFound keeps the stop Tool's recovery hint:
// an unknown id names the ids the registry knows.
func TestTaskOutputToolNamesKnownIDsOnNotFound(t *testing.T) {
	t.Parallel()

	registry := newTaskOutputRegistry(t)
	snapshot := launchTaskOutputTask(t, registry, runningStart())

	tool := NewTaskOutputTool(
		registryRead(registry),
		func(context.Context) []string { return []string{snapshot.ID} },
	)

	header, body := execTaskOutputTool(t, tool, `{"task_id":"monitor-missing"}`)
	assert.False(t, header.OK)
	assert.Equal(t, "not_found", header.Code)
	require.NotNil(t, header.TaskOutput)
	assert.Equal(t, background.StatusNotFound, header.TaskOutput.Status)
	assert.Equal(t, `Task "monitor-missing" not found. Known task ids: `+snapshot.ID+`.`, body)
}

// TestTaskOutputToolRendersAMultiRead pins the multi shape: the mode, one entry
// per asked id, and the completed-count summary.
func TestTaskOutputToolRendersAMultiRead(t *testing.T) {
	t.Parallel()

	registry := newTaskOutputRegistry(t)
	first := launchTaskOutputTask(t, registry, finishedStart("one"))
	second := launchTaskOutputTask(t, registry, runningStart())
	waitTaskTerminal(t, registry, first.ID)

	tool := NewTaskOutputTool(registryRead(registry), nil)

	header, body := execTaskOutputTool(
		t, tool, `{"task_ids":["`+first.ID+`","`+second.ID+`"]}`,
	)
	assert.True(t, header.OK)
	require.NotNil(t, header.TaskOutputs)
	assert.Equal(t, TaskOutputModePoll, header.TaskOutputs.Mode)
	require.Len(t, header.TaskOutputs.Results, 2)
	assert.Equal(t, "1/2 tasks completed (poll)", header.TaskOutputs.Summary)

	assert.Contains(t, body, "=== Multi-wait (poll) ===")
	assert.Contains(t, body, "--- Task "+first.ID+" [completed] ---")
	assert.Contains(t, body, "--- Task "+second.ID+" [running] ---")
	assert.Contains(t, body, "1/2 tasks completed (poll)")

	// A known id beside an unknown one stays ok, and the unknown one renders as
	// its own not_found record.
	header, body = execTaskOutputTool(
		t, tool, `{"task_ids":["`+first.ID+`","monitor-missing"]}`,
	)
	assert.True(t, header.OK)
	require.NotNil(t, header.TaskOutputs)
	assert.Equal(t, "1/2 tasks completed (poll)", header.TaskOutputs.Summary)
	assert.Contains(t, body, "--- Task monitor-missing [not_found] ---")
	assert.Contains(t, body, "Task monitor-missing not found.")

	// A list where nothing is known is a failed call, not an empty success.
	header, body = execTaskOutputTool(
		t, tool, `{"task_ids":["monitor-missing","monitor-other"]}`,
	)
	assert.False(t, header.OK)
	assert.Equal(t, "not_found", header.Code)
	assert.Contains(t, body, "not found")
}

// TestTaskOutputToolsAreAbsentWithoutARegistry pins the registration boundary: a
// catalog built with no background read offers neither Tool.
func TestTaskOutputToolsAreAbsentWithoutARegistry(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	opened, err := workspace.Open(root)
	require.NoError(t, err)

	tree, err := workspace.OpenTree(opened)
	require.NoError(t, err)

	t.Cleanup(func() { _ = tree.Close() })

	policy := catalog.AllowAll("test", catalog.RiskWrite)

	plain, err := NewCatalog(tree, DefaultLimits())
	require.NoError(t, err)

	snapshot, err := plain.Snapshot(t.Context(), policy)
	require.NoError(t, err)

	for _, tool := range snapshot {
		assert.NotEqual(t, TaskOutputName, tool.Decl().Name)
		assert.NotEqual(t, WaitTasksName, tool.Decl().Name)
	}

	registry := newTaskOutputRegistry(t)

	withRead, err := NewCatalog(tree, DefaultLimits(),
		WithReadBackgroundTasks(registryRead(registry)),
	)
	require.NoError(t, err)

	names := make(map[string]bool)

	snapshot, err = withRead.Snapshot(t.Context(), policy)
	require.NoError(t, err)

	for _, tool := range snapshot {
		names[tool.Decl().Name] = true
	}

	assert.True(t, names[TaskOutputName], "the task-output Tool must be registered with a read surface")
	assert.True(t, names[WaitTasksName], "the wait Tool must be registered with a read surface")
}

// TestTaskOutputToolsNameBothIDSources pins the declaration a model reads: the
// same two Tools resolve a background task and a delegated subagent, so each
// declaration has to say where an id comes from.
func TestTaskOutputToolsNameBothIDSources(t *testing.T) {
	t.Parallel()

	registry := newTaskOutputRegistry(t)

	for _, tool := range []agent.Tool{
		NewTaskOutputTool(registryRead(registry), nil),
		NewWaitTasksTool(registryRead(registry), nil),
	} {
		decl := tool.Decl()
		assert.Contains(t, decl.Description, "subagent", decl.Name)
		assert.Contains(t, string(decl.InputSchema.RawJSON), "subagent", decl.Name)
	}
}

// TestTaskOutputToolsNotFoundSentenceIgnoresTheIDShape pins the enumeration rule
// for both Tools: an unknown id of either shape reads the same not-found
// sentence, and that sentence names the registry's ids only, because the stop
// Tool shares it and cannot stop a child.
func TestTaskOutputToolsNotFoundSentenceIgnoresTheIDShape(t *testing.T) {
	t.Parallel()

	read := func(
		_ context.Context, ids []string, _ string, _ time.Duration,
	) ([]background.TaskOutput, background.WaitOutcome) {
		records := make([]background.TaskOutput, 0, len(ids))
		for _, id := range ids {
			records = append(records, background.NotFoundTaskOutput(id))
		}

		return records, background.WaitOutcomeCompleted
	}
	known := func(context.Context) []string { return []string{"monitor-1-aaaa"} }

	for _, id := range []string{"monitor-2-bbbb", "s-00000000-0000-4000-8000-000000000000"} {
		header, body := execTaskOutputTool(t, NewTaskOutputTool(read, known), `{"task_ids":["`+id+`"]}`)
		assert.False(t, header.OK)
		assert.Equal(t, string(background.OutcomeNotFound), header.Code)
		assert.Equal(t, `Task "`+id+`" not found. Known task ids: monitor-1-aaaa.`, body)

		_, waitBody := execTaskOutputTool(t, NewWaitTasksTool(read, known),
			`{"task_ids":["`+id+`"],"mode":"wait_any"}`)
		assert.Equal(t, body, waitBody)
	}
}
