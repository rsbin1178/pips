package openai_test

import (
	"encoding/base64"
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

func TestImageGeneration(t *testing.T) {
	t.Parallel()

	pixel := base64.StdEncoding.EncodeToString([]byte{0x89, 0x50, 0x4e, 0x47})

	var captured map[string]any

	base := localServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/images/generations", r.URL.Path)
		assert.NoError(t, decodeBody(r, &captured))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"` + pixel + `"}],"usage":{"input_tokens":5,"output_tokens":100}}`))
	})

	model := openai.NewImageModel("gpt-image-1",
		openai.WithAPIKey("sk-test"), openai.WithBaseURL(base),
		openai.WithAllowHTTP(), openai.WithAllowPrivateIPs())

	resp, err := model.GenerateImages(t.Context(), ai.ImageRequest{Prompt: "a cat", Size: "1024x1024"})
	require.NoError(t, err)

	assert.Equal(t, "gpt-image-1", captured["model"])
	assert.Equal(t, "a cat", captured["prompt"])
	assert.Equal(t, "1024x1024", captured["size"])

	require.Len(t, resp.Images, 1)
	assert.Equal(t, []byte{0x89, 0x50, 0x4e, 0x47}, resp.Images[0].Data)
	assert.Equal(t, "image/png", resp.Images[0].MIMEType)
	assert.Equal(t, 100, resp.Usage.OutputTokens)
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

func TestImageModelInterfaceIDs(t *testing.T) {
	t.Parallel()

	img := openai.NewImageModel("gpt-image-1", openai.WithAPIKey("x"))
	assert.Equal(t, ai.ProviderOpenAI, img.Provider())
	assert.Equal(t, "gpt-image-1", img.ModelID())

	emb := openai.NewEmbeddingModel("text-embedding-3-small", openai.WithAPIKey("x"))
	assert.Equal(t, "text-embedding-3-small", emb.ModelID())
}
