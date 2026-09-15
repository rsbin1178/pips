package openai_test

import (
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/jsonx"
	"github.com/rsbin1178/pips/ai/openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// multipartPart is one decoded multipart request part.
type multipartPart struct {
	field       string
	filename    string
	contentType string
	body        string
}

// readMultipartParts drains a multipart request body into ordered parts. It
// reports failures with assert so it stays safe to call from an HTTP handler
// goroutine.
func readMultipartParts(t *testing.T, r *http.Request) []multipartPart {
	t.Helper()

	var parts []multipartPart

	reader, err := r.MultipartReader()
	if err != nil {
		assert.Fail(t, "multipart reader", "%v", err)

		return nil
	}

	for {
		part, partErr := reader.NextPart()
		if errors.Is(partErr, io.EOF) {
			break
		}

		if partErr != nil {
			assert.Fail(t, "reading multipart part", "%v", partErr)

			return parts
		}

		data, readErr := io.ReadAll(part)
		if readErr != nil {
			assert.Fail(t, "reading multipart part body", "%v", readErr)

			return parts
		}

		parts = append(parts, multipartPart{
			field:       part.FormName(),
			filename:    part.FileName(),
			contentType: part.Header.Get("Content-Type"),
			body:        string(data),
		})
	}

	return parts
}

// imagePart builds an inline source image part.
func imagePart(data []byte) ai.ImagePart {
	return ai.ImagePart{Source: ai.MediaSource{MIMEType: "image/png", Data: data}}
}

func TestImageEditMultipartWireFormat(t *testing.T) {
	t.Parallel()

	var (
		gotParts []multipartPart
		gotPath  string
	)

	model := newImageModel(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotParts = readMultipartParts(t, r)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(imageInlineResponse))
	}, "gpt-image-1")

	compression := 90

	resp, err := model.EditImage(t.Context(), ai.ImageEditRequest{
		Prompt:  "add a hat",
		Images:  []ai.ImagePart{imagePart([]byte("first")), imagePart([]byte("second"))},
		Mask:    &ai.ImagePart{Source: ai.MediaSource{MIMEType: "image/png", Data: []byte("mask")}},
		N:       2,
		Size:    "1024x1024",
		Quality: "high",

		OutputFormat: "webp",
		ProviderOptions: imageOptionsWith(openai.ImageOptions{
			Background:        "opaque",
			InputFidelity:     "high",
			OutputCompression: &compression,
			User:              "user-1234",
		}),
	})
	require.NoError(t, err)

	assert.Equal(t, "/v1/images/edits", gotPath)

	// Text fields are ordinary form fields in a stable order; unset optional
	// parameters are omitted entirely.
	require.Len(t, gotParts, 13)
	assert.Equal(t, multipartPart{field: "model", body: "gpt-image-1"}, gotParts[0])
	assert.Equal(t, multipartPart{field: "prompt", body: "add a hat"}, gotParts[1])
	assert.Equal(t, multipartPart{field: "n", body: "2"}, gotParts[2])
	assert.Equal(t, multipartPart{field: "size", body: "1024x1024"}, gotParts[3])
	assert.Equal(t, multipartPart{field: "quality", body: "high"}, gotParts[4])
	assert.Equal(t, multipartPart{field: "output_format", body: "webp"}, gotParts[5])
	assert.Equal(t, multipartPart{field: "output_compression", body: "90"}, gotParts[6])
	assert.Equal(t, multipartPart{field: "background", body: "opaque"}, gotParts[7])
	assert.Equal(t, multipartPart{field: "input_fidelity", body: "high"}, gotParts[8])
	assert.Equal(t, multipartPart{field: "user", body: "user-1234"}, gotParts[9])

	// Every source image is a repeated "image[]" file part; the mask is its
	// own part.
	assert.Equal(t, multipartPart{
		field: "image[]", filename: "image-0.png", contentType: "image/png", body: "first",
	}, gotParts[10])
	assert.Equal(t, multipartPart{
		field: "image[]", filename: "image-1.png", contentType: "image/png", body: "second",
	}, gotParts[11])
	assert.Equal(t, multipartPart{
		field: "mask", filename: "mask.png", contentType: "image/png", body: "mask",
	}, gotParts[12])

	require.Len(t, resp.Images, 1)
	assert.Equal(t, []byte("pixels"), resp.Images[0].Data)
	assert.Equal(t, "image/webp", resp.Images[0].MIMEType)
	assert.Equal(t, "transparent", resp.Background)
	assert.Equal(t, 100, resp.Usage.TotalTokens)
}

