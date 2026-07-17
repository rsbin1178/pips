package ai_test

import (
	"encoding/json"
	"testing"

	"github.com/rsbin/pips/ai"
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
			msg: ai.Message{Role: ai.RoleTool, Parts: []ai.Part{
				ai.ToolResultPart{
					ToolCallID: "call_1",
					Name:       "screenshot",
					Content: []ai.Part{
						ai.Text("here is the screen"),
						ai.ImageData("image/jpeg", []byte{0xff, 0xd8, 0xff}),
					},
				},
			}},
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

			var got ai.Message
			require.NoError(t, json.Unmarshal(data, &got))
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

	var m ai.Message

	err := json.Unmarshal([]byte(`{"role":"user","parts":[{"type":"bogus"}]}`), &m)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bogus")
}
