package xai_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
	"github.com/rsbin1178/pips/ai/xai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestXAIChat(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/responses", r.URL.Path)
		assert.Equal(t, "Bearer test-xai-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_xai_1","model":"grok-2-1212","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello from grok"}]}],"status":"completed"}`))
	}))
	t.Cleanup(server.Close)

	model := xai.New("grok-2-1212",
		xai.WithBaseURL(server.URL),
		xai.WithAPIKey("test-xai-key"),
		xai.WithAllowHTTP(),
		xai.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderXAI, model.Provider())
	assert.Equal(t, "grok-2-1212", model.ModelID())

	resp, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("hi")},
	})
	require.NoError(t, err)
	assert.Equal(t, ai.ProviderXAI, resp.Provider)
	assert.Equal(t, "hello from grok", resp.Text())
}

func TestXAIWithChatCompletions(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer test-xai-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat_xai_1","model":"grok-2-1212","choices":[{"message":{"role":"assistant","content":"hello via chat completions"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := xai.New("grok-2-1212",
		xai.WithBaseURL(server.URL),
		xai.WithAPIKey("test-xai-key"),
		xai.WithAPI(openai.APIChatCompletions),
		xai.WithAllowHTTP(),
		xai.WithAllowPrivateIPs(),
	)

	resp, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("hi")},
	})
	require.NoError(t, err)
	assert.Equal(t, "hello via chat completions", resp.Text())
}

func TestXAIEnvironmentFallback(t *testing.T) {
	t.Setenv("XAI_API_KEY", "env-xai-key")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer env-xai-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_xai_2","model":"grok-2-1212","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"status":"completed"}`))
	}))
	t.Cleanup(server.Close)

	model := xai.New("grok-2-1212",
		xai.WithBaseURL(server.URL),
		xai.WithAllowHTTP(),
		xai.WithAllowPrivateIPs(),
	)

	_, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("hi")}})
	require.NoError(t, err)
}
