package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEventPayloadContract(t *testing.T) {
	t.Parallel()

	usage := ai.Usage{InputTokens: 3, OutputTokens: 2}
	call := ai.ToolCallPart{ID: "call", Name: "lookup", Args: ai.JSON(`{"q":"pips"}`)}
	result := ai.ToolResultPart{ToolCallID: call.ID, Name: call.Name, Content: []ai.Part{ai.Text("done")}}
	tests := []struct {
		name    string
		payload agent.EventPayload
		want    agent.EventType
	}{
		{name: "run started", payload: agent.RunStarted{}, want: agent.EventRunStarted},
		{name: "turn started", payload: agent.TurnStarted{Turn: 1}, want: agent.EventTurnStarted},
		{
			name: "model stream",
			payload: agent.ModelStreamEvent{
				Turn: 1, Event: ai.StreamEvent{Type: ai.StreamTextDelta, Text: "hi"},
			},
			want: agent.EventModelStream,
		},
		{
			name:    "message committed",
			payload: agent.MessageCommitted{Turn: 1, Message: ai.AssistantText("hi")},
			want:    agent.EventMessageCommitted,
		},
		{
			name:    "candidate discarded",
			payload: agent.CandidateDiscarded{Turn: 1},
			want:    agent.EventCandidateDiscarded,
		},
		{name: "tool started", payload: agent.ToolStarted{Turn: 1, Call: call}, want: agent.EventToolStarted},
		{
			name:    "tool updated",
			payload: agent.ToolUpdated{Turn: 1, Call: call, Update: []ai.Part{ai.Text("half")}},
			want:    agent.EventToolUpdated,
		},
		{
			name:    "tool completed",
			payload: agent.ToolCompleted{Turn: 1, Call: call, Result: result},
			want:    agent.EventToolCompleted,
		},
		{
			name:    "turn completed",
			payload: agent.TurnCompleted{Turn: 1, Usage: usage},
			want:    agent.EventTurnCompleted,
		},
		{
			name:    "run completed",
			payload: agent.RunCompleted{Turns: 1, Stop: agent.StopEndTurn, Usage: usage},
			want:    agent.EventRunCompleted,
		},
	}

	meta := agent.RunMetadata{RunID: "run", ParentRunID: "parent", Agent: "helper"}
	offset := time.FixedZone("test", 8*60*60)
	occurredAt := time.Date(2026, time.August, 9, 12, 0, 0, 0, offset)

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			event, err := agent.NewEvent(meta, occurredAt, test.payload)
			require.NoError(t, err)
			assert.Equal(t, test.want, event.Type())
			assert.Equal(t, meta.RunID, event.RunID)
			assert.Equal(t, meta.ParentRunID, event.ParentRunID)
			assert.Equal(t, meta.Agent, event.Agent)
			assert.Equal(t, occurredAt.UTC(), event.Time)
			assert.Equal(t, time.UTC, event.Time.Location())
			assert.Equal(t, test.payload, event.Payload())
			require.NoError(t, event.Validate())
		})
	}
}

func TestEventValidationRejectsInvalidValues(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	meta := agent.RunMetadata{RunID: "run"}
	tests := []struct {
		name    string
		meta    agent.RunMetadata
		at      time.Time
		payload agent.EventPayload
	}{
		{name: "empty run id", at: now, payload: agent.RunStarted{}},
		{name: "zero time", meta: meta, payload: agent.RunStarted{}},
		{name: "pointer payload", meta: meta, at: now, payload: &agent.RunStarted{}},
		{name: "turn started zero", meta: meta, at: now, payload: agent.TurnStarted{}},
		{
			name: "unknown model stream event",
			meta: meta,
			at:   now,
			payload: agent.ModelStreamEvent{
				Turn: 1, Event: ai.StreamEvent{Type: ai.StreamEventType("future")},
			},
		},
		{name: "message turn zero", meta: meta, at: now, payload: agent.MessageCommitted{}},
		{name: "candidate turn zero", meta: meta, at: now, payload: agent.CandidateDiscarded{}},
		{name: "tool start turn zero", meta: meta, at: now, payload: agent.ToolStarted{}},
		{name: "tool update turn zero", meta: meta, at: now, payload: agent.ToolUpdated{}},
		{name: "tool completion turn zero", meta: meta, at: now, payload: agent.ToolCompleted{}},
		{name: "turn completion zero", meta: meta, at: now, payload: agent.TurnCompleted{}},
		{
			name: "run completion turns zero",
			meta: meta,
			at:   now,
			payload: agent.RunCompleted{
				Stop: agent.StopEndTurn,
			},
		},
		{
			name:    "run completion unknown stop",
			meta:    meta,
			at:      now,
			payload: agent.RunCompleted{Turns: 1, Stop: agent.StopReason("future")},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := agent.NewEvent(test.meta, test.at, test.payload)
			require.ErrorIs(t, err, agent.ErrInvalidEvent)
		})
	}

	var zero agent.Event
	assert.Empty(t, zero.Type())
	assert.Nil(t, zero.Payload())
	require.ErrorIs(t, zero.Validate(), agent.ErrInvalidEvent)

	nonUTC, err := agent.NewEvent(meta, now, agent.RunStarted{})
	require.NoError(t, err)

	nonUTC.Time = nonUTC.Time.In(time.FixedZone("not-utc", 60*60))
	require.ErrorIs(t, nonUTC.Validate(), agent.ErrInvalidEvent)
}

