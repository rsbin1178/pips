package cohere_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/cohere"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func localServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return server.URL + "/v1"
}

func decodeBody(r *http.Request, v any) error {
	defer r.Body.Close() //nolint:errcheck // test helper

	return json.NewDecoder(r.Body).Decode(v)
}

func as[T any](t *testing.T, value any) T {
	t.Helper()

	result, ok := value.(T)
	require.True(t, ok, "expected %T, got %T", result, value)

	return result
}

func TestRerankSuccess(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	base := localServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/rerank", r.URL.Path)
		assert.Equal(t, "Bearer sk-cohere-test", r.Header.Get("Authorization"))
		assert.NoError(t, decodeBody(r, &captured))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "rerank-test-id",
			"results": [
				{
					"index": 1,
					"relevance_score": 0.985,
					"document": {"text": "Paris is the capital of France."}
				},
				{
					"index": 0,
					"relevance_score": 0.124,
					"document": {"text": "London is the capital of England."}
				}
			],
			"meta": {
				"tokens": {
					"input_tokens": 25,
					"output_tokens": 0
				}
			}
		}`))
	})

	model := cohere.NewRerankModel("rerank-v3.5",
		cohere.WithAPIKey("sk-cohere-test"),
		cohere.WithBaseURL(base),
		cohere.WithAllowHTTP(),
		cohere.WithAllowPrivateIPs(),
	)

	resp, err := model.Rerank(t.Context(), ai.RerankRequest{
		Query: "capital of France",
		Documents: []string{
			"London is the capital of England.",
			"Paris is the capital of France.",
		},
		ReturnDocuments: true,
	})
	require.NoError(t, err)

	assert.Equal(t, "rerank-v3.5", captured["model"])
	assert.Equal(t, "capital of France", captured["query"])
	assert.Equal(t, true, captured["return_documents"])

	require.Len(t, resp.Results, 2)
	assert.Equal(t, 1, resp.Results[0].Index)
	assert.InDelta(t, 0.985, resp.Results[0].RelevanceScore, 1e-4)
	assert.Equal(t, "Paris is the capital of France.", resp.Results[0].Document)

	assert.Equal(t, 0, resp.Results[1].Index)
	assert.InDelta(t, 0.124, resp.Results[1].RelevanceScore, 1e-4)
	assert.Equal(t, "London is the capital of England.", resp.Results[1].Document)

	assert.Equal(t, 25, resp.Usage.InputTokens)
}

func TestRerankStringDocument(t *testing.T) {
	t.Parallel()

	base := localServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"results": [
				{
					"index": 0,
					"relevance_score": 0.95,
					"document": "String document content"
				}
			],
			"usage": {
				"total_tokens": 18
			}
		}`))
	})

	model := cohere.NewRerankModel("rerank-v3.5",
		cohere.WithAPIKey("sk-test"),
		cohere.WithBaseURL(base),
		cohere.WithAllowHTTP(),
		cohere.WithAllowPrivateIPs(),
	)

	resp, err := model.Rerank(t.Context(), ai.RerankRequest{
		Query:     "test",
		Documents: []string{"String document content"},
	})
	require.NoError(t, err)

	require.Len(t, resp.Results, 1)
	assert.Equal(t, 0, resp.Results[0].Index)
	assert.InDelta(t, 0.95, resp.Results[0].RelevanceScore, 1e-4)
	assert.Equal(t, "String document content", resp.Results[0].Document)
	assert.Equal(t, 18, resp.Usage.InputTokens)
}

func TestRerankReturnDocumentsBackfill(t *testing.T) {
	t.Parallel()

	base := localServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Server only returns index and score, omitting document
		_, _ = w.Write([]byte(`{
			"results": [
				{"index": 1, "relevance_score": 0.9},
				{"index": 0, "relevance_score": 0.3}
			]
		}`))
	})

	model := cohere.NewRerankModel("rerank-v3.5",
		cohere.WithAPIKey("sk-test"),
		cohere.WithBaseURL(base),
		cohere.WithAllowHTTP(),
		cohere.WithAllowPrivateIPs(),
	)

	// Case 1: ReturnDocuments = true -> client backfills from request documents
	resp, err := model.Rerank(t.Context(), ai.RerankRequest{
		Query:           "capital",
		Documents:       []string{"London", "Paris"},
		ReturnDocuments: true,
	})
	require.NoError(t, err)

	require.Len(t, resp.Results, 2)
	assert.Equal(t, 1, resp.Results[0].Index)
	assert.Equal(t, "Paris", resp.Results[0].Document)
	assert.Equal(t, 0, resp.Results[1].Index)
	assert.Equal(t, "London", resp.Results[1].Document)

	// Case 2: ReturnDocuments = false -> document remains empty
	respNoDoc, err := model.Rerank(t.Context(), ai.RerankRequest{
		Query:           "capital",
		Documents:       []string{"London", "Paris"},
		ReturnDocuments: false,
	})
	require.NoError(t, err)

	require.Len(t, respNoDoc.Results, 2)
	assert.Empty(t, respNoDoc.Results[0].Document)
	assert.Empty(t, respNoDoc.Results[1].Document)
}

