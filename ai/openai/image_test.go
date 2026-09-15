package openai_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/jsonx"
	"github.com/rsbin1178/pips/ai/openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// imageInlineResponse is a GPT-image-style response: inline bytes, the
// metadata block, and the full usage breakdown.
const imageInlineResponse = `{
	"created": 1713833628,
	"data": [{"b64_json": "cGl4ZWxz", "revised_prompt": "a sea otter, watercolor"}],
	"background": "transparent",
	"output_format": "webp",
	"size": "1536x1024",
	"quality": "high",
	"usage": {
		"total_tokens": 100,
		"input_tokens": 50,
		"output_tokens": 50,
		"input_tokens_details": {"text_tokens": 10, "image_tokens": 40},
		"output_tokens_details": {"text_tokens": 4, "image_tokens": 46}
	}
}`

// imageURLResponse is a dall-e-style response that returns a hosted link
// instead of bytes.
const imageURLResponse = `{
	"created": 1713833628,
	"data": [{"url": "https://images.example.test/otter.webp?expires=60", "revised_prompt": "a sea otter with a pearl earring"}]
}`

// imageMinimalResponse carries only the fields the API always returns.
const imageMinimalResponse = `{"created": 1713833628, "data": [{"b64_json": "cGl4ZWxz"}]}`

// newImageModel returns an image model bound to a local test server.
func newImageModel(t *testing.T, handler http.HandlerFunc, model string, opts ...openai.Option) *openai.ImageModel {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	base := []openai.Option{
		openai.WithAPIKey("sk-test"),
		openai.WithBaseURL(server.URL + "/v1"),
		openai.WithAllowHTTP(),
		openai.WithAllowPrivateIPs(),
	}

	return openai.NewImageModel(model, append(base, opts...)...)
}

