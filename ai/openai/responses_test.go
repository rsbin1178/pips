package openai_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newResponsesModel builds a Model pinned to the Responses API surface.
func newResponsesModel(t *testing.T, handler http.HandlerFunc) *openai.Model {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return openai.New("gpt-5",
		openai.WithAPIKey("sk-test"),
		openai.WithBaseURL(server.URL+"/v1"),
		openai.WithAllowHTTP(),
		openai.WithAllowPrivateIPs(),
		openai.WithAPI(openai.APIResponses),
	)
}

func serveResponsesFixture(t *testing.T, name string, captured *map[string]any) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/responses", r.URL.Path)

		if captured != nil {
			assert.NoError(t, json.NewDecoder(r.Body).Decode(captured))
		}

		data, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // fixture path from test constants
		assert.NoError(t, err)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}
}

func TestResponsesGenerateText(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newResponsesModel(t, serveResponsesFixture(t, "responses_text.json", &captured))

	resp, err := model.Generate(t.Context(), ai.Request{
		System:    "You are terse.",
		Messages:  []ai.Message{ai.UserText("Capital of France?")},
		Reasoning: &ai.ReasoningConfig{Effort: ai.ReasoningHigh, IncludeSummary: true},
	})
	require.NoError(t, err)

	// System becomes top-level instructions; messages become typed input items.
	assert.Equal(t, "You are terse.", captured["instructions"])
	input := as[[]any](t, captured["input"])
	require.Len(t, input, 1)
	item := as[map[string]any](t, input[0])
	assert.Equal(t, "message", item["type"])
	assert.Equal(t, "user", item["role"])
	content := as[[]any](t, item["content"])
	assert.Equal(t, "input_text", as[map[string]any](t, content[0])["type"])

	// Reasoning effort + summary requested.
	reasoning := as[map[string]any](t, captured["reasoning"])
	assert.Equal(t, "high", reasoning["effort"])
	assert.Equal(t, "auto", reasoning["summary"])

	// Normalized response: reasoning summary + text, reasoning-token usage.
	assert.Equal(t, "resp_abc123", resp.ID)
	assert.Equal(t, "The capital of France is Paris.", resp.Text())
	assert.Equal(t, "Simple factual question.", resp.Reasoning())
	assert.Equal(t, ai.FinishStop, resp.FinishReason)
	assert.Equal(t, 14, resp.Usage.InputTokens)
	assert.Equal(t, 6, resp.Usage.CachedInputTokens)
	assert.Equal(t, 12, resp.Usage.ReasoningTokens)
}

func TestResponsesVisionAndToolWireFormat(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newResponsesModel(t, serveResponsesFixture(t, "responses_tools.json", &captured))

	resp, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.User(
			ai.Text("what is this?"),
			ai.ImageData("image/png", []byte{1, 2, 3}),
		)},
		Tools: []ai.Tool{{
			Name:        "get_weather",
			InputSchema: &ai.Schema{Type: "object", Properties: map[string]*ai.Schema{"city": {Type: "string"}}, Required: []string{"city"}},
		}},
	})
	require.NoError(t, err)

	// Vision content uses input_image.
	input := as[[]any](t, captured["input"])
	content := as[[]any](t, as[map[string]any](t, input[0])["content"])
	img := as[map[string]any](t, content[1])
	assert.Equal(t, "input_image", img["type"])
	assert.Equal(t, "data:image/png;base64,AQID", img["image_url"])

	// Tools are flat (no nested "function" wrapper).
	tools := as[[]any](t, captured["tools"])
	tool := as[map[string]any](t, tools[0])
	assert.Equal(t, "function", tool["type"])
	assert.Equal(t, "get_weather", tool["name"])

	// function_call output normalizes to tool_calls.
	assert.Equal(t, ai.FinishToolCalls, resp.FinishReason)
	calls := resp.ToolCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, "call_w1", calls[0].ID)
	assert.Equal(t, "get_weather", calls[0].Name)
}

func TestResponsesToolResultHistoryWireFormat(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newResponsesModel(t, serveResponsesFixture(t, "responses_text.json", &captured))

	_, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{
			ai.UserText("weather?"),
			ai.Assistant(ai.ToolCallPart{ID: "call_w1", Name: "get_weather", Args: ai.JSON(`{"city":"Paris"}`)}),
			ai.ToolResultText("call_w1", "get_weather", `{"temp":21}`),
		},
	})
	require.NoError(t, err)

	input := as[[]any](t, captured["input"])
	require.Len(t, input, 3)

	call := as[map[string]any](t, input[1])
	assert.Equal(t, "function_call", call["type"])
	assert.Equal(t, "call_w1", call["call_id"])

	output := as[map[string]any](t, input[2])
	assert.Equal(t, "function_call_output", output["type"])
	assert.Equal(t, "call_w1", output["call_id"])
	assert.Equal(t, `{"temp":21}`, output["output"])
}

