package tools

import (
	"context"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// delayedStart runs for one delay and then exits, standing in for a task that
// finishes while a wait is blocked on it.
func delayedStart(delay time.Duration, line string) background.Start {
	return func(ctx context.Context, sink execution.Sink) (execution.Result, error) {
		timer := time.NewTimer(delay)
		defer timer.Stop()

		select {
		case <-timer.C:
			return finishedStart(line)(ctx, sink)
		case <-ctx.Done():
			return execution.Result{Status: execution.StatusCanceled, ExitCode: -1}, ctx.Err()
		}
	}
}

func TestWaitTasksToolDeclarationPinsTheModeAndTheCeiling(t *testing.T) {
	t.Parallel()

	tool := NewWaitTasksTool(registryRead(newTaskOutputRegistry(t)), nil)
	decl := tool.Decl()

	assert.Equal(t, WaitTasksName, decl.Name)

	schema := string(decl.InputSchema.RawJSON)
	assert.Contains(t, schema, `"required":["task_ids","mode"]`)
	assert.Contains(t, schema, `"wait_any"`)
	assert.Contains(t, schema, `"wait_all"`)
	assert.Contains(t, schema, `"maxItems":20`)
	assert.Contains(t, schema, `"maximum":3600000`)
	assert.Contains(t, decl.Description, "30000ms")
}

// TestWaitTasksBudgetDefaultsWhenAWaitIsIntended pins the difference from the
// task-output Tool: a wait is the point here, so an omitted or zero timeout is
// the default budget, and an oversized ask is clamped to the ceiling.
func TestWaitTasksBudgetDefaultsWhenAWaitIsIntended(t *testing.T) {
	t.Parallel()

	requested, budget := waitTasksBudget(nil)
	assert.Equal(t, 30*time.Second, requested)
	assert.Equal(t, 30*time.Second, budget)

	zero := int64(0)
	requested, budget = waitTasksBudget(&zero)
	assert.Equal(t, 30*time.Second, requested)
	assert.Equal(t, 30*time.Second, budget)

	oversized := int64(7_200_000)
	requested, budget = waitTasksBudget(&oversized)
	assert.Equal(t, 2*time.Hour, requested)
	assert.Equal(t, time.Hour, budget)
}

// TestWaitTasksToolWaitsForAllOnTheSharedMultiPath pins wait_all: it shares the
// task-output Tool's multi path and reports the completed-count summary.
func TestWaitTasksToolWaitsForAllOnTheSharedMultiPath(t *testing.T) {
	t.Parallel()

	registry := newTaskOutputRegistry(t)
	first := launchTaskOutputTask(t, registry, finishedStart("one"))
	second := launchTaskOutputTask(t, registry, finishedStart("two"))

	header, body := execTaskOutputTool(
		t, NewWaitTasksTool(registryRead(registry), nil),
		`{"task_ids":["`+first.ID+`","`+second.ID+`"],"mode":"wait_all","timeout_ms":30000}`,
	)
	assert.True(t, header.OK)
	require.NotNil(t, header.TaskOutputs)
	assert.Equal(t, TaskOutputModeWaitAll, header.TaskOutputs.Mode)
	assert.Equal(t, "2/2 tasks completed (wait_all)", header.TaskOutputs.Summary)
	assert.Contains(t, body, "=== Multi-wait (wait_all) ===")
}

// TestWaitTasksToolReturnsAtTheFirstCompletionForWaitAny pins the one behaviour
// the task-output Tool has no place for: wait_any returns as soon as one id
// resolves, while another keeps running.
func TestWaitTasksToolReturnsAtTheFirstCompletionForWaitAny(t *testing.T) {
	t.Parallel()

	registry := newTaskOutputRegistry(t)
	second := launchTaskOutputTask(t, registry, delayedStart(200*time.Millisecond, "done"))
	first := launchTaskOutputTask(t, registry, runningStart())

	header, body := execTaskOutputTool(
		t, NewWaitTasksTool(registryRead(registry), nil),
		`{"task_ids":["`+first.ID+`","`+second.ID+`"],"mode":"wait_any","timeout_ms":30000}`,
	)
	assert.True(t, header.OK)
	require.NotNil(t, header.TaskOutputs)
	assert.Equal(t, TaskOutputModeWaitAny, header.TaskOutputs.Mode)
	assert.Equal(t, "1/2 tasks completed (wait_any)", header.TaskOutputs.Summary)

	statuses := make(map[string]string, len(header.TaskOutputs.Results))
	for _, record := range header.TaskOutputs.Results {
		statuses[record.TaskID] = record.Status
	}

	assert.Equal(t, background.StatusCompleted, statuses[second.ID])
	assert.Equal(t, background.StatusRunning, statuses[first.ID])

	assert.Contains(t, body, "=== Multi-wait (wait_any) ===")
	assert.Contains(t, body, "--- Task "+second.ID+" [completed] ---")
	assert.Contains(t, body, "--- Task "+first.ID+" [running] ---")
	assert.Contains(t, body, "Wait returned early because another finished.")
}

// TestWaitTasksToolRefusesAnInvalidModeAndNamesKnownIDs keeps the argument
// boundary and the recovery hint in one place.
func TestWaitTasksToolRefusesAnInvalidModeAndNamesKnownIDs(t *testing.T) {
	t.Parallel()

	registry := newTaskOutputRegistry(t)
	snapshot := launchTaskOutputTask(t, registry, runningStart())

	tool := NewWaitTasksTool(
		registryRead(registry),
		func(context.Context) []string { return []string{snapshot.ID} },
	)

	_, err := tool.Exec(t.Context(), agent.ToolCall{
		ID: "call-1", Name: WaitTasksName,
		Args: ai.JSON(`{"task_ids":["` + snapshot.ID + `"],"mode":"wait_some"}`),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wait_any or wait_all")

	header, body := execTaskOutputTool(
		t, tool, `{"task_ids":["monitor-missing"],"mode":"wait_all","timeout_ms":1000}`,
	)
	assert.False(t, header.OK)
	assert.Equal(t, "not_found", header.Code)
	assert.Equal(t, `Task "monitor-missing" not found. Known task ids: `+snapshot.ID+`.`, body)
}
