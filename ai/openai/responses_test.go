package openai_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const responsesTextResponse = `{
  "id": "resp_abc123",
  "object": "response",
  "model": "gpt-5-2025-08-07",
  "status": "completed",
  "output": [
    {
      "type": "reasoning",
      "id": "rs_1",
      "summary": [ { "type": "summary_text", "text": "Simple factual question." } ]
    },
    {
      "type": "message",
      "id": "msg_1",
      "role": "assistant",
      "content": [ { "type": "output_text", "text": "The capital of France is Paris." } ]
    }
  ],
  "usage": {
    "input_tokens": 14,
    "output_tokens": 20,
    "input_tokens_details": { "cached_tokens": 6 },
    "output_tokens_details": { "reasoning_tokens": 12 }
  }
}`

const responsesToolsResponse = `{
  "id": "resp_tool456",
  "object": "response",
  "model": "gpt-5-2025-08-07",
  "status": "completed",
  "output": [
    {
      "type": "function_call",
      "id": "fc_1",
      "call_id": "call_w1",
      "name": "get_weather",
      "arguments": "{\"city\":\"Paris\"}"
    }
  ],
  "usage": { "input_tokens": 40, "output_tokens": 15 }
}`

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

func serveResponsesJSON(t *testing.T, response string, captured *map[string]any) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/responses", r.URL.Path)

		if captured != nil {
			assert.NoError(t, json.NewDecoder(r.Body).Decode(captured))
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}
}

func TestResponsesGenerateText(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newResponsesModel(t, serveResponsesJSON(t, responsesTextResponse, &captured))

	resp, err := model.Generate(t.Context(), ai.Request{
		Messages: ai.Messages{
			ai.SystemText("You are terse."),
			ai.SystemText("Answer in one sentence."),
			ai.UserText("Capital of France?"),
		},
		Reasoning: &ai.ReasoningConfig{Effort: ai.ReasoningHigh, IncludeSummary: true},
	})
	require.NoError(t, err)

	// System becomes top-level instructions; messages become typed input items.
	assert.Equal(t, "You are terse.\nAnswer in one sentence.", captured["instructions"])
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

func TestResponsesTypedGenerationControls(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newResponsesModel(t, serveResponsesJSON(t, responsesTextResponse, &captured))

	_, err := model.Generate(t.Context(), ai.Request{
		Messages:    []ai.Message{ai.UserText("hi")},
		Temperature: ai.Ptr(0.0),
		TopP:        ai.Ptr(0.9),
		MaxTokens:   ai.Ptr(2048),
		LogProbs:    &ai.LogProbsConfig{Enabled: true, Top: 0},
	})
	require.NoError(t, err)

	assert.InDelta(t, 0, as[float64](t, captured["temperature"]), 1e-9)
	assert.InDelta(t, 0.9, as[float64](t, captured["top_p"]), 1e-9)
	assert.InDelta(t, 2048, as[float64](t, captured["max_output_tokens"]), 1e-9)
	assert.InDelta(t, 0, as[float64](t, captured["top_logprobs"]), 1e-9)
}

func TestResponsesDisabledReasoningUsesNoneEffort(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newResponsesModel(t, serveResponsesJSON(t, responsesTextResponse, &captured))

	_, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("hi")},
		Reasoning: &ai.ReasoningConfig{
			Mode:   ai.ReasoningModeDisabled,
			Effort: ai.ReasoningHigh,
		},
	})
	require.NoError(t, err)

	reasoning := as[map[string]any](t, captured["reasoning"])
	assert.Equal(t, "none", reasoning["effort"])
}

func TestResponsesVisionAndToolWireFormat(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newResponsesModel(t, serveResponsesJSON(t, responsesToolsResponse, &captured))

	resp, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.User(
			ai.Text("what is this?"),
			ai.ImageData("image/png", []byte{1, 2, 3}),
		)},
		Tools: []ai.Tool{
			{
				Name:        "get_weather",
				InputSchema: &ai.Schema{Type: "object", Properties: map[string]*ai.Schema{"city": {Type: "string"}}, Required: []string{"city"}},
			},
			{Name: "ping"},
		},
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
	require.Len(t, tools, 2)
	tool := as[map[string]any](t, tools[0])
	assert.Equal(t, "function", tool["type"])
	assert.Equal(t, "get_weather", tool["name"])
	params := as[map[string]any](t, tool["parameters"])
	assert.Contains(t, as[map[string]any](t, params["properties"]), "city")
	assert.Equal(t, []any{"city"}, params["required"])

	noArgs := as[map[string]any](t, tools[1])
	assert.Equal(t, "ping", noArgs["name"])
	noArgsParams := as[map[string]any](t, noArgs["parameters"])
	assert.Equal(t, "object", noArgsParams["type"])
	assert.Empty(t, as[map[string]any](t, noArgsParams["properties"]))

	// function_call output normalizes to tool_calls.
	assert.Equal(t, ai.FinishToolCalls, resp.FinishReason)
	calls := resp.ToolCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, "call_w1", calls[0].ID)
	assert.Equal(t, "get_weather", calls[0].Name)
}

func TestResponsesFileReferencesWireFormat(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newResponsesModel(t, serveResponsesJSON(t, responsesTextResponse, &captured))

	_, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.User(
		ai.FileID("uploaded.pdf", "application/pdf", "file_123"),
		ai.FileURL("remote.pdf", "application/pdf", "https://example.com/remote.pdf"),
		ai.FileData("inline.pdf", "application/pdf", []byte("%PDF")),
	)}})
	require.NoError(t, err)

	input := as[[]any](t, captured["input"])
	content := as[[]any](t, as[map[string]any](t, input[0])["content"])
	require.Len(t, content, 3)
	assert.Equal(t, map[string]any{"type": "input_file", "file_id": "file_123"}, content[0])
	assert.Equal(t, map[string]any{"type": "input_file", "file_url": "https://example.com/remote.pdf"}, content[1])
	assert.Equal(t, map[string]any{
		"type":      "input_file",
		"filename":  "inline.pdf",
		"file_data": "data:application/pdf;base64,JVBERg==",
	}, content[2])
}

