package tasklist_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/tasklist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeAndProjectLatestValidUpdate(t *testing.T) {
	t.Parallel()

	first := `{"plan":[{"step":"Inspect","status":"completed"},{"step":"Implement","status":"in_progress"}]}`
	second := `{"explanation":"verified","plan":[{"step":"Inspect","status":"completed"},{"step":"Implement","status":"completed"}]}`
	malformed := `{"plan":[],"unknown":true}`
	messages := []ai.Message{
		ai.Assistant(ai.ToolCallPart{ID: "1", Name: tasklist.ToolName, Args: ai.JSON(first)}),
		ai.Assistant(ai.ToolCallPart{ID: "2", Name: tasklist.ToolName, Args: ai.JSON(malformed)}),
		ai.Assistant(ai.ToolCallPart{ID: "3", Name: tasklist.ToolName, Args: ai.JSON(second)}),
	}

	snapshot := tasklist.Project(messages)
	assert.Equal(t, 2, snapshot.Completed)
	assert.Equal(t, 2, snapshot.Total)
	assert.Zero(t, snapshot.InProgress)

	snapshot.Items[0].Step = "mutated"
	assert.Equal(t, "Inspect", tasklist.Project(messages).Items[0].Step)
}

func TestDecodeRejectsUnsafeUpdates(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"empty":         `{"plan":[]}`,
		"unknown":       `{"plan":[{"step":"A","status":"pending"}],"extra":true}`,
		"duplicate key": `{"plan":[{"step":"A","step":"B","status":"pending"}]}`,
		"bad status":    `{"plan":[{"step":"A","status":"running"}]}`,
		"blank step":    `{"plan":[{"step":" ","status":"pending"}]}`,
		"trailing json": `{"plan":[{"step":"A","status":"pending"}]} {}`,
	}

	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := tasklist.Decode([]byte(raw))
			require.Error(t, err)
		})
	}

	items := make([]tasklist.Item, tasklist.MaxItems+1)
	for index := range items {
		items[index] = tasklist.Item{Step: "A", Status: tasklist.StatusPending}
	}

	oversized, err := json.Marshal(tasklist.Update{Plan: items})
	require.NoError(t, err)
	_, err = tasklist.Decode(oversized)
	require.Error(t, err)

	longStep := strings.Repeat("x", tasklist.MaxStepBytes+1)
	encoded, err := json.Marshal(tasklist.Update{Plan: []tasklist.Item{{
		Step: longStep, Status: tasklist.StatusPending,
	}}})
	require.NoError(t, err)
	_, err = tasklist.Decode(encoded)
	require.Error(t, err)
}

// TestDecodeAcceptsCancelledAndMeta pins the two additions: a cancelled step is
// a valid terminal status that never counts as in progress, and an item's meta
// object round-trips within its bound.
// TestClearRemovesThePlan pins the explicit clear operation: it is the only
// way to fold an empty plan, and it rejects items and merge in the same call.
func TestClearRemovesThePlan(t *testing.T) {
	t.Parallel()

	current, err := tasklist.Apply(tasklist.Snapshot{}, tasklist.Update{Plan: []tasklist.Item{{
		Step: "Inspect", Status: tasklist.StatusInProgress,
	}}})
	require.NoError(t, err)
	require.Equal(t, 1, current.Total)

	update, err := tasklist.Decode([]byte(`{"clear":true,"explanation":"done"}`))
	require.NoError(t, err)
	folded, err := tasklist.Apply(current, update)
	require.NoError(t, err)
	assert.Empty(t, folded.Items)
	assert.Zero(t, folded.Total)

	_, err = tasklist.Decode([]byte(`{"clear":true,"plan":[{"step":"Inspect"}]}`))
	require.Error(t, err)
	merge := true
	_, err = tasklist.Apply(current, tasklist.Update{Clear: true, Merge: &merge})
	require.Error(t, err)
}

// TestMultipleInProgressAreAccepted pins the Grok Build-style policy: parallel
// work may have more than one active item; the tool treats the count as a plan
// choice rather than a validation error.
func TestMultipleInProgressAreAccepted(t *testing.T) {
	t.Parallel()

	update, err := tasklist.Decode([]byte(`{"plan":[
		{"step":"Audit A","status":"in_progress"},
		{"step":"Audit B","status":"in_progress"},
		{"step":"Audit C","status":"in_progress"}
	]}`))
	require.NoError(t, err)
	folded, err := tasklist.Apply(tasklist.Snapshot{}, update)
	require.NoError(t, err)
	assert.Equal(t, 3, folded.Total)
	assert.Equal(t, 3, folded.InProgress)
}

