package agnes_test

import (
	"net/http"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/agnes"
	"github.com/rsbin1178/pips/ai/internal/jsonx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImageEditWireBody(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	// Edits hit the generations endpoint; there is no separate edits path.
	model := newImageModel(t, serveJSON(t, agnesURLResponse, "/v1/images/generations", &captured))

	resp, err := model.EditImage(t.Context(), ai.ImageEditRequest{
		Prompt: "merge them",
		Size:   "2K",
		Images: []ai.ImagePart{
			{Source: ai.MediaSource{URL: "https://images.example.test/otter.png"}},
			{Source: ai.MediaSource{MIMEType: "image/jpeg", Data: []byte("pixels")}},
			{Source: ai.MediaSource{Data: []byte("pixels")}},
		},
		ProviderOptions: agnesOptions(agnes.ImageOptions{Ratio: "4:3", ReturnBase64: true}),
	})
	require.NoError(t, err)

	// return_base64 is documented for generation only, so the edit body must
	// not carry it even when the option is set.
	assert.Equal(t, map[string]any{
		"model":  "agnes-image-2.5-flash",
		"prompt": "merge them",
		"size":   "2K",
		"ratio":  "4:3",
		"extra_body": map[string]any{
			"image": []any{
				"https://images.example.test/otter.png",
				"data:image/jpeg;base64,cGl4ZWxz",
				"data:image/png;base64,cGl4ZWxz",
			},
		},
	}, captured)

	require.Len(t, resp.Images, 1)
	assert.Equal(t, "https://storage.example.test/agnes/otter.png", resp.Images[0].URL)
}

func TestImageEditValidatesLocally(t *testing.T) {
	t.Parallel()

	valid := ai.ImagePart{Source: ai.MediaSource{MIMEType: "image/png", Data: []byte("pixels")}}

	cases := []struct {
		name string
		req  ai.ImageEditRequest
		want error
	}{
		{
			name: "empty prompt",
			req:  ai.ImageEditRequest{Images: []ai.ImagePart{valid}},
			want: ai.ErrInvalidRequest,
		},
		{
			name: "no source images",
			req:  ai.ImageEditRequest{Prompt: "merge them"},
			want: ai.ErrInvalidRequest,
		},
		{
			name: "file ID source",
			req: ai.ImageEditRequest{
				Prompt: "merge them",
				Images: []ai.ImagePart{{Source: ai.MediaSource{ID: "file-123"}}},
			},
			want: ai.ErrUnsupported,
		},
		{
			name: "empty source",
			req: ai.ImageEditRequest{
				Prompt: "merge them",
				Images: []ai.ImagePart{{}},
			},
			want: ai.ErrInvalidRequest,
		},
		{
			name: "ambiguous source",
			req: ai.ImageEditRequest{
				Prompt: "merge them",
				Images: []ai.ImagePart{{Source: ai.MediaSource{URL: "https://x.test/a.png", Data: []byte("pixels")}}},
			},
			want: ai.ErrInvalidRequest,
		},
		{
			name: "file ID alongside an expressible source",
			req: ai.ImageEditRequest{
				Prompt: "merge them",
				Images: []ai.ImagePart{{Source: ai.MediaSource{
					ID:  "file-123",
					URL: "https://x.test/a.png",
				}}},
			},
			want: ai.ErrUnsupported,
		},
		{
			name: "mask",
			req: ai.ImageEditRequest{
				Prompt: "merge them",
				Images: []ai.ImagePart{valid},
				Mask:   &ai.ImagePart{Source: ai.MediaSource{MIMEType: "image/png", Data: []byte("pixels")}},
			},
			want: ai.ErrUnsupported,
		},
		{
			name: "more than one image",
			req: ai.ImageEditRequest{
				Prompt: "merge them",
				Images: []ai.ImagePart{valid},
				N:      2,
			},
			want: ai.ErrUnsupported,
		},
		{
			name: "negative count",
			req: ai.ImageEditRequest{
				Prompt: "merge them",
				Images: []ai.ImagePart{valid},
				N:      -1,
			},
			want: ai.ErrInvalidRequest,
		},
		{
			name: "ratio outside the documented set",
			req: ai.ImageEditRequest{
				Prompt:          "merge them",
				Images:          []ai.ImagePart{valid},
				ProviderOptions: agnesOptions(agnes.ImageOptions{Ratio: "7:5"}),
			},
			want: ai.ErrInvalidRequest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var called bool

			model := newImageModel(t, func(w http.ResponseWriter, _ *http.Request) {
				called = true

				w.WriteHeader(http.StatusInternalServerError)
			})

			_, err := model.EditImage(t.Context(), tc.req)
			require.ErrorIs(t, err, tc.want)
			assert.False(t, called, "an invalid request must not reach the API")
		})
	}
}

func TestImageEditExtraFields(t *testing.T) {
	t.Parallel()

	var called bool

	model := newImageModel(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true

		w.WriteHeader(http.StatusInternalServerError)
	})

	_, err := model.EditImage(t.Context(), ai.ImageEditRequest{
		Prompt: "merge them",
		Images: []ai.ImagePart{{Source: ai.MediaSource{MIMEType: "image/png", Data: []byte("pixels")}}},
		ProviderOptions: agnesOptions(agnes.ImageOptions{
			ExtraFields: map[string]any{"image": []any{"https://x.test/a.png"}},
		}),
	})
	require.ErrorIs(t, err, jsonx.ErrUnsafeExtension)
	assert.False(t, called)
}