func TestRerankTopN(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	base := localServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, decodeBody(r, &captured))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"results": [
				{"index": 2, "relevance_score": 0.99}
			]
		}`))
	})

	model := cohere.NewRerankModel("rerank-v3.5",
		cohere.WithAPIKey("sk-test"),
		cohere.WithBaseURL(base),
		cohere.WithAllowHTTP(),
		cohere.WithAllowPrivateIPs(),
	)

	resp, err := model.Rerank(t.Context(), ai.RerankRequest{
		Query:     "search",
		Documents: []string{"doc 0", "doc 1", "doc 2"},
		TopN:      ai.Ptr(1),
	})
	require.NoError(t, err)

	assert.InDelta(t, 1, as[float64](t, captured["top_n"]), 1e-9)
	require.Len(t, resp.Results, 1)
	assert.Equal(t, 2, resp.Results[0].Index)
}

func TestRerankInputValidation(t *testing.T) {
	t.Parallel()

	model := cohere.NewRerankModel("rerank-v3.5", cohere.WithAPIKey("sk-test"))

	// Empty query
	_, err := model.Rerank(t.Context(), ai.RerankRequest{
		Query:     "",
		Documents: []string{"doc 1"},
	})
	require.Error(t, err)
	require.ErrorIs(t, err, ai.ErrInvalidRequest)
	assert.Contains(t, err.Error(), "query cannot be empty")

	// Whitespace query
	_, err = model.Rerank(t.Context(), ai.RerankRequest{
		Query:     "   ",
		Documents: []string{"doc 1"},
	})
	require.Error(t, err)
	require.ErrorIs(t, err, ai.ErrInvalidRequest)

	// Empty documents
	_, err = model.Rerank(t.Context(), ai.RerankRequest{
		Query:     "query",
		Documents: []string{},
	})
	require.Error(t, err)
	require.ErrorIs(t, err, ai.ErrInvalidRequest)
	assert.Contains(t, err.Error(), "documents cannot be empty")

	// Non-positive TopN
	_, err = model.Rerank(t.Context(), ai.RerankRequest{
		Query:     "query",
		Documents: []string{"doc 1"},
		TopN:      ai.Ptr(0),
	})
	require.Error(t, err)
	require.ErrorIs(t, err, ai.ErrInvalidRequest)
	assert.Contains(t, err.Error(), "top_n must be greater than 0")

	_, err = model.Rerank(t.Context(), ai.RerankRequest{
		Query:     "query",
		Documents: []string{"doc 1"},
		TopN:      ai.Ptr(-1),
	})
	require.Error(t, err)
	require.ErrorIs(t, err, ai.ErrInvalidRequest)
}

func TestRerankIndexOutOfRange(t *testing.T) {
	t.Parallel()

	base := localServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"results": [
				{"index": 5, "relevance_score": 0.9}
			]
		}`))
	})

	model := cohere.NewRerankModel("rerank-v3.5",
		cohere.WithAPIKey("sk-test"),
		cohere.WithBaseURL(base),
		cohere.WithAllowHTTP(),
		cohere.WithAllowPrivateIPs(),
	)

	_, err := model.Rerank(t.Context(), ai.RerankRequest{
		Query:     "test",
		Documents: []string{"doc 0", "doc 1"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "out of range")
}

func TestRerankErrors(t *testing.T) {
	t.Parallel()

	t.Run("401 unauthorized cohere style", func(t *testing.T) {
		t.Parallel()

		base := localServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message": "invalid api key"}`))
		})

		model := cohere.NewRerankModel("rerank-v3.5",
			cohere.WithAPIKey("bad-key"),
			cohere.WithBaseURL(base),
			cohere.WithAllowHTTP(),
			cohere.WithAllowPrivateIPs(),
		)

		_, err := model.Rerank(t.Context(), ai.RerankRequest{
			Query:     "test",
			Documents: []string{"doc"},
		})
		require.Error(t, err)
		require.ErrorIs(t, err, ai.ErrAuth)

		var apiErr *ai.Error
		require.ErrorAs(t, err, &apiErr)
		assert.Equal(t, 401, apiErr.StatusCode)
		assert.Equal(t, "invalid api key", apiErr.Message)
	})

	t.Run("429 rate limited with retry-after", func(t *testing.T) {
		t.Parallel()

		base := localServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message": "rate limit exceeded"}`))
		})

		model := cohere.NewRerankModel("rerank-v3.5",
			cohere.WithAPIKey("key"),
			cohere.WithBaseURL(base),
			cohere.WithAllowHTTP(),
			cohere.WithAllowPrivateIPs(),
		)

		_, err := model.Rerank(t.Context(), ai.RerankRequest{
			Query:     "test",
			Documents: []string{"doc"},
		})
		require.Error(t, err)
		require.ErrorIs(t, err, ai.ErrRateLimited)

		var apiErr *ai.Error
		require.ErrorAs(t, err, &apiErr)
		assert.Equal(t, 429, apiErr.StatusCode)
		assert.Equal(t, 120*time.Second, apiErr.RetryAfter)
	})

	t.Run("503 overloaded", func(t *testing.T) {
		t.Parallel()

		base := localServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"message": "server busy"}`))
		})

		model := cohere.NewRerankModel("rerank-v3.5",
			cohere.WithAPIKey("key"),
			cohere.WithBaseURL(base),
			cohere.WithAllowHTTP(),
			cohere.WithAllowPrivateIPs(),
		)

		_, err := model.Rerank(t.Context(), ai.RerankRequest{
			Query:     "test",
			Documents: []string{"doc"},
		})
		require.Error(t, err)
		require.ErrorIs(t, err, ai.ErrOverloaded)
	})

	t.Run("400 invalid request with openai error envelope", func(t *testing.T) {
		t.Parallel()

		base := localServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error": {"message": "malformed request", "type": "invalid_request_error", "code": "bad_param"}}`))
		})

		model := cohere.NewRerankModel("rerank-v3.5",
			cohere.WithAPIKey("key"),
			cohere.WithBaseURL(base),
			cohere.WithAllowHTTP(),
			cohere.WithAllowPrivateIPs(),
		)

		_, err := model.Rerank(t.Context(), ai.RerankRequest{
			Query:     "test",
			Documents: []string{"doc"},
		})
		require.Error(t, err)
		require.ErrorIs(t, err, ai.ErrInvalidRequest)

		var apiErr *ai.Error
		require.ErrorAs(t, err, &apiErr)
		assert.Equal(t, "malformed request", apiErr.Message)
		assert.Equal(t, "invalid_request_error", apiErr.Type)
		assert.Equal(t, "bad_param", apiErr.Code)
	})
}

