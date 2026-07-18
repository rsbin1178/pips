package gemini_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/gemini"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func localServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return server.URL + "/v1beta"
}

func decodeBody(r *http.Request, v any) error {
	defer r.Body.Close() //nolint:errcheck // test helper

	return json.NewDecoder(r.Body).Decode(v)
}

func TestImageGeneration(t *testing.T) {
	t.Parallel()

	pixel := base64.StdEncoding.EncodeToString([]byte{1, 2, 3, 4})

	var captured map[string]any

	base := localServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1beta/models/gemini-2.5-flash-image:generateContent", r.URL.Path)
		assert.NoError(t, decodeBody(r, &captured))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"` + pixel + `"}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":6,"candidatesTokenCount":1290}}`))
	})

	model := gemini.NewImageModel("gemini-2.5-flash-image",
		gemini.WithAPIKey("gm-test"), gemini.WithBaseURL(base),
		gemini.WithAllowHTTP(), gemini.WithAllowPrivateIPs())

	resp, err := model.GenerateImages(t.Context(), ai.ImageRequest{Prompt: "a cat"})
	require.NoError(t, err)

	// responseModalities requests IMAGE output.
	gc := as[map[string]any](t, captured["generationConfig"])
	modalities := as[[]any](t, gc["responseModalities"])
	assert.Equal(t, "IMAGE", modalities[0])

	require.Len(t, resp.Images, 1)
	assert.Equal(t, []byte{1, 2, 3, 4}, resp.Images[0].Data)
	assert.Equal(t, "image/png", resp.Images[0].MIMEType)
}

func TestEmbeddings(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	base := localServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1beta/models/text-embedding-004:batchEmbedContents", r.URL.Path)
		assert.NoError(t, decodeBody(r, &captured))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"embeddings":[{"values":[0.1,0.2]},{"values":[0.3,0.4]}]}`))
	})

	model := gemini.NewEmbeddingModel("text-embedding-004",
		gemini.WithAPIKey("gm-test"), gemini.WithBaseURL(base),
		gemini.WithAllowHTTP(), gemini.WithAllowPrivateIPs())

	resp, err := model.Embed(t.Context(), ai.EmbeddingRequest{Input: []string{"hello", "world"}})
	require.NoError(t, err)

	requests := as[[]any](t, captured["requests"])
	require.Len(t, requests, 2)
	first := as[map[string]any](t, requests[0])
	assert.Equal(t, "models/text-embedding-004", first["model"])

	require.Len(t, resp.Embeddings, 2)
	assert.Equal(t, []float32{0.1, 0.2}, resp.Embeddings[0])
	assert.Equal(t, []float32{0.3, 0.4}, resp.Embeddings[1])
}
