package openai_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const chatTextResponse = `{
  "id": "chatcmpl-abc123",
  "object": "chat.completion",
  "created": 1755000000,
  "model": "gpt-4o-2024-11-20",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "The capital of France is Paris."
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 14,
    "completion_tokens": 8,
    "total_tokens": 22,
    "prompt_tokens_details": { "cached_tokens": 6 },
    "completion_tokens_details": { "reasoning_tokens": 0 }
  }
}`

const chatToolsResponse = `{
  "id": "chatcmpl-tool456",
  "object": "chat.completion",
  "created": 1755000001,
  "model": "gpt-4o-2024-11-20",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": null,
        "tool_calls": [
          {
            "id": "call_w1",
            "type": "function",
            "function": {
              "name": "get_weather",
              "arguments": "{\"city\":\"Paris\",\"unit\":\"celsius\"}"
            }
          },
          {
            "id": "call_w2",
            "type": "function",
            "function": {
              "name": "get_time",
              "arguments": "{\"tz\":\"Europe/Paris\"}"
            }
          }
        ]
      },
      "finish_reason": "tool_calls"
    }
  ],
  "usage": {
    "prompt_tokens": 80,
    "completion_tokens": 40,
    "total_tokens": 120
  }
}`

// newTestModel builds a Model pointed at a fixture-serving httptest server
// and returns the captured request bodies.
func newTestModel(t *testing.T, handler http.HandlerFunc, opts ...openai.Option) *openai.Model {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	base := []openai.Option{
		openai.WithAPIKey("sk-test"),
		openai.WithBaseURL(server.URL + "/v1"),
		openai.WithAllowHTTP(),
		openai.WithAllowPrivateIPs(),
		openai.WithAPI(openai.APIChatCompletions),
	}

	return openai.New("gpt-4o", append(base, opts...)...)
}

// serveJSON replies with response and captures the request body.
func serveJSON(t *testing.T, response, wantPath string, captured *map[string]any) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, wantPath, r.URL.Path)
		assert.Equal(t, "Bearer sk-test", r.Header.Get("Authorization"))

		var body map[string]any
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))

		if captured != nil {
			*captured = body
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}
}

// as asserts v's dynamic type and returns it, failing the test otherwise.
func as[T any](t *testing.T, v any) T {
	t.Helper()

	out, ok := v.(T)
	require.True(t, ok, "expected %T, got %T (%v)", out, v, v)

	return out
}

func TestChatGenerateText(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, chatTextResponse, "/v1/chat/completions", &captured))

	resp, err := model.Generate(t.Context(), ai.Request{
		System:      "You are terse.",
		Messages:    []ai.Message{ai.UserText("Capital of France?")},
		Temperature: ai.Ptr(0.2),
		MaxTokens:   ai.Ptr(100),
	})
	require.NoError(t, err)

	// Outgoing wire shape.
	assert.Equal(t, "gpt-4o", captured["model"])
	assert.InDelta(t, 0.2, as[float64](t, captured["temperature"]), 1e-9)
	assert.InDelta(t, 100, as[float64](t, captured["max_completion_tokens"]), 1e-9)
	assert.NotContains(t, captured, "max_tokens")
	messages := as[[]any](t, captured["messages"])
	require.Len(t, messages, 2)
	assert.Equal(t, map[string]any{"role": "system", "content": "You are terse."}, messages[0])
	assert.Equal(t, map[string]any{"role": "user", "content": "Capital of France?"}, messages[1])

	// Normalized response.
	assert.Equal(t, "chatcmpl-abc123", resp.ID)
	assert.Equal(t, "gpt-4o-2024-11-20", resp.Model)
	assert.Equal(t, ai.ProviderOpenAI, resp.Provider)
	assert.Equal(t, "The capital of France is Paris.", resp.Text())
	assert.Equal(t, ai.FinishStop, resp.FinishReason)
	assert.Equal(t, ai.Usage{
		InputTokens:       14,
		OutputTokens:      8,
		CachedInputTokens: 6,
	}, resp.Usage)
	assert.NotEmpty(t, resp.Raw)
}

func TestChatGenerateVisionWireFormat(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, chatTextResponse, "/v1/chat/completions", &captured))

	_, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.User(
			ai.Text("what is this?"),
			ai.ImageURL("https://example.com/cat.png"),
			ai.ImageData("image/png", []byte{1, 2, 3}),
		)},
	})
	require.NoError(t, err)

	messages := as[[]any](t, captured["messages"])
	require.Len(t, messages, 1)
	content := as[[]any](t, as[map[string]any](t, messages[0])["content"])
	require.Len(t, content, 3)

	assert.Equal(t, map[string]any{"type": "text", "text": "what is this?"}, content[0])
	assert.Equal(t, map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": "https://example.com/cat.png"},
	}, content[1])
	assert.Equal(t, map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": "data:image/png;base64,AQID"},
	}, content[2])
}