// TestExplicitEmptyReplaceClearsThePlan pins the Grok Build-compatible clear
// path: merge false with no items removes the checklist, while an omitted merge
// still refuses an empty plan.
func TestExplicitEmptyReplaceClearsThePlan(t *testing.T) {
	t.Parallel()

	current, err := tasklist.Apply(tasklist.Snapshot{}, tasklist.Update{Plan: []tasklist.Item{{
		Step: "Inspect", Status: tasklist.StatusInProgress,
	}}})
	require.NoError(t, err)

	update, err := tasklist.Decode([]byte(`{"merge":false,"plan":[]}`))
	require.NoError(t, err)
	folded, err := tasklist.Apply(current, update)
	require.NoError(t, err)
	assert.Empty(t, folded.Items)
	assert.Zero(t, folded.Total)

	_, err = tasklist.Decode([]byte(`{"plan":[]}`))
	require.Error(t, err, "an empty plan without an explicit replace still has a hint")
	assert.Contains(t, err.Error(), "clear true")
}

func TestDecodeAcceptsCancelledAndMeta(t *testing.T) {
	t.Parallel()

	raw := `{"plan":[` +
		`{"step":"blocked by a policy","status":"cancelled","meta":{"reason":"denied"}},` +
		`{"step":"still going","status":"in_progress"}]}`

	update, err := tasklist.Decode([]byte(raw))
	require.NoError(t, err)
	require.Len(t, update.Plan, 2)
	assert.Equal(t, tasklist.StatusCancelled, update.Plan[0].Status)
	assert.Equal(t, "denied", update.Plan[0].Meta["reason"])

	snapshot, foldErr := tasklist.Apply(tasklist.Snapshot{}, update)
	require.NoError(t, foldErr)
	assert.Equal(t, 2, snapshot.Total)
	assert.Zero(t, snapshot.Completed)
	assert.Equal(t, 1, snapshot.InProgress)
	assert.Equal(t, 1, snapshot.Cancelled)
	require.NoError(t, tasklist.ValidateSnapshot(snapshot))

	// The meta object survives a re-encode and a re-decode.
	encoded, err := json.Marshal(update)
	require.NoError(t, err)
	roundTripped, err := tasklist.Decode(encoded)
	require.NoError(t, err)
	assert.Equal(t, update.Plan[0].Meta, roundTripped.Plan[0].Meta)

	// Only in-progress items are capped at one; cancelled items are terminal.
	_, err = tasklist.Decode([]byte(
		`{"plan":[{"step":"A","status":"cancelled"},{"step":"B","status":"cancelled"}]}`,
	))
	require.NoError(t, err)

	// A cancelled item is not a second in-progress item.
	_, err = tasklist.Decode([]byte(
		`{"plan":[{"step":"A","status":"cancelled"},{"step":"B","status":"in_progress"}]}`,
	))
	require.NoError(t, err)
}

// TestDecodeRejectsUnsafeMeta pins meta's bound and its shape: it must be a
// bounded JSON object, not a scalar, array, or an oversized blob.
func TestDecodeRejectsUnsafeMeta(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"array meta":     `{"plan":[{"step":"A","status":"pending","meta":[1,2]}]}`,
		"scalar meta":    `{"plan":[{"step":"A","status":"pending","meta":"text"}]}`,
		"oversized meta": `{"plan":[{"step":"A","status":"pending","meta":{"blob":"` + strings.Repeat("x", tasklist.MaxMetaBytes) + `"}}]}`,
	}

	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := tasklist.Decode([]byte(raw))
			require.Error(t, err)
		})
	}

	// A meta exactly at the bound is accepted.
	raw := `{"plan":[{"step":"A","status":"pending","meta":{"blob":"` +
		strings.Repeat("x", tasklist.MaxMetaBytes-16) + `"}}]}`
	_, err := tasklist.Decode([]byte(raw))
	require.NoError(t, err)
}

