package anthropic_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/anthropic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const textResponse = `{
  "id": "msg_abc123",
  "type": "message",
  "role": "assistant",
  "model": "claude-sonnet-4-5-20250929",
  "content": [
    { "type": "text", "text": "The capital of France is Paris." }
  ],
  "stop_reason": "end_turn",
  "usage": {
    "input_tokens": 14,
    "output_tokens": 8,
    "cache_read_input_tokens": 4,
    "cache_creation_input_tokens": 0
  }
}`

const toolsResponse = `{
  "id": "msg_tool456",
  "type": "message",
  "role": "assistant",
  "model": "claude-sonnet-4-5-20250929",
  "content": [
    { "type": "text", "text": "Let me check the weather." },
    {
      "type": "tool_use",
      "id": "toolu_w1",
      "name": "get_weather",
      "input": { "city": "Paris", "unit": "celsius" }
    }
  ],
  "stop_reason": "tool_use",
  "usage": { "input_tokens": 80, "output_tokens": 40 }
}`

const structuredResponse = `{
  "id": "msg_struct789",
  "type": "message",
  "role": "assistant",
  "model": "claude-sonnet-4-5-20250929",
  "content": [
    { "type": "text", "text": "{\"temp\":21.5}" }
  ],
  "stop_reason": "end_turn",
  "usage": { "input_tokens": 30, "output_tokens": 12 }
}`

func newTestModel(t *testing.T, handler http.HandlerFunc, opts ...anthropic.Option) *anthropic.Model {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	base := []anthropic.Option{
		anthropic.WithAPIKey("sk-ant-test"),
		anthropic.WithBaseURL(server.URL + "/v1"),
		anthropic.WithAllowHTTP(),
		anthropic.WithAllowPrivateIPs(),
	}

	return anthropic.New("claude-sonnet-4-5", append(base, opts...)...)
}

func serveJSON(t *testing.T, response, wantPath string, captured *map[string]any) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, wantPath, r.URL.Path)
		assert.Equal(t, "sk-ant-test", r.Header.Get("x-api-key"))
		assert.Equal(t, "2023-06-01", r.Header.Get("anthropic-version"))

		if captured != nil {
			assert.NoError(t, json.NewDecoder(r.Body).Decode(captured))
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}
}

func serveSSE(t *testing.T, events string) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(events))
	}
}

// serveRawSSE streams a literal SSE body.
func serveRawSSE(t *testing.T, body string) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}
}

func as[T any](t *testing.T, v any) T {
	t.Helper()

	out, ok := v.(T)
	require.True(t, ok, "expected %T, got %T (%v)", out, v, v)

	return out
}

func TestGenerateText(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, textResponse, "/v1/messages", &captured))

	resp, err := model.Generate(t.Context(), ai.Request{
		System:    "You are terse.",
		Messages:  []ai.Message{ai.UserText("Capital of France?")},
		MaxTokens: ai.Ptr(100),
	})
	require.NoError(t, err)

	// System is a top-level array of text blocks, not a message.
	system := as[[]any](t, captured["system"])
	require.Len(t, system, 1)
	assert.Equal(t, "You are terse.", as[map[string]any](t, system[0])["text"])
	assert.InDelta(t, 100, as[float64](t, captured["max_tokens"]), 1e-9)

	assert.Equal(t, "msg_abc123", resp.ID)
	assert.Equal(t, ai.ProviderAnthropic, resp.Provider)
	assert.Equal(t, "The capital of France is Paris.", resp.Text())
	assert.Equal(t, ai.FinishStop, resp.FinishReason)
	// input_tokens folds in cache reads.
	assert.Equal(t, 18, resp.Usage.InputTokens)
	assert.Equal(t, 4, resp.Usage.CachedInputTokens)
}

func TestGenerateDefaultMaxTokens(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, textResponse, "/v1/messages", &captured))

	_, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("hi")}})
	require.NoError(t, err)
	// The API requires max_tokens; the adapter supplies its default.
	assert.InDelta(t, 4096, as[float64](t, captured["max_tokens"]), 1e-9)
}

func TestGenerateVisionWireFormat(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, textResponse, "/v1/messages", &captured))

	_, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.User(
			ai.Text("what is this?"),
			ai.ImageData("image/png", []byte{1, 2, 3}),
		)},
	})
	require.NoError(t, err)

	messages := as[[]any](t, captured["messages"])
	content := as[[]any](t, as[map[string]any](t, messages[0])["content"])
	require.Len(t, content, 2)

	image := as[map[string]any](t, content[1])
	assert.Equal(t, "image", image["type"])
	source := as[map[string]any](t, image["source"])
	assert.Equal(t, "base64", source["type"])
	assert.Equal(t, "image/png", source["media_type"])
	assert.Equal(t, "AQID", source["data"])
}

