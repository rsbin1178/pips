package openai_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
	"github.com/rsbin1178/pips/ai/openai/compat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenRouterReasoningDetailsToolContinuation(t *testing.T) {
	t.Parallel()

	wantDetails := []any{
		map[string]any{
			"type": "reasoning.text", "text": "checking", "signature": "sig-1",
			"id": "reasoning-1", "format": "unknown", "index": float64(0),
		},
		map[string]any{
			"type": "reasoning.encrypted", "data": "opaque-state",
			"id": "reasoning-2", "format": "unknown", "index": float64(1),
		},
		map[string]any{
			"type": "reasoning.summary", "summary": "used a lookup",
			"id": "reasoning-3", "format": "unknown", "index": float64(2),
		},
	}

	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
			return
		}

		w.Header().Set("Content-Type", "application/json")

		if calls.Add(1) == 1 {
			_, _ = w.Write([]byte(`{
				"id":"chat_or_1","model":"reasoning-model","choices":[{
					"message":{"role":"assistant","content":null,"reasoning":"checkingused a lookup","reasoning_details":[
						{"type":"reasoning.text","text":"checking","signature":"sig-1","id":"reasoning-1","format":"unknown","index":0},
						{"type":"reasoning.encrypted","data":"opaque-state","id":"reasoning-2","format":"unknown","index":1},
						{"type":"reasoning.summary","summary":"used a lookup","id":"reasoning-3","format":"unknown","index":2}
					],"tool_calls":[{"id":"call_or_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
					"finish_reason":"tool_calls"
				}]
			}`))

			return
		}

		messages := as[[]any](t, body["messages"])
		if !assert.Len(t, messages, 3) {
			return
		}

		assistant := as[map[string]any](t, messages[1])
		if !assert.Equal(t, wantDetails, assistant["reasoning_details"]) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"reasoning_details must be replayed unchanged","type":"invalid_request_error"}}`))

			return
		}

		assert.NotContains(t, assistant, "reasoning")

		_, _ = w.Write([]byte(`{"id":"chat_or_2","model":"reasoning-model","choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := compat.OpenRouter("reasoning-model", compatibleOptions(server.URL)...)
	first, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("look it up")}})
	require.NoError(t, err)
	require.Len(t, first.Message.Parts, 2)

	reasoning := as[ai.ReasoningPart](t, first.Message.Parts[0])
	assert.Equal(t, "checkingused a lookup", reasoning.Text)
	assert.NotEmpty(t, reasoning.Signature)

	second, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{
		ai.UserText("look it up"),
		first.Message,
		ai.ToolResultText("call_or_1", "lookup", "value"),
	}})
	require.NoError(t, err)
	assert.Equal(t, "done", second.Text())
}

func TestMistralThinkingChunkToolContinuation(t *testing.T) {
	t.Parallel()

	wantContent := []any{
		map[string]any{
			"type": "thinking",
			"thinking": []any{
				map[string]any{"type": "text", "text": "need "},
				map[string]any{"type": "text", "text": "lookup"},
			},
			"signature": "mistral-signature",
			"closed":    true,
		},
		map[string]any{"type": "text", "text": "I'll check."},
	}

	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
			return
		}

		w.Header().Set("Content-Type", "application/json")

		if calls.Add(1) == 1 {
			_, _ = w.Write([]byte(`{
				"id":"chat_m_1","model":"magistral","choices":[{
					"message":{"role":"assistant","content":[
						{"type":"thinking","thinking":[{"type":"text","text":"need "},{"type":"text","text":"lookup"}],"signature":"mistral-signature","closed":true},
						{"type":"text","text":"I'll check."}
					],"tool_calls":[{"id":"call_m_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
					"finish_reason":"tool_calls"
				}]
			}`))

			return
		}

		messages := as[[]any](t, body["messages"])
		if !assert.Len(t, messages, 3) {
			return
		}

		assistant := as[map[string]any](t, messages[1])
		if !assert.Equal(t, wantContent, assistant["content"]) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"thinking chunk must be replayed unchanged"}`))

			return
		}

		assert.NotContains(t, assistant, "reasoning")

		_, _ = w.Write([]byte(`{"id":"chat_m_2","model":"magistral","choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := compat.Mistral("magistral", compatibleOptions(server.URL)...)
	first, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("look it up")}})
	require.NoError(t, err)
	require.Len(t, first.Message.Parts, 3)

	reasoning := as[ai.ReasoningPart](t, first.Message.Parts[0])
	assert.Equal(t, "need lookup", reasoning.Text)
	assert.NotEmpty(t, reasoning.Signature)
	assert.Equal(t, ai.TextPart{Text: "I'll check."}, first.Message.Parts[1])

	second, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{
		ai.UserText("look it up"),
		first.Message,
		ai.ToolResultText("call_m_1", "lookup", "value"),
	}})
	require.NoError(t, err)
	assert.Equal(t, "done", second.Text())
}