// TestValidateSnapshotAcceptsOlderProjection pins the replay contract: a
// snapshot written before cancelled and meta existed still validates, whatever
// its shape, and a mismatched cancelled count is rejected.
func TestValidateSnapshotAcceptsOlderProjection(t *testing.T) {
	t.Parallel()

	older := tasklist.Snapshot{
		Items:     []tasklist.Item{{Step: "A", Status: tasklist.StatusCompleted}},
		Completed: 1, Total: 1,
	}
	require.NoError(t, tasklist.ValidateSnapshot(older))

	// A redacted count-only projection carries no items at all.
	require.NoError(t, tasklist.ValidateSnapshot(tasklist.Snapshot{
		Completed: 1, InProgress: 1, Total: 3,
	}))

	mismatched := tasklist.Snapshot{
		Items: []tasklist.Item{{Step: "A", Status: tasklist.StatusCancelled}},
		Total: 1, Cancelled: 0,
	}
	require.Error(t, tasklist.ValidateSnapshot(mismatched))
}

// TestSnapshotCloneDetachesItemMeta pins the clone's nested copy: a caller that
// mutates a cloned item's meta cannot reach the snapshot it came from.
func TestSnapshotCloneDetachesItemMeta(t *testing.T) {
	t.Parallel()

	source, err := tasklist.Apply(tasklist.Snapshot{}, tasklist.Update{Plan: []tasklist.Item{{
		Step: "A", Status: tasklist.StatusPending, Meta: map[string]any{"reason": "denied"},
	}}})
	require.NoError(t, err)

	cloned := source.Clone()
	cloned.Items[0].Meta["reason"] = "changed"

	assert.Equal(t, "denied", source.Items[0].Meta["reason"])
}

// TestApplyMergeDetachesMeta pins the aliasing rule: a merge that stores a meta
// object shares it with neither the decoded call nor the caller's snapshot, and
// the folded snapshot is detached in both directions.
func TestApplyMergeDetachesMeta(t *testing.T) {
	t.Parallel()

	current, err := tasklist.Apply(tasklist.Snapshot{}, tasklist.Update{Plan: []tasklist.Item{
		{Step: "Inspect", Status: tasklist.StatusPending, Meta: map[string]any{"owner": "me"}},
	}})
	require.NoError(t, err)

	update := tasklist.Update{
		Merge: new(true),
		Plan:  []tasklist.Item{{ID: "Inspect", Meta: map[string]any{"k": "v"}}},
	}

	folded, err := tasklist.Apply(current, update)
	require.NoError(t, err)

	update.Plan[0].Meta["k"] = "changed by the call"
	current.Items[0].Meta["owner"] = "changed by the caller"
	assert.Equal(t, map[string]any{"k": "v"}, folded.Items[0].Meta,
		"the folded meta is detached from the call")

	folded.Items[0].Meta["k"] = "changed by the folded snapshot"
	assert.Equal(t, "changed by the call", update.Plan[0].Meta["k"])
	assert.Equal(t, "changed by the caller", current.Items[0].Meta["owner"])
}

// TestApplyMergeFlipsOneStatusAndKeepsTheRest pins the point of the fold: a
// status-only merge changes one item and leaves every other item as it was.
func TestApplyMergeFlipsOneStatusAndKeepsTheRest(t *testing.T) {
	t.Parallel()

	current, err := tasklist.Apply(tasklist.Snapshot{}, tasklist.Update{Plan: []tasklist.Item{
		{Step: "Inspect", Status: tasklist.StatusInProgress, Meta: map[string]any{"owner": "me"}},
		{Step: "Implement", Status: tasklist.StatusPending},
		{Step: "Verify", Status: tasklist.StatusPending},
	}})
	require.NoError(t, err)

	folded, err := tasklist.Apply(current, tasklist.Update{
		Merge: new(true),
		Plan:  []tasklist.Item{{ID: "Inspect", Status: tasklist.StatusCompleted}},
	})
	require.NoError(t, err)

	require.Len(t, folded.Items, 3)
	assert.Equal(t, tasklist.Item{
		ID: "Inspect", Step: "Inspect", Status: tasklist.StatusCompleted,
		Meta: map[string]any{"owner": "me"},
	}, folded.Items[0])
	assert.Equal(t, current.Items[1], folded.Items[1])
	assert.Equal(t, current.Items[2], folded.Items[2])
	assert.Equal(t, 1, folded.Completed)
	assert.Zero(t, folded.InProgress)
	assert.Equal(t, 3, folded.Total)
}

