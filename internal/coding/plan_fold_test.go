package coding

import (
	"testing"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/planmode"
	"github.com/rsbin1178/pips/internal/coding/tasklist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bootstrapPlan folds a transcript path through the bootstrap replay.
func bootstrapPlan(t *testing.T, path []harness.Entry) tasklist.Snapshot {
	t.Helper()

	result, err := BootstrapState(BootstrapOptions{
		SessionID: "session-1", Provider: ai.ProviderOpenAI, ModelID: "gpt-test",
		PlanMode: planmode.StateInactive, Path: path,
	})
	require.NoError(t, err)

	return result.State.Tasks
}

// reducePlan folds committed assistant messages through the live reducer.
func reducePlan(t *testing.T, messages []ai.Message) tasklist.Snapshot {
	t.Helper()

	events := []Event{
		newSessionEvent(EventSessionOpened, SessionOpened{Provider: ai.ProviderOpenAI, ModelID: "gpt-test"}),
		newInteractionEvent(EventInteractionStarted, InteractionStarted{}),
		newStatusEvent(EventStatusChanged, StatusChanged{Phase: PhaseRunning}),
		newTestEvent(EventRunStarted, RunStarted{Agent: "coding"}),
		newTestEvent(EventTurnStarted, TurnStarted{Turn: 1}),
	}

	for _, message := range messages {
		events = append(events, newTestEvent(EventMessageCommitted, MessageCommitted{Message: message}))
	}

	var state State

	for index := range events {
		events[index].Sequence = uint64(index + 1)

		var err error

		state, err = Reduce(state, events[index])
		require.NoError(t, err)
	}

	return state.Tasks
}

// TestLegacyPlanTranscriptReplaysExactly pins the compatibility contract: a
// transcript written before the plan gained ids and a merge replays to exactly
// the plan it showed then. The pre-phase plan carried no id; the fold derives it
// from the step text, and every step, status, meta and count is byte-identical
// to what the transcript showed then. All three fold sites must agree.
func TestLegacyPlanTranscriptReplaysExactly(t *testing.T) {
	t.Parallel()

	first := ai.Assistant(ai.ToolCallPart{
		ID: "task-1", Name: tasklist.ToolName,
		Args: ai.JSON(`{"explanation":"first pass","plan":[` +
			`{"step":"Inspect","status":"in_progress"},` +
			`{"step":"Implement","status":"pending"},` +
			`{"step":"Verify","status":"pending"}]}`),
	})
	second := ai.Assistant(ai.ToolCallPart{
		ID: "task-2", Name: tasklist.ToolName,
		Args: ai.JSON(`{"plan":[` +
			`{"step":"Inspect","status":"completed","meta":{"note":"done"}},` +
			`{"step":"Implement","status":"in_progress"},` +
			`{"step":"Verify","status":"cancelled"}]}`),
	})
	path := []harness.Entry{
		{Kind: harness.KindMessage, ID: "entry-1", Message: first},
		{Kind: harness.KindMessage, ID: "entry-2", Message: second},
	}

	snapshots := map[string]tasklist.Snapshot{
		"bootstrap":     bootstrapPlan(t, path),
		"reducer":       reducePlan(t, []ai.Message{first, second}),
		"tasksFromPath": tasksFromPath(path),
	}

	expected := []tasklist.Item{
		{ID: "Inspect", Step: "Inspect", Status: tasklist.StatusCompleted, Meta: map[string]any{"note": "done"}},
		{ID: "Implement", Step: "Implement", Status: tasklist.StatusInProgress},
		{ID: "Verify", Step: "Verify", Status: tasklist.StatusCancelled},
	}

	for site, snapshot := range snapshots {
		t.Run(site, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, expected, snapshot.Items)
			assert.Equal(t, 1, snapshot.Completed)
			assert.Equal(t, 1, snapshot.InProgress)
			assert.Equal(t, 1, snapshot.Cancelled)
			assert.Equal(t, 3, snapshot.Total)
		})
	}
}

// TestLegacyPlanShrinkAndStatusFlipReplaysExactly extends the compatibility
// contract to the three shapes a pre-phase transcript could carry: a full
// replacement, one that shrinks the list, and one that re-sends the list with a
// status changed. None carries an id or a merge, so every call replaces and the
// final plan is exactly the last call's items with the id derived from the step.
func TestLegacyPlanShrinkAndStatusFlipReplaysExactly(t *testing.T) {
	t.Parallel()

	replace := ai.Assistant(ai.ToolCallPart{
		ID: "task-1", Name: tasklist.ToolName,
		Args: ai.JSON(`{"plan":[` +
			`{"step":"Inspect","status":"in_progress"},` +
			`{"step":"Implement","status":"pending"},` +
			`{"step":"Verify","status":"pending"}]}`),
	})
	shrink := ai.Assistant(ai.ToolCallPart{
		ID: "task-2", Name: tasklist.ToolName,
		Args: ai.JSON(`{"plan":[` +
			`{"step":"Inspect","status":"completed"},` +
			`{"step":"Implement","status":"in_progress"}]}`),
	})
	statusFlip := ai.Assistant(ai.ToolCallPart{
		ID: "task-3", Name: tasklist.ToolName,
		Args: ai.JSON(`{"plan":[` +
			`{"step":"Inspect","status":"completed"},` +
			`{"step":"Implement","status":"completed","meta":{"note":"done"}}]}`),
	})
	path := []harness.Entry{
		{Kind: harness.KindMessage, ID: "entry-1", Message: replace},
		{Kind: harness.KindMessage, ID: "entry-2", Message: shrink},
		{Kind: harness.KindMessage, ID: "entry-3", Message: statusFlip},
	}

	snapshots := map[string]tasklist.Snapshot{
		"bootstrap":     bootstrapPlan(t, path),
		"reducer":       reducePlan(t, []ai.Message{replace, shrink, statusFlip}),
		"tasksFromPath": tasksFromPath(path),
	}

	expected := []tasklist.Item{
		{ID: "Inspect", Step: "Inspect", Status: tasklist.StatusCompleted},
		{ID: "Implement", Step: "Implement", Status: tasklist.StatusCompleted, Meta: map[string]any{"note": "done"}},
	}

	for site, snapshot := range snapshots {
		t.Run(site, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, expected, snapshot.Items)
			assert.Equal(t, 2, snapshot.Completed)
			assert.Zero(t, snapshot.InProgress)
			assert.Equal(t, 2, snapshot.Total)
		})
	}
}