func TestImageEditMultipartFiles(t *testing.T) {
	t.Parallel()

	var gotParts []multipartPart

	model := newImageModel(t, func(w http.ResponseWriter, r *http.Request) {
		gotParts = readMultipartParts(t, r)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(imageMinimalResponse))
	}, "gpt-image-1")

	_, err := model.EditImage(t.Context(), ai.ImageEditRequest{
		Prompt: "add a hat",
		Images: []ai.ImagePart{
			imagePart([]byte("first")),
			{Source: ai.MediaSource{MIMEType: "image/webp", Data: []byte("second")}},
		},
		Mask: &ai.ImagePart{Source: ai.MediaSource{Data: []byte("mask")}},
	})
	require.NoError(t, err)

	require.Len(t, gotParts, 5)
	assert.Equal(t, multipartPart{field: "model", body: "gpt-image-1"}, gotParts[0])
	assert.Equal(t, multipartPart{field: "prompt", body: "add a hat"}, gotParts[1])
	assert.Equal(t, multipartPart{
		field: "image[]", filename: "image-0.png", contentType: "image/png", body: "first",
	}, gotParts[2])
	assert.Equal(t, multipartPart{
		field: "image[]", filename: "image-1.webp", contentType: "image/webp", body: "second",
	}, gotParts[3])
	// A mask without a media type is treated as PNG.
	assert.Equal(t, multipartPart{
		field: "mask", filename: "mask.png", contentType: "image/png", body: "mask",
	}, gotParts[4])
}

func TestImageEditJSONWireFormat(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newImageModel(t, serveJSON(t, imageMinimalResponse, "/v1/images/edits", &captured), "gpt-image-1")

	partials := 1

	_, err := model.EditImage(t.Context(), ai.ImageEditRequest{
		Prompt: "add a watercolor effect",
		Images: []ai.ImagePart{
			{Source: ai.MediaSource{URL: "https://example.test/source.png"}},
			{Source: ai.MediaSource{ID: "file-abc123"}},
		},
		Mask: &ai.ImagePart{Source: ai.MediaSource{MIMEType: "image/png", Data: []byte("pixels")}},
		N:    1,

		Size: "1024x1024",
		ProviderOptions: imageOptionsWith(openai.ImageOptions{
			InputFidelity: "low",
			PartialImages: &partials,
		}),
	})
	require.NoError(t, err)

	images := as[[]any](t, captured["images"])
	require.Len(t, images, 2)
	assert.Equal(t, map[string]any{"image_url": "https://example.test/source.png"}, as[map[string]any](t, images[0]))
	assert.Equal(t, map[string]any{"file_id": "file-abc123"}, as[map[string]any](t, images[1]))

	mask := as[map[string]any](t, captured["mask"])
	assert.Equal(t, "data:image/png;base64,"+base64.StdEncoding.EncodeToString([]byte("pixels")), mask["image_url"])

	assert.Equal(t, "gpt-image-1", captured["model"])
	assert.Equal(t, "add a watercolor effect", captured["prompt"])
	assert.Equal(t, "low", captured["input_fidelity"])
	assert.InDelta(t, 1, as[float64](t, captured["partial_images"]), 1e-9)
	assert.NotContains(t, captured, "stream")
	assert.NotContains(t, captured, "background")
}

func TestImageEditEncodingSelection(t *testing.T) {
	t.Parallel()

	inline := []ai.ImagePart{imagePart([]byte("pixels"))}
	remote := []ai.ImagePart{{Source: ai.MediaSource{URL: "https://example.test/source.png"}}}

	cases := []struct {
		name         string
		images       []ai.ImagePart
		encoding     openai.EditEncoding
		wantEncoding string
		wantError    error
	}{
		{name: "auto with inline bytes", images: inline, wantEncoding: "multipart/form-data"},
		{name: "auto with url sources", images: remote, wantEncoding: "application/json"},
		{name: "forced json with inline bytes", images: inline, encoding: openai.EditEncodingJSON, wantEncoding: "application/json"},
		{name: "forced multipart with url sources", images: remote, encoding: openai.EditEncodingMultipart, wantError: ai.ErrInvalidRequest},
		{name: "unknown encoding", images: inline, encoding: "compressed", wantError: ai.ErrInvalidRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var gotContentType string

			model := newImageModel(t, func(w http.ResponseWriter, r *http.Request) {
				gotContentType = r.Header.Get("Content-Type")

				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(imageMinimalResponse))
			}, "gpt-image-1")

			_, err := model.EditImage(t.Context(), ai.ImageEditRequest{
				Prompt:          "add a hat",
				Images:          tc.images,
				ProviderOptions: imageOptionsWith(openai.ImageOptions{EditEncoding: tc.encoding}),
			})

			if tc.wantError != nil {
				require.ErrorIs(t, err, tc.wantError)
				assert.Empty(t, gotContentType)

				return
			}

			require.NoError(t, err)
			assert.Contains(t, gotContentType, tc.wantEncoding)
		})
	}
}