func TestGenerateToolsRoundTrip(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, toolsResponse, "/v1/messages", &captured))

	resp, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("weather in paris?")},
		Tools: []ai.Tool{
			{
				Name:        "get_weather",
				Description: "Get current weather",
				InputSchema: &ai.Schema{Type: "object", Properties: map[string]*ai.Schema{"city": {Type: "string"}}, Required: []string{"city"}},
			},
			{Name: "ping"},
		},
		ToolChoice: ai.ToolChoice{Mode: ai.ToolChoiceAuto},
	})
	require.NoError(t, err)

	tools := as[[]any](t, captured["tools"])
	require.Len(t, tools, 2)
	tool := as[map[string]any](t, tools[0])
	assert.Equal(t, "get_weather", tool["name"])
	params := as[map[string]any](t, tool["input_schema"])
	assert.Contains(t, as[map[string]any](t, params["properties"]), "city")
	assert.Equal(t, []any{"city"}, params["required"])

	noArgs := as[map[string]any](t, tools[1])
	assert.Equal(t, "ping", noArgs["name"])
	noArgsParams := as[map[string]any](t, noArgs["input_schema"])
	assert.Equal(t, "object", noArgsParams["type"])
	assert.Empty(t, as[map[string]any](t, noArgsParams["properties"]))
	assert.Equal(t, map[string]any{"type": "auto"}, captured["tool_choice"])

	assert.Equal(t, ai.FinishToolCalls, resp.FinishReason)
	calls := resp.ToolCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, "toolu_w1", calls[0].ID)
	assert.Equal(t, "get_weather", calls[0].Name)
	assert.JSONEq(t, `{"city":"Paris","unit":"celsius"}`, string(calls[0].Args))
}

func TestGenerateToolResultHistoryWireFormat(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, textResponse, "/v1/messages", &captured))

	_, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{
			ai.UserText("weather?"),
			ai.Assistant(ai.ToolCallPart{ID: "toolu_w1", Name: "get_weather", Args: ai.JSON(`{"city":"Paris"}`)}),
			ai.ToolResultText("toolu_w1", "get_weather", `{"temp":21}`),
		},
	})
	require.NoError(t, err)

	messages := as[[]any](t, captured["messages"])
	require.Len(t, messages, 3)

	assistant := as[map[string]any](t, messages[1])
	assert.Equal(t, "assistant", assistant["role"])
	toolUse := as[map[string]any](t, as[[]any](t, assistant["content"])[0])
	assert.Equal(t, "tool_use", toolUse["type"])
	assert.Equal(t, "toolu_w1", toolUse["id"])

	// Tool results come back as a user message with a tool_result block.
	toolMsg := as[map[string]any](t, messages[2])
	assert.Equal(t, "user", toolMsg["role"])
	result := as[map[string]any](t, as[[]any](t, toolMsg["content"])[0])
	assert.Equal(t, "tool_result", result["type"])
	assert.Equal(t, "toolu_w1", result["tool_use_id"])
}

func TestGenerateStructuredOutputNative(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, structuredResponse, "/v1/messages", &captured))

	resp, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("weather")},
		ResponseFormat: &ai.ResponseFormat{
			Name:   "weather",
			Schema: &ai.Schema{Type: "object", Properties: map[string]*ai.Schema{"temp": {Type: "number"}}, Required: []string{"temp"}},
		},
	})
	require.NoError(t, err)

	outputConfig := as[map[string]any](t, captured["output_config"])
	format := as[map[string]any](t, outputConfig["format"])
	assert.Equal(t, "json_schema", format["type"])
	schema := as[map[string]any](t, format["schema"])
	assert.Equal(t, "object", schema["type"])
	assert.NotContains(t, captured, "tool_choice")

	assert.JSONEq(t, `{"temp":21.5}`, resp.Text())
	assert.Equal(t, ai.FinishStop, resp.FinishReason)
}

func TestThinkingWireFormat(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, textResponse, "/v1/messages", &captured))

	_, err := model.Generate(t.Context(), ai.Request{
		Messages:  []ai.Message{ai.UserText("think")},
		Reasoning: &ai.ReasoningConfig{Effort: ai.ReasoningHigh},
	})
	require.NoError(t, err)

	thinking := as[map[string]any](t, captured["thinking"])
	assert.Equal(t, "enabled", thinking["type"])
	assert.InDelta(t, 16384, as[float64](t, thinking["budget_tokens"]), 1e-9)
}

func TestAdaptiveThinkingAndTypedSamplingWireFormat(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, textResponse, "/v1/messages", &captured))

	_, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("think")},
		TopK:     ai.Ptr(50),
		Stop:     []string{"done"},
		Reasoning: &ai.ReasoningConfig{
			Mode:   ai.ReasoningModeAdaptive,
			Effort: ai.ReasoningMax,
		},
	})
	require.NoError(t, err)

	assert.InDelta(t, 50, as[float64](t, captured["top_k"]), 1e-9)
	assert.Equal(t, []any{"done"}, as[[]any](t, captured["stop_sequences"]))
	thinking := as[map[string]any](t, captured["thinking"])
	assert.Equal(t, "adaptive", thinking["type"])
	assert.NotContains(t, thinking, "budget_tokens")
	outputConfig := as[map[string]any](t, captured["output_config"])
	assert.Equal(t, "max", outputConfig["effort"])
}

