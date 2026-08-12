package gemini_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/gemini"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const textResponse = `{
  "candidates": [
    {
      "content": {
        "role": "model",
        "parts": [ { "text": "The capital of France is Paris." } ]
      },
      "finishReason": "STOP",
      "index": 0
    }
  ],
  "usageMetadata": {
    "promptTokenCount": 12,
    "candidatesTokenCount": 8,
    "totalTokenCount": 20,
    "cachedContentTokenCount": 3
  },
  "modelVersion": "gemini-2.5-flash",
  "responseId": "resp-abc123"
}`

const toolsResponse = `{
  "candidates": [
    {
      "content": {
        "role": "model",
        "parts": [
          {
            "functionCall": {
              "name": "get_weather",
              "args": { "city": "Paris", "unit": "celsius" }
            }
          }
        ]
      },
      "finishReason": "STOP",
      "index": 0
    }
  ],
  "usageMetadata": {
    "promptTokenCount": 40,
    "candidatesTokenCount": 15,
    "totalTokenCount": 55
  },
  "modelVersion": "gemini-2.5-flash",
  "responseId": "resp-tool456"
}`

func newTestModel(t *testing.T, handler http.HandlerFunc, opts ...gemini.Option) *gemini.Model {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	base := []gemini.Option{
		gemini.WithAPIKey("gm-test"),
		gemini.WithBaseURL(server.URL + "/v1beta"),
		gemini.WithAllowHTTP(),
		gemini.WithAllowPrivateIPs(),
	}

	return gemini.New("gemini-2.5-flash", append(base, opts...)...)
}

func serveJSON(t *testing.T, response, wantPath string, captured *map[string]any) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, wantPath, r.URL.Path)
		assert.Equal(t, "gm-test", r.Header.Get("x-goog-api-key"))

		if captured != nil {
			assert.NoError(t, json.NewDecoder(r.Body).Decode(captured))
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}
}

func serveSSE(t *testing.T, events string) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "sse", r.URL.Query().Get("alt"))

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(events))
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

	model := newTestModel(t, serveJSON(t, textResponse, "/v1beta/models/gemini-2.5-flash:generateContent", &captured))

	resp, err := model.Generate(t.Context(), ai.Request{
		Messages: ai.Messages{
			ai.SystemText("You are terse."),
			ai.SystemText("Answer in one sentence."),
			ai.UserText("Capital of France?"),
		},
	})
	require.NoError(t, err)

	// systemInstruction is a top-level field, not a message.
	system := as[map[string]any](t, captured["systemInstruction"])
	sysParts := as[[]any](t, system["parts"])
	require.Len(t, sysParts, 2)
	assert.Equal(t, "You are terse.", as[map[string]any](t, sysParts[0])["text"])
	assert.Equal(t, "Answer in one sentence.", as[map[string]any](t, sysParts[1])["text"])

	// Contents carry role "user".
	contents := as[[]any](t, captured["contents"])
	require.Len(t, contents, 1)
	assert.Equal(t, "user", as[map[string]any](t, contents[0])["role"])

	assert.Equal(t, "resp-abc123", resp.ID)
	assert.Equal(t, ai.ProviderGemini, resp.Provider)
	assert.Equal(t, "The capital of France is Paris.", resp.Text())
	assert.Equal(t, ai.FinishStop, resp.FinishReason)
	assert.Equal(t, 12, resp.Usage.InputTokens)
	assert.Equal(t, 3, resp.Usage.CachedInputTokens)
}

func TestGenerateVisionWireFormat(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, textResponse, "/v1beta/models/gemini-2.5-flash:generateContent", &captured))

	_, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.User(
			ai.Text("what is this?"),
			ai.ImageData("image/png", []byte{1, 2, 3}),
		)},
	})
	require.NoError(t, err)

	contents := as[[]any](t, captured["contents"])
	parts := as[[]any](t, as[map[string]any](t, contents[0])["parts"])
	require.Len(t, parts, 2)

	inline := as[map[string]any](t, as[map[string]any](t, parts[1])["inlineData"])
	assert.Equal(t, "image/png", inline["mimeType"])
	assert.Equal(t, "AQID", inline["data"])
}

func TestCachedContentAndMediaURIWireFormat(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, textResponse, "/v1beta/models/gemini-2.5-flash:generateContent", &captured))

	_, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.User(
			ai.Text("transcribe"),
			ai.FileURL("audio.mp3", "audio/mpeg", "https://generativelanguage.googleapis.com/v1beta/files/audio-1"),
		)},
		ProviderOptions: map[ai.Provider]any{
			ai.ProviderGemini: gemini.RequestOptions{CachedContent: "cachedContents/cache-1"},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, "cachedContents/cache-1", captured["cachedContent"])
	contents := as[[]any](t, captured["contents"])
	parts := as[[]any](t, as[map[string]any](t, contents[0])["parts"])
	fileData := as[map[string]any](t, as[map[string]any](t, parts[1])["fileData"])
	assert.Equal(t, "audio/mpeg", fileData["mimeType"])
	assert.Equal(t, "https://generativelanguage.googleapis.com/v1beta/files/audio-1", fileData["fileUri"])
}

func TestProviderFileIDIsRejected(t *testing.T) {
	t.Parallel()

	model := gemini.New("gemini-2.5-flash", gemini.WithAPIKey("key"))

	_, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.User(
		ai.FileID("audio.mp3", "audio/mpeg", "file_123"),
	)}})
	require.ErrorIs(t, err, ai.ErrUnsupported)
}

func TestCapabilities(t *testing.T) {
	t.Parallel()

	caps := gemini.New("gemini-2.5-flash", gemini.WithAPIKey("key")).Capabilities()
	assert.True(t, caps.Documents)
	assert.True(t, caps.AudioInput)
	assert.True(t, caps.VideoInput)
	assert.True(t, caps.PromptCaching)
	assert.True(t, caps.TokenCounting)
}

func TestGenerateToolsSynthesizesID(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, toolsResponse, "/v1beta/models/gemini-2.5-flash:generateContent", &captured))

	resp, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("weather in paris?")},
		Tools: []ai.Tool{
			{
				Name:        "get_weather",
				InputSchema: &ai.Schema{Type: "object", Properties: map[string]*ai.Schema{"city": {Type: "string"}}, Required: []string{"city"}},
			},
			{Name: "ping"},
		},
		ToolChoice: ai.ToolChoice{Mode: ai.ToolChoiceAuto},
	})
	require.NoError(t, err)

	// functionDeclarations wire shape.
	tools := as[[]any](t, captured["tools"])
	decls := as[[]any](t, as[map[string]any](t, tools[0])["functionDeclarations"])
	require.Len(t, decls, 2)
	decl := as[map[string]any](t, decls[0])
	assert.Equal(t, "get_weather", decl["name"])
	params := as[map[string]any](t, decl["parameters"])
	assert.Contains(t, as[map[string]any](t, params["properties"]), "city")
	assert.Equal(t, []any{"city"}, params["required"])

	noArgs := as[map[string]any](t, decls[1])
	assert.Equal(t, "ping", noArgs["name"])
	noArgsParams := as[map[string]any](t, noArgs["parameters"])
	assert.Equal(t, "object", noArgsParams["type"])
	assert.Empty(t, as[map[string]any](t, noArgsParams["properties"]))

	// The response's STOP is normalized to tool_calls, and the id is
	// synthesized since the wire supplied none.
	assert.Equal(t, ai.FinishToolCalls, resp.FinishReason)
	calls := resp.ToolCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, "get_weather", calls[0].Name)
	assert.NotEmpty(t, calls[0].ID)
	assert.JSONEq(t, `{"city":"Paris","unit":"celsius"}`, string(calls[0].Args))
}

func TestToolResultRoundTripUsesSynthesizedID(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, textResponse, "/v1beta/models/gemini-2.5-flash:generateContent", &captured))

	// A synthesized id (no embedded real id) should produce a functionResponse
	// matched by name, with no id leaking onto the wire.
	_, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{
			ai.UserText("weather?"),
			ai.Assistant(ai.ToolCallPart{ID: "call_0", Name: "get_weather", Args: ai.JSON(`{"city":"Paris"}`)}),
			ai.ToolResultText("call_0", "get_weather", `{"temp":21}`),
		},
	})
	require.NoError(t, err)

	contents := as[[]any](t, captured["contents"])
	require.Len(t, contents, 3)

	toolMsg := as[map[string]any](t, contents[2])
	assert.Equal(t, "user", toolMsg["role"])
	fr := as[map[string]any](t, as[map[string]any](t, as[[]any](t, toolMsg["parts"])[0])["functionResponse"])
	assert.Equal(t, "get_weather", fr["name"])
	assert.NotContains(t, fr, "id") // synthesized id is not sent back
	resp := as[map[string]any](t, fr["response"])
	assert.InDelta(t, 21, as[float64](t, resp["temp"]), 1e-9)
}

func TestGeminiThreeToolIDRoundTrip(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&captured))
		w.Header().Set("Content-Type", "application/json")
		// Gemini-3-style: functionCall carries a real id and thoughtSignature.
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"fc-real-1","name":"get_weather","args":{"city":"Paris"}},"thoughtSignature":"sig-1"}]},"finishReason":"STOP","index":0}],"modelVersion":"gemini-3-flash","responseId":"r1"}`))
	})

	resp, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("weather?")}})
	require.NoError(t, err)

	call := resp.ToolCalls()[0]

	// Feed the exact call id back as a tool result; the real id and signature
	// must reappear on the wire.
	_, err = model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{
			ai.UserText("weather?"),
			ai.Assistant(call),
			ai.ToolResultText(call.ID, "get_weather", `{"temp":21}`),
		},
	})
	require.NoError(t, err)

	contents := as[[]any](t, captured["contents"])
	modelTurn := as[map[string]any](t, contents[1])
	callPart := as[map[string]any](t, as[[]any](t, modelTurn["parts"])[0])
	assert.Equal(t, "sig-1", callPart["thoughtSignature"])
	fc := as[map[string]any](t, callPart["functionCall"])
	assert.Equal(t, "fc-real-1", fc["id"])

	toolTurn := as[map[string]any](t, contents[2])
	fr := as[map[string]any](t, as[map[string]any](t, as[[]any](t, toolTurn["parts"])[0])["functionResponse"])
	assert.Equal(t, "fc-real-1", fr["id"])
}

