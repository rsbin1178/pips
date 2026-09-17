package openrouter_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openrouter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenRouterChat(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer test-openrouter-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"gen_1","model":"openai/gpt-5.2","choices":[{"message":{"role":"assistant","content":"hello from openrouter"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := openrouter.New("openai/gpt-5.2",
		openrouter.WithBaseURL(server.URL),
		openrouter.WithAPIKey("test-openrouter-key"),
		openrouter.WithAllowHTTP(),
		openrouter.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderOpenRouter, model.Provider())
	assert.Equal(t, "openai/gpt-5.2", model.ModelID())

	resp, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("hi")},
	})
	require.NoError(t, err)
	assert.Equal(t, ai.ProviderOpenRouter, resp.Provider)
	assert.Equal(t, "hello from openrouter", resp.Text())
}

func TestOpenRouterAttributionHeaders(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "https://example.com", r.Header.Get("HTTP-Referer"))
		assert.Equal(t, "Pips", r.Header.Get("X-OpenRouter-Title"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"gen_2","model":"openai/gpt-5.2","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := openrouter.New("openai/gpt-5.2",
		openrouter.WithBaseURL(server.URL),
		openrouter.WithAPIKey("test-openrouter-key"),
		openrouter.WithReferer("https://example.com"),
		openrouter.WithAppTitle("Pips"),
		openrouter.WithAllowHTTP(),
		openrouter.WithAllowPrivateIPs(),
	)

	_, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("hi")}})
	require.NoError(t, err)
}

func TestOpenRouterEnvironmentFallback(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "env-openrouter-key")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer env-openrouter-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"gen_3","model":"openai/gpt-5.2","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := openrouter.New("openai/gpt-5.2",
		openrouter.WithBaseURL(server.URL),
		openrouter.WithAllowHTTP(),
		openrouter.WithAllowPrivateIPs(),
	)

	_, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("hi")}})
	require.NoError(t, err)
}