func TestImageGenerationSendsEveryParameter(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newImageModel(t, serveJSON(t, imageInlineResponse, "/v1/images/generations", &captured), "gpt-image-1")

	compression := 80
	partials := 2

	resp, err := model.GenerateImages(t.Context(), ai.ImageRequest{
		Prompt:       "a sea otter",
		N:            3,
		Size:         "1536x1024",
		Quality:      "high",
		OutputFormat: "webp",
		ProviderOptions: map[ai.Provider]any{
			ai.ProviderOpenAI: openai.ImageOptions{
				Background:        "transparent",
				Moderation:        "low",
				ResponseFormat:    "b64_json",
				Style:             "natural",
				User:              "user-1234",
				OutputCompression: &compression,
				PartialImages:     &partials,
			},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, map[string]any{
		"model":              "gpt-image-1",
		"prompt":             "a sea otter",
		"n":                  float64(3),
		"size":               "1536x1024",
		"quality":            "high",
		"output_format":      "webp",
		"response_format":    "b64_json",
		"background":         "transparent",
		"moderation":         "low",
		"style":              "natural",
		"user":               "user-1234",
		"output_compression": float64(80),
		"partial_images":     float64(2),
	}, captured)

	require.Len(t, resp.Images, 1)
	assert.Equal(t, []byte("pixels"), resp.Images[0].Data)
	assert.Equal(t, "image/webp", resp.Images[0].MIMEType)
	assert.Equal(t, "a sea otter, watercolor", resp.Images[0].RevisedPrompt)

	// Response metadata and the usage breakdown reach the portable type.
	assert.Equal(t, time.Unix(1713833628, 0).UTC(), resp.CreatedAt)
	assert.Equal(t, "webp", resp.OutputFormat)
	assert.Equal(t, "1536x1024", resp.Size)
	assert.Equal(t, "high", resp.Quality)
	assert.Equal(t, "transparent", resp.Background)
	assert.Equal(t, 100, resp.Usage.TotalTokens)
	assert.Equal(t, 50, resp.Usage.InputTokens)
	assert.Equal(t, 50, resp.Usage.OutputTokens)
	assert.Equal(t, 10, resp.Usage.InputTextTokens)
	assert.Equal(t, 40, resp.Usage.InputImageTokens)
	assert.Equal(t, 4, resp.Usage.OutputTextTokens)
	assert.Equal(t, 46, resp.Usage.OutputImageTokens)
	assert.NotEmpty(t, resp.Raw)
}

func TestImageGenerationOmitsUnsetParameters(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newImageModel(t, serveJSON(t, imageMinimalResponse, "/v1/images/generations", &captured), "dall-e-3")

	resp, err := model.GenerateImages(t.Context(), ai.ImageRequest{Prompt: "a cat"})
	require.NoError(t, err)

	// Only explicitly set fields are sent, so the API applies its defaults.
	assert.Equal(t, map[string]any{"model": "dall-e-3", "prompt": "a cat"}, captured)

	// Inline bytes with no format information are PNG, the API default.
	require.Len(t, resp.Images, 1)
	assert.Equal(t, "image/png", resp.Images[0].MIMEType)
	assert.Equal(t, []byte("pixels"), resp.Images[0].Data)
	assert.Zero(t, resp.Usage.TotalTokens)
	assert.True(t, resp.CreatedAt.Equal(time.Unix(1713833628, 0).UTC()))
}

func TestImageGenerationSendsExplicitZero(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newImageModel(t, serveJSON(t, imageMinimalResponse, "/v1/images/generations", &captured), "gpt-image-1")

	// Pointer fields keep an explicit zero distinct from "unset", which is why
	// they are pointers rather than omitempty values.
	_, err := model.GenerateImages(t.Context(), ai.ImageRequest{
		Prompt: "a cat",
		ProviderOptions: imageOptionsWith(openai.ImageOptions{
			OutputCompression: ai.Ptr(0),
			PartialImages:     ai.Ptr(0),
		}),
	})
	require.NoError(t, err)

	assert.InDelta(t, 0, as[float64](t, captured["output_compression"]), 1e-9)
	assert.InDelta(t, 0, as[float64](t, captured["partial_images"]), 1e-9)
}

func TestImageGenerationURLResponse(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newImageModel(t, serveJSON(t, imageURLResponse, "/v1/images/generations", &captured), "dall-e-3")

	resp, err := model.GenerateImages(t.Context(), ai.ImageRequest{Prompt: "a sea otter"})
	require.NoError(t, err)

	require.Len(t, resp.Images, 1)
	assert.Equal(t, "https://images.example.test/otter.webp?expires=60", resp.Images[0].URL)
	assert.Empty(t, resp.Images[0].Data, "URL responses must not be reported as empty byte images")
	assert.Equal(t, "image/webp", resp.Images[0].MIMEType, "the URL extension carries the format")
	assert.Equal(t, "a sea otter with a pearl earring", resp.Images[0].RevisedPrompt)
}

func TestImageGenerationMIMEInference(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name            string
		response        string
		requestedFormat string
		want            string
	}{
		{
			name:     "response output_format wins",
			response: `{"data":[{"b64_json":"cGl4ZWxz"}],"output_format":"jpeg"}`,
			want:     "image/jpeg",
		},
		{
			name:            "requested format is the fallback",
			response:        `{"data":[{"b64_json":"cGl4ZWxz"}]}`,
			requestedFormat: "webp",
			want:            "image/webp",
		},
		{
			name:     "inline bytes default to png",
			response: `{"data":[{"b64_json":"cGl4ZWxz"}]}`,
			want:     "image/png",
		},
		{
			name:     "unknown URL extension leaves the type empty",
			response: `{"data":[{"url":"https://images.example.test/otter"}]}`,
			want:     "",
		},
		{
			name:     "URL response output_format wins over the extension",
			response: `{"data":[{"url":"https://images.example.test/otter.png"}],"output_format":"webp"}`,
			want:     "image/webp",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			model := newImageModel(t, serveJSON(t, tc.response, "/v1/images/generations", nil), "gpt-image-1")

			resp, err := model.GenerateImages(t.Context(), ai.ImageRequest{
				Prompt:       "a cat",
				OutputFormat: tc.requestedFormat,
			})
			require.NoError(t, err)

			require.Len(t, resp.Images, 1)
			assert.Equal(t, tc.want, resp.Images[0].MIMEType)
		})
	}
}