func TestStructuredOutputWireFormat(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, textResponse, "/v1beta/models/gemini-2.5-flash:generateContent", &captured))

	_, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("extract")},
		ResponseFormat: &ai.ResponseFormat{
			Schema: &ai.Schema{Type: "object", Properties: map[string]*ai.Schema{"temp": {Type: "number"}}, Required: []string{"temp"}},
		},
	})
	require.NoError(t, err)

	gc := as[map[string]any](t, captured["generationConfig"])
	assert.Equal(t, "application/json", gc["responseMimeType"])
	assert.Contains(t, gc, "responseSchema")
}

func TestThinkingWireFormat(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, textResponse, "/v1beta/models/gemini-2.5-flash:generateContent", &captured))

	_, err := model.Generate(t.Context(), ai.Request{
		Messages:  []ai.Message{ai.UserText("think")},
		Reasoning: &ai.ReasoningConfig{Effort: ai.ReasoningLow, IncludeSummary: true},
	})
	require.NoError(t, err)

	gc := as[map[string]any](t, captured["generationConfig"])
	tc := as[map[string]any](t, gc["thinkingConfig"])
	assert.Equal(t, "LOW", tc["thinkingLevel"])
	assert.NotContains(t, tc, "thinkingBudget")
	assert.Equal(t, true, tc["includeThoughts"])
}

