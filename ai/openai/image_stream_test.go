package openai_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// imageGenerationStream is a full generations stream: two partial images, then
// the completion. Payloads follow the documented event examples, which omit
// fields the schema marks required.
const imageGenerationStream = `event: image_generation.partial_image
data: {"type":"image_generation.partial_image","b64_json":"cGFydC0w","created_at":1713833628,"size":"1024x1024","quality":"high","background":"auto","output_format":"webp","partial_image_index":0}

event: image_generation.partial_image
data: {"type":"image_generation.partial_image","b64_json":"cGFydC0x","partial_image_index":1}

event: image_generation.completed
data: {"type":"image_generation.completed","b64_json":"cGl4ZWxz","usage":{"total_tokens":100,"input_tokens":50,"output_tokens":50,"input_tokens_details":{"text_tokens":10,"image_tokens":40},"output_tokens_details":{"text_tokens":0,"image_tokens":100}}}
`

// imageEditStream is the edits namespace of the same dialect.
const imageEditStream = `event: image_edit.partial_image
data: {"type":"image_edit.partial_image","b64_json":"cGFydC0w","output_format":"webp","partial_image_index":0}

event: image_edit.completed
data: {"type":"image_edit.completed","b64_json":"cGl4ZWxz","usage":{"total_tokens":100,"input_tokens":50,"output_tokens":50,"input_tokens_details":{"text_tokens":10,"image_tokens":40}}}
`

// imageGenerationStreamNoisy interleaves events the endpoint does not define
// with a healthy generation stream.
const imageGenerationStreamNoisy = `event: response.created
data: {"type":"response.created","response":{"id":"resp_1"}}

event: image_generation.partial_image
data: {"type":"image_generation.partial_image","b64_json":"cGFydC0w","partial_image_index":0}

data: {"type":"image_edit.partial_image","b64_json":"cGFydC0x","partial_image_index":4}

event: image_generation.in_progress
data: {"type":"image_generation.in_progress"}

data: [DONE]

event: image_generation.completed
data: {"type":"image_generation.completed","b64_json":"cGl4ZWxz"}
`

// imageGenerationStreamBroken fails after the first partial image.
const imageGenerationStreamBroken = `event: image_generation.partial_image
data: {"type":"image_generation.partial_image","b64_json":"cGFydC0w","partial_image_index":0}

data: {"type":"image_generation.partial_image",
`

func collectImageStream(stream ai.ImageStream) ([]ai.ImageStreamEvent, []error) {
	var (
		events []ai.ImageStreamEvent
		errs   []error
	)

	for event, err := range stream {
		if err != nil {
			errs = append(errs, err)

			continue
		}

		events = append(events, event)
	}

	return events, errs
}

func TestImageGenerationStream(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newImageModel(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "text/event-stream", r.Header.Get("Accept"))
		assert.NoError(t, decodeBody(r, &captured))

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(imageGenerationStream))
	}, "gpt-image-1")

	partials := 2

	events, errs := collectImageStream(model.StreamImages(t.Context(), ai.ImageRequest{
		Prompt:          "a sea otter",
		OutputFormat:    "webp",
		ProviderOptions: imageOptionsWith(openai.ImageOptions{PartialImages: &partials}),
	}))
	require.Empty(t, errs)

	assert.Equal(t, true, captured["stream"])
	assert.InDelta(t, 2, as[float64](t, captured["partial_images"]), 1e-9)

	require.Len(t, events, 3)

	assert.Equal(t, ai.ImageStreamPartial, events[0].Type)
	assert.Equal(t, 0, events[0].Index)
	assert.Equal(t, []byte("part-0"), events[0].Image.Data)
	assert.Equal(t, "image/webp", events[0].Image.MIMEType)
	assert.Nil(t, events[0].Usage)

	assert.Equal(t, ai.ImageStreamPartial, events[1].Type)
	assert.Equal(t, 1, events[1].Index)
	assert.Equal(t, []byte("part-1"), events[1].Image.Data)
	// The second event omits output_format; the requested format still applies.
	assert.Equal(t, "image/webp", events[1].Image.MIMEType)

	last := events[2]
	assert.Equal(t, ai.ImageStreamCompleted, last.Type)
	assert.Zero(t, last.Index)
	assert.Equal(t, []byte("pixels"), last.Image.Data)
	assert.Equal(t, "image/webp", last.Image.MIMEType)
	require.NotNil(t, last.Usage)
	assert.Equal(t, 100, last.Usage.TotalTokens)
	assert.Equal(t, 50, last.Usage.InputTokens)
	assert.Equal(t, 10, last.Usage.InputTextTokens)
	assert.Equal(t, 40, last.Usage.InputImageTokens)
	assert.Equal(t, 100, last.Usage.OutputImageTokens)
}

