package compat_test

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

type constructor func(string, ...openai.Option) *openai.Model

func as[T any](t *testing.T, value any) T {
	t.Helper()

	result, ok := value.(T)
	require.True(t, ok, "expected %T, got %T", result, value)

	return result
}

func TestNamedProfiles(t *testing.T) {
	t.Parallel()

	profiles := []struct {
		name     string
		provider ai.Provider
		newModel constructor
		path     string
	}{
		{"deepseek", ai.ProviderDeepSeek, compat.DeepSeek, "/v1/chat/completions"},
		{"groq", ai.ProviderGroq, compat.Groq, "/v1/chat/completions"},
		{"xai", ai.ProviderXAI, compat.XAI, "/v1/responses"},
		{"openrouter", ai.ProviderOpenRouter, compat.OpenRouter, "/v1/chat/completions"},
		{"cerebras", ai.ProviderCerebras, compat.Cerebras, "/v1/chat/completions"},
		{"together", ai.ProviderTogether, compat.Together, "/v1/chat/completions"},
		{"mistral", ai.ProviderMistral, compat.Mistral, "/v1/chat/completions"},
	}

	for _, tc := range profiles {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var gotPath string

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")

				if tc.provider == ai.ProviderXAI {
					_, _ = w.Write([]byte(`{"id":"resp_1","model":"grok-4.6","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`))
					return
				}

				_, _ = w.Write([]byte(`{"id":"chat_1","model":"served","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
			}))
			t.Cleanup(server.Close)

			model := tc.newModel("model-id",
				openai.WithBaseURL(server.URL+"/v1"),
				openai.WithAPIKey("test-key"),
				openai.WithAllowHTTP(),
				openai.WithAllowPrivateIPs(),
			)
			assert.Equal(t, tc.provider, model.Provider())
			assert.True(t, model.Capabilities().Text)

			resp, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("hi")}})
			require.NoError(t, err)
			assert.Equal(t, tc.path, gotPath)
			assert.Equal(t, tc.provider, resp.Provider)
			assert.Equal(t, "ok", resp.Text())
		})
	}
}

func TestDeepSeekReasoningToolContinuation(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&captured))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chat_ds","model":"deepseek-reasoner","choices":[{
				"message":{"role":"assistant","content":null,"reasoning_content":"new reasoning","tool_calls":[{"id":"call_2","type":"function","function":{"name":"search","arguments":"{}"}}]},
				"finish_reason":"tool_calls"
			}]}`))
	}))
	t.Cleanup(server.Close)

	model := compat.DeepSeek("deepseek-reasoner",
		openai.WithBaseURL(server.URL+"/v1"),
		openai.WithAPIKey("key"),
		openai.WithAllowHTTP(),
		openai.WithAllowPrivateIPs(),
	)

	resp, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{
			ai.UserText("find it"),
			ai.Assistant(
				ai.ReasoningPart{Text: "prior reasoning"},
				ai.ToolCallPart{ID: "call_1", Name: "search", Args: ai.JSON(`{"q":"x"}`)},
			),
			ai.ToolResultText("call_1", "search", "result"),
		},
		MaxTokens: ai.Ptr(256),
		Seed:      ai.Ptr[int64](7),
		Reasoning: &ai.ReasoningConfig{Effort: ai.ReasoningLow},
		ResponseFormat: &ai.ResponseFormat{
			Name: "answer", Schema: &ai.Schema{Type: "object"}, Strict: true,
		},
	})
	require.NoError(t, err)

	assert.Contains(t, captured, "max_tokens")
	assert.NotContains(t, captured, "max_completion_tokens")
	assert.Equal(t, "high", captured["reasoning_effort"])
	assert.Equal(t, map[string]any{"type": "enabled"}, captured["thinking"])
	assert.Equal(t, map[string]any{"type": "json_object"}, captured["response_format"])
	assert.InDelta(t, 7, captured["seed"], 1e-9)
	assert.True(t, model.Capabilities().StructuredOutput)

	messages := as[[]any](t, captured["messages"])
	assistant := as[map[string]any](t, messages[1])
	assert.Equal(t, "prior reasoning", assistant["reasoning_content"])
	assert.Equal(t, "new reasoning", resp.Reasoning())
	assert.Equal(t, ai.ProviderDeepSeek, resp.Provider)
	assert.Equal(t, ai.FinishToolCalls, resp.FinishReason)
}