func TestImageEditValidatesSourcesLocally(t *testing.T) {
	t.Parallel()

	tooMany := make([]ai.ImagePart, 17)
	for index := range tooMany {
		tooMany[index] = imagePart([]byte("pixels"))
	}

	cases := []struct {
		name string
		req  ai.ImageEditRequest
	}{
		{name: "no images", req: ai.ImageEditRequest{Prompt: "add a hat"}},
		{
			name: "too many images",
			req:  ai.ImageEditRequest{Prompt: "add a hat", Images: tooMany},
		},
		{
			name: "source with url and file id",
			req: ai.ImageEditRequest{
				Prompt: "add a hat",
				Images: []ai.ImagePart{{Source: ai.MediaSource{URL: "https://example.test/a.png", ID: "file-a"}}},
			},
		},
		{
			name: "source with no reference",
			req:  ai.ImageEditRequest{Prompt: "add a hat", Images: []ai.ImagePart{{}}},
		},
		{
			name: "mask with no reference",
			req: ai.ImageEditRequest{
				Prompt: "add a hat",
				Images: []ai.ImagePart{{Source: ai.MediaSource{URL: "https://example.test/a.png"}}},
				Mask:   &ai.ImagePart{},
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

			_, err := model.EditImage(t.Context(), tc.req)
			require.ErrorIs(t, err, ai.ErrInvalidRequest)
			assert.False(t, called, "an invalid request must not reach the API")
		})
	}
}

func TestImageEditMultipartExtraFields(t *testing.T) {
	t.Parallel()

	t.Run("scalar keys unset by typed fields are delivered", func(t *testing.T) {
		t.Parallel()

		var gotParts []multipartPart

		model := newImageModel(t, func(w http.ResponseWriter, r *http.Request) {
			gotParts = readMultipartParts(t, r)

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(imageMinimalResponse))
		}, "gpt-image-1")

		_, err := model.EditImage(t.Context(), ai.ImageEditRequest{
			Prompt: "add a hat",
			Images: []ai.ImagePart{imagePart([]byte("pixels"))},
			ProviderOptions: imageOptionsWith(openai.ImageOptions{ExtraFields: map[string]any{
				"background": "transparent",
				"seed":       7,
			}}),
		})
		require.NoError(t, err)

		// Extras follow the typed fields in a stable order; the typed
		// background field was unset, so the extra supplies it.
		require.Len(t, gotParts, 5)
		assert.Equal(t, multipartPart{field: "model", body: "gpt-image-1"}, gotParts[0])
		assert.Equal(t, multipartPart{field: "prompt", body: "add a hat"}, gotParts[1])
		assert.Equal(t, multipartPart{field: "background", body: "transparent"}, gotParts[2])
		assert.Equal(t, multipartPart{field: "seed", body: "7"}, gotParts[3])
		assert.Equal(t, multipartPart{
			field: "image[]", filename: "image-0.png", contentType: "image/png", body: "pixels",
		}, gotParts[4])
	})

	cases := []struct {
		name  string
		typed openai.ImageOptions
	}{
		{name: "nested value", typed: openai.ImageOptions{ExtraFields: map[string]any{"moderation_hint": map[string]any{"level": "low"}}}},
		{name: "reserved key", typed: openai.ImageOptions{ExtraFields: map[string]any{"model": "dall-e-2"}}},
		{
			name:  "collides with a set typed field",
			typed: openai.ImageOptions{Background: "opaque", ExtraFields: map[string]any{"background": "transparent"}},
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

			_, err := model.EditImage(t.Context(), ai.ImageEditRequest{
				Prompt:          "add a hat",
				Images:          []ai.ImagePart{imagePart([]byte("pixels"))},
				ProviderOptions: imageOptionsWith(tc.typed),
			})
			require.ErrorIs(t, err, jsonx.ErrUnsafeExtension)
			assert.False(t, called, "an invalid extension must not reach the API")
		})
	}
}