func TestChatGenerateToolsRoundTrip(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, chatToolsResponse, "/v1/chat/completions", &captured))

	weatherTool := ai.Tool{
		Name:        "get_weather",
		Description: "Get current weather",
		InputSchema: &ai.Schema{
			Type: "object",
			Properties: map[string]*ai.Schema{
				"city": {Type: "string"},
			},
			Required: []string{"city"},
		},
	}

	resp, err := model.Generate(t.Context(), ai.Request{
		Messages:   []ai.Message{ai.UserText("weather in paris?")},
		Tools:      []ai.Tool{weatherTool},
		ToolChoice: ai.ToolChoice{Mode: ai.ToolChoiceAuto},
	})
	require.NoError(t, err)

	// Outgoing tools.
	tools := as[[]any](t, captured["tools"])
	require.Len(t, tools, 1)
	fn := as[map[string]any](t, as[map[string]any](t, tools[0])["function"])
	assert.Equal(t, "get_weather", fn["name"])
	assert.Equal(t, "auto", captured["tool_choice"])

	params := as[map[string]any](t, fn["parameters"])
	assert.Equal(t, "object", params["type"])

	// Normalized tool calls.
	assert.Equal(t, ai.FinishToolCalls, resp.FinishReason)
	calls := resp.ToolCalls()
	require.Len(t, calls, 2)
	assert.Equal(t, "call_w1", calls[0].ID)
	assert.Equal(t, "get_weather", calls[0].Name)
	assert.JSONEq(t, `{"city":"Paris","unit":"celsius"}`, string(calls[0].Args))
	assert.Equal(t, "get_time", calls[1].Name)
}

func TestChatToolResultAndAssistantHistoryWireFormat(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, chatTextResponse, "/v1/chat/completions", &captured))

	_, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{
			ai.UserText("weather?"),
			ai.Assistant(
				ai.Text("Let me check."),
				ai.ToolCallPart{ID: "call_w1", Name: "get_weather", Args: ai.JSON(`{"city":"Paris"}`)},
			),
			ai.ToolResultText("call_w1", "get_weather", `{"temp":21}`),
		},
	})
	require.NoError(t, err)

	messages := as[[]any](t, captured["messages"])
	require.Len(t, messages, 3)

	assistant := as[map[string]any](t, messages[1])
	assert.Equal(t, "assistant", assistant["role"])
	assert.Equal(t, "Let me check.", assistant["content"])
	toolCalls := as[[]any](t, assistant["tool_calls"])
	require.Len(t, toolCalls, 1)
	call := as[map[string]any](t, toolCalls[0])
	assert.Equal(t, "call_w1", call["id"])
	assert.Equal(t, "function", call["type"])

	toolMsg := as[map[string]any](t, messages[2])
	assert.Equal(t, "tool", toolMsg["role"])
	assert.Equal(t, "call_w1", toolMsg["tool_call_id"])
	assert.Equal(t, `{"temp":21}`, toolMsg["content"])
}

func TestChatStructuredOutputWireFormat(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, chatTextResponse, "/v1/chat/completions", &captured))

	_, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("extract")},
		ResponseFormat: &ai.ResponseFormat{
			Name:   "weather",
			Schema: &ai.Schema{Type: "object", Properties: map[string]*ai.Schema{"temp": {Type: "number"}}, Required: []string{"temp"}, AdditionalProperties: false},
			Strict: true,
		},
	})
	require.NoError(t, err)

	rf := as[map[string]any](t, captured["response_format"])
	assert.Equal(t, "json_schema", rf["type"])
	js := as[map[string]any](t, rf["json_schema"])
	assert.Equal(t, "weather", js["name"])
	assert.Equal(t, true, js["strict"])
	schema := as[map[string]any](t, js["schema"])
	assert.Equal(t, false, schema["additionalProperties"])
}

func TestChatCompatMode(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t,
		serveJSON(t, chatTextResponse, "/v1/chat/completions", &captured),
		openai.WithCompatMode(),
	)

	_, err := model.Generate(t.Context(), ai.Request{
		Messages:  []ai.Message{ai.UserText("hi")},
		MaxTokens: ai.Ptr(64),
	})
	require.NoError(t, err)

	assert.InDelta(t, 64, as[float64](t, captured["max_tokens"]), 1e-9)
	assert.NotContains(t, captured, "max_completion_tokens")
}

func TestChatProviderOptionsExtraFields(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, chatTextResponse, "/v1/chat/completions", &captured))

	_, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("hi")},
		ProviderOptions: map[ai.Provider]any{
			ai.ProviderOpenAI: openai.RequestOptions{ExtraFields: map[string]any{
				"seed": 7,
				"user": "tester",
			}},
		},
	})
	require.NoError(t, err)

	assert.InDelta(t, 7, as[float64](t, captured["seed"]), 1e-9)
	assert.Equal(t, "tester", captured["user"])
}

func TestChatErrorMapping(t *testing.T) {
	t.Parallel()

	model := newTestModel(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"Rate limit reached","type":"rate_limit_error","code":"rate_limit_exceeded"}}`))
	})

	_, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("hi")}})
	require.Error(t, err)
	require.ErrorIs(t, err, ai.ErrRateLimited)
	assert.True(t, ai.IsRetryable(err))

	var apiErr *ai.Error
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, 429, apiErr.StatusCode)
	assert.Equal(t, "Rate limit reached", apiErr.Message)
	assert.Equal(t, "rate_limit_error", apiErr.Type)
	assert.Equal(t, "rate_limit_exceeded", apiErr.Code)
	assert.Equal(t, 12*time.Second, apiErr.RetryAfter)
}

func TestChatInvalidRequestNotRetryable(t *testing.T) {
	t.Parallel()

	model := newTestModel(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Unknown parameter","type":"invalid_request_error"}}`))
	})

	_, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("hi")}})
	require.ErrorIs(t, err, ai.ErrInvalidRequest)
	assert.False(t, ai.IsRetryable(err))
}