func TestXAIResponsesReasoningReplay(t *testing.T) {
	t.Parallel()

	var (
		calls  atomic.Int32
		second map[string]any
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any

		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))

		w.Header().Set("Content-Type", "application/json")

		if calls.Add(1) == 1 {
			assert.Equal(t, []any{"reasoning.encrypted_content"}, body["include"])

			_, _ = w.Write([]byte(`{
				"id":"resp_x1","model":"grok-4.6","status":"completed","output":[
					{"type":"reasoning","id":"rs_x1","encrypted_content":"encrypted-state","summary":[{"type":"summary_text","text":"checking"}],"content":[{"type":"reasoning_text","text":"provider reasoning content"}],"status":"completed"},
					{"type":"function_call","call_id":"call_x1","name":"lookup","arguments":"{}"}
				]
			}`))

			return
		}

		second = body

		input, inputOK := body["input"].([]any)
		if !inputOK || len(input) < 2 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Missing required input history","type":"invalid_request_error"}}`))

			return
		}

		replayed, replayOK := input[1].(map[string]any)

		_, summaryOK := replayed["summary"]
		if !replayOK || !summaryOK {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Missing required parameter: 'input[1].summary'.","type":"invalid_request_error","param":"input[1].summary"}}`))

			return
		}

		_, _ = w.Write([]byte(`{"id":"resp_x2","model":"grok-4.6","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`))
	}))
	t.Cleanup(server.Close)

	model := compat.XAI("grok-4.6",
		openai.WithBaseURL(server.URL+"/v1"),
		openai.WithAPIKey("key"),
		openai.WithAllowHTTP(),
		openai.WithAllowPrivateIPs(),
	)

	first, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("lookup")}})
	require.NoError(t, err)
	require.Len(t, first.Message.Parts, 2)

	reasoning := as[ai.ReasoningPart](t, first.Message.Parts[0])
	assert.Equal(t, "checking", reasoning.Text)
	assert.NotEmpty(t, reasoning.Signature)

	secondResp, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{
		ai.UserText("lookup"),
		first.Message,
		ai.ToolResultText("call_x1", "lookup", "value"),
	}})
	require.NoError(t, err)
	assert.Equal(t, "done", secondResp.Text())

	input := as[[]any](t, second["input"])
	require.Len(t, input, 4)

	replayed := as[map[string]any](t, input[1])
	assert.Equal(t, "reasoning", replayed["type"])
	assert.Equal(t, "rs_x1", replayed["id"])
	assert.Equal(t, []any{
		map[string]any{"type": "summary_text", "text": "checking"},
	}, replayed["summary"])
	assert.Equal(t, []any{
		map[string]any{"type": "reasoning_text", "text": "provider reasoning content"},
	}, replayed["content"])
	assert.Equal(t, "encrypted-state", replayed["encrypted_content"])
	assert.Equal(t, "completed", replayed["status"])
}

func TestProfileErrorsAndStreamUseRealProvider(t *testing.T) {
	t.Parallel()

	t.Run("error", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"slow down","type":"rate_limit_error"}}`))
		}))
		t.Cleanup(server.Close)

		model := compat.Groq("llama",
			openai.WithBaseURL(server.URL+"/v1"), openai.WithAPIKey("key"),
			openai.WithAllowHTTP(), openai.WithAllowPrivateIPs())
		_, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("hi")}})
		require.Error(t, err)

		var apiErr *ai.Error
		require.ErrorAs(t, err, &apiErr)
		assert.Equal(t, ai.ProviderGroq, apiErr.Provider)
		assert.Contains(t, err.Error(), "groq: chat completions")
	})

	t.Run("stream", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"llama\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
		}))
		t.Cleanup(server.Close)

		model := compat.Groq("llama",
			openai.WithBaseURL(server.URL+"/v1"), openai.WithAPIKey("key"),
			openai.WithAllowHTTP(), openai.WithAllowPrivateIPs())
		resp, err := ai.Collect(model.Stream(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("hi")}}))
		require.NoError(t, err)
		assert.Equal(t, ai.ProviderGroq, resp.Provider)
		assert.Equal(t, "ok", resp.Text())
	})
}

func TestCustomProfileDoesNotUseOpenAIKeyFallback(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "wrong-key")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := compat.New(compat.Profile{
		Provider:  ai.Provider("local"),
		BaseURL:   server.URL + "/v1",
		APIKeyEnv: []string{"UNSET_LOCAL_KEY"},
	}, "model", openai.WithAllowHTTP(), openai.WithAllowPrivateIPs())

	_, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("hi")}})
	require.NoError(t, err)
}
