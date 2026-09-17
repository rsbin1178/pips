package cerebras_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/cerebras"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCerebrasChat(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer test-cerebras-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat_cerebras_1","model":"llama3.1-8b","choices":[{"message":{"role":"assistant","content":"hello from cerebras"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := cerebras.New("llama3.1-8b",
		cerebras.WithBaseURL(server.URL),
		cerebras.WithAPIKey("test-cerebras-key"),
		cerebras.WithAllowHTTP(),
		cerebras.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderCerebras, model.Provider())
	assert.Equal(t, "llama3.1-8b", model.ModelID())

	resp, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("hi")},
	})
	require.NoError(t, err)
	assert.Equal(t, ai.ProviderCerebras, resp.Provider)
	assert.Equal(t, "hello from cerebras", resp.Text())
}

func TestCerebrasEnvironmentFallback(t *testing.T) {
	t.Setenv("CEREBRAS_API_KEY", "env-cerebras-key")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer env-cerebras-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat_c_2","model":"llama3.1-8b","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := cerebras.New("llama3.1-8b",
		cerebras.WithBaseURL(server.URL),
		cerebras.WithAllowHTTP(),
		cerebras.WithAllowPrivateIPs(),
	)

	_, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("hi")}})
	require.NoError(t, err)
}
