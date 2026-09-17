package groq_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/groq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGroqChat(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer test-groq-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat_groq_1","model":"llama-3.3-70b-versatile","choices":[{"message":{"role":"assistant","content":"hello from groq"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := groq.New("llama-3.3-70b-versatile",
		groq.WithBaseURL(server.URL),
		groq.WithAPIKey("test-groq-key"),
		groq.WithAllowHTTP(),
		groq.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderGroq, model.Provider())
	assert.Equal(t, "llama-3.3-70b-versatile", model.ModelID())

	resp, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("hi")},
	})
	require.NoError(t, err)
	assert.Equal(t, ai.ProviderGroq, resp.Provider)
	assert.Equal(t, "hello from groq", resp.Text())
}

func TestGroqReasoningFormat(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/chat/completions", r.URL.Path)

		var body map[string]any

		_ = json.NewDecoder(r.Body).Decode(&body)
		assert.Equal(t, "parsed", body["reasoning_format"])

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat_groq_r1","model":"deepseek-r1-distill-llama-70b","choices":[{"message":{"role":"assistant","content":"reasoning result"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := groq.New("deepseek-r1-distill-llama-70b",
		groq.WithBaseURL(server.URL),
		groq.WithAPIKey("test-groq-key"),
		groq.WithAllowHTTP(),
		groq.WithAllowPrivateIPs(),
	)

	resp, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("solve this")},
		ProviderOptions: map[ai.Provider]any{
			ai.ProviderGroq: groq.RequestOptions(groq.ReasoningFormatParsed),
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "reasoning result", resp.Text())
}

func TestGroqEnvironmentFallback(t *testing.T) {
	t.Setenv("GROQ_API_KEY", "env-groq-key")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer env-groq-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat_groq_2","model":"llama-3.3-70b-versatile","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := groq.New("llama-3.3-70b-versatile",
		groq.WithBaseURL(server.URL),
		groq.WithAllowHTTP(),
		groq.WithAllowPrivateIPs(),
	)

	_, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("hi")}})
	require.NoError(t, err)
}
