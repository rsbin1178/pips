package coding

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEventWriterAssignsSequenceAfterValidation(t *testing.T) {
	t.Parallel()

	writer, err := newEventWriter("session-1", func() time.Time { return eventTestTime })
	require.NoError(t, err)

	first, err := writer.write("", "", EventSessionOpened, SessionOpened{})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), first.Sequence)

	_, err = writer.write("", "", EventToolStarted, ToolStarted{})
	require.ErrorIs(t, err, ErrInvalidEvent)

	second, err := writer.write("", "", EventStatusChanged, StatusChanged{Phase: PhaseIdle})
	require.NoError(t, err)
	assert.Equal(t, uint64(2), second.Sequence)
}

func TestEventWriterSerializesConcurrentWriters(t *testing.T) {
	t.Parallel()

	writer, err := newEventWriter("session-1", func() time.Time { return eventTestTime })
	require.NoError(t, err)

	const count = 64

	sequences := make([]uint64, count)

	var wait sync.WaitGroup
	wait.Add(count)

	for index := range count {
		go func() {
			defer wait.Done()

			event, writeErr := writer.write(
				"",
				"",
				EventIntegrationDiagnostic,
				IntegrationDiagnostic{Component: "runtime", Code: "test"},
			)
			if writeErr != nil {
				t.Errorf("write event: %v", writeErr)
				return
			}

			sequences[index] = event.Sequence
		}()
	}

	wait.Wait()

	slices.Sort(sequences)

	for index, sequence := range sequences {
		assert.Equal(t, uint64(index+1), sequence)
	}
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

	events := []agent.Event{
		{Type: agent.EventRunStart},
		{Type: agent.EventTurnStart, Turn: 1},
		{Type: agent.EventDelta, Turn: 1, Delta: ai.StreamEvent{Type: ai.StreamTextDelta, Text: "hi"}},
		{Type: agent.EventMessage, Turn: 1, Message: &message},
		{Type: agent.EventToolStart, Turn: 1, Call: &call},
		{Type: agent.EventToolUpdate, Turn: 1, Call: &call, Update: []ai.Part{ai.Text("half")}},
		{Type: agent.EventToolEnd, Turn: 1, Call: &call, Result: &result},
		{Type: agent.EventTurnEnd, Turn: 1, Usage: usage},
		{Type: agent.EventRunEnd, Turn: 1, Stop: agent.StopEndTurn, Usage: usage},
	}
	wantTypes := []EventType{
		EventRunStarted,
		EventTurnStarted,
		EventMessageDelta,
		EventMessageCommitted,
		EventToolStarted,
		EventToolUpdated,
		EventToolCompleted,
		EventTurnCompleted,
		EventRunCompleted,
	}

	for index := range events {
		events[index].RunID = "run-1"
		events[index].Agent = "coding"
		events[index].Time = eventTestTime

		projected, projectErr := projector.project(events[index])
		require.NoError(t, projectErr)
		assert.Equal(t, wantTypes[index], projected.Type)
		assert.Equal(t, uint64(index+1), projected.Sequence)
		assert.Equal(t, "interaction-1", projected.InteractionID)
		assert.Equal(t, "run-1", projected.RunID)
	}
}

func TestAgentProjectorRejectsIncompleteEvent(t *testing.T) {
	t.Parallel()

	writer, err := newEventWriter("session-1", func() time.Time { return eventTestTime })
	require.NoError(t, err)
	projector, err := newAgentProjector(writer, "interaction-1")
	require.NoError(t, err)

	_, err = projector.project(agent.Event{
		Type: agent.EventMessage, RunID: "run-1", Time: eventTestTime,
	})
	require.ErrorIs(t, err, ErrInvalidEvent)
}
