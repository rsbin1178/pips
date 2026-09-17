package minimax_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/minimax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMiniMaxOpenAIChat(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer test-minimax-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"mm_1","model":"MiniMax-M3","choices":[{"message":{"role":"assistant","content":"hello from minimax"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := minimax.New("MiniMax-M3",
		minimax.WithBaseURL(server.URL),
		minimax.WithAPIKey("test-minimax-key"),
		minimax.WithAllowHTTP(),
		minimax.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderMiniMax, model.Provider())
	assert.Equal(t, "MiniMax-M3", model.ModelID())

	resp, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("hi")},
	})
	require.NoError(t, err)
	assert.Equal(t, ai.ProviderMiniMax, resp.Provider)
	assert.Equal(t, "hello from minimax", resp.Text())
}

func TestMiniMaxAnthropicMessages(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/messages", r.URL.Path)
		assert.Equal(t, "test-minimax-key", r.Header.Get("x-api-key"))
		// MiniMax rejects x-api-key alone on its Anthropic-compatible route.
		assert.Equal(t, "Bearer test-minimax-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_mm_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello via anthropic wire"}],"model":"MiniMax-M3","stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":10}}`))
	}))
	t.Cleanup(server.Close)

	model := minimax.NewAnthropic("MiniMax-M3",
		minimax.WithBaseURL(server.URL),
		minimax.WithAPIKey("test-minimax-key"),
		minimax.WithAllowHTTP(),
		minimax.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderMiniMax, model.Provider())
	assert.Equal(t, "MiniMax-M3", model.ModelID())

	resp, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("hi")},
	})
	require.NoError(t, err)
	assert.Equal(t, ai.ProviderMiniMax, resp.Provider)
	assert.Equal(t, "hello via anthropic wire", resp.Text())
}

func TestMiniMaxCapabilityInference(t *testing.T) {
	t.Parallel()

	tests := []struct {
		model string
		want  ai.Capabilities
	}{
		{
			model: "MiniMax-M2.1",
			want:  ai.Capabilities{Text: true, Tools: true, Vision: true, Reasoning: true},
		},
		{
			model: "MiniMax-M3",
			want: ai.Capabilities{
				Text: true, Tools: true, Vision: true, VideoInput: true, Reasoning: true,
			},
		},
		{
			model: "abab6.5s-chat",
			want:  ai.Capabilities{Text: true, Tools: true},
		},
	}

	for _, test := range tests {
		t.Run(test.model, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, minimax.New(test.model).Capabilities())
		})
	}
}

func TestMiniMaxRegionBaseURLs(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "https://api.minimax.io/v1", minimax.DefaultBaseURL)
	assert.Equal(t, "https://api.minimaxi.com/v1", minimax.DefaultChinaBaseURL)
	assert.Equal(t, "https://api.minimax.io/anthropic/v1", minimax.DefaultAnthropicBaseURL)
	assert.Equal(t, "https://api.minimaxi.com/anthropic/v1", minimax.DefaultChinaAnthropicBaseURL)
}

func TestMiniMaxEnvironmentFallback(t *testing.T) {
	t.Setenv("MINIMAX_API_KEY", "env-minimax-key")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer env-minimax-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"mm_2","model":"MiniMax-M3","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := minimax.New("MiniMax-M3",
		minimax.WithBaseURL(server.URL),
		minimax.WithAllowHTTP(),
		minimax.WithAllowPrivateIPs(),
	)

	_, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("hi")}})
	require.NoError(t, err)
}