func TestImageEditStreamUsesJSONEncoding(t *testing.T) {
	t.Parallel()

	var captured map[string]any

	model := newImageModel(t, func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, decodeBody(r, &captured))

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(imageEditStream))
	}, "gpt-image-1")

	events, errs := collectImageStream(model.StreamImageEdits(t.Context(), ai.ImageEditRequest{
		Prompt: "add a hat",
		Images: []ai.ImagePart{{Source: ai.MediaSource{URL: "https://example.test/source.png"}}},
	}))
	require.Empty(t, errs)

	assert.Equal(t, true, captured["stream"])
	assert.Equal(t, "gpt-image-1", captured["model"])

	require.Len(t, events, 2)
	assert.Equal(t, ai.ImageStreamPartial, events[0].Type)
	assert.Equal(t, []byte("part-0"), events[0].Image.Data)
	assert.Equal(t, "image/webp", events[0].Image.MIMEType)
	assert.Equal(t, ai.ImageStreamCompleted, events[1].Type)
	assert.Equal(t, []byte("pixels"), events[1].Image.Data)
	require.NotNil(t, events[1].Usage)
	assert.Equal(t, 100, events[1].Usage.TotalTokens)
}

func TestImageEditStreamUsesMultipartEncoding(t *testing.T) {
	t.Parallel()

	var gotParts []multipartPart

	model := newImageModel(t, func(w http.ResponseWriter, r *http.Request) {
		gotParts = readMultipartParts(t, r)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(imageEditStream))
	}, "gpt-image-1")

	events, errs := collectImageStream(model.StreamImageEdits(t.Context(), ai.ImageEditRequest{
		Prompt: "add a hat",
		Images: []ai.ImagePart{imagePart([]byte("pixels"))},
	}))
	require.Empty(t, errs)

	require.Len(t, gotParts, 4)
	assert.Equal(t, multipartPart{field: "model", body: "gpt-image-1"}, gotParts[0])
	assert.Equal(t, multipartPart{field: "prompt", body: "add a hat"}, gotParts[1])
	assert.Equal(t, multipartPart{field: "stream", body: "true"}, gotParts[2])
	assert.Equal(t, multipartPart{
		field: "image[]", filename: "image-0.png", contentType: "image/png", body: "pixels",
	}, gotParts[3])

	require.Len(t, events, 2)
	assert.Equal(t, ai.ImageStreamCompleted, events[1].Type)
}

func TestImageStreamIgnoresUnknownEvents(t *testing.T) {
	t.Parallel()

	model := newImageModel(t, serveSSE(t, imageGenerationStreamNoisy), "gpt-image-1")

	events, errs := collectImageStream(model.StreamImages(t.Context(), ai.ImageRequest{Prompt: "a sea otter"}))
	require.Empty(t, errs)

	// Foreign namespaces, unknown event types, and the sentinel are dropped.
	require.Len(t, events, 2)
	assert.Equal(t, ai.ImageStreamPartial, events[0].Type)
	assert.Equal(t, 0, events[0].Index)
	assert.Equal(t, ai.ImageStreamCompleted, events[1].Type)
	assert.Equal(t, []byte("pixels"), events[1].Image.Data)
}