func TestNoneThinkingDisablesAndUnmappedEffortFails(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, textResponse, "/v1/messages", &captured))

	_, err := model.Generate(t.Context(), ai.Request{
		Messages:  []ai.Message{ai.UserText("think")},
		Reasoning: &ai.ReasoningConfig{Effort: ai.ReasoningNone},
	})
	require.NoError(t, err)

	thinking := as[map[string]any](t, captured["thinking"])
	assert.Equal(t, "disabled", thinking["type"])

	_, err = model.Generate(t.Context(), ai.Request{
		Messages:  []ai.Message{ai.UserText("think")},
		Reasoning: &ai.ReasoningConfig{Effort: ai.ReasoningXHigh},
	})
	require.ErrorIs(t, err, ai.ErrUnsupported)

	_, err = model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("think")},
		Reasoning: &ai.ReasoningConfig{
			Mode:   ai.ReasoningModeAdaptive,
			Effort: ai.ReasoningMinimal,
		},
	})
	require.ErrorIs(t, err, ai.ErrUnsupported)
}

func TestProviderOptionsRejectOutputConfigWhenUnset(t *testing.T) {
	t.Parallel()

	model := newTestModel(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("unsafe extension reached HTTP")

		_, _ = w.Write([]byte(textResponse))
	})

	_, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("hi")},
		ProviderOptions: map[ai.Provider]any{
			ai.ProviderAnthropic: anthropic.RequestOptions{
				ExtraFields: map[string]any{"output_config": map[string]any{"effort": "high"}},
			},
		},
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "reserved")
}

func TestCacheControlViaProviderOptions(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, textResponse, "/v1/messages", &captured))

	_, err := model.Generate(t.Context(), ai.Request{
		System:   "big reusable prompt",
		Messages: []ai.Message{ai.UserText("hi")},
		ProviderOptions: map[ai.Provider]any{
			ai.ProviderAnthropic: anthropic.RequestOptions{CacheSystem: true},
		},
	})
	require.NoError(t, err)

	system := as[[]any](t, captured["system"])
	cc := as[map[string]any](t, as[map[string]any](t, system[0])["cache_control"])
	assert.Equal(t, "ephemeral", cc["type"])
}

func TestAutomaticCacheControlWithTTL(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, textResponse, "/v1/messages", &captured))

	_, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("hi")},
		ProviderOptions: map[ai.Provider]any{
			ai.ProviderAnthropic: anthropic.RequestOptions{
				AutomaticCache: true,
				CacheTTL:       anthropic.CacheTTL1Hour,
			},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, map[string]any{"type": "ephemeral", "ttl": "1h"}, captured["cache_control"])
}

func TestProviderFileIDWireFormat(t *testing.T) {
	t.Parallel()

	var (
		captured map[string]any
		beta     []string
	)

	model := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		beta = r.Header.Values("anthropic-beta")
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&captured))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(textResponse))
	})

	_, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.User(
		ai.Text("summarize"),
		ai.FileID("report.pdf", "application/pdf", "file_123"),
	)}})
	require.NoError(t, err)

	messages := as[[]any](t, captured["messages"])
	content := as[[]any](t, as[map[string]any](t, messages[0])["content"])
	document := as[map[string]any](t, content[1])
	assert.Equal(t, "document", document["type"])
	assert.Equal(t, map[string]any{"type": "file", "file_id": "file_123"}, document["source"])
	assert.Contains(t, beta, "files-api-2025-04-14")
}

func TestErrorMappingOverloaded(t *testing.T) {
	t.Parallel()

	model := newTestModel(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(529)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
	})

	_, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("hi")}})
	require.ErrorIs(t, err, ai.ErrOverloaded)
	assert.True(t, ai.IsRetryable(err))

	var apiErr *ai.Error
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, "overloaded_error", apiErr.Type)
}

func TestCountTokens(t *testing.T) {
	t.Parallel()

	model := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/messages/count_tokens", r.URL.Path)

		var body map[string]any
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.NotContains(t, body, "max_tokens")
		assert.NotContains(t, body, "stream")

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":42}`))
	})

	n, err := model.CountTokens(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("count me")}})
	require.NoError(t, err)
	assert.Equal(t, 42, n)
}

type weatherOut struct {
	Temp float64 `json:"temp"`
}

// TestGenerateTypedThroughMessages covers Anthropic native structured output
// round-tripping into a typed value.
func TestGenerateTypedThroughMessages(t *testing.T) {
	t.Parallel()

	model := newTestModel(t, serveJSON(t, structuredResponse, "/v1/messages", nil))

	got, resp, err := ai.GenerateTyped[weatherOut](t.Context(), model, ai.Request{
		Messages: []ai.Message{ai.UserText("weather")},
		ResponseFormat: &ai.ResponseFormat{
			Name:   "weather",
			Schema: &ai.Schema{Type: "object", Properties: map[string]*ai.Schema{"temp": {Type: "number"}}, Required: []string{"temp"}},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.InDelta(t, 21.5, got.Temp, 1e-9)
	assert.Equal(t, ai.FinishStop, resp.FinishReason)
}