func TestEventRejectsRawJSON(t *testing.T) {
	t.Parallel()

	event, err := agent.NewEvent(
		agent.RunMetadata{RunID: "run"},
		time.Now().UTC(),
		agent.RunStarted{},
	)
	require.NoError(t, err)

	_, err = json.Marshal(event)
	require.ErrorIs(t, err, agent.ErrEventWireFormat)

	var decoded agent.Event

	err = json.Unmarshal([]byte(`{"RunID":"run"}`), &decoded)
	require.ErrorIs(t, err, agent.ErrEventWireFormat)
}

func TestStreamRejectsUnknownModelStreamEvent(t *testing.T) {
	t.Parallel()

	a, err := agent.New(invalidStreamModel{})
	require.NoError(t, err)

	var (
		types     []agent.EventType
		streamErr error
	)

	for event, err := range a.Stream(t.Context(), agent.NewSession(), ai.UserText("go")) {
		if err != nil {
			streamErr = err
			continue
		}

		types = append(types, event.Type())
		require.NoError(t, event.Validate())
	}

	require.ErrorIs(t, streamErr, agent.ErrInvalidEvent)
	assert.Equal(t, []agent.EventType{agent.EventRunStarted, agent.EventTurnStarted}, types)
}

func TestEventSnapshotsMutablePayload(t *testing.T) {
	t.Parallel()

	imageData := []byte("image")
	fileData := []byte("file")
	callArgs := ai.JSON(`{"path":"main.go"}`)
	nestedData := []byte("nested")
	message := ai.Message{Role: ai.RoleAssistant, Parts: []ai.Part{
		ai.ImagePart{Source: ai.MediaSource{Data: imageData, MIMEType: "image/png"}},
		ai.FilePart{Source: ai.MediaSource{Data: fileData, MIMEType: "application/pdf"}},
		ai.ToolCallPart{ID: "call", Name: "read", Args: callArgs},
		ai.ToolResultPart{Content: []ai.Part{
			ai.ImagePart{Source: ai.MediaSource{Data: nestedData, MIMEType: "image/png"}},
		}},
	}}

	event, err := agent.NewEvent(
		agent.RunMetadata{RunID: "run"},
		time.Now().UTC(),
		agent.MessageCommitted{Turn: 1, Message: message},
	)
	require.NoError(t, err)

	imageData[0] = 'X'
	fileData[0] = 'X'
	callArgs[0] = '['
	nestedData[0] = 'X'
	message.Parts[0] = ai.Text("replaced")

	committed := requireType[agent.MessageCommitted](t, event.Payload())
	assert.Equal(t, []byte("image"), requireType[ai.ImagePart](t, committed.Message.Parts[0]).Source.Data)
	assert.Equal(t, []byte("file"), requireType[ai.FilePart](t, committed.Message.Parts[1]).Source.Data)
	assert.JSONEq(t, `{"path":"main.go"}`, string(requireType[ai.ToolCallPart](t, committed.Message.Parts[2]).Args))
	nestedResult := requireType[ai.ToolResultPart](t, committed.Message.Parts[3])
	nested := requireType[ai.ImagePart](t, nestedResult.Content[0])
	assert.Equal(t, []byte("nested"), nested.Source.Data)

	updateData := []byte("update")
	updateArgs := ai.JSON(`{"id":1}`)
	updateEvent, err := agent.NewEvent(
		agent.RunMetadata{RunID: "run"},
		time.Now().UTC(),
		agent.ToolUpdated{
			Turn: 1,
			Call: ai.ToolCallPart{ID: "call", Name: "read", Args: updateArgs},
			Update: []ai.Part{
				ai.FilePart{Source: ai.MediaSource{Data: updateData, MIMEType: "text/plain"}},
			},
		},
	)
	require.NoError(t, err)

	updateData[0] = 'X'
	updateArgs[0] = '['
	updated := requireType[agent.ToolUpdated](t, updateEvent.Payload())
	assert.JSONEq(t, `{"id":1}`, string(updated.Call.Args))
	assert.Equal(t, []byte("update"), requireType[ai.FilePart](t, updated.Update[0]).Source.Data)

	streamUsage := &ai.Usage{InputTokens: 7}
	streamEvent, err := agent.NewEvent(
		agent.RunMetadata{RunID: "run"},
		time.Now().UTC(),
		agent.ModelStreamEvent{
			Turn:  1,
			Event: ai.StreamEvent{Type: ai.StreamMessageEnd, Usage: streamUsage},
		},
	)
	require.NoError(t, err)

	streamUsage.InputTokens = 99
	streamed := requireType[agent.ModelStreamEvent](t, streamEvent.Payload())
	require.NotNil(t, streamed.Event.Usage)
	assert.Equal(t, 7, streamed.Event.Usage.InputTokens)
}

