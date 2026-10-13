package tasklist_test

import (
	"encoding/json"
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
		"two active":    `{"plan":[{"step":"A","status":"in_progress"},{"step":"B","status":"in_progress"}]}`,
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

	snapshot := tasklist.FromUpdate(update)
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

	source := tasklist.FromUpdate(tasklist.Update{Plan: []tasklist.Item{{
		Step: "A", Status: tasklist.StatusPending, Meta: map[string]any{"reason": "denied"},
	}}})

	cloned := source.Clone()
	cloned.Items[0].Meta["reason"] = "changed"

	assert.Equal(t, "denied", source.Items[0].Meta["reason"])
}