func TestPlainReasoningHistoryProfiles(t *testing.T) {
	t.Parallel()

	profiles := []struct {
		name     string
		newModel func(string, ...openai.Option) *openai.Model
	}{
		{name: "groq", newModel: compat.Groq},
		{name: "cerebras", newModel: compat.Cerebras},
		{name: "openrouter legacy fallback", newModel: compat.OpenRouter},
	}

	for _, tc := range profiles {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var captured map[string]any

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&captured)) {
					return
				}

				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"chat_1","model":"reasoning-model","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
			}))
			t.Cleanup(server.Close)

			model := tc.newModel("reasoning-model", compatibleOptions(server.URL)...)
			_, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{
				ai.UserText("question"),
				ai.Assistant(ai.ReasoningPart{Text: "prior reasoning"}, ai.Text("prior answer")),
			}})
			require.NoError(t, err)

			messages := as[[]any](t, captured["messages"])
			assistant := as[map[string]any](t, messages[1])
			assert.Equal(t, "prior reasoning", assistant["reasoning"])
		})
	}
}

func TestMistralPlainReasoningHistoryUsesThinkingChunk(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&captured)) {
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat_1","model":"magistral","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := compat.Mistral("magistral", compatibleOptions(server.URL)...)
	_, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{
		ai.UserText("question"),
		ai.Assistant(ai.ReasoningPart{Text: "prior reasoning"}, ai.Text("prior answer")),
	}})
	require.NoError(t, err)

	messages := as[[]any](t, captured["messages"])
	assistant := as[map[string]any](t, messages[1])
	assert.Equal(t, []any{
		map[string]any{
			"type": "thinking",
			"thinking": []any{
				map[string]any{"type": "text", "text": "prior reasoning"},
			},
		},
		map[string]any{"type": "text", "text": "prior answer"},
	}, assistant["content"])
	assert.NotContains(t, assistant, "reasoning")
}

