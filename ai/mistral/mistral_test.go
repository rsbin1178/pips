package mistral_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/mistral"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMistralChat(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer test-mistral-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat_mistral_1","model":"mistral-large-latest","choices":[{"message":{"role":"assistant","content":"bonjour from mistral"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := mistral.New("mistral-large-latest",
		mistral.WithBaseURL(server.URL),
		mistral.WithAPIKey("test-mistral-key"),
		mistral.WithAllowHTTP(),
		mistral.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderMistral, model.Provider())
	assert.Equal(t, "mistral-large-latest", model.ModelID())

	resp, err := model.Generate(t.Context(), ai.Request{
		Messages: []ai.Message{ai.UserText("hi")},
	})
	require.NoError(t, err)
	assert.Equal(t, ai.ProviderMistral, resp.Provider)
	assert.Equal(t, "bonjour from mistral", resp.Text())
}

func TestMistralEmbedding(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/embeddings", r.URL.Path)
		assert.Equal(t, "Bearer test-mistral-key", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2,0.3]}],"usage":{"prompt_tokens":5,"total_tokens":5}}`))
	}))
	t.Cleanup(server.Close)

	emb := mistral.NewEmbeddingModel("mistral-embed",
		mistral.WithBaseURL(server.URL),
		mistral.WithAPIKey("test-mistral-key"),
		mistral.WithAllowHTTP(),
		mistral.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderMistral, emb.Provider())
	assert.Equal(t, "mistral-embed", emb.ModelID())

	resp, err := emb.Embed(t.Context(), ai.EmbeddingRequest{Input: []string{"test"}})
	require.NoError(t, err)
	require.Len(t, resp.Embeddings, 1)
	assert.Equal(t, []float32{0.1, 0.2, 0.3}, resp.Embeddings[0])
}

func TestMistralEnvironmentFallback(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "env-mistral-key")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer env-mistral-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat_m_2","model":"mistral-large-latest","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)

	model := mistral.New("mistral-large-latest",
		mistral.WithBaseURL(server.URL),
		mistral.WithAllowHTTP(),
		mistral.WithAllowPrivateIPs(),
	)

	_, err := model.Generate(t.Context(), ai.Request{Messages: []ai.Message{ai.UserText("hi")}})
	require.NoError(t, err)
}
