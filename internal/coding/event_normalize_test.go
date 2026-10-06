package coding

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func prepareRunEvent(t *testing.T, eventType EventType, payload EventPayload) Event {
	t.Helper()

	writer, err := newEventWriter("session-1", func() time.Time { return eventTestTime })
	require.NoError(t, err)

	event, err := writer.prepare("interaction-1", "run-1", eventType, payload)
	require.NoError(t, err)

	return event
}

func TestEventWriterNormalizesControlCharacters(t *testing.T) {
	t.Parallel()

	payload := ToolCompleted{
		Turn: 1,
		Call: ToolCall{ID: "call-1", Name: "grep", Arguments: ai.JSON(`{"pattern":"x"}`)},
		Result: ai.ToolResults(ai.ToolResultPart{
			ToolCallID: "call-1",
			Name:       "grep",
			Content:    []ai.Part{ai.Text("before\x1b[31mred\x07after")},
		}),
	}
	require.ErrorIs(t, ValidateEvent(newTestEvent(EventToolCompleted, payload)), ErrInvalidEvent)

	event := prepareRunEvent(t, EventToolCompleted, payload)

	completed, ok := event.Payload.(ToolCompleted)
	require.True(t, ok)

	text, ok := completed.Result.Parts[0].Content[0].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, "before_[31mred_after", text.Text)
}

func TestEventWriterBoundsOversizedText(t *testing.T) {
	t.Parallel()

	payload := MessageCommitted{Message: ai.AssistantText(strings.Repeat("x", maxEventTextBytes+32))}
	require.ErrorIs(t, ValidateEvent(newTestEvent(EventMessageCommitted, payload)), ErrInvalidEvent)

	event := prepareRunEvent(t, EventMessageCommitted, payload)

	committed, ok := event.Payload.(MessageCommitted)
	require.True(t, ok)

	parts, err := ai.MessageParts(committed.Message)
	require.NoError(t, err)

	text, ok := parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Len(t, text.Text, maxEventTextBytes)
}

func TestEventWriterNormalizesSessionTranscript(t *testing.T) {
	t.Parallel()

	payload := SessionTreeChanged{
		Tree: SessionTree{
			SessionID: "session-1", LeafID: "node-1", TotalNodes: 1,
			Nodes: []SessionNode{{
				ID: "node-1", Kind: SessionNodeMessage, CreatedAt: eventTestTime,
				Current: true, OnActivePath: true,
			}},
		},
		Transcript: ai.Messages{
			ai.ToolResults(ai.ToolResultPart{
				ToolCallID: "call-1",
				Name:       "grep",
				Content:    []ai.Part{ai.Text("bad\x1btext")},
			}),
		},
	}
	require.ErrorIs(t, ValidateEvent(newStatusEvent(EventSessionTreeChanged, payload)), ErrInvalidEvent)

	writer, err := newEventWriter("session-1", func() time.Time { return eventTestTime })
	require.NoError(t, err)

	event, err := writer.prepare("", "", EventSessionTreeChanged, payload)
	require.NoError(t, err)

	changed, ok := event.Payload.(SessionTreeChanged)
	require.True(t, ok)

	parts, err := ai.MessageParts(changed.Transcript[0])
	require.NoError(t, err)

	result, ok := parts[0].(ai.ToolResultPart)
	require.True(t, ok)

	text, ok := result.Content[0].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, "bad_text", text.Text)
}

func TestEventWriterNormalizesToolArguments(t *testing.T) {
	t.Parallel()

	message := ai.Assistant(ai.ToolCallPart{ID: "call-1", Name: "read", Args: ai.JSON(`{"path":`)})
	raw := newTestEvent(EventMessageCommitted, MessageCommitted{Message: message})
	require.ErrorIs(t, ValidateEvent(raw), ErrInvalidEvent)

	event := prepareRunEvent(t, EventMessageCommitted, MessageCommitted{Message: message})

	committed, ok := event.Payload.(MessageCommitted)
	require.True(t, ok)

	parts, err := ai.MessageParts(committed.Message)
	require.NoError(t, err)

	call, ok := parts[0].(ai.ToolCallPart)
	require.True(t, ok)
	assert.True(t, json.Valid(call.Args), "normalized arguments must be valid JSON")
	assert.Contains(t, string(call.Args), `{\"path\":`)
}

func TestValidateEventAcceptsToolArgumentShapes(t *testing.T) {
	t.Parallel()

	for _, arguments := range []ai.JSON{nil, ai.JSON(`{}`), ai.JSON(`[]`), ai.JSON(`"x"`), ai.JSON(`123`)} {
		event := newTestEvent(EventToolCompleted, ToolCompleted{
			Turn: 1,
			Call: ToolCall{ID: "call-1", Name: "read", Arguments: arguments},
			Result: ai.ToolResults(ai.ToolResultPart{
				ToolCallID: "call-1",
				Name:       "read",
				Content:    []ai.Part{ai.Text("ok")},
			}),
		})
		assert.NoError(t, ValidateEvent(event), "arguments %q", string(arguments))
	}
}

func TestInvalidPayloadReportsInnerReason(t *testing.T) {
	t.Parallel()

	messageErr := ValidateEvent(newTestEvent(EventMessageCommitted, MessageCommitted{
		Message: ai.AssistantText("bad\x1btext"),
	}))
	require.ErrorIs(t, messageErr, ErrInvalidEvent)
	assert.Contains(t, messageErr.Error(), "invalid committed message")

	toolErr := ValidateEvent(newTestEvent(EventToolCompleted, ToolCompleted{
		Turn: 1,
		Call: ToolCall{ID: "call-1", Name: "grep", Arguments: ai.JSON(`{}`)},
		Result: ai.ToolResults(ai.ToolResultPart{
			ToolCallID: "call-1",
			Name:       "grep",
			Content:    []ai.Part{ai.Text("bad\x1btext")},
		}),
	}))
	require.ErrorIs(t, toolErr, ErrInvalidEvent)
	assert.Contains(t, toolErr.Error(), "invalid tool message")
}