func TestImageStreamFallsBackToEventName(t *testing.T) {
	t.Parallel()

	model := newImageModel(t, serveSSE(t, `event: image_generation.completed
data: {"b64_json":"cGl4ZWxz"}

`), "gpt-image-1")

	events, errs := collectImageStream(model.StreamImages(t.Context(), ai.ImageRequest{Prompt: "a sea otter"}))
	require.Empty(t, errs)

	require.Len(t, events, 1)
	assert.Equal(t, ai.ImageStreamCompleted, events[0].Type)
	assert.Equal(t, []byte("pixels"), events[0].Image.Data)
	assert.Nil(t, events[0].Usage)
}

func TestImageStreamSetupFailureIsRetryable(t *testing.T) {
	t.Parallel()

	model := newImageModel(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"overloaded","type":"service_unavailable_error"}}`))
	}, "gpt-image-1")

	events, errs := collectImageStream(model.StreamImages(t.Context(), ai.ImageRequest{Prompt: "a sea otter"}))
	assert.Empty(t, events)
	require.Len(t, errs, 1)
	require.ErrorIs(t, errs[0], ai.ErrOverloaded)
	assert.True(t, ai.IsRetryable(errs[0]))

	var apiErr *ai.Error
	require.ErrorAs(t, errs[0], &apiErr)
	assert.Equal(t, http.StatusServiceUnavailable, apiErr.StatusCode)
	assert.Equal(t, 3*time.Second, apiErr.RetryAfter)
}

func TestImageStreamFailureAfterOutputIsTerminal(t *testing.T) {
	t.Parallel()

	model := newImageModel(t, serveSSE(t, imageGenerationStreamBroken), "gpt-image-1")

	events, errs := collectImageStream(model.StreamImages(t.Context(), ai.ImageRequest{Prompt: "a sea otter"}))

	// The delivered partial image is not replayed, and the stream stops.
	require.Len(t, events, 1)
	assert.Equal(t, ai.ImageStreamPartial, events[0].Type)
	assert.Equal(t, []byte("part-0"), events[0].Image.Data)
	require.Len(t, errs, 1)
	assert.ErrorContains(t, errs[0], "decoding event")
}

func TestImageStreamCompletedRequiresImage(t *testing.T) {
	t.Parallel()

	model := newImageModel(t, serveSSE(t, `event: image_generation.completed
data: {"type":"image_generation.completed","usage":{"total_tokens":100,"input_tokens":50,"output_tokens":50,"input_tokens_details":{"text_tokens":10,"image_tokens":40}}}

`), "gpt-image-1")

	events, errs := collectImageStream(model.StreamImages(t.Context(), ai.ImageRequest{Prompt: "a sea otter"}))

	// No stream event carries a url, so a byte-less completion cannot deliver
	// the final image and must fail instead of yielding a zero-value image.
	assert.Empty(t, events)
	require.Len(t, errs, 1)
	assert.ErrorContains(t, errs[0], "completed event carries no image")
}

func TestImageStreamValidatesLocally(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		start func(*openai.ImageModel) ai.ImageStream
	}{
		{
			name: "generations",
			start: func(model *openai.ImageModel) ai.ImageStream {
				return model.StreamImages(t.Context(), ai.ImageRequest{})
			},
		},
		{
			name: "edits",
			start: func(model *openai.ImageModel) ai.ImageStream {
				return model.StreamImageEdits(t.Context(), ai.ImageEditRequest{Prompt: "add a hat"})
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

			events, errs := collectImageStream(tc.start(model))
			assert.Empty(t, events)
			require.Len(t, errs, 1)
			require.ErrorIs(t, errs[0], ai.ErrInvalidRequest)
			assert.False(t, called, "an invalid request must not reach the API")
		})
	}
}