func TestResponsesStructuredOutputWireFormat(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newResponsesModel(t, serveResponsesFixture(t, "responses_text.json", &captured))

	_, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("extract")},
		ResponseFormat: &ai.ResponseFormat{
			Name:   "weather",
			Schema: &ai.Schema{Type: "object", Properties: map[string]*ai.Schema{"temp": {Type: "number"}}, Required: []string{"temp"}, AdditionalProperties: false},
			Strict: true,
		},
	})
	require.NoError(t, err)

	text := as[map[string]any](t, captured["text"])
	format := as[map[string]any](t, text["format"])
	assert.Equal(t, "json_schema", format["type"])
	assert.Equal(t, "weather", format["name"])
	assert.Equal(t, true, format["strict"])
}

func TestResponsesStreamText(t *testing.T) {
	t.Parallel()

	model := newResponsesModel(t, serveSSE(t, "responses_stream_text.sse"))

	resp, err := ai.Collect(model.Stream(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("hi")},
	}))
	require.NoError(t, err)

	assert.Equal(t, "resp_s1", resp.ID)
	assert.Equal(t, "Hello!", resp.Text())
	assert.Equal(t, ai.FinishStop, resp.FinishReason)
	assert.Equal(t, 9, resp.Usage.InputTokens)
	assert.Equal(t, 3, resp.Usage.OutputTokens)
}

func TestResponsesStreamToolCall(t *testing.T) {
	t.Parallel()

	model := newResponsesModel(t, serveSSE(t, "responses_stream_tools.sse"))

	resp, err := ai.Collect(model.Stream(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("weather?")},
		Tools:    []ai.Tool{{Name: "get_weather"}},
	}))
	require.NoError(t, err)

	assert.Equal(t, ai.FinishToolCalls, resp.FinishReason)
	calls := resp.ToolCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, "call_a", calls[0].ID)
	assert.Equal(t, "get_weather", calls[0].Name)
	assert.JSONEq(t, `{"city":"Paris"}`, string(calls[0].Args))
}

// TestAPIAutoRouting verifies AC3's routing half: reasoning-family models go
// to Responses, others to Chat Completions, under APIAuto.
func TestAPIAutoRouting(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		model    string
		wantPath string
	}{
		{"gpt-5", "/v1/responses"},
		{"o3-mini", "/v1/responses"},
		{"gpt-4o", "/v1/chat/completions"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			t.Parallel()

			var gotPath string

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path

				w.Header().Set("Content-Type", "application/json")

				if r.URL.Path == "/v1/responses" {
					_, _ = w.Write([]byte(`{"id":"r","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`))
				} else {
					_, _ = w.Write([]byte(`{"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
				}
			}))
			t.Cleanup(server.Close)

			model := openai.New(tc.model,
				openai.WithAPIKey("sk-test"),
				openai.WithBaseURL(server.URL+"/v1"),
				openai.WithAllowHTTP(), openai.WithAllowPrivateIPs(),
			) // APIAuto by default

			_, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("hi")}})
			require.NoError(t, err)
			assert.Equal(t, tc.wantPath, gotPath)
		})
	}
}

// TestResponsesAndChatEquivalentShape verifies AC3: the same portable request
// answered by both surfaces normalizes to the same Response shape.
func TestResponsesAndChatEquivalentShape(t *testing.T) {
	t.Parallel()

	chat := newTestModel(t, serveFixture(t, "chat_text.json", "/v1/chat/completions", nil))
	responses := newResponsesModel(t, serveResponsesFixture(t, "responses_text.json", nil))

	req := ai.Request{Messages: []ai.Message{ai.UserText("Capital of France?")}}

	chatResp, err := chat.Generate(t.Context(), req)
	require.NoError(t, err)
	respResp, err := responses.Generate(t.Context(), req)
	require.NoError(t, err)

	// Same provider, role, finish reason, and equivalent text output.
	assert.Equal(t, chatResp.Provider, respResp.Provider)
	assert.Equal(t, chatResp.Message.Role, respResp.Message.Role)
	assert.Equal(t, chatResp.FinishReason, respResp.FinishReason)
	assert.Equal(t, "The capital of France is Paris.", chatResp.Text())
	assert.Equal(t, "The capital of France is Paris.", respResp.Text())
}
