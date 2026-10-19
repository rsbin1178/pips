package openai_test

import (
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestChatToolResultPreservesMedia pins the behavior the media-aware read tool
// depends on: a Chat Completions tool result keeps its non-text parts as a
// content-part array instead of being flattened to text. A regression here
// would make tool images invisible on every Chat-compatible endpoint.
func TestChatToolResultPreservesMedia(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, chatTextResponse, "/v1/chat/completions", &captured))

	_, err := model.Generate(t.Context(), ai.Request{Messages: ai.Messages{
		ai.UserText("inspect"),
		ai.Assistant(ai.ToolCallPart{ID: "call_w1", Name: "read", Args: ai.JSON(`{}`)}),
		ai.ToolResults(ai.ToolResultPart{
			ToolCallID: "call_w1",
			Name:       "read",
			Content: []ai.Part{
				ai.Text("here is the screen"),
				ai.ImageData("image/png", []byte{1, 2, 3}),
				ai.FileData("inline.pdf", "application/pdf", []byte("%PDF")),
			},
		}),
	}})
	require.NoError(t, err)

	messages := as[[]any](t, captured["messages"])
	require.Len(t, messages, 3)

	toolMsg := as[map[string]any](t, messages[2])
	assert.Equal(t, "tool", toolMsg["role"])
	assert.Equal(t, "call_w1", toolMsg["tool_call_id"])
	assert.Equal(t, []any{
		map[string]any{"type": "text", "text": "here is the screen"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AQID"}},
		map[string]any{"type": "file", "file": map[string]any{"filename": "inline.pdf", "file_data": "data:application/pdf;base64,JVBERg=="}},
	}, toolMsg["content"])
}

// TestChatToolResultTextOnlyStaysString keeps the text-only shape pinned: the
// common case must not silently become a content-part array.
func TestChatToolResultTextOnlyStaysString(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, chatTextResponse, "/v1/chat/completions", &captured))

	_, err := model.Generate(t.Context(), ai.Request{Messages: ai.Messages{
		ai.UserText("inspect"),
		ai.Assistant(ai.ToolCallPart{ID: "call_w1", Name: "read", Args: ai.JSON(`{}`)}),
		ai.ToolResultText("call_w1", "read", `{"temp":21}`),
	}})
	require.NoError(t, err)

	messages := as[[]any](t, captured["messages"])
	toolMsg := as[map[string]any](t, messages[2])
	assert.Equal(t, `{"temp":21}`, toolMsg["content"])
}
