package agnes_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/agnes"
	"github.com/rsbin1178/pips/ai/internal/jsonx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// agnesURLResponse is the documented response shape: a hosted link, with the
// unused fields present as null.
const agnesURLResponse = `{
	"created": 1780000000,
	"data": [{"url": "https://storage.example.test/agnes/otter.png", "b64_json": null, "revised_prompt": null}]
}`

// agnesInlineResponse is the return_base64 response shape.
const agnesInlineResponse = `{
	"created": 1780000000,
	"data": [{"url": null, "b64_json": "cGl4ZWxz", "revised_prompt": "a sea otter"}]
}`

// newImageModel returns an image model bound to a local test server.
func newImageModel(t *testing.T, handler http.HandlerFunc, opts ...agnes.Option) *agnes.ImageModel {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	base := []agnes.Option{
		agnes.WithAPIKey("sk-test"),
		agnes.WithBaseURL(server.URL + "/v1"),
		agnes.WithAllowHTTP(),
		agnes.WithAllowPrivateIPs(),
	}

	return agnes.NewImageModel("agnes-image-2.5-flash", append(base, opts...)...)
}

// serveJSON answers with response while asserting the request path and
// authentication header.
func serveJSON(t *testing.T, response, wantPath string, captured *map[string]any) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, wantPath, r.URL.Path)
		assert.Equal(t, "Bearer sk-test", r.Header.Get("Authorization"))

		var body map[string]any
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))

		if captured != nil {
			*captured = body
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}
}

func TestImageGenerationWireBody(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newImageModel(t, serveJSON(t, agnesInlineResponse, "/v1/images/generations", &captured))

	resp, err := model.GenerateImages(t.Context(), ai.ImageRequest{
		Prompt:       "a sea otter",
		Size:         "2K",
		OutputFormat: "png",
		ProviderOptions: map[ai.Provider]any{
			ai.ProviderAgnes: agnes.ImageOptions{
				Ratio:          "16:9",
				ResponseFormat: "b64_json",
				ReturnBase64:   true,
			},
		},
	})
	require.NoError(t, err)

	// response_format must sit inside extra_body, never at the top level.
	assert.Equal(t, map[string]any{
		"model":         "agnes-image-2.5-flash",
		"prompt":        "a sea otter",
		"size":          "2K",
		"ratio":         "16:9",
		"return_base64": true,
		"extra_body":    map[string]any{"response_format": "b64_json"},
	}, captured)

	require.Len(t, resp.Images, 1)
	assert.Equal(t, []byte("pixels"), resp.Images[0].Data)
	assert.Equal(t, "image/png", resp.Images[0].MIMEType)
	assert.Equal(t, "a sea otter", resp.Images[0].RevisedPrompt)
	assert.Empty(t, resp.Images[0].URL)

	assert.Equal(t, time.Unix(1780000000, 0).UTC(), resp.CreatedAt)
	assert.NotEmpty(t, resp.Raw)
}

func TestImageGenerationOmitsUnsetOptions(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newImageModel(t, serveJSON(t, agnesInlineResponse, "/v1/images/generations", &captured))

	_, err := model.GenerateImages(t.Context(), ai.ImageRequest{Prompt: "a cat", Size: "1K"})
	require.NoError(t, err)

	// Only explicitly set fields are sent, so the vendor's defaults apply.
	assert.Equal(t, map[string]any{
		"model":  "agnes-image-2.5-flash",
		"prompt": "a cat",
		"size":   "1K",
	}, captured)
}

func TestImageGenerationURLResponse(t *testing.T) {
	t.Parallel()

	model := newImageModel(t, serveJSON(t, agnesURLResponse, "/v1/images/generations", nil))

	resp, err := model.GenerateImages(t.Context(), ai.ImageRequest{Prompt: "a sea otter", Size: "1K"})
	require.NoError(t, err)

	require.Len(t, resp.Images, 1)
	assert.Equal(t, "https://storage.example.test/agnes/otter.png", resp.Images[0].URL)
	assert.Empty(t, resp.Images[0].Data, "URL responses must not be reported as empty byte images")
	assert.Equal(t, "image/png", resp.Images[0].MIMEType, "the URL extension carries the format")

	// A requested format outranks the URL extension.
	resp, err = model.GenerateImages(t.Context(), ai.ImageRequest{
		Prompt:       "a sea otter",
		Size:         "1K",
		OutputFormat: "webp",
	})
	require.NoError(t, err)
	require.Len(t, resp.Images, 1)
	assert.Equal(t, "image/webp", resp.Images[0].MIMEType)
}

