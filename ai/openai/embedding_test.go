package openai_test

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
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

func encodeBase64Floats(floats []float32) string {
	buf := make([]byte, len(floats)*4)
	for i, f := range floats {
		binary.LittleEndian.PutUint32(buf[i*4:(i+1)*4], math.Float32bits(f))
	}

	return base64.StdEncoding.EncodeToString(buf)
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

func TestEmbeddingsBase64(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	vec0 := []float32{0.1, 0.2, 0.3}
	vec1 := []float32{0.4, 0.5, 0.6}

	b64_0 := encodeBase64Floats(vec0)
	b64_1 := encodeBase64Floats(vec1)

	base := localServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/embeddings", r.URL.Path)
		assert.NoError(t, decodeBody(r, &captured))
		w.Header().Set("Content-Type", "application/json")

		respJSON := fmt.Sprintf(`{"data":[{"index":0,"embedding":%q},{"index":1,"embedding":%q}],"usage":{"prompt_tokens":12,"total_tokens":12}}`, b64_0, b64_1)
		_, _ = w.Write([]byte(respJSON))
	})

	model := openai.NewEmbeddingModel("text-embedding-3-small",
		openai.WithAPIKey("sk-test"), openai.WithBaseURL(base),
		openai.WithAllowHTTP(), openai.WithAllowPrivateIPs())

	resp, err := model.Embed(t.Context(), ai.EmbeddingRequest{
		Input:          []string{"hello", "world"},
		EncodingFormat: ai.EmbeddingEncodingFormatBase64,
	})
	require.NoError(t, err)

	assert.Equal(t, "base64", captured["encoding_format"])
	require.Len(t, resp.Embeddings, 2)
	assert.Equal(t, vec0, resp.Embeddings[0])
	assert.Equal(t, vec1, resp.Embeddings[1])
	assert.Equal(t, 12, resp.Usage.InputTokens)
}

func TestEmbeddingsInvalidEncodingFormat(t *testing.T) {
	t.Parallel()

	model := openai.NewEmbeddingModel("text-embedding-3-small", openai.WithAPIKey("sk-test"))

	_, err := model.Embed(t.Context(), ai.EmbeddingRequest{
		Input:          []string{"test"},
		EncodingFormat: "unsupported-format",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ai.ErrUnsupported)
}

func TestEmbeddingsInvalidBase64(t *testing.T) {
	t.Parallel()

	base := localServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Not a multiple of 4 bytes
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":"QUJD"}]}`))
	})

	model := openai.NewEmbeddingModel("text-embedding-3-small",
		openai.WithAPIKey("sk-test"), openai.WithBaseURL(base),
		openai.WithAllowHTTP(), openai.WithAllowPrivateIPs())

	_, err := model.Embed(t.Context(), ai.EmbeddingRequest{
		Input:          []string{"test"},
		EncodingFormat: ai.EmbeddingEncodingFormatBase64,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid base64 embedding byte length")
}

func TestEmbeddingModelInterfaceIDs(t *testing.T) {
	t.Parallel()

	emb := openai.NewEmbeddingModel("text-embedding-3-small", openai.WithAPIKey("x"))
	assert.Equal(t, ai.ProviderOpenAI, emb.Provider())
	assert.Equal(t, "text-embedding-3-small", emb.ModelID())
}