// TestApplyMergeAppendsUnknownID pins the new-item rule: an unknown id is
// appended with the id as its step fallback and a defaulted pending status.
func TestApplyMergeAppendsUnknownID(t *testing.T) {
	t.Parallel()

	current, err := tasklist.Apply(tasklist.Snapshot{}, tasklist.Update{Plan: []tasklist.Item{
		{Step: "Inspect", Status: tasklist.StatusCompleted},
	}})
	require.NoError(t, err)

	folded, err := tasklist.Apply(current, tasklist.Update{
		Merge: new(true),
		Plan:  []tasklist.Item{{ID: "deploy"}},
	})
	require.NoError(t, err)

	require.Len(t, folded.Items, 2)
	assert.Equal(t, tasklist.Item{ID: "deploy", Step: "deploy", Status: tasklist.StatusPending}, folded.Items[1])
}

// TestApplyMergeKeepsOmittedFields pins the reference's "omitted keeps" rule: an
// omitted step, status, and meta each keep the stored value, and an empty step
// never wipes the stored text.
func TestApplyMergeKeepsOmittedFields(t *testing.T) {
	t.Parallel()

	current, err := tasklist.Apply(tasklist.Snapshot{}, tasklist.Update{Plan: []tasklist.Item{
		{Step: "Inspect", Status: tasklist.StatusInProgress, Meta: map[string]any{"owner": "me"}},
	}})
	require.NoError(t, err)

	folded, err := tasklist.Apply(current, tasklist.Update{
		Merge: new(true),
		Plan:  []tasklist.Item{{ID: "Inspect"}},
	})
	require.NoError(t, err)
	assert.Equal(t, current.Items[0], folded.Items[0])

	folded, err = tasklist.Apply(current, tasklist.Update{
		Merge: new(true),
		Plan:  []tasklist.Item{{ID: "Inspect", Step: "", Status: tasklist.StatusCompleted}},
	})
	require.NoError(t, err)
	assert.Equal(t, "Inspect", folded.Items[0].Step)
	assert.Equal(t, tasklist.StatusCompleted, folded.Items[0].Status)
	assert.Equal(t, map[string]any{"owner": "me"}, folded.Items[0].Meta)
}

// TestApplyReplaceRebuildsAndDefaults pins a replace: the plan becomes exactly
// the call's items, an omitted step falls back to the id, and an omitted status
// defaults to pending.
func TestApplyReplaceRebuildsAndDefaults(t *testing.T) {
	t.Parallel()

	current, err := tasklist.Apply(tasklist.Snapshot{}, tasklist.Update{
		Merge: new(false),
		Plan:  []tasklist.Item{{Step: "Old", Status: tasklist.StatusCompleted}},
	})
	require.NoError(t, err)

	folded, err := tasklist.Apply(current, tasklist.Update{
		Merge: new(false),
		Plan: []tasklist.Item{
			{ID: "fresh"},
			{Step: "Second", Status: tasklist.StatusInProgress},
		},
	})
	require.NoError(t, err)

	require.Len(t, folded.Items, 2)
	assert.Equal(t, tasklist.Item{ID: "fresh", Step: "fresh", Status: tasklist.StatusPending}, folded.Items[0])
	assert.Equal(t, tasklist.Item{ID: "Second", Step: "Second", Status: tasklist.StatusInProgress}, folded.Items[1])
	assert.Equal(t, 1, folded.InProgress)
	assert.Equal(t, 2, folded.Total)
}

// TestApplyIDDefaultsToTheStepText pins that an item written without an id is
// addressable by its step text, so a plan written before ids existed still
// merges.
func TestApplyIDDefaultsToTheStepText(t *testing.T) {
	t.Parallel()

	update, err := tasklist.Decode([]byte(`{"plan":[{"step":"Inspect","status":"in_progress"}]}`))
	require.NoError(t, err)

	current, err := tasklist.Apply(tasklist.Snapshot{}, update)
	require.NoError(t, err)
	require.Len(t, current.Items, 1)
	assert.Equal(t, "Inspect", current.Items[0].ID)

	folded, err := tasklist.Apply(current, tasklist.Update{
		Merge: new(true),
		Plan:  []tasklist.Item{{ID: "Inspect", Status: tasklist.StatusCompleted}},
	})
	require.NoError(t, err)
	assert.Equal(t, tasklist.StatusCompleted, folded.Items[0].Status)
	assert.Equal(t, "Inspect", folded.Items[0].Step)
}

