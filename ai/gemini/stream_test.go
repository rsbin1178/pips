package gemini_test

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/rsbin1178/pips/ai"
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

func TestStreamedFunctionCallThoughtSignatureContinuation(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32

	model := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
			return
		}

		if calls.Add(1) == 1 {
			assert.Equal(t, "sse", r.URL.Query().Get("alt"))
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"fc-stream-1","name":"get_weather","args":{"city":"Paris"}},"thoughtSignature":"stream-thought-signature"}]} ,"finishReason":"STOP","index":0}],"modelVersion":"gemini-3-flash","responseId":"resp-stream-tool"}

`))

			return
		}

		contents := as[[]any](t, body["contents"])
		if !assert.Len(t, contents, 3) {
			return
		}

		modelTurn := as[map[string]any](t, contents[1])
		callPart := as[map[string]any](t, as[[]any](t, modelTurn["parts"])[0])
		functionCall := as[map[string]any](t, callPart["functionCall"])

		if callPart["thoughtSignature"] != "stream-thought-signature" || functionCall["id"] != "fc-stream-1" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":400,"message":"thoughtSignature must be replayed unchanged","status":"INVALID_ARGUMENT"}}`))

			return
		}

		toolTurn := as[map[string]any](t, contents[2])
		responsePart := as[map[string]any](t, as[[]any](t, toolTurn["parts"])[0])
		functionResponse := as[map[string]any](t, responsePart["functionResponse"])
		assert.Equal(t, "fc-stream-1", functionResponse["id"])

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]},"finishReason":"STOP","index":0}],"modelVersion":"gemini-3-flash","responseId":"resp-2"}`))
	})

	first, err := ai.Collect(model.Stream(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("weather?")},
		Tools:    []ai.Tool{{Name: "get_weather"}},
	}))
	require.NoError(t, err)
	require.Len(t, first.ToolCalls(), 1)

	call := first.ToolCalls()[0]
	second, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{
		ai.UserText("weather?"),
		first.Message,
		ai.ToolResultText(call.ID, call.Name, `{"temp":21}`),
	}})
	require.NoError(t, err)
	assert.Equal(t, "done", second.Text())
}
