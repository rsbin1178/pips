package deepseek_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/deepseek"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeepSeekOpenAIFormat(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer test-deepseek-key", r.Header.Get("Authorization"))
		assert.Equal(t, "custom-header-value", r.Header.Get("X-Custom"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat_ds_1","model":"deepseek-flash","choices":[{"message":{"role":"assistant","content":"hello from deepseek-flash"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := deepseek.New("deepseek-flash",
		deepseek.WithBaseURL(server.URL),
		deepseek.WithAPIKey("test-deepseek-key"),
		deepseek.WithHeader("X-Custom", "custom-header-value"),
		deepseek.WithAllowHTTP(),
		deepseek.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderDeepSeek, model.Provider())
	assert.Equal(t, "deepseek-flash", model.ModelID())

	resp, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("hi")},
	})
	require.NoError(t, err)
	assert.Equal(t, ai.ProviderDeepSeek, resp.Provider)
	assert.Equal(t, "hello from deepseek-flash", resp.Text())
}

func TestDeepSeekAnthropicFormat(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/messages", r.URL.Path)
		assert.Equal(t, "test-deepseek-key", r.Header.Get("x-api-key"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_ds_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello from deepseek via anthropic format"}],"model":"deepseek-v4-pro","stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":10}}`))
	}))
	t.Cleanup(server.Close)

	model := deepseek.NewAnthropic("deepseek-v4-pro",
		deepseek.WithBaseURL(server.URL),
		deepseek.WithAPIKey("test-deepseek-key"),
		deepseek.WithAllowHTTP(),
		deepseek.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderDeepSeek, model.Provider())
	assert.Equal(t, "deepseek-v4-pro", model.ModelID())

	resp, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("hi")},
	})
	require.NoError(t, err)
	assert.Equal(t, ai.ProviderDeepSeek, resp.Provider)
	assert.Equal(t, "hello from deepseek via anthropic format", resp.Text())
}

func TestDeepSeekEnvironmentFallback(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "env-deepseek-key")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer env-deepseek-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat_ds_2","model":"deepseek-flash","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := deepseek.New("deepseek-flash",
		deepseek.WithBaseURL(server.URL),
		deepseek.WithAllowHTTP(),
		deepseek.WithAllowPrivateIPs(),
	)

	_, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("hi")}})
	require.NoError(t, err)
}