func TestTypedGenerationControlsAndThinkingBudgetWireFormat(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, textResponse, "/v1beta/models/gemini-2.5-flash:generateContent", &captured))

	_, err := model.Generate(t.Context(), ai.Request{
		Messages:         []ai.Message{ai.UserText("think")},
		TopK:             ai.Ptr(32),
		Seed:             ai.Ptr(int64(0)),
		FrequencyPenalty: ai.Ptr(0.2),
		PresencePenalty:  ai.Ptr(-0.1),
		LogProbs:         &ai.LogProbsConfig{Enabled: true, Top: 4},
		Reasoning: &ai.ReasoningConfig{
			BudgetTokens:   4096,
			IncludeSummary: true,
		},
	})
	require.NoError(t, err)

	config := as[map[string]any](t, captured["generationConfig"])
	assert.InDelta(t, 32, as[float64](t, config["topK"]), 1e-9)
	assert.InDelta(t, 0, as[float64](t, config["seed"]), 1e-9)
	assert.InDelta(t, 0.2, as[float64](t, config["frequencyPenalty"]), 1e-9)
	assert.InDelta(t, -0.1, as[float64](t, config["presencePenalty"]), 1e-9)
	assert.Equal(t, true, config["responseLogprobs"])
	assert.InDelta(t, 4, as[float64](t, config["logprobs"]), 1e-9)
	thinking := as[map[string]any](t, config["thinkingConfig"])
	assert.InDelta(t, 4096, as[float64](t, thinking["thinkingBudget"]), 1e-9)
	assert.NotContains(t, thinking, "thinkingLevel")
	assert.Equal(t, true, thinking["includeThoughts"])
}