func TestEventSnapshotsIsolateObserverStreamAndSession(t *testing.T) {
	t.Parallel()

	call := ai.ToolCallPart{ID: "call", Name: "add", Args: ai.JSON(`{"a":2,"b":3}`)}
	model := newScriptedModel(
		respond(callResponse(call)),
		respond(textResponse("5")),
	)

	a, err := agent.New(
		model,
		agent.WithTools(addTool()),
		agent.WithOnEvent(func(_ context.Context, event agent.Event) {
			if streamed, ok := event.Payload().(agent.ModelStreamEvent); ok &&
				streamed.Event.Usage != nil {
				streamed.Event.Usage.InputTokens = 999
			}

			committed, ok := event.Payload().(agent.MessageCommitted)
			if !ok || committed.Message.Role != ai.RoleAssistant {
				return
			}

			for _, part := range committed.Message.Parts {
				if toolCall, isCall := part.(ai.ToolCallPart); isCall && len(toolCall.Args) > 0 {
					toolCall.Args[0] = '['
				}
			}
		}),
	)
	require.NoError(t, err)

	session := agent.NewSession()
	sawCommittedCall := false
	sawStreamUsage := false

	for event, streamErr := range a.Stream(t.Context(), session, ai.UserText("add")) {
		require.NoError(t, streamErr)

		if streamed, ok := event.Payload().(agent.ModelStreamEvent); ok && streamed.Event.Usage != nil {
			sawStreamUsage = true

			assert.NotEqual(t, 999, streamed.Event.Usage.InputTokens)
		}

		committed, ok := event.Payload().(agent.MessageCommitted)
		if !ok || committed.Message.Role != ai.RoleAssistant {
			continue
		}

		for _, part := range committed.Message.Parts {
			if toolCall, isCall := part.(ai.ToolCallPart); isCall {
				sawCommittedCall = true

				assert.JSONEq(t, `{"a":2,"b":3}`, string(toolCall.Args))
			}
		}
	}

	require.True(t, sawCommittedCall)
	require.True(t, sawStreamUsage)

	messages := session.Messages()
	require.Len(t, messages, 4)
	storedCall := requireType[ai.ToolCallPart](t, messages[1].Parts[0])
	assert.JSONEq(t, `{"a":2,"b":3}`, string(storedCall.Args))
}

func TestEventSnapshotsCyclicToolResults(t *testing.T) {
	t.Parallel()

	parts := make([]ai.Part, 1)
	parts[0] = ai.ToolResultPart{ToolCallID: "call", Content: parts}

	event, err := agent.NewEvent(
		agent.RunMetadata{RunID: "run"},
		time.Now().UTC(),
		agent.MessageCommitted{Turn: 1, Message: ai.Message{Role: ai.RoleTool, Parts: parts}},
	)
	require.NoError(t, err)

	committed := requireType[agent.MessageCommitted](t, event.Payload())
	result := requireType[ai.ToolResultPart](t, committed.Message.Parts[0])
	result.Content[0] = ai.Text("changed through cycle")
	assert.Equal(t, ai.Text("changed through cycle"), committed.Message.Parts[0])
	assert.IsType(t, ai.ToolResultPart{}, parts[0], "the producer graph must remain isolated")
}

func FuzzEventOperations(f *testing.F) {
	f.Add("run", int64(1), true)
	f.Add("", int64(0), false)

	f.Fuzz(func(t *testing.T, runID string, turn int64, complete bool) {
		var payload agent.EventPayload = agent.TurnStarted{Turn: int(turn)}
		if complete {
			payload = agent.RunCompleted{
				Turns: int(turn),
				Stop:  agent.StopEndTurn,
			}
		}

		event, err := agent.NewEvent(
			agent.RunMetadata{RunID: runID},
			time.Unix(1, 0).UTC(),
			payload,
		)
		if err == nil {
			_ = event.Type()
			_ = event.Payload()
			require.NoError(t, event.Validate())
		}

		_, marshalErr := json.Marshal(event)
		if err == nil {
			require.ErrorIs(t, marshalErr, agent.ErrEventWireFormat)
		}
	})
}

func TestEventErrorsAreDistinct(t *testing.T) {
	t.Parallel()

	assert.NotErrorIs(t, agent.ErrInvalidEvent, agent.ErrEventWireFormat)
}

func requireType[T any](t *testing.T, value any) T {
	t.Helper()

	typed, ok := value.(T)
	require.True(t, ok)

	return typed
}

type invalidStreamModel struct{}

func (invalidStreamModel) Generate(context.Context, ai.Request) (*ai.Response, error) {
	return nil, errors.New("Generate is not used by this test")
}

func (invalidStreamModel) Stream(context.Context, ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		yield(ai.StreamEvent{Type: ai.StreamEventType("future")}, nil)
	}
}

func (invalidStreamModel) Provider() ai.Provider { return "test" }
func (invalidStreamModel) ModelID() string       { return "invalid-stream" }
func (invalidStreamModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true}
}
