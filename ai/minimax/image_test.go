package minimax_test

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/minimax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMiniMaxImageURLs(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/image_generation", r.URL.Path)
		assert.Equal(t, "Bearer test-minimax-key", r.Header.Get("Authorization"))

		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.NoError(t, json.Unmarshal(body, &captured))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "trace-1",
			"data": {"image_urls": ["https://example.com/1.png", "https://example.com/2.png"]},
			"metadata": {"success_count": "2", "failed_count": "0"},
			"base_resp": {"status_code": 0, "status_msg": "success"}
		}`))
	}))
	t.Cleanup(server.Close)

	model := minimax.NewImageModel("image-01",
		minimax.WithBaseURL(server.URL),
		minimax.WithAPIKey("test-minimax-key"),
		minimax.WithAllowHTTP(),
		minimax.WithAllowPrivateIPs(),
	)

	assert.Equal(t, ai.ProviderMiniMax, model.Provider())
	assert.True(t, model.Capabilities().ImageGeneration)

	optimizer := true
	resp, err := model.GenerateImages(t.Context(), ai.ImageRequest{
		Prompt: "a cat",
		N:      2,
		Size:   "16:9",
		ProviderOptions: map[ai.Provider]any{
			ai.ProviderMiniMax: minimax.ImageOptions{PromptOptimizer: &optimizer},
		},
	})
	require.NoError(t, err)

	require.Len(t, resp.Images, 2)
	assert.Equal(t, "https://example.com/1.png", resp.Images[0].URL)
	assert.Equal(t, "https://example.com/2.png", resp.Images[1].URL)

	// Aspect-ratio sizes pass through; the string-valued metadata counters in
	// the response must not break decoding.
	assert.Equal(t, "16:9", captured["aspect_ratio"])
	assert.InDelta(t, 2, captured["n"], 0.0001)
	assert.Equal(t, true, captured["prompt_optimizer"])
}

func TestMiniMaxImageBase64AndSize(t *testing.T) {
	t.Parallel()

	encoded := base64.StdEncoding.EncodeToString([]byte("jpeg-bytes"))

	var captured map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.NoError(t, json.Unmarshal(body, &captured))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"image_base64":["` + encoded + `"]},"base_resp":{"status_code":0}}`))
	}))
	t.Cleanup(server.Close)

	model := minimax.NewImageModel("image-01",
		minimax.WithBaseURL(server.URL),
		minimax.WithAPIKey("test-minimax-key"),
		minimax.WithAllowHTTP(),
		minimax.WithAllowPrivateIPs(),
	)

	resp, err := model.GenerateImages(t.Context(), ai.ImageRequest{
		Prompt: "a cat",
		Size:   "1024x1024",
		ProviderOptions: map[ai.Provider]any{
			ai.ProviderMiniMax: minimax.ImageOptions{ResponseFormat: "base64"},
		},
	})
	require.NoError(t, err)

	require.Len(t, resp.Images, 1)
	assert.Equal(t, []byte("jpeg-bytes"), resp.Images[0].Data)
	assert.Equal(t, "image/jpeg", resp.Images[0].MIMEType)
	assert.Empty(t, resp.Images[0].URL)

	assert.InDelta(t, 1024, captured["width"], 0.0001)
	assert.InDelta(t, 1024, captured["height"], 0.0001)
	assert.Equal(t, "base64", captured["response_format"])
}

func TestMiniMaxImageRejectsInvalidSize(t *testing.T) {
	t.Parallel()

	model := minimax.NewImageModel("image-01", minimax.WithAPIKey("test"))

	_, err := model.GenerateImages(t.Context(), ai.ImageRequest{Prompt: "a cat", Size: "5:4"})
	require.ErrorIs(t, err, ai.ErrInvalidRequest)

	_, err = model.GenerateImages(t.Context(), ai.ImageRequest{Prompt: "a cat", Size: "100x100"})
	require.ErrorIs(t, err, ai.ErrInvalidRequest)

	_, err = model.GenerateImages(t.Context(), ai.ImageRequest{Prompt: "a cat", N: 10})
	require.ErrorIs(t, err, ai.ErrInvalidRequest)
}

func TestMiniMaxImageMapsBaseResponseCodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		code    int
		message string
		want    error
	}{
		{name: "rate limited", code: 1002, message: "rate limit", want: ai.ErrRateLimited},
		{name: "invalid key", code: 2049, message: "invalid api key", want: ai.ErrAuth},
		{name: "sensitive", code: 1026, message: "sensitive content", want: ai.ErrInvalidRequest},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"base_resp":{"status_code":` +
					strconv.Itoa(test.code) + `,"status_msg":"` + test.message + `"}}`))
			}))
			t.Cleanup(server.Close)

			model := minimax.NewImageModel("image-01",
				minimax.WithBaseURL(server.URL),
				minimax.WithAPIKey("test-minimax-key"),
				minimax.WithAllowHTTP(),
				minimax.WithAllowPrivateIPs(),
			)

			_, err := model.GenerateImages(t.Context(), ai.ImageRequest{Prompt: "a cat"})
			require.ErrorIs(t, err, test.want)
			assert.Contains(t, err.Error(), test.message)
		})
	}
}