func TestImageGenerationRequestedFormatMIME(t *testing.T) {
	t.Parallel()

	model := newImageModel(t, serveJSON(t, agnesInlineResponse, "/v1/images/generations", nil))

	resp, err := model.GenerateImages(t.Context(), ai.ImageRequest{
		Prompt:       "a sea otter",
		Size:         "1K",
		OutputFormat: "webp",
	})
	require.NoError(t, err)

	require.Len(t, resp.Images, 1)
	assert.Equal(t, "image/webp", resp.Images[0].MIMEType)
}

func TestImageGenerationRejectsDatumWithoutPayload(t *testing.T) {
	t.Parallel()

	model := newImageModel(t, serveJSON(
		t, `{"created":1,"data":[{"revised_prompt":"a cat"}]}`, "/v1/images/generations", nil,
	))

	_, err := model.GenerateImages(t.Context(), ai.ImageRequest{Prompt: "a cat", Size: "1K"})
	require.ErrorContains(t, err, "data item 0 carries neither b64_json nor url")
}

func TestImageGenerationValidatesLocally(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		req  ai.ImageRequest
		want error
	}{
		{name: "empty prompt", req: ai.ImageRequest{Size: "1K"}, want: ai.ErrInvalidRequest},
		{name: "blank prompt", req: ai.ImageRequest{Prompt: "   ", Size: "1K"}, want: ai.ErrInvalidRequest},
		{name: "missing size", req: ai.ImageRequest{Prompt: "a cat"}, want: ai.ErrInvalidRequest},
		{name: "blank size", req: ai.ImageRequest{Prompt: "a cat", Size: " "}, want: ai.ErrInvalidRequest},
		{
			name: "ratio outside the documented set",
			req: ai.ImageRequest{
				Prompt:          "a cat",
				Size:            "1K",
				ProviderOptions: agnesOptions(agnes.ImageOptions{Ratio: "5:4"}),
			},
			want: ai.ErrInvalidRequest,
		},
		{name: "more than one image", req: ai.ImageRequest{Prompt: "a cat", Size: "1K", N: 2}, want: ai.ErrUnsupported},
		{name: "negative count", req: ai.ImageRequest{Prompt: "a cat", Size: "1K", N: -1}, want: ai.ErrInvalidRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var called bool

			model := newImageModel(t, func(w http.ResponseWriter, _ *http.Request) {
				called = true

				w.WriteHeader(http.StatusInternalServerError)
			})

			_, err := model.GenerateImages(t.Context(), tc.req)
			require.ErrorIs(t, err, tc.want)
			assert.False(t, called, "an invalid request must not reach the API")
		})
	}
}

func TestImageGenerationErrorMapping(t *testing.T) {
	t.Parallel()

	model := newImageModel(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"Slow down","type":"AgnesAI_error","code":"rate_limited"}}`))
	})

	_, err := model.GenerateImages(t.Context(), ai.ImageRequest{Prompt: "a cat", Size: "1K"})
	require.Error(t, err)
	require.ErrorIs(t, err, ai.ErrRateLimited)
	assert.True(t, ai.IsRetryable(err))

	var apiErr *ai.Error
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, ai.ProviderAgnes, apiErr.Provider)
	assert.Equal(t, http.StatusTooManyRequests, apiErr.StatusCode)
	assert.Equal(t, "Slow down", apiErr.Message)
	assert.Equal(t, "AgnesAI_error", apiErr.Type)
	assert.Equal(t, "rate_limited", apiErr.Code)
	assert.Equal(t, 12*time.Second, apiErr.RetryAfter)
	assert.Contains(t, string(apiErr.Raw), "Slow down")
}

func TestImageGenerationClassifiesFailures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		status    int
		sentinel  error
		retryable bool
	}{
		{name: "authentication", status: http.StatusUnauthorized, sentinel: ai.ErrAuth},
		{name: "payment required", status: http.StatusPaymentRequired, sentinel: ai.ErrInvalidRequest},
		{name: "rate limited", status: http.StatusTooManyRequests, sentinel: ai.ErrRateLimited, retryable: true},
		{name: "unavailable", status: http.StatusServiceUnavailable, sentinel: ai.ErrOverloaded, retryable: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			model := newImageModel(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":{"message":"nope","type":"AgnesAI_error","code":""}}`))
			})

			_, err := model.GenerateImages(t.Context(), ai.ImageRequest{Prompt: "a cat", Size: "1K"})
			require.ErrorIs(t, err, tc.sentinel)
			assert.Equal(t, tc.retryable, ai.IsRetryable(err), "status %d", tc.status)
		})
	}
}

