package openai_test

import (
	"net/http"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// imageVariationResponse is a dall-e-2 variation response: hosted links by
// default.
const imageVariationResponse = `{"created":1713833628,"data":[{"url":"https://images.example.test/variation.png"}]}`

func TestImageVariationsWireFormat(t *testing.T) {
	t.Parallel()

	var gotParts []multipartPart

	model := newImageModel(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/images/variations", r.URL.Path)
		gotParts = readMultipartParts(t, r)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(imageVariationResponse))
	}, "dall-e-2")

	resp, err := model.CreateVariations(t.Context(), ai.ImageVariationRequest{
		Image: imagePart([]byte("pixels")),
		N:     2,
		Size:  "512x512",
		ProviderOptions: imageOptionsWith(openai.ImageOptions{
			ResponseFormat: "url",
			User:           "user-1234",
		}),
	})
	require.NoError(t, err)

	require.Len(t, gotParts, 6)
	assert.Equal(t, multipartPart{field: "model", body: "dall-e-2"}, gotParts[0])
	assert.Equal(t, multipartPart{field: "n", body: "2"}, gotParts[1])
	assert.Equal(t, multipartPart{field: "size", body: "512x512"}, gotParts[2])
	assert.Equal(t, multipartPart{field: "response_format", body: "url"}, gotParts[3])
	assert.Equal(t, multipartPart{field: "user", body: "user-1234"}, gotParts[4])
	assert.Equal(t, multipartPart{
		field: "image", filename: "image.png", contentType: "image/png", body: "pixels",
	}, gotParts[5])

	require.Len(t, resp.Images, 1)
	assert.Equal(t, "https://images.example.test/variation.png", resp.Images[0].URL)
	assert.Equal(t, "image/png", resp.Images[0].MIMEType)
	assert.Empty(t, resp.Images[0].Data)
}

func TestImageVariationsValidateLocally(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		req  ai.ImageVariationRequest
	}{
		{
			name: "url source",
			req:  ai.ImageVariationRequest{Image: ai.ImagePart{Source: ai.MediaSource{URL: "https://example.test/a.png"}}},
		},
		{name: "file id source", req: ai.ImageVariationRequest{Image: ai.ImagePart{Source: ai.MediaSource{ID: "file-a"}}}},
		{name: "no source", req: ai.ImageVariationRequest{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var called bool

			model := newImageModel(t, func(w http.ResponseWriter, _ *http.Request) {
				called = true

				w.WriteHeader(http.StatusInternalServerError)
			}, "dall-e-2")

			_, err := model.CreateVariations(t.Context(), tc.req)
			require.ErrorIs(t, err, ai.ErrInvalidRequest)
			assert.False(t, called, "an invalid request must not reach the API")
		})
	}
}
