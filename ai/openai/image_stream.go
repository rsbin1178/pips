package openai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/jsonx"
	"github.com/rsbin1178/pips/ai/internal/sse"
)

// Image SSE namespaces, event-type suffixes, and stream labels.
const (
	imageGenerationEventNamespace = "image_generation."
	imageEditEventNamespace       = "image_edit."

	imagePartialEventSuffix   = "partial_image"
	imageCompletedEventSuffix = "completed"

	imageGenerationStreamLabel = "images generations stream"
	imageEditStreamLabel       = "images edits stream"
)

// imageStreamPayload is the wire shape shared by the partial and completed
// image stream events. Only the fields this adapter maps are decoded; the
// documented example payloads omit others the schema marks required, so a
// missing field stays a zero value rather than an error.
type imageStreamPayload struct {
	Type              string      `json:"type"`
	B64JSON           string      `json:"b64_json"`
	OutputFormat      string      `json:"output_format"`
	PartialImageIndex *int        `json:"partial_image_index"`
	Usage             *imageUsage `json:"usage"`
}

// StreamImages implements ai.ImageStreamer. It sets stream and sends
// [ImageOptions.PartialImages] when configured.
func (m *ImageModel) StreamImages(ctx context.Context, req ai.ImageRequest) ai.ImageStream {
	opts := imageOptions(req.ProviderOptions, m.model.provider)

	body, err := m.imageGenerationBody(req, opts, true)

	return func(yield func(ai.ImageStreamEvent, error) bool) {
		if err != nil {
			yield(ai.ImageStreamEvent{}, err)
			return
		}

		m.model.runImageStream(imageGenerationStreamLabel, imageGenerationEventNamespace, req.OutputFormat, yield, func() (io.ReadCloser, error) {
			return m.model.client.PostStream(ctx, imagesPath, m.model.authHeaders(), body, m.model.decodeError)
		})
	}
}

// StreamImageEdits implements ai.ImageStreamer. The request uses the same
// [ImageOptions.EditEncoding] selection as [ImageModel.EditImage].
func (m *ImageModel) StreamImageEdits(ctx context.Context, req ai.ImageEditRequest) ai.ImageStream {
	opts := imageOptions(req.ProviderOptions, m.model.provider)

	open, err := m.editStreamOpener(ctx, req, opts)

	return func(yield func(ai.ImageStreamEvent, error) bool) {
		if err != nil {
			yield(ai.ImageStreamEvent{}, err)
			return
		}

		m.model.runImageStream(imageEditStreamLabel, imageEditEventNamespace, req.OutputFormat, yield, open)
	}
}

// editStreamOpener prepares the streaming POST for the selected edit encoding.
func (m *ImageModel) editStreamOpener(ctx context.Context, req ai.ImageEditRequest, opts ImageOptions) (func() (io.ReadCloser, error), error) {
	if err := validateImageEditRequest(req, opts); err != nil {
		return nil, err
	}

	if editEncoding(req, opts) == EditEncodingJSON {
		body, err := m.editJSONBody(req, opts, true)
		if err != nil {
			return nil, err
		}

		return func() (io.ReadCloser, error) {
			return m.model.client.PostStream(ctx, imageEditsPath, m.model.authHeaders(), body, m.model.decodeError)
		}, nil
	}

	fields, files, err := m.editMultipartParts(req, opts, true)
	if err != nil {
		return nil, err
	}

	return func() (io.ReadCloser, error) {
		return m.model.client.PostMultipartStream(ctx, imageEditsPath, m.model.authHeaders(), fields, files, m.model.decodeError)
	}, nil
}

// runImageStream opens one image SSE response and emits its events. The
// request is only sent on first iteration, so a setup failure surfaces as a
// retryable error event instead of blocking the call. requestedFormat is the
// caller's requested output format, used when an event omits its own.
func (m *Model) runImageStream(label, namespace, requestedFormat string, yield func(ai.ImageStreamEvent, error) bool, open func() (io.ReadCloser, error)) {
	stream, err := open()
	if err != nil {
		yield(ai.ImageStreamEvent{}, fmt.Errorf("%s: %s: %w", m.label(), label, err))
		return
	}

	defer stream.Close() //nolint:errcheck // best-effort cleanup on all exit paths

	emitImageStream(namespace, requestedFormat, newSSEParser(stream, m.client.MaxStreamLineSize()), yield)
}

