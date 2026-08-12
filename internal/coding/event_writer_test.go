package coding

import (
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEventWriterCommitsSequenceOnlyAfterAcceptedPreparation(t *testing.T) {
	t.Parallel()

	writer, err := newEventWriter("session-1", func() time.Time { return eventTestTime })
	require.NoError(t, err)

	first, err := writer.prepare("", "", EventSessionOpened, SessionOpened{Mode: ModeAgent})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), first.Sequence)

	_, err = writer.prepare("", "", EventToolStarted, ToolStarted{})
	require.ErrorIs(t, err, ErrInvalidEvent)

	uncommitted, err := writer.prepare("", "", EventStatusChanged, StatusChanged{Phase: PhaseIdle})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), uncommitted.Sequence)
	require.NoError(t, writer.commit(first))

	second, err := writer.prepare("", "", EventStatusChanged, StatusChanged{Phase: PhaseIdle})
	require.NoError(t, err)
	assert.Equal(t, uint64(2), second.Sequence)
}

func TestAgentProjectorMapsLifecycle(t *testing.T) {
	t.Parallel()

	writer, err := newEventWriter("session-1", func() time.Time { return eventTestTime })
	require.NoError(t, err)
	projector, err := newAgentProjector(writer, "interaction-1")
	require.NoError(t, err)

	call := ai.ToolCallPart{ID: "call-1", Name: "read", Args: ai.JSON(`{"path":"main.go"}`)}
	result := ai.ToolResultPart{
		ToolCallID: call.ID, Name: call.Name, Content: []ai.Part{ai.Text("done")},
	}
	message := ai.Assistant(call)
	usage := ai.Usage{InputTokens: 10, OutputTokens: 2}

	payloads := []agent.EventPayload{
		agent.RunStarted{},
		agent.TurnStarted{Turn: 1},
		agent.ModelStreamEvent{Turn: 1, Event: ai.StreamEvent{Type: ai.StreamTextDelta, Text: "hi"}},
		agent.CandidateDiscarded{Turn: 1},
		agent.MessageCommitted{Turn: 1, Message: message},
		agent.ToolStarted{Turn: 1, Call: call},
		agent.ToolUpdated{Turn: 1, Call: call, Update: []ai.Part{ai.Text("half")}},
		agent.ToolCompleted{Turn: 1, Call: call, Result: result},
		agent.TurnCompleted{Turn: 1, Usage: usage},
		agent.RunCompleted{Turns: 1, Stop: agent.StopEndTurn, Usage: usage},
	}
	wantTypes := []EventType{
		EventRunStarted,
		EventTurnStarted,
		EventMessageDelta,
		EventMessageDiscarded,
		EventMessageCommitted,
		EventToolStarted,
		EventToolUpdated,
		EventToolCompleted,
		EventTurnCompleted,
		EventRunCompleted,
	}

	for index, payload := range payloads {
		event, eventErr := agent.NewEvent(
			agent.RunMetadata{RunID: "run-1", Agent: "coding"},
			eventTestTime,
			payload,
		)
		require.NoError(t, eventErr)

		projected, projectErr := projector.project(event)
		require.NoError(t, projectErr)
		assert.Equal(t, wantTypes[index], projected.Type)
		assert.Equal(t, uint64(index+1), projected.Sequence)
		assert.Equal(t, "interaction-1", projected.InteractionID)
		assert.Equal(t, "run-1", projected.RunID)
		require.NoError(t, writer.commit(projected))
	}
}

func TestAgentProjectorRejectsIncompleteEvent(t *testing.T) {
	t.Parallel()

	writer, err := newEventWriter("session-1", func() time.Time { return eventTestTime })
	require.NoError(t, err)
	projector, err := newAgentProjector(writer, "interaction-1")
	require.NoError(t, err)

	_, err = projector.project(agent.Event{RunID: "run-1", Time: eventTestTime})
	require.ErrorIs(t, err, ErrInvalidEvent)
	require.ErrorIs(t, err, agent.ErrInvalidEvent)
}
