package gemini_test

import (
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const textStream = `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]},"index":0}],"modelVersion":"gemini-2.5-flash","responseId":"resp-s1"}

data: {"candidates":[{"content":{"role":"model","parts":[{"text":"lo!"}]},"index":0}]}

data: {"candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"totalTokenCount":8}}

`

const toolsStream = `data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"Paris"}}}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":40,"candidatesTokenCount":10,"totalTokenCount":50},"modelVersion":"gemini-2.5-flash","responseId":"resp-s2"}

`

func TestStreamText(t *testing.T) {
	t.Parallel()

	model := newTestModel(t, serveSSE(t, textStream))

	resp, err := ai.Collect(model.Stream(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("hi")},
	}))
	require.NoError(t, err)

	assert.Equal(t, "resp-s1", resp.ID)
	assert.Equal(t, "Hello!", resp.Text())
	assert.Equal(t, ai.FinishStop, resp.FinishReason)
	assert.Equal(t, 5, resp.Usage.InputTokens)
	assert.Equal(t, 3, resp.Usage.OutputTokens)
}

func TestStreamToolCall(t *testing.T) {
	t.Parallel()

	model := newTestModel(t, serveSSE(t, toolsStream))

	resp, err := ai.Collect(model.Stream(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("weather?")},
		Tools:    []ai.Tool{{Name: "get_weather"}},
	}))
	require.NoError(t, err)

	// A whole-chunk functionCall becomes tool_calls, not stop.
	assert.Equal(t, ai.FinishToolCalls, resp.FinishReason)
	calls := resp.ToolCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, "get_weather", calls[0].Name)
	assert.JSONEq(t, `{"city":"Paris"}`, string(calls[0].Args))
}
