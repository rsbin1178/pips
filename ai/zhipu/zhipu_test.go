package zhipu_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/zhipu"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestZhipuChat(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer test-zhipu-key", r.Header.Get("Authorization"))
		assert.Equal(t, "custom-val", r.Header.Get("X-Custom"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat_1","model":"glm-4-plus","choices":[{"message":{"role":"assistant","content":"hello from glm"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := zhipu.New("glm-4-plus",
		zhipu.WithBaseURL(server.URL),
		zhipu.WithAPIKey("test-zhipu-key"),
		zhipu.WithHeader("X-Custom", "custom-val"),
		zhipu.WithAllowHTTP(),
		zhipu.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderZhipu, model.Provider())
	assert.Equal(t, "glm-4-plus", model.ModelID())

	resp, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("hi")},
	})
	require.NoError(t, err)
	assert.Equal(t, ai.ProviderZhipu, resp.Provider)
	assert.Equal(t, "hello from glm", resp.Text())
}

func TestZhipuEmbedding(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/embeddings", r.URL.Path)
		assert.Equal(t, "Bearer test-zhipu-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2,0.3]}],"usage":{"prompt_tokens":5,"total_tokens":5}}`))
	}))
	t.Cleanup(server.Close)

	emb := zhipu.NewEmbeddingModel("embedding-3",
		zhipu.WithBaseURL(server.URL),
		zhipu.WithAPIKey("test-zhipu-key"),
		zhipu.WithAllowHTTP(),
		zhipu.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderZhipu, emb.Provider())
	assert.Equal(t, "embedding-3", emb.ModelID())

	resp, err := emb.Embed(t.Context(), ai.EmbeddingRequest{Input: []string{"test"}})
	require.NoError(t, err)
	require.Len(t, resp.Embeddings, 1)
	assert.Equal(t, []float32{0.1, 0.2, 0.3}, resp.Embeddings[0])
	assert.Equal(t, 5, resp.Usage.InputTokens)
}

func TestZhipuRerank(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/rerank", r.URL.Path)
		assert.Equal(t, "Bearer test-zhipu-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "20241120141244890ab4ee4af84acf",
			"results": [
				{"index": 0, "relevance_score": 0.9986, "document": "docA"}
			],
			"usage": {"prompt_tokens": 72, "total_tokens": 72}
		}`))
	}))
	t.Cleanup(server.Close)

	// Test default model name resolution
	rerank := zhipu.NewRerankModel("",
		zhipu.WithBaseURL(server.URL),
		zhipu.WithAPIKey("test-zhipu-key"),
		zhipu.WithAllowHTTP(),
		zhipu.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderZhipu, rerank.Provider())
	assert.Equal(t, "rerank", rerank.ModelID())
	assert.True(t, rerank.Capabilities().Reranking)

	resp, err := rerank.Rerank(t.Context(), ai.RerankRequest{
		Query:           "query",
		Documents:       []string{"docA"},
		ReturnDocuments: true,
	})
	require.NoError(t, err)
	require.Len(t, resp.Results, 1)
	assert.InDelta(t, 0.9986, resp.Results[0].RelevanceScore, 1e-4)
	assert.Equal(t, "docA", resp.Results[0].Document)
	assert.Equal(t, 72, resp.Usage.InputTokens)
}

func TestZhipuImage(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/images/generations", r.URL.Path)
		assert.Equal(t, "Bearer test-zhipu-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1713833628,"data":[{"url":"https://example.com/cogview.png"}]}`))
	}))
	t.Cleanup(server.Close)

	img := zhipu.NewImageModel("cogview-4",
		zhipu.WithBaseURL(server.URL),
		zhipu.WithAPIKey("test-zhipu-key"),
		zhipu.WithAllowHTTP(),
		zhipu.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderZhipu, img.Provider())
	assert.Equal(t, "cogview-4", img.ModelID())

	resp, err := img.GenerateImages(t.Context(), ai.ImageRequest{
		Prompt: "a golden retriever in autumn leaves",
	})
	require.NoError(t, err)
	require.Len(t, resp.Images, 1)
	assert.Equal(t, "https://example.com/cogview.png", resp.Images[0].URL)
}

func TestZhipuEnvironmentFallback(t *testing.T) {
	t.Setenv("ZHIPU_API_KEY", "env-zhipu-key")

	chat := zhipu.New("glm-4")
	assert.Equal(t, ai.ProviderZhipu, chat.Provider())

	emb := zhipu.NewEmbeddingModel("embedding-3")
	assert.Equal(t, ai.ProviderZhipu, emb.Provider())

	rerank := zhipu.NewRerankModel("rerank")
	assert.Equal(t, ai.ProviderZhipu, rerank.Provider())

	img := zhipu.NewImageModel("cogview-4")
	assert.Equal(t, ai.ProviderZhipu, img.Provider())
}
