package tui

import (
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func childStatusEvent(sequence uint64, sessionID string, phase coding.Phase) coding.Event {
	return coding.Event{
		Schema: coding.EventSchema, Sequence: sequence, Time: time.Unix(1, 0).UTC(),
		SessionID: sessionID, Type: coding.EventStatusChanged,
		Payload: coding.StatusChanged{Phase: phase},
	}
}

func finishedChildState(sessionID string) coding.State {
	return coding.State{
		SessionID: sessionID, SessionOpen: true, Sequence: 1,
		Provider: ai.ProviderOpenAI, ModelID: "test-model",
		Mode: coding.ModeAgent, Phase: coding.PhaseRunning,
		Interaction: coding.InteractionState{
			ID: sessionID, Active: false, Outcome: coding.InteractionSucceeded,
		},
	}
}

func TestReduceChildEventReleasesTerminalProjection(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	state := finishedChildState("child-1")
	model.childStates = map[string]coding.State{"child-1": state}

	model.reduceChildEvent(childStatusEvent(2, "child-1", coding.PhaseIdle))

	require.NoError(t, model.streamErr)
	assert.NotContains(t, model.childStates, "child-1",
		"the terminal child projection is released after its final status")

	// A late bookkeeping record cannot extend the released projection.
	model.reduceChildEvent(childStatusEvent(3, "child-1", coding.PhaseIdle))
	require.NoError(t, model.streamErr)
	assert.NotContains(t, model.childStates, "child-1")
}

func TestReduceChildEventStartsAReopenedChild(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.reduceChildEvent(coding.Event{
		Schema: coding.EventSchema, Sequence: 1, Time: time.Unix(1, 0).UTC(),
		SessionID: "child-1", Type: coding.EventSessionOpened,
		Payload: coding.SessionOpened{
			Provider: ai.ProviderOpenAI, ModelID: "test-model", Mode: coding.ModeAgent,
		},
	})

	require.NoError(t, model.streamErr)
	require.Contains(t, model.childStates, "child-1")
	assert.Equal(t, uint64(1), model.childStates["child-1"].Sequence)
	assert.True(t, model.childStates["child-1"].SessionOpen)
}

func TestAdoptChildStateIgnoresTerminalProjection(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.adoptChildState("child-1", finishedChildState("child-1"))
	assert.NotContains(t, model.childStates, "child-1")
}

func TestLiveChildStatesTakesTheObservationWithoutCloning(t *testing.T) {
	t.Parallel()

	liveState := coding.State{
		SessionID: "live", SessionOpen: true, Phase: coding.PhaseRunning,
		Transcript: []ai.Message{ai.UserText("child message")},
	}
	observation := map[string]coding.State{
		"live":     liveState,
		"finished": finishedChildState("finished"),
	}

	states := liveChildStates(observation)

	require.Contains(t, states, "live")
	assert.NotContains(t, states, "finished")

	source := observation["live"]
	taken := states["live"]
	assert.Same(t, &source.Transcript[0], &taken.Transcript[0],
		"the exclusive observation's transcript is kept instead of cloned")
}