func TestImageGenerationRejectsDatumWithoutPayload(t *testing.T) {
	t.Parallel()

	model := newImageModel(t, serveJSON(
		t, `{"data":[{"revised_prompt":"a cat"}]}`, "/v1/images/generations", nil,
	), "gpt-image-1")

	_, err := model.GenerateImages(t.Context(), ai.ImageRequest{Prompt: "a cat"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "data item 0 carries neither b64_json nor url")
}

func TestImageGenerationRejectsUndecodablePayload(t *testing.T) {
	t.Parallel()

	model := newImageModel(t, serveJSON(
		t, `{"data":[{"b64_json":"not-base64!"}]}`, "/v1/images/generations", nil,
	), "gpt-image-1")

	_, err := model.GenerateImages(t.Context(), ai.ImageRequest{Prompt: "a cat"})
	require.ErrorContains(t, err, "decoding image 0")
}

func TestImageGenerationValidatesLocally(t *testing.T) {
	t.Parallel()

	negative := -1
	tooManyPartials := 4
	tooMuchCompression := 101
	negativeCompression := -1

	cases := []struct {
		name string
		req  ai.ImageRequest
	}{
		{name: "empty prompt", req: ai.ImageRequest{}},
		{name: "blank prompt", req: ai.ImageRequest{Prompt: "   "}},
		{
			name: "partial_images above the range",
			req: ai.ImageRequest{
				Prompt:          "a cat",
				ProviderOptions: imageOptionsWith(openai.ImageOptions{PartialImages: &tooManyPartials}),
			},
		},
		{
			name: "partial_images below the range",
			req: ai.ImageRequest{
				Prompt:          "a cat",
				ProviderOptions: imageOptionsWith(openai.ImageOptions{PartialImages: &negative}),
			},
		},
		{
			name: "output_compression above the range",
			req: ai.ImageRequest{
				Prompt:          "a cat",
				ProviderOptions: imageOptionsWith(openai.ImageOptions{OutputCompression: &tooMuchCompression}),
			},
		},
		{
			name: "output_compression below the range",
			req: ai.ImageRequest{
				Prompt:          "a cat",
				ProviderOptions: imageOptionsWith(openai.ImageOptions{OutputCompression: &negativeCompression}),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var called bool

			model := newImageModel(t, func(w http.ResponseWriter, _ *http.Request) {
				called = true

				w.WriteHeader(http.StatusInternalServerError)
			}, "gpt-image-1")

			_, err := model.GenerateImages(t.Context(), tc.req)
			require.ErrorIs(t, err, ai.ErrInvalidRequest)
			assert.False(t, called, "an invalid request must not reach the API")
		})
	}
}

func TestImageGenerationExtraFields(t *testing.T) {
	t.Parallel()

	t.Run("documented optional keys are deliverable", func(t *testing.T) {
		t.Parallel()

		var captured map[string]any

		model := newImageModel(t, serveJSON(t, imageMinimalResponse, "/v1/images/generations", &captured), "dall-e-3")

		_, err := model.GenerateImages(t.Context(), ai.ImageRequest{
			Prompt: "a cat",
			ProviderOptions: imageOptionsWith(openai.ImageOptions{ExtraFields: map[string]any{
				"background":      "transparent",
				"response_format": "url",
			}}),
		})
		require.NoError(t, err)

		assert.Equal(t, "transparent", captured["background"])
		assert.Equal(t, "url", captured["response_format"])
	})

	t.Run("typed and credential-shaped keys are rejected", func(t *testing.T) {
		t.Parallel()

		model := newImageModel(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}, "dall-e-3")

		for _, extra := range []map[string]any{
			{"model": "dall-e-2"},
			{"n": 4},
			{"image[]": "x"},
			// stream stays reserved: only the streaming entry points can read
			// an event-stream response.
			{"stream": true},
			{"api_key": "sk-leak"},
		} {
			_, err := model.GenerateImages(t.Context(), ai.ImageRequest{
				Prompt:          "a cat",
				ProviderOptions: imageOptionsWith(openai.ImageOptions{ExtraFields: extra}),
			})
			require.ErrorIs(t, err, jsonx.ErrUnsafeExtension, "extra %v", extra)
		}
	})
}

func TestImageGenerationErrorMapping(t *testing.T) {
	t.Parallel()

	model := newImageModel(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"Slow down","type":"rate_limit_error","code":"slow_down"}}`))
	}, "gpt-image-1")

	_, err := model.GenerateImages(t.Context(), ai.ImageRequest{Prompt: "a cat"})
	require.Error(t, err)
	require.ErrorIs(t, err, ai.ErrRateLimited)
	assert.True(t, ai.IsRetryable(err))

	var apiErr *ai.Error
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusTooManyRequests, apiErr.StatusCode)
	assert.Equal(t, "Slow down", apiErr.Message)
	assert.Equal(t, "rate_limit_error", apiErr.Type)
	assert.Equal(t, "slow_down", apiErr.Code)
	assert.Equal(t, 12*time.Second, apiErr.RetryAfter)
	assert.Contains(t, string(apiErr.Raw), "Slow down")
}

func TestImageModelInterfaceIDs(t *testing.T) {
	t.Parallel()

	img := openai.NewImageModel("gpt-image-1", openai.WithAPIKey("x"))
	assert.Equal(t, ai.ProviderOpenAI, img.Provider())
	assert.Equal(t, "gpt-image-1", img.ModelID())

	// The optional image capabilities are discovered by type assertion.
	var (
		_ ai.ImageModel    = img
		_ ai.ImageEditor   = img
		_ ai.ImageVariator = img
		_ ai.ImageStreamer = img
	)
}

// imageOptionsWith wraps ImageOptions in the ProviderOptions map.
func imageOptionsWith(opts openai.ImageOptions) map[ai.Provider]any {
	return map[ai.Provider]any{ai.ProviderOpenAI: opts}
}
