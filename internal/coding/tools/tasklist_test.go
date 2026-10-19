package tools_test

import (
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/tasklist"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func taskExec(t *testing.T, value *catalog.Catalog, args string) (string, error) {
	t.Helper()

	snapshot, err := value.Snapshot(t.Context(), catalog.AllowAll("test", catalog.RiskRead))
	require.NoError(t, err)
	require.Len(t, snapshot, 1)

	parts, err := snapshot[0].Exec(t.Context(), agent.ToolCall{
		ID: "task-1", Name: tasklist.ToolName, Args: ai.JSON(args),
	})
	if err != nil {
		return "", err
	}

	require.Len(t, parts, 1)

	text, ok := parts[0].(ai.TextPart)
	require.True(t, ok)

	return text.Text, nil
}

// TestTaskCatalogMergeReportsTheFoldedPlan pins the tool's counts: a one-item
// merge reports the plan's real totals rather than the call's item count.
func TestTaskCatalogMergeReportsTheFoldedPlan(t *testing.T) {
	t.Parallel()

	plan := tasklist.Snapshot{
		Items: []tasklist.Item{
			{ID: "Inspect", Step: "Inspect", Status: tasklist.StatusInProgress},
			{ID: "Implement", Step: "Implement", Status: tasklist.StatusPending},
			{ID: "Verify", Step: "Verify", Status: tasklist.StatusPending},
		},
		InProgress: 1, Total: 3,
	}
	value, err := tools.NewTaskCatalog(func() tasklist.Snapshot { return plan })
	require.NoError(t, err)

	text, err := taskExec(t, value, `{"merge":true,"plan":[{"id":"Inspect","status":"completed"}]}`)
	require.NoError(t, err)
	assert.JSONEq(t, `{"updated":true,"completed":1,"total":3}`, text)
}

// TestTaskCatalogAutoUpgradeNetReportsTheFoldedPlan pins that the net reaches
// the tool too: a status-only call that omits merge reports the plan's totals.
func TestTaskCatalogAutoUpgradeNetReportsTheFoldedPlan(t *testing.T) {
	t.Parallel()

	plan := tasklist.Snapshot{
		Items: []tasklist.Item{
			{ID: "Inspect", Step: "Inspect", Status: tasklist.StatusInProgress},
			{ID: "Implement", Step: "Implement", Status: tasklist.StatusPending},
		},
		InProgress: 1, Total: 2,
	}
	value, err := tools.NewTaskCatalog(func() tasklist.Snapshot { return plan })
	require.NoError(t, err)

	text, err := taskExec(t, value, `{"plan":[{"id":"Inspect","status":"completed"}]}`)
	require.NoError(t, err)
	assert.JSONEq(t, `{"updated":true,"completed":1,"total":2}`, text)
}

// TestTaskCatalogAllowsParallelInProgress pins the Grok Build-style policy: a
// merge may make a second item active while another is already running.
func TestTaskCatalogAllowsParallelInProgress(t *testing.T) {
	t.Parallel()

	plan := tasklist.Snapshot{
		Items: []tasklist.Item{
			{ID: "First", Step: "First", Status: tasklist.StatusInProgress},
			{ID: "Second", Step: "Second", Status: tasklist.StatusPending},
		},
		InProgress: 1, Total: 2,
	}
	value, err := tools.NewTaskCatalog(func() tasklist.Snapshot { return plan })
	require.NoError(t, err)

	text, err := taskExec(t, value, `{"merge":true,"plan":[{"id":"Second","status":"in_progress"}]}`)
	require.NoError(t, err)
	assert.Contains(t, text, `"updated":true`)
	assert.Equal(t, tasklist.StatusPending, plan.Items[1].Status, "the caller's plan is unchanged")
}

// TestTaskCatalogReplaceResultIsUnchanged pins the historical result shape for a
// replace: the counts are the call's own.
func TestTaskCatalogReplaceResultIsUnchanged(t *testing.T) {
	t.Parallel()

	value, err := tools.NewTaskCatalog(nil)
	require.NoError(t, err)

	text, err := taskExec(t, value,
		`{"merge":false,"plan":[{"step":"Inspect","status":"completed"},{"step":"Implement","status":"pending"}]}`)
	require.NoError(t, err)
	assert.JSONEq(t, `{"updated":true,"completed":1,"total":2}`, text)
}

// TestTaskCatalogWithoutAProviderValidatesTheCallAlone pins the nil-provider
// behaviour: the fold sees an empty plan, so the net cannot fire and the counts
// describe the call's own items.
func TestTaskCatalogWithoutAProviderValidatesTheCallAlone(t *testing.T) {
	t.Parallel()

	value, err := tools.NewTaskCatalog(nil)
	require.NoError(t, err)

	text, err := taskExec(t, value, `{"plan":[{"id":"Inspect","status":"completed"}]}`)
	require.NoError(t, err)
	assert.JSONEq(t, `{"updated":true,"completed":1,"total":1}`, text)
}
