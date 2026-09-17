package siliconflow_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/siliconflow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSiliconFlowChat(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer test-sf-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat_sf_1","model":"deepseek-ai/DeepSeek-V3.2","choices":[{"message":{"role":"assistant","content":"hello from siliconflow"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := siliconflow.New("deepseek-ai/DeepSeek-V3.2",
		siliconflow.WithBaseURL(server.URL),
		siliconflow.WithAPIKey("test-sf-key"),
		siliconflow.WithAllowHTTP(),
		siliconflow.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderSiliconFlow, model.Provider())
	assert.Equal(t, "deepseek-ai/DeepSeek-V3.2", model.ModelID())

	resp, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("hi")},
	})
	require.NoError(t, err)
	assert.Equal(t, ai.ProviderSiliconFlow, resp.Provider)
	assert.Equal(t, "hello from siliconflow", resp.Text())
}

func TestSiliconFlowEmbedding(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/embeddings", r.URL.Path)
		assert.Equal(t, "Bearer test-sf-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2,0.3]}],"usage":{"prompt_tokens":5,"total_tokens":5}}`))
	}))
	t.Cleanup(server.Close)

	emb := siliconflow.NewEmbeddingModel("BAAI/bge-large-zh-v1.5",
		siliconflow.WithBaseURL(server.URL),
		siliconflow.WithAPIKey("test-sf-key"),
		siliconflow.WithAllowHTTP(),
		siliconflow.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderSiliconFlow, emb.Provider())
	assert.Equal(t, "BAAI/bge-large-zh-v1.5", emb.ModelID())

	resp, err := emb.Embed(t.Context(), ai.EmbeddingRequest{Input: []string{"test"}})
	require.NoError(t, err)
	require.Len(t, resp.Embeddings, 1)
	assert.Equal(t, []float32{0.1, 0.2, 0.3}, resp.Embeddings[0])
}

func TestSiliconFlowRerank(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/rerank", r.URL.Path)
		assert.Equal(t, "Bearer test-sf-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "rerank_sf_1",
			"results": [
				{"index": 0, "relevance_score": 0.99, "document": "doc1"}
			],
			"usage": {"prompt_tokens": 50, "total_tokens": 50}
		}`))
	}))
	t.Cleanup(server.Close)

	rerank := siliconflow.NewRerankModel("",
		siliconflow.WithBaseURL(server.URL),
		siliconflow.WithAPIKey("test-sf-key"),
		siliconflow.WithAllowHTTP(),
		siliconflow.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderSiliconFlow, rerank.Provider())
	assert.Equal(t, siliconflow.DefaultRerankModel, rerank.ModelID())

	resp, err := rerank.Rerank(t.Context(), ai.RerankRequest{
		Query:     "query",
		Documents: []string{"doc1"},
	})
	require.NoError(t, err)
	require.Len(t, resp.Results, 1)
	assert.InDelta(t, 0.99, resp.Results[0].RelevanceScore, 1e-4)
}

func TestSiliconFlowImage(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/images/generations", r.URL.Path)
		assert.Equal(t, "Bearer test-sf-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1713833628,"data":[{"url":"https://example.com/flux.png"}]}`))
	}))
	t.Cleanup(server.Close)

	img := siliconflow.NewImageModel("black-forest-labs/FLUX.1-schnell",
		siliconflow.WithBaseURL(server.URL),
		siliconflow.WithAPIKey("test-sf-key"),
		siliconflow.WithAllowHTTP(),
		siliconflow.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderSiliconFlow, img.Provider())
	assert.Equal(t, "black-forest-labs/FLUX.1-schnell", img.ModelID())

	resp, err := img.GenerateImages(t.Context(), ai.ImageRequest{
		Prompt: "a tranquil mountain lake at dawn",
	})
	require.NoError(t, err)
	require.Len(t, resp.Images, 1)
	assert.Equal(t, "https://example.com/flux.png", resp.Images[0].URL)
}

func TestSiliconFlowEnvironmentFallback(t *testing.T) {
	t.Setenv("SILICONFLOW_API_KEY", "env-sf-key")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer env-sf-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat_sf_2","model":"m","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := siliconflow.New("m",
		siliconflow.WithBaseURL(server.URL),
		siliconflow.WithAllowHTTP(),
		siliconflow.WithAllowPrivateIPs(),
	)

	_, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("hi")}})
	require.NoError(t, err)

	img := siliconflow.NewImageModel("black-forest-labs/FLUX.1-schnell")
	assert.Equal(t, ai.ProviderSiliconFlow, img.Provider())
}
