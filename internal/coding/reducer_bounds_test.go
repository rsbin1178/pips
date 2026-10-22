//nolint:wsl_v5 // The bound fixtures and their final assertion stay adjacent.
package coding

import (
	"fmt"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func boundEvent(state State, eventType EventType, runID string, payload EventPayload) Event {
	return Event{
		Schema: EventSchema, Sequence: state.Sequence + 1, Time: eventTestTime,
		SessionID: state.SessionID, InteractionID: state.Interaction.ID, RunID: runID,
		Type: eventType, Payload: payload,
	}
}

func TestStateBoundsRunHistory(t *testing.T) {
	t.Parallel()

	state := fixtureState(t, nil)

	for index := 0; index < 4*maxRecentRuns; index++ {
		runID := fmt.Sprintf("run-%04d", index)

		var err error
		state, err = Reduce(state, boundEvent(state, EventRunStarted, runID, RunStarted{}))
		require.NoError(t, err)
		state, err = Reduce(state, boundEvent(state, EventRunCompleted, runID, RunCompleted{Stop: agent.StopEndTurn}))
		require.NoError(t, err)
	}

	require.Len(t, state.Runs, maxRecentRuns)
	assert.Equal(t, fmt.Sprintf("run-%04d", 4*maxRecentRuns-1), state.Runs[len(state.Runs)-1].ID)
	assert.Contains(t, state.Runs[0].ID, "bench-run", "the fixture's active run is never dropped")
}

func TestStateKeepsActiveRunsWhileBoundingHistory(t *testing.T) {
	t.Parallel()

	state := fixtureState(t, nil)

	// Fill the history with completed runs, then start one that stays active.
	for index := 0; index < 2*maxRecentRuns; index++ {
		runID := fmt.Sprintf("run-%04d", index)

		var err error
		state, err = Reduce(state, boundEvent(state, EventRunStarted, runID, RunStarted{}))
		require.NoError(t, err)
		state, err = Reduce(state, boundEvent(state, EventRunCompleted, runID, RunCompleted{Stop: agent.StopEndTurn}))
		require.NoError(t, err)
	}

	state, err := Reduce(state, boundEvent(state, EventRunStarted, "run-active", RunStarted{}))
	require.NoError(t, err)

	require.LessOrEqual(t, len(state.Runs), maxRecentRuns+1)
	assert.Contains(t, state.Runs[len(state.Runs)-1].ID, "run-active")
	_, active := state.activeRuns["run-active"]
	assert.True(t, active, "trimming rebuilds the run index")
}

func TestStateBoundsDiagnostics(t *testing.T) {
	t.Parallel()

	state := fixtureState(t, nil)

	for index := 0; index < 4*maxRecentDiagnostics; index++ {
		var err error
		state, err = Reduce(state, boundEvent(state, EventIntegrationDiagnostic, "", IntegrationDiagnostic{
			Component: "test", Code: fmt.Sprintf("code-%d", index), Message: "diagnostic",
		}))
		require.NoError(t, err)
	}

	require.Len(t, state.Diagnostics, maxRecentDiagnostics)
	assert.Equal(t, fmt.Sprintf("code-%d", 4*maxRecentDiagnostics-1), state.Diagnostics[len(state.Diagnostics)-1].Code)
}
