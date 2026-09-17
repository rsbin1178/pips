package qwen_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/qwen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQwenRerank(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/services/rerank/text-rerank/text-rerank", r.URL.Path)
		assert.Equal(t, "Bearer test-qwen-key", r.Header.Get("Authorization"))

		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.NoError(t, json.Unmarshal(body, &captured))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"output": {"results": [
				{"index": 1, "relevance_score": 0.93, "document": {"text": "doc one"}},
				{"index": 0, "relevance_score": 0.31}
			]},
			"usage": {"total_tokens": 79},
			"request_id": "req-1"
		}`))
	}))
	t.Cleanup(server.Close)

	model := qwen.NewRerankModel("qwen3-rerank",
		qwen.WithBaseURL(server.URL),
		qwen.WithAPIKey("test-qwen-key"),
		qwen.WithAllowHTTP(),
		qwen.WithAllowPrivateIPs(),
	)

	topN := 2
	resp, err := model.Rerank(t.Context(), ai.RerankRequest{
		Query:           "what is a rerank model",
		Documents:       []string{"doc zero", "doc one"},
		TopN:            &topN,
		ReturnDocuments: true,
		ProviderOptions: map[ai.Provider]any{
			ai.ProviderQwen: qwen.RerankOptions{Instruct: "Retrieve relevant passages."},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, ai.ProviderQwen, model.Provider())
	assert.Equal(t, "qwen3-rerank", model.ModelID())

	require.Len(t, resp.Results, 2)
	assert.Equal(t, 1, resp.Results[0].Index)
	assert.InDelta(t, 0.93, resp.Results[0].RelevanceScore, 0.0001)
	assert.Equal(t, "doc one", resp.Results[0].Document)
	assert.Equal(t, 0, resp.Results[1].Index)
	assert.Equal(t, "doc zero", resp.Results[1].Document, "requested documents are backfilled")
	assert.Equal(t, 79, resp.Usage.InputTokens)

	// The portable request maps onto DashScope's nested input/parameters shape.
	assert.Equal(t, "qwen3-rerank", captured["model"])
	input, ok := captured["input"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "what is a rerank model", input["query"])
	assert.Len(t, input["documents"], 2)

	parameters, ok := captured["parameters"].(map[string]any)
	require.True(t, ok)
	assert.InDelta(t, 2, parameters["top_n"], 0.0001)
	assert.Equal(t, true, parameters["return_documents"])
	assert.Equal(t, "Retrieve relevant passages.", parameters["instruct"])
}

func TestQwenRerankRejectsInvalidRequests(t *testing.T) {
	t.Parallel()

	model := qwen.NewRerankModel("qwen3-rerank", qwen.WithAPIKey("test"))
	topN := 0

	tests := []struct {
		name string
		req  ai.RerankRequest
	}{
		{name: "empty query", req: ai.RerankRequest{Documents: []string{"a"}}},
		{name: "no documents", req: ai.RerankRequest{Query: "q"}},
		{name: "zero top_n", req: ai.RerankRequest{Query: "q", Documents: []string{"a"}, TopN: &topN}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := model.Rerank(t.Context(), test.req)
			require.ErrorIs(t, err, ai.ErrInvalidRequest)
		})
	}
}

func TestQwenRerankMapsBodyErrorCode(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"Throttling.RateQuota","message":"Requests rate limit exceeded.","request_id":"req-2"}`))
	}))
	t.Cleanup(server.Close)

	model := qwen.NewRerankModel("qwen3-rerank",
		qwen.WithBaseURL(server.URL),
		qwen.WithAPIKey("test-qwen-key"),
		qwen.WithAllowHTTP(),
		qwen.WithAllowPrivateIPs(),
	)

	_, err := model.Rerank(t.Context(), ai.RerankRequest{
		Query: "q", Documents: []string{"a"},
	})
	require.ErrorIs(t, err, ai.ErrRateLimited)
}