func TestOpenRouterStreamedReasoningDetailsToolContinuation(t *testing.T) {
	t.Parallel()

	wantDetails := []any{
		map[string]any{
			"type": "reasoning.text", "text": "need lookup", "signature": "sig-1",
			"id": "reasoning-1", "format": "unknown", "index": float64(0),
		},
		map[string]any{
			"type": "reasoning.encrypted", "data": "opaque-state",
			"id": "reasoning-2", "format": "unknown", "index": float64(1),
		},
	}

	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
			return
		}

		if calls.Add(1) == 1 {
			assert.Equal(t, true, body["stream"])
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`data: {"id":"chat_or_s1","model":"reasoning-model","choices":[{"index":0,"delta":{"reasoning_details":[{"type":"reasoning.text","text":"need ","signature":"sig-","id":"reasoning-1","format":"unknown","index":0}]}}]}

data: {"id":"chat_or_s1","model":"reasoning-model","choices":[{"index":0,"delta":{"reasoning_details":[{"type":"reasoning.text","text":"lookup","signature":"1","index":0},{"type":"reasoning.encrypted","data":"opaque-","id":"reasoning-2","format":"unknown","index":1}]}}]}

data: {"id":"chat_or_s1","model":"reasoning-model","choices":[{"index":0,"delta":{"reasoning_details":[{"type":"reasoning.encrypted","data":"state","index":1}]}}]}

data: {"id":"chat_or_s1","model":"reasoning-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_or_s1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}

data: [DONE]

`))

			return
		}

		messages := as[[]any](t, body["messages"])

		assistant := as[map[string]any](t, messages[1])
		if !assert.Equal(t, wantDetails, assistant["reasoning_details"]) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"streamed reasoning_details were not reconstructed","type":"invalid_request_error"}}`))

			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat_or_s2","model":"reasoning-model","choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := compat.OpenRouter("reasoning-model", compatibleOptions(server.URL)...)
	first, err := ai.Collect(model.Stream(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("look it up")}}))
	require.NoError(t, err)
	require.Len(t, first.Message.Parts, 2)
	assert.Equal(t, "need lookup", as[ai.ReasoningPart](t, first.Message.Parts[0]).Text)

	_, err = model.Generate(t.Context(), ai.Request{Messages: []ai.Message{
		ai.UserText("look it up"),
		first.Message,
		ai.ToolResultText("call_or_s1", "lookup", "value"),
	}})
	require.NoError(t, err)
}

func TestMistralStreamedThinkingChunkToolContinuation(t *testing.T) {
	t.Parallel()

	wantContent := []any{
		map[string]any{
			"type": "thinking",
			"thinking": []any{
				map[string]any{"type": "text", "text": "need "},
				map[string]any{"type": "text", "text": "lookup"},
			},
			"signature": "mistral-stream-signature",
			"closed":    true,
		},
		map[string]any{"type": "text", "text": "I'll check."},
	}

	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
			return
		}

		if calls.Add(1) == 1 {
			assert.Equal(t, true, body["stream"])
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`data: {"id":"chat_m_s1","model":"magistral","choices":[{"index":0,"delta":{"content":[{"type":"thinking","thinking":[{"type":"text","text":"need "}]}]}}]}

data: {"id":"chat_m_s1","model":"magistral","choices":[{"index":0,"delta":{"content":[{"type":"thinking","thinking":[{"type":"text","text":"lookup"}],"signature":"mistral-stream-signature","closed":true}]}}]}

data: {"id":"chat_m_s1","model":"magistral","choices":[{"index":0,"delta":{"content":[{"type":"text","text":"I'll check."}],"tool_calls":[{"index":0,"id":"call_m_s1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}

data: [DONE]

`))

			return
		}

		messages := as[[]any](t, body["messages"])

		assistant := as[map[string]any](t, messages[1])
		if !assert.Equal(t, wantContent, assistant["content"]) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"streamed thinking chunk was not reconstructed"}`))

			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat_m_s2","model":"magistral","choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := compat.Mistral("magistral", compatibleOptions(server.URL)...)
	first, err := ai.Collect(model.Stream(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("look it up")}}))
	require.NoError(t, err)
	require.Len(t, first.Message.Parts, 3)
	assert.Equal(t, "need lookup", as[ai.ReasoningPart](t, first.Message.Parts[0]).Text)
	assert.Equal(t, ai.TextPart{Text: "I'll check."}, first.Message.Parts[1])

	_, err = model.Generate(t.Context(), ai.Request{Messages: []ai.Message{
		ai.UserText("look it up"),
		first.Message,
		ai.ToolResultText("call_m_s1", "lookup", "value"),
	}})
	require.NoError(t, err)
}

func compatibleOptions(serverURL string) []openai.Option {
	return []openai.Option{
		openai.WithBaseURL(serverURL + "/v1"),
		openai.WithAPIKey("key"),
		openai.WithAllowHTTP(),
		openai.WithAllowPrivateIPs(),
	}
}
