package anthropic_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/anthropic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func serveFixture(t *testing.T, name, wantPath string, captured *map[string]any) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, wantPath, r.URL.Path)
		assert.Equal(t, "sk-ant-test", r.Header.Get("x-api-key"))
		assert.Equal(t, "2023-06-01", r.Header.Get("anthropic-version"))

		if captured != nil {
			assert.NoError(t, json.NewDecoder(r.Body).Decode(captured))
		}

		data, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // fixture path from test constants
		assert.NoError(t, err)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}
}

func serveSSE(t *testing.T, name string) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, _ *http.Request) {
		data, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // fixture path from test constants
		assert.NoError(t, err)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(data)
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

	model := newTestModel(t, serveFixture(t, "text.json", "/v1/messages", &captured))

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

	model := newTestModel(t, serveFixture(t, "text.json", "/v1/messages", &captured))

	_, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("hi")}})
	require.NoError(t, err)
	// The API requires max_tokens; the adapter supplies its default.
	assert.InDelta(t, 4096, as[float64](t, captured["max_tokens"]), 1e-9)
}

func TestGenerateVisionWireFormat(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveFixture(t, "text.json", "/v1/messages", &captured))

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

	model := newTestModel(t, serveFixture(t, "tools.json", "/v1/messages", &captured))

	resp, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("weather in paris?")},
		Tools: []ai.Tool{{
			Name:        "get_weather",
			Description: "Get current weather",
			InputSchema: &ai.Schema{Type: "object", Properties: map[string]*ai.Schema{"city": {Type: "string"}}, Required: []string{"city"}},
		}},
		ToolChoice: ai.ToolChoice{Mode: ai.ToolChoiceAuto},
	})
	require.NoError(t, err)

	tools := as[[]any](t, captured["tools"])
	require.Len(t, tools, 1)
	assert.Equal(t, "get_weather", as[map[string]any](t, tools[0])["name"])
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

	model := newTestModel(t, serveFixture(t, "text.json", "/v1/messages", &captured))

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

func TestGenerateStructuredOutputForcesTool(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveFixture(t, "structured.json", "/v1/messages", &captured))

	resp, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("weather")},
		ResponseFormat: &ai.ResponseFormat{
			Name:   "weather",
			Schema: &ai.Schema{Type: "object", Properties: map[string]*ai.Schema{"temp": {Type: "number"}}, Required: []string{"temp"}},
		},
	})
	require.NoError(t, err)

	// Structured output is coerced via a forced tool call.
	tools := as[[]any](t, captured["tools"])
	require.Len(t, tools, 1)
	assert.Equal(t, "weather", as[map[string]any](t, tools[0])["name"])
	choice := as[map[string]any](t, captured["tool_choice"])
	assert.Equal(t, "tool", choice["type"])
	assert.Equal(t, "weather", choice["name"])

	// The tool_use result is unwrapped to text, and the finish reason is
	// normalized to stop (not tool_calls).
	assert.JSONEq(t, `{"temp":21.5}`, resp.Text())
	assert.Equal(t, ai.FinishStop, resp.FinishReason)
}

func TestThinkingWireFormat(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveFixture(t, "text.json", "/v1/messages", &captured))

	_, err := model.Generate(t.Context(), ai.Request{
		Messages:  []ai.Message{ai.UserText("think")},
		Reasoning: &ai.ReasoningConfig{Effort: ai.ReasoningHigh},
	})
	require.NoError(t, err)

	thinking := as[map[string]any](t, captured["thinking"])
	assert.Equal(t, "enabled", thinking["type"])
	assert.InDelta(t, 16384, as[float64](t, thinking["budget_tokens"]), 1e-9)
}

func TestCacheControlViaProviderOptions(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveFixture(t, "text.json", "/v1/messages", &captured))

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