// TestRefusedPlanCallLeavesEverySiteAlone pins the tolerance at all three fold
// sites: a call the fold refuses leaves the plan the earlier calls produced.
func TestRefusedPlanCallLeavesEverySiteAlone(t *testing.T) {
	t.Parallel()

	replace := ai.Assistant(ai.ToolCallPart{
		ID: "task-1", Name: tasklist.ToolName,
		Args: ai.JSON(`{"plan":[` +
			`{"step":"Inspect","status":"in_progress"},` +
			`{"step":"Implement","status":"pending"}]}`),
	})
	refused := ai.Assistant(ai.ToolCallPart{
		ID: "task-2", Name: tasklist.ToolName,
		Args: ai.JSON(`{"merge":true,"plan":[{"id":"Implement","status":"in_progress"}]}`),
	})
	path := []harness.Entry{
		{Kind: harness.KindMessage, ID: "entry-1", Message: replace},
		{Kind: harness.KindMessage, ID: "entry-2", Message: refused},
	}

	snapshots := map[string]tasklist.Snapshot{
		"bootstrap":     bootstrapPlan(t, path),
		"reducer":       reducePlan(t, []ai.Message{replace, refused}),
		"tasksFromPath": tasksFromPath(path),
	}

	for site, snapshot := range snapshots {
		t.Run(site, func(t *testing.T) {
			t.Parallel()

			require.Len(t, snapshot.Items, 2)
			assert.Equal(t, "Inspect", snapshot.Items[0].Step)
			assert.Equal(t, tasklist.StatusInProgress, snapshot.Items[0].Status)
			assert.Equal(t, "Implement", snapshot.Items[1].Step)
			assert.Equal(t, tasklist.StatusPending, snapshot.Items[1].Status)
			assert.Equal(t, 1, snapshot.InProgress)
			assert.Equal(t, 2, snapshot.Total)
		})
	}
}

// TestPlanMergeFoldsThroughEverySite pins the one-fold rule: a replace followed
// by a status-only merge replays to the merged plan through the bootstrap, the
// live reducer, and the transcript scan alike, whether the merge is explicit or
// the auto-upgrade net reads it as one.
func TestPlanMergeFoldsThroughEverySite(t *testing.T) {
	t.Parallel()

	for name, mergeArgs := range map[string]string{
		"explicit merge":   `{"merge":true,"plan":[{"id":"Inspect","status":"completed"}]}`,
		"auto-upgrade net": `{"plan":[{"id":"Inspect","status":"completed"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			replace := ai.Assistant(ai.ToolCallPart{
				ID: "task-1", Name: tasklist.ToolName,
				Args: ai.JSON(`{"plan":[` +
					`{"step":"Inspect","status":"in_progress"},` +
					`{"step":"Implement","status":"pending"}]}`),
			})
			merge := ai.Assistant(ai.ToolCallPart{
				ID: "task-2", Name: tasklist.ToolName, Args: ai.JSON(mergeArgs),
			})
			path := []harness.Entry{
				{Kind: harness.KindMessage, ID: "entry-1", Message: replace},
				{Kind: harness.KindMessage, ID: "entry-2", Message: merge},
			}

			snapshots := map[string]tasklist.Snapshot{
				"bootstrap":     bootstrapPlan(t, path),
				"reducer":       reducePlan(t, []ai.Message{replace, merge}),
				"tasksFromPath": tasksFromPath(path),
			}

			for site, snapshot := range snapshots {
				t.Run(site, func(t *testing.T) {
					t.Parallel()

					require.Len(t, snapshot.Items, 2)
					assert.Equal(t, "Inspect", snapshot.Items[0].Step)
					assert.Equal(t, tasklist.StatusCompleted, snapshot.Items[0].Status)
					assert.Equal(t, "Implement", snapshot.Items[1].Step)
					assert.Equal(t, tasklist.StatusPending, snapshot.Items[1].Status)
					assert.Equal(t, 1, snapshot.Completed)
					assert.Zero(t, snapshot.InProgress)
					assert.Equal(t, 2, snapshot.Total)
				})
			}
		})
	}
}