// TestApplyRefusesDuplicateIDAndNamesIt pins the duplicate rule: two items that
// resolve to one id are refused, and the message names the id.
func TestApplyRefusesDuplicateIDAndNamesIt(t *testing.T) {
	t.Parallel()

	_, err := tasklist.Apply(tasklist.Snapshot{}, tasklist.Update{Plan: []tasklist.Item{
		{ID: "dup", Status: tasklist.StatusPending},
		{Step: "dup", Status: tasklist.StatusPending},
	}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"dup"`)
}

// TestApplyAllowsAFoldWithTwoInProgress pins the aligned policy: a merge may
// leave multiple items active for genuinely parallel work.
func TestApplyAllowsAFoldWithTwoInProgress(t *testing.T) {
	t.Parallel()

	current, err := tasklist.Apply(tasklist.Snapshot{}, tasklist.Update{Plan: []tasklist.Item{
		{Step: "First", Status: tasklist.StatusInProgress},
		{Step: "Second", Status: tasklist.StatusPending},
	}})
	require.NoError(t, err)

	folded, err := tasklist.Apply(current, tasklist.Update{
		Merge: new(true),
		Plan:  []tasklist.Item{{ID: "Second", Status: tasklist.StatusInProgress}},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, folded.InProgress)
}

// TestApplyRefusesTheSixtyFifthItem pins the folded item bound: a merge that
// would grow the plan past MaxItems is refused.
func TestApplyRefusesTheSixtyFifthItem(t *testing.T) {
	t.Parallel()

	items := make([]tasklist.Item, tasklist.MaxItems)
	for index := range items {
		items[index] = tasklist.Item{ID: fmt.Sprintf("item-%d", index), Status: tasklist.StatusPending}
	}

	current, err := tasklist.Apply(tasklist.Snapshot{}, tasklist.Update{Plan: items})
	require.NoError(t, err)
	require.Len(t, current.Items, tasklist.MaxItems)

	_, err = tasklist.Apply(current, tasklist.Update{
		Merge: new(true),
		Plan:  []tasklist.Item{{ID: "extra"}},
	})
	require.Error(t, err)
}

// TestDecodeAcceptsAStatusOnlyMergeItem pins the decoded shape the merge exists
// for: an item carrying only an id and a status.
func TestDecodeAcceptsAStatusOnlyMergeItem(t *testing.T) {
	t.Parallel()

	update, err := tasklist.Decode([]byte(`{"merge":true,"plan":[{"id":"a","status":"completed"}]}`))
	require.NoError(t, err)
	require.NotNil(t, update.Merge)
	assert.True(t, *update.Merge)
	require.Len(t, update.Plan, 1)
	assert.Equal(t, "a", update.Plan[0].ID)
	assert.Empty(t, update.Plan[0].Step)
	assert.Equal(t, tasklist.StatusCompleted, update.Plan[0].Status)
}

// TestDecodeRefusesAnItemWithNeitherIDNorStep pins the call rule: nothing
// identifies an item that carries neither.
func TestDecodeRefusesAnItemWithNeitherIDNorStep(t *testing.T) {
	t.Parallel()

	_, err := tasklist.Decode([]byte(`{"plan":[{"status":"pending"}]}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "neither an id nor a step")
}

// TestProjectIgnoresACallThatCannotBeApplied pins the tolerance: a refused call
// leaves the previous plan alone.
func TestProjectIgnoresACallThatCannotBeApplied(t *testing.T) {
	t.Parallel()

	replace := `{"plan":[{"step":"Inspect","status":"in_progress"},{"step":"Implement","status":"pending"}]}`
	refused := `{"merge":true,"plan":[{"id":"Implement","status":"running"}]}`
	messages := []ai.Message{
		ai.Assistant(ai.ToolCallPart{ID: "1", Name: tasklist.ToolName, Args: ai.JSON(replace)}),
		ai.Assistant(ai.ToolCallPart{ID: "2", Name: tasklist.ToolName, Args: ai.JSON(refused)}),
	}

	snapshot := tasklist.Project(messages)
	assert.Equal(t, 2, snapshot.Total)
	assert.Equal(t, 1, snapshot.InProgress)
	require.Len(t, snapshot.Items, 2)
	assert.Equal(t, "Inspect", snapshot.Items[0].Step)
	assert.Equal(t, "Implement", snapshot.Items[1].Step)
}

// TestApplyAutoUpgradeNet pins the reference's safety net: an explicit
// merge=false status-only call is applied as a merge, while a call that carries
// a step, names an unknown id, or runs on an empty plan replaces.
func TestApplyAutoUpgradeNet(t *testing.T) {
	t.Parallel()

	base, err := tasklist.Apply(tasklist.Snapshot{}, tasklist.Update{
		Merge: new(false),
		Plan: []tasklist.Item{
			{Step: "Inspect", Status: tasklist.StatusInProgress},
			{Step: "Implement", Status: tasklist.StatusPending},
		},
	})
	require.NoError(t, err)

	statusOnly := []tasklist.Item{{ID: "Inspect", Status: tasklist.StatusCompleted}}
	folded, err := tasklist.Apply(base, tasklist.Update{Merge: new(false), Plan: statusOnly})
	require.NoError(t, err)
	require.Len(t, folded.Items, 2)
	assert.Equal(t, tasklist.StatusCompleted, folded.Items[0].Status)
	assert.Equal(t, "Inspect", folded.Items[0].Step)
	assert.Equal(t, tasklist.StatusPending, folded.Items[1].Status)

	// Any step in the call means the model wrote a full list, so it replaces.
	folded, err = tasklist.Apply(base, tasklist.Update{Merge: new(false), Plan: []tasklist.Item{
		{ID: "Inspect", Step: "Inspect again", Status: tasklist.StatusCompleted},
	}})
	require.NoError(t, err)
	require.Len(t, folded.Items, 1)
	assert.Equal(t, "Inspect again", folded.Items[0].Step)

	// One item with a step is enough: the net needs every item step-less, so
	// this replaces — the omitted status defaults to pending rather than the
	// stored in_progress being kept.
	folded, err = tasklist.Apply(base, tasklist.Update{Merge: new(false), Plan: []tasklist.Item{
		{ID: "Inspect"},
		{ID: "Implement", Step: "Implement anew"},
	}})
	require.NoError(t, err)
	require.Len(t, folded.Items, 2)
	assert.Equal(t, tasklist.Item{ID: "Inspect", Step: "Inspect", Status: tasklist.StatusPending}, folded.Items[0])
	assert.Equal(t, "Implement anew", folded.Items[1].Step)

	// A status-only call naming an unknown id replaces, which adds that item.
	folded, err = tasklist.Apply(base, tasklist.Update{Merge: new(false), Plan: []tasklist.Item{
		{ID: "unknown", Status: tasklist.StatusPending},
	}})
	require.NoError(t, err)
	require.Len(t, folded.Items, 1)
	assert.Equal(t, "unknown", folded.Items[0].ID)

	// The net never fires on an empty plan.
	folded, err = tasklist.Apply(tasklist.Snapshot{}, tasklist.Update{Merge: new(false), Plan: []tasklist.Item{
		{ID: "fresh", Status: tasklist.StatusCompleted},
	}})
	require.NoError(t, err)
	require.Len(t, folded.Items, 1)
	assert.Equal(t, "fresh", folded.Items[0].Step)
	assert.Equal(t, tasklist.StatusCompleted, folded.Items[0].Status)
}

// TestTodoWriteDefaultsToMerge pins the renamed tool's default: a call that
// omits merge updates by id instead of replacing the whole list. A legacy
// update_plan call keeps the old replace default.
func TestTodoWriteDefaultsToMerge(t *testing.T) {
	t.Parallel()

	base, err := tasklist.Apply(tasklist.Snapshot{}, tasklist.Update{Merge: new(false), Plan: []tasklist.Item{
		{Step: "Inspect", Status: tasklist.StatusInProgress},
		{Step: "Implement", Status: tasklist.StatusPending},
	}})
	require.NoError(t, err)

	folded, err := tasklist.Apply(base, tasklist.Update{Plan: []tasklist.Item{
		{ID: "Inspect", Step: "Inspect again", Status: tasklist.StatusCompleted},
	}})
	require.NoError(t, err)
	require.Len(t, folded.Items, 2, "todo_write merges by default")
	assert.Equal(t, "Inspect again", folded.Items[0].Step)
	assert.Equal(t, tasklist.StatusPending, folded.Items[1].Status)

	legacy := ai.Assistant(ai.ToolCallPart{
		ID: "legacy-1", Name: tasklist.LegacyToolName,
		Args: ai.JSON(`{"plan":[{"step":"Only","status":"pending"}]}`),
	})
	update, ok := tasklist.FromMessage(legacy)
	require.True(t, ok)
	folded, err = tasklist.Apply(base, update)
	require.NoError(t, err)
	require.Len(t, folded.Items, 1, "legacy update_plan still replaces by default")
	assert.Equal(t, "Only", folded.Items[0].Step)
}