func TestResponsesToolResultHistoryWireFormat(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newResponsesModel(t, serveResponsesJSON(t, responsesTextResponse, &captured))

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

func TestResponsesReasoningReplayEmptySummary(t *testing.T) {
	t.Parallel()

	const firstResponse = `{
		"id":"resp_1","model":"gpt-5","status":"completed","output":[
			{"type":"reasoning","id":"rs_empty","summary":[],"encrypted_content":"encrypted-state"},
			{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}
		]
	}`

	firstModel := newResponsesModel(t, serveResponsesJSON(t, firstResponse, nil))
	first, err := firstModel.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("lookup")},
	})
	require.NoError(t, err)

	var captured map[string]any

	replayModel := newResponsesModel(t, serveResponsesJSON(t, responsesTextResponse, &captured))
	_, err = replayModel.Generate(t.Context(), ai.Request{Messages: []ai.Message{
		ai.UserText("lookup"),
		first.Message,
		ai.ToolResultText("call_1", "lookup", "value"),
	}})
	require.NoError(t, err)

	input := as[[]any](t, captured["input"])
	require.Len(t, input, 4)
	replayed := as[map[string]any](t, input[1])
	assert.Equal(t, "reasoning", replayed["type"])
	assert.Equal(t, "rs_empty", replayed["id"])
	assert.Equal(t, []any{}, replayed["summary"])
	assert.Equal(t, "encrypted-state", replayed["encrypted_content"])
}

func TestResponsesStructuredOutputWireFormat(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newResponsesModel(t, serveResponsesJSON(t, responsesTextResponse, &captured))

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

	model := newResponsesModel(t, serveSSE(t, responsesTextStream))

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

	model := newResponsesModel(t, serveSSE(t, responsesToolsStream))

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

func TestResponsesStreamPreservesEncryptedReasoning(t *testing.T) {
	t.Parallel()

	const stream = `event: response.created
data: {"type":"response.created","response":{"id":"resp_r1","model":"gpt-5","status":"in_progress"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}

event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"checking"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"encrypted-state"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"lookup"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{}"}

event: response.function_call_arguments.done
data: {"type":"response.function_call_arguments.done","output_index":1}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_r1","model":"gpt-5","status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}],"usage":{"input_tokens":4,"output_tokens":5}}}

`

	model := newResponsesModel(t, serveSSE(t, stream))
	resp, err := ai.Collect(model.Stream(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("check")}}))
	require.NoError(t, err)
	require.Len(t, resp.Message.Parts, 2)

	reasoning := as[ai.ReasoningPart](t, resp.Message.Parts[0])
	assert.Equal(t, "checking", reasoning.Text)
	assert.NotEmpty(t, reasoning.Signature)

	calls := resp.ToolCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, "call_1", calls[0].ID)
	assert.Equal(t, "lookup", calls[0].Name)
	assert.JSONEq(t, `{}`, string(calls[0].Args))

	var captured map[string]any

	replayModel := newResponsesModel(t, serveResponsesJSON(t, responsesTextResponse, &captured))
	_, err = replayModel.Generate(t.Context(), ai.Request{Messages: []ai.Message{
		ai.UserText("check"),
		resp.Message,
		ai.ToolResultText("call_1", "lookup", "value"),
	}})
	require.NoError(t, err)

	input := as[[]any](t, captured["input"])
	require.Len(t, input, 4)
	replayed := as[map[string]any](t, input[1])
	assert.Equal(t, "reasoning", replayed["type"])
	assert.Equal(t, "rs_1", replayed["id"])
	assert.Equal(t, []any{
		map[string]any{"type": "summary_text", "text": "checking"},
	}, replayed["summary"])
	assert.Equal(t, "encrypted-state", replayed["encrypted_content"])
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

	chat := newTestModel(t, serveJSON(t, chatTextResponse, "/v1/chat/completions", nil))
	responses := newResponsesModel(t, serveResponsesJSON(t, responsesTextResponse, nil))

	req := ai.Request{Messages: []ai.Message{ai.UserText("Capital of France?")}}

	chatResp, err := chat.Generate(t.Context(), req)
	require.NoError(t, err)
	respResp, err := responses.Generate(t.Context(), req)
	require.NoError(t, err)

	// Same provider, concrete message type, finish reason, and equivalent text output.
	assert.Equal(t, chatResp.Provider, respResp.Provider)
	assert.IsType(t, ai.AssistantMessage{}, chatResp.Message)
	assert.IsType(t, ai.AssistantMessage{}, respResp.Message)
	assert.Equal(t, chatResp.FinishReason, respResp.FinishReason)
	assert.Equal(t, "The capital of France is Paris.", chatResp.Text())
	assert.Equal(t, "The capital of France is Paris.", respResp.Text())
}

func TestResponsesFailedStatusSurfacesError(t *testing.T) {
	t.Parallel()

	model := newResponsesModel(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_f","status":"failed","error":{"code":"server_error","message":"model overloaded"},"output":[]}`))
	})

	_, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("hi")}})
	require.Error(t, err)
	require.ErrorIs(t, err, ai.ErrOverloaded)

	var apiErr *ai.Error
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, "server_error", apiErr.Code)
	assert.Equal(t, "model overloaded", apiErr.Message)
}