func TestRerankProviderOptions(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	base := localServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, decodeBody(r, &captured))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results": [{"index": 0, "relevance_score": 1.0}]}`))
	})

	model := cohere.NewRerankModel("rerank-v3.5",
		cohere.WithAPIKey("sk-test"),
		cohere.WithBaseURL(base),
		cohere.WithAllowHTTP(),
		cohere.WithAllowPrivateIPs(),
	)

	_, err := model.Rerank(t.Context(), ai.RerankRequest{
		Query:     "test",
		Documents: []string{"doc 0"},
		ProviderOptions: map[ai.Provider]any{
			ai.ProviderCohere: cohere.RerankOptions{
				MaxTokensPerDoc: ai.Ptr(2048),
				Priority:        ai.Ptr(5),
				ExtraFields: map[string]any{
					"custom_setting": "value",
				},
			},
		},
	})
	require.NoError(t, err)

	assert.InDelta(t, 2048, as[float64](t, captured["max_tokens_per_doc"]), 1e-9)
	assert.InDelta(t, 5, as[float64](t, captured["priority"]), 1e-9)
	assert.Equal(t, "value", captured["custom_setting"])
}

func TestRerankModelMetadata(t *testing.T) {
	t.Parallel()

	model := cohere.NewRerankModel("rerank-v3.5",
		cohere.WithAPIKey("test-key"),
		cohere.WithProvider(ai.ProviderCohere),
	)

	assert.Equal(t, ai.ProviderCohere, model.Provider())
	assert.Equal(t, "rerank-v3.5", model.ModelID())
	assert.True(t, model.Capabilities().Reranking)

	customCaps := ai.Capabilities{Reranking: true, Reasoning: true}
	modelWithCaps := cohere.NewRerankModel("rerank-v3.5",
		cohere.WithCapabilities(customCaps),
	)
	assert.Equal(t, customCaps, modelWithCaps.Capabilities())
}
