package openai_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
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

func TestEmbeddings(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	base := localServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/embeddings", r.URL.Path)
		assert.NoError(t, decodeBody(r, &captured))
		w.Header().Set("Content-Type", "application/json")
		// Return out of order to prove index-based placement.
		_, _ = w.Write([]byte(`{"data":[{"index":1,"embedding":[0.3,0.4]},{"index":0,"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":8,"total_tokens":8}}`))
	})

	model := openai.NewEmbeddingModel("text-embedding-3-small",
		openai.WithAPIKey("sk-test"), openai.WithBaseURL(base),
		openai.WithAllowHTTP(), openai.WithAllowPrivateIPs())

	resp, err := model.Embed(t.Context(), ai.EmbeddingRequest{Input: []string{"hello", "world"}, Dimensions: ai.Ptr(2)})
	require.NoError(t, err)

	input := as[[]any](t, captured["input"])
	require.Len(t, input, 2)
	assert.InDelta(t, 2, as[float64](t, captured["dimensions"]), 1e-9)

	require.Len(t, resp.Embeddings, 2)
	assert.Equal(t, []float32{0.1, 0.2}, resp.Embeddings[0])
	assert.Equal(t, []float32{0.3, 0.4}, resp.Embeddings[1])
	assert.Equal(t, 8, resp.Usage.InputTokens)
}

func TestEmbeddingModelInterfaceIDs(t *testing.T) {
	t.Parallel()

	emb := openai.NewEmbeddingModel("text-embedding-3-small", openai.WithAPIKey("x"))
	assert.Equal(t, ai.ProviderOpenAI, emb.Provider())
	assert.Equal(t, "text-embedding-3-small", emb.ModelID())
}
