package qwen_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/qwen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQwenChat(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer test-qwen-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_qwen_1","model":"qwen3.7-max","choices":[{"message":{"role":"assistant","content":"hello from qwen"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := qwen.New("qwen3.7-max",
		qwen.WithBaseURL(server.URL),
		qwen.WithAPIKey("test-qwen-key"),
		qwen.WithAllowHTTP(),
		qwen.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderQwen, model.Provider())
	assert.Equal(t, "qwen3.7-max", model.ModelID())

	resp, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("hi")},
	})
	require.NoError(t, err)
	assert.Equal(t, ai.ProviderQwen, resp.Provider)
	assert.Equal(t, "hello from qwen", resp.Text())
}

func TestQwenEmbedding(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/embeddings", r.URL.Path)
		assert.Equal(t, "Bearer test-qwen-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2,0.3]}],"usage":{"prompt_tokens":5,"total_tokens":5}}`))
	}))
	t.Cleanup(server.Close)

	emb := qwen.NewEmbeddingModel("text-embedding-v4",
		qwen.WithBaseURL(server.URL),
		qwen.WithAPIKey("test-qwen-key"),
		qwen.WithAllowHTTP(),
		qwen.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderQwen, emb.Provider())
	assert.Equal(t, "text-embedding-v4", emb.ModelID())

	resp, err := emb.Embed(t.Context(), ai.EmbeddingRequest{Input: []string{"test"}})
	require.NoError(t, err)
	require.Len(t, resp.Embeddings, 1)
	assert.Equal(t, []float32{0.1, 0.2, 0.3}, resp.Embeddings[0])
}

func TestQwenCapabilityInference(t *testing.T) {
	t.Parallel()

	tests := []struct {
		model string
		want  ai.Capabilities
	}{
		{model: "qwen3.7-max", want: ai.Capabilities{Text: true, Tools: true, Reasoning: true}},
		{model: "qwen-plus", want: ai.Capabilities{Text: true, Tools: true}},
		{model: "qwen3-vl-plus", want: ai.Capabilities{Text: true, Tools: true, Vision: true, Reasoning: true}},
		{model: "qwen-vl-max", want: ai.Capabilities{Text: true, Tools: true, Vision: true}},
		{model: "qwq-32b", want: ai.Capabilities{Text: true, Tools: true, Reasoning: true}},
	}

	for _, test := range tests {
		t.Run(test.model, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, qwen.New(test.model).Capabilities())
		})
	}
}

func TestQwenRegionBaseURLs(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "https://dashscope-intl.aliyuncs.com/compatible-mode/v1", qwen.DefaultBaseURL)
	assert.Equal(t, "https://dashscope.aliyuncs.com/compatible-mode/v1", qwen.DefaultChinaBaseURL)
	assert.Equal(t, "https://dashscope-us.aliyuncs.com/compatible-mode/v1", qwen.DefaultUSBaseURL)
}

func TestQwenEnvironmentFallback(t *testing.T) {
	t.Setenv("DASHSCOPE_API_KEY", "env-qwen-key")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer env-qwen-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_qwen_2","model":"qwen-plus","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := qwen.New("qwen-plus",
		qwen.WithBaseURL(server.URL),
		qwen.WithAllowHTTP(),
		qwen.WithAllowPrivateIPs(),
	)

	_, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("hi")}})
	require.NoError(t, err)
}
