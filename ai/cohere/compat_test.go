package cohere_test

import (
	"net/http"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/cohere"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSiliconFlowRerank(t *testing.T) {
	t.Parallel()

	base := localServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/rerank", r.URL.Path)
		assert.Equal(t, "Bearer sf-test-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"results": [
				{"index": 0, "relevance_score": 0.99, "document": "apple"}
			],
			"usage": {"total_tokens": 10}
		}`))
	})

	model := cohere.SiliconFlowRerank("BAAI/bge-reranker-v2-m3",
		cohere.WithAPIKey("sf-test-key"),
		cohere.WithBaseURL(base),
		cohere.WithAllowHTTP(),
		cohere.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderSiliconFlow, model.Provider())
	assert.Equal(t, "BAAI/bge-reranker-v2-m3", model.ModelID())
	assert.True(t, model.Capabilities().Reranking)

	resp, err := model.Rerank(t.Context(), ai.RerankRequest{
		Query:     "fruits",
		Documents: []string{"apple"},
	})
	require.NoError(t, err)
	require.Len(t, resp.Results, 1)
	assert.InDelta(t, 0.99, resp.Results[0].RelevanceScore, 1e-4)
	assert.Equal(t, "apple", resp.Results[0].Document)
	assert.Equal(t, 10, resp.Usage.InputTokens)
}

func TestJinaRerank(t *testing.T) {
	t.Parallel()

	base := localServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/rerank", r.URL.Path)
		assert.Equal(t, "Bearer jina-test-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"results": [
				{"index": 0, "relevance_score": 0.98}
			]
		}`))
	})

	model := cohere.JinaRerank("jina-reranker-v3.5",
		cohere.WithAPIKey("jina-test-key"),
		cohere.WithBaseURL(base),
		cohere.WithAllowHTTP(),
		cohere.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderJina, model.Provider())
	assert.Equal(t, "jina-reranker-v3.5", model.ModelID())

	resp, err := model.Rerank(t.Context(), ai.RerankRequest{
		Query:     "test",
		Documents: []string{"doc 1"},
	})
	require.NoError(t, err)
	require.Len(t, resp.Results, 1)
	assert.InDelta(t, 0.98, resp.Results[0].RelevanceScore, 1e-4)
}

func TestTogetherRerank(t *testing.T) {
	t.Parallel()

	base := localServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/rerank", r.URL.Path)
		assert.Equal(t, "Bearer together-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"results": [
				{"index": 0, "relevance_score": 0.92}
			]
		}`))
	})

	model := cohere.TogetherRerank("Salesforce/Llama-Rank-v1",
		cohere.WithAPIKey("together-key"),
		cohere.WithBaseURL(base),
		cohere.WithAllowHTTP(),
		cohere.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderTogether, model.Provider())
	assert.Equal(t, "Salesforce/Llama-Rank-v1", model.ModelID())

	resp, err := model.Rerank(t.Context(), ai.RerankRequest{
		Query:     "query",
		Documents: []string{"doc"},
	})
	require.NoError(t, err)
	require.Len(t, resp.Results, 1)
	assert.InDelta(t, 0.92, resp.Results[0].RelevanceScore, 1e-4)
}

func TestPresetEnvironmentFallback(t *testing.T) {
	t.Setenv("SILICONFLOW_API_KEY", "env-sf-key")
	t.Setenv("JINA_API_KEY", "env-jina-key")
	t.Setenv("TOGETHER_API_KEY", "env-together-key")

	sf := cohere.SiliconFlowRerank("model-1")
	assert.Equal(t, ai.ProviderSiliconFlow, sf.Provider())

	jina := cohere.JinaRerank("model-2")
	assert.Equal(t, ai.ProviderJina, jina.Provider())

	together := cohere.TogetherRerank("model-3")
	assert.Equal(t, ai.ProviderTogether, together.Provider())
}
