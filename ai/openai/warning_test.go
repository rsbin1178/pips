package openai_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
	"github.com/rsbin1178/pips/ai/openai/compat"
	"github.com/rsbin1178/pips/ai/zhipu"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStructuredOutputDowngradeWarns covers the compatibility half of the
// warning contract: the request still goes out, but in a weaker encoding than
// asked for, and the caller can tell
// (.trellis/spec/backend/provider-compatibility-policy.md R5).
func TestStructuredOutputDowngradeWarns(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&captured)) {
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(
			`{"id":"chat_1","model":"glm-5.3","choices":[{"message":{"role":"assistant","content":"{}"},"finish_reason":"stop"}]}`,
		))
	}))
	t.Cleanup(server.Close)

	schema, err := ai.ParseSchema([]byte(`{"type":"object","properties":{"answer":{"type":"string"}}}`))
	require.NoError(t, err)

	// The reviewed Zhipu profile accepts only json_object, never a schema.
	model := zhipu.New("glm-5.3",
		zhipu.WithBaseURL(server.URL+"/v1"),
		zhipu.WithAPIKey("key"),
		zhipu.WithAllowHTTP(),
		zhipu.WithAllowPrivateIPs(),
	)
	response, err := model.Generate(t.Context(), ai.Request{
		Messages:       []ai.Message{ai.UserText("answer with json")},
		ResponseFormat: &ai.ResponseFormat{Name: "answer", Schema: schema, Strict: true},
	})
	require.NoError(t, err)

	format := as[map[string]any](t, captured["response_format"])
	assert.Equal(t, "json_object", format["type"])

	require.Len(t, response.Warnings, 1)
	assert.Equal(t, ai.WarningCompatibility, response.Warnings[0].Type)
	assert.Equal(t, "response_format", response.Warnings[0].Feature)
	assert.Contains(t, response.Warnings[0].Details, "json_object")
}

// TestStructuredOutputOmissionWarns covers a protocol that has no
// response_format at all: the field is dropped, and the drop is reported.
func TestStructuredOutputOmissionWarns(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&captured)) {
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(
			`{"id":"chat_1","model":"m","choices":[{"message":{"role":"assistant","content":"{}"},"finish_reason":"stop"}]}`,
		))
	}))
	t.Cleanup(server.Close)

	schema, err := ai.ParseSchema([]byte(`{"type":"object"}`))
	require.NoError(t, err)

	model := openai.New("m",
		openai.WithAPIKey("key"),
		openai.WithBaseURL(server.URL+"/v1"),
		openai.WithAllowHTTP(),
		openai.WithAllowPrivateIPs(),
		openai.WithAPI(openai.APIChatCompletions),
		openai.WithCompatibility(openai.Compatibility{StructuredOutput: openai.StructuredOutputOmit}),
	)

	response, err := model.Generate(t.Context(), ai.Request{
		Messages:       []ai.Message{ai.UserText("answer with json")},
		ResponseFormat: &ai.ResponseFormat{Schema: schema},
	})
	require.NoError(t, err)

	assert.NotContains(t, captured, "response_format")

	require.Len(t, response.Warnings, 1)
	assert.Equal(t, ai.WarningUnsupported, response.Warnings[0].Type)
	assert.Equal(t, "response_format", response.Warnings[0].Feature)
}

// TestStreamCarriesEncodingWarnings proves a streaming caller sees the same
// notices as a buffered one: they ride the terminal event and survive Collect.
func TestStreamCarriesEncodingWarnings(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"chat_s1","model":"m","choices":[{"index":0,"delta":{"content":"ok"}}]}

data: {"id":"chat_s1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`))
	}))
	t.Cleanup(server.Close)

	model := openai.New("m",
		openai.WithAPIKey("key"),
		openai.WithBaseURL(server.URL+"/v1"),
		openai.WithAllowHTTP(),
		openai.WithAllowPrivateIPs(),
		openai.WithAPI(openai.APIChatCompletions),
		openai.WithCompatibility(openai.Compatibility{BuiltinTools: openai.BuiltinToolsStrip}),
	)

	response, err := ai.Collect(model.Stream(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("search the web")},
		Tools:    []ai.Tool{openai.WebSearch()},
	}))
	require.NoError(t, err)

	require.Len(t, response.Warnings, 1)
	assert.Equal(t, ai.WarningUnsupported, response.Warnings[0].Type)
	assert.Equal(t, "web_search", response.Warnings[0].Feature)
	assert.Equal(t, "ok", response.Text())
}

// TestExactEncodingReportsNoWarnings is the negative control: warnings mean
// something, so a request that was sent exactly as written carries none.
func TestExactEncodingReportsNoWarnings(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(
			`{"id":"chat_1","model":"m","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
		))
	}))
	t.Cleanup(server.Close)

	schema, err := ai.ParseSchema([]byte(`{"type":"object"}`))
	require.NoError(t, err)

	model := compat.OpenRouter("m", compatibleOptions(server.URL)...)
	response, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("hi")},
		Tools:    []ai.Tool{{Name: "calc", Description: "calc"}},
	})
	require.NoError(t, err)
	assert.Empty(t, response.Warnings)

	// A schema on a profile that encodes json_schema is also exact.
	response, err = model.Generate(t.Context(), ai.Request{
		Messages:       []ai.Message{ai.UserText("hi")},
		ResponseFormat: &ai.ResponseFormat{Schema: schema},
	})
	require.NoError(t, err)
	assert.Empty(t, response.Warnings)
}
