package anthropic_test

import (
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStreamThinkingAndText(t *testing.T) {
	t.Parallel()

	model := newTestModel(t, serveSSE(t, "stream_thinking.sse"))

	resp, err := ai.Collect(model.Stream(t.Context(), ai.Request{
		Messages:  []ai.Message{ai.UserText("capital?")},
		Reasoning: &ai.ReasoningConfig{Effort: ai.ReasoningMedium},
	}))
	require.NoError(t, err)

	require.Len(t, resp.Message.Parts, 2)
	reasoning := as[ai.ReasoningPart](t, resp.Message.Parts[0])
	assert.Equal(t, "The user asks a simple question.", reasoning.Text)
	assert.Equal(t, "sig-xyz", reasoning.Signature)
	assert.Equal(t, "Paris is the capital.", resp.Text())
	assert.Equal(t, ai.FinishStop, resp.FinishReason)

	// input_tokens folds in cache reads (25 + 5), output from message_delta.
	assert.Equal(t, 30, resp.Usage.InputTokens)
	assert.Equal(t, 5, resp.Usage.CachedInputTokens)
	assert.Equal(t, 15, resp.Usage.OutputTokens)
}

func TestStreamToolCalls(t *testing.T) {
	t.Parallel()

	model := newTestModel(t, serveSSE(t, "stream_tools.sse"))

	resp, err := ai.Collect(model.Stream(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("weather?")},
		Tools:    []ai.Tool{{Name: "get_weather"}},
	}))
	require.NoError(t, err)

	assert.Equal(t, ai.FinishToolCalls, resp.FinishReason)
	calls := resp.ToolCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, "toolu_a", calls[0].ID)
	assert.Equal(t, "get_weather", calls[0].Name)
	assert.JSONEq(t, `{"city":"Paris"}`, string(calls[0].Args))
	assert.Equal(t, 20, resp.Usage.OutputTokens)
}

func TestStreamErrorEvent(t *testing.T) {
	t.Parallel()

	model := newTestModel(t, serveRawSSE(t,
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"))

	var gotErr error

	for _, err := range model.Stream(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("hi")}}) {
		if err != nil {
			gotErr = err
			break
		}
	}

	require.Error(t, gotErr)

	var apiErr *ai.Error
	require.ErrorAs(t, gotErr, &apiErr)
	assert.Equal(t, "overloaded_error", apiErr.Type)
}