func TestNoneThinkingDisablesAndAdaptiveFails(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, serveJSON(t, textResponse, "/v1beta/models/gemini-2.5-flash:generateContent", &captured))

	_, err := model.Generate(t.Context(), ai.Request{
		Messages:  []ai.Message{ai.UserText("think")},
		Reasoning: &ai.ReasoningConfig{Effort: ai.ReasoningNone},
	})
	require.NoError(t, err)
	config := as[map[string]any](t, captured["generationConfig"])
	thinking := as[map[string]any](t, config["thinkingConfig"])
	assert.InDelta(t, 0, as[float64](t, thinking["thinkingBudget"]), 1e-9)

	_, err = model.Generate(t.Context(), ai.Request{
		Messages:  []ai.Message{ai.UserText("think")},
		Reasoning: &ai.ReasoningConfig{Mode: ai.ReasoningModeAdaptive},
	})
	require.ErrorIs(t, err, ai.ErrUnsupported)

	_, err = model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("think")},
		Seed:     ai.Ptr(int64(1 << 31)),
	})
	require.ErrorIs(t, err, ai.ErrInvalidRequest)

	_, err = model.Generate(t.Context(), ai.Request{
		Messages:  []ai.Message{ai.UserText("think")},
		Reasoning: &ai.ReasoningConfig{Effort: ai.ReasoningXHigh},
	})
	require.ErrorIs(t, err, ai.ErrUnsupported)
}

func TestProviderOptionsRejectResponseModalitiesWhenUnset(t *testing.T) {
	t.Parallel()

	model := newTestModel(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("unsafe extension reached HTTP")

		_, _ = w.Write([]byte(textResponse))
	})

	_, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("hi")},
		ProviderOptions: map[ai.Provider]any{
			ai.ProviderGemini: gemini.RequestOptions{
				ExtraFields: map[string]any{
					"generationConfig": map[string]any{"responseModalities": []any{"IMAGE"}},
				},
			},
		},
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "reserved")
}

func TestErrorMapping(t *testing.T) {
	t.Parallel()

	model := newTestModel(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429,"message":"Resource exhausted","status":"RESOURCE_EXHAUSTED"}}`))
	})

	_, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("hi")}})
	require.ErrorIs(t, err, ai.ErrRateLimited)

	var apiErr *ai.Error
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, "RESOURCE_EXHAUSTED", apiErr.Type)
	assert.Equal(t, "Resource exhausted", apiErr.Message)
}

func TestCountTokens(t *testing.T) {
	t.Parallel()

	model := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1beta/models/gemini-2.5-flash:countTokens", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"totalTokens":33}`))
	})

	n, err := model.CountTokens(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("count me")}})
	require.NoError(t, err)
	assert.Equal(t, 33, n)
}

type weatherOut struct {
	Temp float64 `json:"temp"`
}

// TestGenerateTypedThroughGenerateContent covers AC6 for Gemini: responseJson
// output decodes into a typed value with the derived schema on the wire.
func TestGenerateTypedThroughGenerateContent(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&captured))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"{\"temp\":21.5}"}]},"finishReason":"STOP"}],"responseId":"r9"}`))
	})

	got, resp, err := ai.GenerateTyped[weatherOut](t.Context(), model, ai.Request{
		Messages: []ai.Message{ai.UserText("weather")},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.InDelta(t, 21.5, got.Temp, 1e-9)

	gc := as[map[string]any](t, captured["generationConfig"])
	assert.Equal(t, "application/json", gc["responseMimeType"])
	assert.Contains(t, gc, "responseSchema")
}
