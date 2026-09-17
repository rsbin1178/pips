package kimi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/kimi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKimiChat(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer test-kimi-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmpl_kimi_1","model":"kimi-k3","choices":[{"message":{"role":"assistant","content":"hello from kimi"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := kimi.New("kimi-k3",
		kimi.WithBaseURL(server.URL),
		kimi.WithAPIKey("test-kimi-key"),
		kimi.WithAllowHTTP(),
		kimi.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderKimi, model.Provider())
	assert.Equal(t, "kimi-k3", model.ModelID())

	resp, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("hi")},
	})
	require.NoError(t, err)
	assert.Equal(t, ai.ProviderKimi, resp.Provider)
	assert.Equal(t, "hello from kimi", resp.Text())
}

func TestKimiCapabilityInference(t *testing.T) {
	t.Parallel()

	tests := []struct {
		model string
		want  ai.Capabilities
	}{
		{
			model: "moonshot-v1-8k",
			want:  ai.Capabilities{Text: true, Tools: true, StructuredOutput: true, PromptCaching: true},
		},
		{
			model: "moonshot-v1-8k-vision-preview",
			want:  ai.Capabilities{Text: true, Tools: true, StructuredOutput: true, PromptCaching: true, Vision: true},
		},
		{
			model: "kimi-k2.6",
			want:  ai.Capabilities{Text: true, Tools: true, StructuredOutput: true, PromptCaching: true, Vision: true, Reasoning: true},
		},
		{
			model: "kimi-k3",
			want:  ai.Capabilities{Text: true, Tools: true, StructuredOutput: true, PromptCaching: true, Vision: true, Reasoning: true},
		},
	}

	for _, test := range tests {
		t.Run(test.model, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, kimi.New(test.model).Capabilities())
		})
	}
}

func TestKimiChinaBaseURL(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "https://api.moonshot.cn/v1", kimi.DefaultChinaBaseURL)
	assert.Equal(t, "https://api.moonshot.ai/v1", kimi.DefaultBaseURL)
}

func TestKimiEnvironmentFallback(t *testing.T) {
	t.Setenv("MOONSHOT_API_KEY", "env-kimi-key")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer env-kimi-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmpl_kimi_2","model":"kimi-k3","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := kimi.New("kimi-k3",
		kimi.WithBaseURL(server.URL),
		kimi.WithAllowHTTP(),
		kimi.WithAllowPrivateIPs(),
	)

	_, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("hi")}})
	require.NoError(t, err)
}