// emitImageStream maps the image SSE dialect onto portable events. Events
// outside the endpoint's namespace, and unknown events inside it, are ignored.
func emitImageStream(namespace, requestedFormat string, events eventSource, yield func(ai.ImageStreamEvent, error) bool) {
	for event, err := range events {
		if err != nil {
			yield(ai.ImageStreamEvent{}, err)
			return
		}

		parsed, ok, err := parseImageStreamEvent(namespace, event)
		if err != nil {
			yield(ai.ImageStreamEvent{}, err)
			return
		}

		if !ok {
			continue
		}

		if !emitImageEvent(namespace, requestedFormat, parsed, yield) {
			return
		}
	}
}

// parseImageStreamEvent identifies one image event by its SSE event name and
// its JSON type. It reports false for anything the endpoint does not define.
func parseImageStreamEvent(namespace string, event sse.Event) (imageStreamPayload, bool, error) {
	data := strings.TrimSpace(event.Data)
	if data == "" || !strings.HasPrefix(data, "{") {
		// No sentinel is documented for these streams; skip anything that is
		// not a JSON object rather than failing a healthy stream.
		return imageStreamPayload{}, false, nil
	}

	var parsed imageStreamPayload

	if err := jsonx.Unmarshal([]byte(data), &parsed); err != nil {
		return imageStreamPayload{}, false, fmt.Errorf("openai: images stream: decoding event: %w", err)
	}

	name := parsed.Type
	if name == "" {
		// The docs' curl examples omit the "event:" line; the JSON type then
		// carries the identity.
		name = event.Type
	}

	switch name {
	case namespace + imagePartialEventSuffix, namespace + imageCompletedEventSuffix:
		parsed.Type = name

		return parsed, true, nil
	default:
		return imageStreamPayload{}, false, nil
	}
}

// emitImageEvent yields the portable event for one parsed image event,
// reporting false when the consumer stopped iterating.
func emitImageEvent(namespace, requestedFormat string, parsed imageStreamPayload, yield func(ai.ImageStreamEvent, error) bool) bool {
	if parsed.Type == namespace+imagePartialEventSuffix {
		return emitPartialImage(parsed, requestedFormat, yield)
	}

	event := ai.ImageStreamEvent{Type: ai.ImageStreamCompleted}

	if parsed.B64JSON == "" {
		// The completed event is the stream's image carrier, and no stream
		// event has a url field, so a payload without bytes would deliver a
		// zero-value image as the terminal result.
		yield(ai.ImageStreamEvent{}, errors.New("openai: images stream: completed event carries no image"))

		return false
	}

	image, err := imageFromDatum(imageDatum{B64JSON: parsed.B64JSON}, imageMIMEFor(parsed.OutputFormat, requestedFormat), 0)
	if err != nil {
		yield(ai.ImageStreamEvent{}, fmt.Errorf("openai: images stream: %w", err))

		return false
	}

	event.Image = image

	if parsed.Usage != nil {
		usage := imageUsageFrom(parsed.Usage)
		event.Usage = &usage
	}

	return yield(event, nil)
}

// emitPartialImage yields one partial image event. A partial event without
// image bytes carries nothing to deliver and is skipped.
func emitPartialImage(parsed imageStreamPayload, requestedFormat string, yield func(ai.ImageStreamEvent, error) bool) bool {
	if parsed.B64JSON == "" {
		return true
	}

	index := 0
	if parsed.PartialImageIndex != nil {
		index = *parsed.PartialImageIndex
	}

	image, err := imageFromDatum(imageDatum{B64JSON: parsed.B64JSON}, imageMIMEFor(parsed.OutputFormat, requestedFormat), index)
	if err != nil {
		yield(ai.ImageStreamEvent{}, fmt.Errorf("openai: images stream: %w", err))

		return false
	}

	return yield(ai.ImageStreamEvent{Type: ai.ImageStreamPartial, Index: index, Image: image}, nil)
}
