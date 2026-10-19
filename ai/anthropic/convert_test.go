package anthropic_test

import (
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestToolResultPreservesMedia pins the behavior the media-aware read tool
// depends on: an Anthropic tool_result keeps its non-text parts as image and
// document blocks instead of being flattened to text. A regression here would
// make tool images invisible on every Messages endpoint.
func TestToolResultPreservesMedia(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, textResponse, "/v1/messages", &captured))

	_, err := model.Generate(t.Context(), ai.Request{Messages: ai.Messages{
		ai.UserText("inspect"),
		ai.Assistant(ai.ToolCallPart{ID: "toolu_w1", Name: "read", Args: ai.JSON(`{}`)}),
		ai.ToolResults(ai.ToolResultPart{
			ToolCallID: "toolu_w1",
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
	assert.Equal(t, "user", toolMsg["role"])

	result := as[map[string]any](t, as[[]any](t, toolMsg["content"])[0])
	assert.Equal(t, "tool_result", result["type"])
	assert.Equal(t, "toolu_w1", result["tool_use_id"])
	assert.Equal(t, []any{
		map[string]any{"type": "text", "text": "here is the screen"},
		map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": "image/png", "data": "AQID",
		}},
		map[string]any{"type": "document", "source": map[string]any{
			"type": "base64", "media_type": "application/pdf", "data": "JVBERg==",
		}},
	}, result["content"])
}

// TestToolResultTextOnlyStaysString keeps the text-only shape pinned: the
// common case must not silently become a block array.
func TestToolResultTextOnlyStaysString(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, textResponse, "/v1/messages", &captured))

	_, err := model.Generate(t.Context(), ai.Request{Messages: ai.Messages{
		ai.UserText("inspect"),
		ai.Assistant(ai.ToolCallPart{ID: "toolu_w1", Name: "read", Args: ai.JSON(`{}`)}),
		ai.ToolResultText("toolu_w1", "read", `{"temp":21}`),
	}})
	require.NoError(t, err)

	messages := as[[]any](t, captured["messages"])
	toolMsg := as[map[string]any](t, messages[2])
	result := as[map[string]any](t, as[[]any](t, toolMsg["content"])[0])
	assert.Equal(t, []any{
		map[string]any{"type": "text", "text": `{"temp":21}`},
	}, result["content"])
}
