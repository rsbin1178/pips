package ai_test

import (
	"encoding/json"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMessageJSONRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		msg  ai.Message
	}{
		{
			name: "system text",
			msg:  ai.SystemText("be concise"),
		},
		{
			name: "user text",
			msg:  ai.UserText("hello"),
		},
		{
			name: "multimodal user",
			msg: ai.User(
				ai.Text("describe this"),
				ai.ImageURL("https://example.com/cat.png"),
				ai.ImageData("image/png", []byte{0x89, 0x50, 0x4e, 0x47}),
				ai.FileData("report.pdf", "application/pdf", []byte("%PDF-1.7")),
				ai.FileURL("audio.mp3", "audio/mpeg", "https://example.com/audio.mp3"),
				ai.FileID("manual.pdf", "application/pdf", "file_123"),
			),
		},
		{
			name: "assistant with reasoning and tool call",
			msg: ai.Assistant(
				ai.ReasoningPart{Text: "let me think", Signature: "sig-123"},
				ai.TextPart{Text: "I'll check the weather"},
				ai.ToolCallPart{ID: "call_1", Name: "get_weather", Args: ai.JSON(`{"city":"Paris"}`)},
			),
		},
		{
			name: "redacted reasoning",
			msg: ai.Assistant(
				ai.ReasoningPart{Redacted: true, Signature: "opaque"},
			),
		},
		{
			name: "tool result with nested image",
			msg: ai.ToolResults(
				ai.ToolResultPart{
					ToolCallID: "call_1",
					Name:       "screenshot",
					Content: []ai.Part{
						ai.Text("here is the screen"),
						ai.ImageData("image/jpeg", []byte{0xff, 0xd8, 0xff}),
					},
				},
			),
		},
		{
			name: "tool error result",
			msg:  ai.ToolResultError("call_2", "get_weather", "city not found"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			data, err := json.Marshal(tc.msg)
			require.NoError(t, err)

			got, err := ai.UnmarshalMessage(data)
			require.NoError(t, err)
			assert.Equal(t, tc.msg, got)
		})
	}
}

func TestMessageJSONStableShape(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(ai.UserText("hi"))
	require.NoError(t, err)
	assert.JSONEq(t, `{"role":"user","parts":[{"type":"text","text":"hi"}]}`, string(data))
}

func TestMessageUnmarshalUnknownPart(t *testing.T) {
	t.Parallel()

	_, err := ai.UnmarshalMessage([]byte(`{"role":"user","parts":[{"type":"bogus"}]}`))
	require.Error(t, err)
	require.ErrorIs(t, err, ai.ErrInvalidMessage)
	assert.Contains(t, err.Error(), "bogus")
}

func TestMessageUnmarshalInvalidJSON(t *testing.T) {
	t.Parallel()

	_, err := ai.UnmarshalMessage([]byte(`{"role":`))
	require.Error(t, err)
	assert.ErrorIs(t, err, ai.ErrInvalidMessage)
}

func TestMessageJSONRejectsRoleSpecificPart(t *testing.T) {
	t.Parallel()

	_, err := ai.UnmarshalMessage([]byte(`{"role":"user","parts":[{"type":"reasoning","text":"secret"}]}`))
	require.Error(t, err)
	assert.ErrorIs(t, err, ai.ErrInvalidMessage)
}

func TestMessagesJSONRoundTripAndSystemPrefix(t *testing.T) {
	t.Parallel()

	messages := ai.Messages{
		ai.SystemText("first"),
		ai.SystemText("second"),
		ai.UserText("hello"),
		ai.AssistantText("hi"),
	}

	data, err := json.Marshal(messages)
	require.NoError(t, err)

	var got ai.Messages
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, messages, got)

	system, conversation, err := got.SplitSystem()
	require.NoError(t, err)
	assert.Equal(t, "first\nsecond", ai.JoinSystemText(system))
	assert.Equal(t, ai.Messages{ai.UserText("hello"), ai.AssistantText("hi")}, conversation)
}

func TestMessagesRejectSystemAfterConversation(t *testing.T) {
	t.Parallel()

	messages := ai.Messages{ai.UserText("hello"), ai.SystemText("too late")}

	err := messages.Validate()
	require.ErrorIs(t, err, ai.ErrInvalidMessage)

	_, err = json.Marshal(messages)
	assert.ErrorIs(t, err, ai.ErrInvalidMessage)
}

func TestValidateMessageRejectsPointerVariant(t *testing.T) {
	t.Parallel()

	var message ai.Message = &ai.UserMessage{}

	err := ai.ValidateMessage(message)
	require.Error(t, err)
	assert.ErrorIs(t, err, ai.ErrInvalidMessage)
}

func TestCloneMessageOwnsMutableContent(t *testing.T) {
	t.Parallel()

	message := ai.User(ai.ImageData("image/png", []byte{1, 2, 3}))
	cloned, err := ai.CloneMessage(message)
	require.NoError(t, err)

	image, ok := message.Parts[0].(ai.ImagePart)
	require.True(t, ok)

	image.Source.Data[0] = 9

	clonedMessage, ok := cloned.(ai.UserMessage)
	require.True(t, ok)
	clonedImage, ok := clonedMessage.Parts[0].(ai.ImagePart)
	require.True(t, ok)
	assert.Equal(t, byte(1), clonedImage.Source.Data[0])
}

func TestMessageJSONRejectsCyclicToolResult(t *testing.T) {
	t.Parallel()

	content := make([]ai.Part, 1)
	content[0] = ai.ToolResultPart{ToolCallID: "nested", Content: content}
	message := ai.ToolResults(ai.ToolResultPart{ToolCallID: "root", Content: content})

	_, err := json.Marshal(message)
	require.Error(t, err)
	assert.ErrorIs(t, err, ai.ErrInvalidMessage)
}

func TestMediaSourceKinds(t *testing.T) {
	t.Parallel()

	assert.True(t, ai.FileURL("x", "text/plain", "https://example.com/x").Source.IsURL())
	assert.True(t, ai.FileID("x", "text/plain", "file_x").Source.IsID())
	assert.False(t, ai.FileData("x", "text/plain", []byte("x")).Source.IsURL())
}