func TestImageGenerationExtraFields(t *testing.T) {
	t.Parallel()

	t.Run("unmodeled keys are deliverable", func(t *testing.T) {
		t.Parallel()

		var captured map[string]any

		model := newImageModel(t, serveJSON(t, agnesInlineResponse, "/v1/images/generations", &captured))

		_, err := model.GenerateImages(t.Context(), ai.ImageRequest{
			Prompt:          "a cat",
			Size:            "1K",
			ProviderOptions: agnesOptions(agnes.ImageOptions{ExtraFields: map[string]any{"seed": 42}}),
		})
		require.NoError(t, err)

		assert.Equal(t, map[string]any{
			"model":  "agnes-image-2.5-flash",
			"prompt": "a cat",
			"size":   "1K",
			"seed":   float64(42),
		}, captured)
	})

	t.Run("reserved and credential-shaped keys are rejected", func(t *testing.T) {
		t.Parallel()

		var called bool

		model := newImageModel(t, func(w http.ResponseWriter, _ *http.Request) {
			called = true

			w.WriteHeader(http.StatusInternalServerError)
		})

		oversized := strings.Repeat("a", 33<<10)

		// Every key a typed field owns is reserved, so an extra can neither
		// override a value nor introduce a second one for the same parameter.
		for _, extra := range []map[string]any{
			{"model": "agnes-image-2.0-flash"},
			{"prompt": "another cat"},
			{"size": "4K"},
			{"ratio": "16:9"},
			{"return_base64": true},
			{"extra_body": map[string]any{"response_format": "url"}},
			{"response_format": "url"},
			{"api_key": "sk-leak"},
			{"note": oversized},
		} {
			_, err := model.GenerateImages(t.Context(), ai.ImageRequest{
				Prompt:          "a cat",
				Size:            "1K",
				ProviderOptions: agnesOptions(agnes.ImageOptions{ExtraFields: extra}),
			})
			require.ErrorIs(t, err, jsonx.ErrUnsafeExtension, "extra %v", extra)
			assert.False(t, called, "an invalid request must not reach the API")
		}
	})
}

func TestImageOptionsLookup(t *testing.T) {
	t.Parallel()

	t.Run("the openai key stays a fallback", func(t *testing.T) {
		t.Parallel()

		var captured map[string]any

		model := newImageModel(t, serveJSON(t, agnesInlineResponse, "/v1/images/generations", &captured))

		_, err := model.GenerateImages(t.Context(), ai.ImageRequest{
			Prompt: "a cat",
			Size:   "1K",
			ProviderOptions: map[ai.Provider]any{
				ai.ProviderOpenAI: agnes.ImageOptions{Ratio: "9:16"},
			},
		})
		require.NoError(t, err)

		assert.Equal(t, "9:16", captured["ratio"])
	})

	t.Run("a foreign options type is ignored", func(t *testing.T) {
		t.Parallel()

		var captured map[string]any

		model := newImageModel(t, serveJSON(t, agnesInlineResponse, "/v1/images/generations", &captured))

		_, err := model.GenerateImages(t.Context(), ai.ImageRequest{
			Prompt: "a cat",
			Size:   "1K",
			ProviderOptions: map[ai.Provider]any{
				ai.ProviderAgnes: struct{ Ratio string }{Ratio: "9:16"},
			},
		})
		require.NoError(t, err)

		assert.NotContains(t, captured, "ratio")
	})
}

func TestImageGenerationReadsAPIKeyFromEnv(t *testing.T) {
	t.Setenv("AGNES_API_KEY", "sk-env")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer sk-env", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(agnesURLResponse))
	}))
	t.Cleanup(server.Close)

	model := agnes.NewImageModel(
		"agnes-image-2.5-flash",
		agnes.WithBaseURL(server.URL+"/v1"),
		agnes.WithAllowHTTP(),
		agnes.WithAllowPrivateIPs(),
	)

	_, err := model.GenerateImages(t.Context(), ai.ImageRequest{Prompt: "a cat", Size: "1K"})
	require.NoError(t, err)
}

func TestImageModelIdentity(t *testing.T) {
	t.Parallel()

	img := agnes.NewImageModel("agnes-image-2.5-flash", agnes.WithAPIKey("x"))
	assert.Equal(t, ai.ProviderAgnes, img.Provider())
	assert.Equal(t, "agnes-image-2.5-flash", img.ModelID())
	assert.True(t, img.Capabilities().ImageGeneration)

	// The optional image capabilities are discovered by type assertion.
	var (
		_ ai.ImageModel  = img
		_ ai.ImageEditor = img
	)

	overridden := agnes.NewImageModel("agnes-image-2.5-flash",
		agnes.WithAPIKey("x"),
		agnes.WithCapabilities(ai.Capabilities{Text: true}),
	)
	assert.True(t, overridden.Capabilities().Text)
	assert.False(t, overridden.Capabilities().ImageGeneration)
}

// agnesOptions wraps ImageOptions in the ProviderOptions map.
func agnesOptions(opts agnes.ImageOptions) map[ai.Provider]any {
	return map[ai.Provider]any{ai.ProviderAgnes: opts}
}
