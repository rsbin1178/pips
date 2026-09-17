package together_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/together"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTogetherChat(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer test-together-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat_together_1","model":"meta-llama/Meta-Llama-3.1-8B-Instruct-Turbo","choices":[{"message":{"role":"assistant","content":"hello from together"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := together.New("meta-llama/Meta-Llama-3.1-8B-Instruct-Turbo",
		together.WithBaseURL(server.URL),
		together.WithAPIKey("test-together-key"),
		together.WithAllowHTTP(),
		together.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderTogether, model.Provider())
	assert.Equal(t, "meta-llama/Meta-Llama-3.1-8B-Instruct-Turbo", model.ModelID())

	resp, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("hi")},
	})
	require.NoError(t, err)
	assert.Equal(t, ai.ProviderTogether, resp.Provider)
	assert.Equal(t, "hello from together", resp.Text())
}

func TestTogetherEmbedding(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/embeddings", r.URL.Path)
		assert.Equal(t, "Bearer test-together-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2,0.3]}],"usage":{"prompt_tokens":5,"total_tokens":5}}`))
	}))
	t.Cleanup(server.Close)

	emb := together.NewEmbeddingModel("togethercomputer/m2-bert-80M-8k-retrieval",
		together.WithBaseURL(server.URL),
		together.WithAPIKey("test-together-key"),
		together.WithAllowHTTP(),
		together.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderTogether, emb.Provider())
	assert.Equal(t, "togethercomputer/m2-bert-80M-8k-retrieval", emb.ModelID())

	resp, err := emb.Embed(t.Context(), ai.EmbeddingRequest{Input: []string{"test"}})
	require.NoError(t, err)
	require.Len(t, resp.Embeddings, 1)
	assert.Equal(t, []float32{0.1, 0.2, 0.3}, resp.Embeddings[0])
}

func TestTogetherRerank(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/rerank", r.URL.Path)
		assert.Equal(t, "Bearer test-together-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "rerank_tog_1",
			"results": [
				{"index": 0, "relevance_score": 0.99, "document": "doc1"}
			],
			"usage": {"prompt_tokens": 50, "total_tokens": 50}
		}`))
	}))
	t.Cleanup(server.Close)

	rerank := together.NewRerankModel("Salesforce/Llama-Rank-v1",
		together.WithBaseURL(server.URL),
		together.WithAPIKey("test-together-key"),
		together.WithAllowHTTP(),
		together.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderTogether, rerank.Provider())
	assert.Equal(t, "Salesforce/Llama-Rank-v1", rerank.ModelID())

	resp, err := rerank.Rerank(t.Context(), ai.RerankRequest{
		Query:     "query",
		Documents: []string{"doc1"},
	})
	require.NoError(t, err)
	require.Len(t, resp.Results, 1)
	assert.InDelta(t, 0.99, resp.Results[0].RelevanceScore, 1e-4)
}

func TestTogetherImage(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/images/generations", r.URL.Path)
		assert.Equal(t, "Bearer test-together-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1713833628,"data":[{"b64_json":"cGl4ZWxz"}]}`))
	}))
	t.Cleanup(server.Close)

	img := together.NewImageModel("black-forest-labs/FLUX.1-schnell",
		together.WithBaseURL(server.URL),
		together.WithAPIKey("test-together-key"),
		together.WithAllowHTTP(),
		together.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderTogether, img.Provider())
	assert.Equal(t, "black-forest-labs/FLUX.1-schnell", img.ModelID())

	resp, err := img.GenerateImages(t.Context(), ai.ImageRequest{
		Prompt: "a cyberpunk cityscape at sunset",
	})
	require.NoError(t, err)
	require.Len(t, resp.Images, 1)
	assert.Equal(t, []byte("pixels"), resp.Images[0].Data)
}

func TestTogetherEnvironmentFallback(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "env-together-key")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer env-together-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat_tog_2","model":"m","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := together.New("m",
		together.WithBaseURL(server.URL),
		together.WithAllowHTTP(),
		together.WithAllowPrivateIPs(),
	)

	_, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("hi")}})
	require.NoError(t, err)

	img := together.NewImageModel("black-forest-labs/FLUX.1-schnell")
	assert.Equal(t, ai.ProviderTogether, img.Provider())
}
