package ai

import (
	"iter"
	"time"
)

// ImageRequest asks an [ImageModel] to generate images.
type ImageRequest struct {
	// Prompt describes the desired image.
	Prompt string
	// N is how many images to generate; zero means one.
	N int
	// Size is the provider-specific dimensions string (for example
	// "1024x1024"). Empty uses the provider default.
	Size string
	// Quality is the provider-specific quality tier (for example "high").
	// Empty uses the provider default.
	Quality string
	// OutputFormat is the desired encoding ("png", "jpeg", or "webp"). Empty
	// uses the provider default. Providers that emit a single format ignore
	// it.
	OutputFormat string

	// ProviderOptions passes provider-specific extensions, keyed like
	// [Request.ProviderOptions].
	ProviderOptions map[Provider]any
}

// GeneratedImage is one produced image. Data and URL are mutually exclusive:
// a provider returns inline bytes or a hosted link, not both.
type GeneratedImage struct {
	// Data is the inline image bytes; empty when the provider returned a URL.
	Data []byte
	// MIMEType is the image's media type (for example "image/webp"). It is
	// empty when the provider reported no usable format.
	MIMEType string
	// URL is the provider-hosted link to the image. It is temporary (OpenAI
	// keeps it valid for 60 minutes). The adapter never downloads it; fetch it
	// yourself if you need bytes.
	URL string
	// RevisedPrompt is the prompt the provider reports having actually used,
	// when it does (OpenAI's dall-e-3). It is empty for providers that do not
	// report one.
	RevisedPrompt string
}

// ImageUsage is token accounting for an image request, normalized across
// providers. The embedded [Usage] keeps InputTokens, OutputTokens, and the
// other generic counters readable exactly as on chat responses.
type ImageUsage struct {
	Usage
	// TotalTokens is the provider-reported total for the call.
	TotalTokens int
	// InputTextTokens is the input text portion, when the provider breaks it
	// out.
	InputTextTokens int
	// InputImageTokens is the input image portion, when the provider breaks it
	// out.
	InputImageTokens int
	// OutputTextTokens is the output text portion, when the provider breaks it
	// out.
	OutputTextTokens int
	// OutputImageTokens is the output image portion, when the provider breaks
	// it out.
	OutputImageTokens int
}

// ImageResponse is the result of an image request.
type ImageResponse struct {
	// Images are the produced images, in provider order.
	Images []GeneratedImage
	// Usage is the token accounting for the request; zero when unreported.
	Usage ImageUsage
	// CreatedAt is when the provider created the response. The zero time means
	// the provider did not report it.
	CreatedAt time.Time
	// OutputFormat is the format the provider reports having produced (for
	// example "webp"); empty when unreported.
	OutputFormat string
	// Size is the produced dimensions string the provider reports (for example
	// "1024x1536"); empty when unreported.
	Size string
	// Quality is the effective quality tier the provider reports; empty when
	// unreported.
	Quality string
	// Background is the effective background the provider reports (for example
	// "transparent"); empty when unreported.
	Background string
	// Raw is the provider's response body, untouched. Providers that return
	// inline image payloads in the body keep them here as well, so Raw is
	// larger than the decoded images.
	Raw JSON
}

// ImageEditRequest asks an [ImageEditor] to edit source images. Providers
// apply the prompt to Images, or to the region a Mask selects within the first
// image.
type ImageEditRequest struct {
	// Prompt describes the desired edit.
	Prompt string
	// Images are the source images, each carrying inline bytes, a URL, or a
	// provider file ID. Providers cap the count (OpenAI: 1 to 16).
	Images []ImagePart
	// Mask is an optional PNG whose transparent pixels mark the region to
	// edit. It applies to Images[0].
	Mask *ImagePart
	// N is how many images to generate; zero means one.
	N int
	// Size is the provider-specific dimensions string. Empty uses the provider
	// default.
	Size string
	// Quality is the provider-specific quality tier. Empty uses the provider
	// default.
	Quality string
	// OutputFormat is the desired encoding ("png", "jpeg", or "webp"). Empty
	// uses the provider default. Providers that emit a single format ignore
	// it.
	OutputFormat string

	// ProviderOptions passes provider-specific extensions, keyed like
	// [Request.ProviderOptions].
	ProviderOptions map[Provider]any
}

// ImageVariationRequest asks an [ImageVariator] for variations of one image.
type ImageVariationRequest struct {
	// Image is the source image. Providers that require inline bytes reject
	// other sources.
	Image ImagePart
	// N is how many images to generate; zero means one.
	N int
	// Size is the provider-specific dimensions string. Empty uses the provider
	// default.
	Size string

	// ProviderOptions passes provider-specific extensions, keyed like
	// [Request.ProviderOptions].
	ProviderOptions map[Provider]any
}

// ImageStream is a sequence of [ImageStreamEvent]s. Errors surface through the
// sequence; a pre-first-event failure is retryable, a failure after output was
// produced is terminal.
type ImageStream = iter.Seq2[ImageStreamEvent, error]

// ImageStreamEventType identifies one kind of [ImageStreamEvent].
type ImageStreamEventType string

// Image stream event types.
const (
	// ImageStreamPartial is a partial image produced while the request is
	// still running.
	ImageStreamPartial ImageStreamEventType = "partial_image"
	// ImageStreamCompleted is the final image of the request. It is the last
	// event of a well-formed stream.
	ImageStreamCompleted ImageStreamEventType = "completed"
)

// ImageStreamEvent is one event of an [ImageStream].
type ImageStreamEvent struct {
	// Type identifies the event.
	Type ImageStreamEventType
	// Index is the 0-based index of the partial image. It is always 0 on the
	// completed event, which carries no index of its own.
	Index int
	// Image is the event's image: inline bytes for partials and for the final
	// image of providers that stream data (OpenAI never streams URLs).
	Image GeneratedImage
	// Usage is the request's token accounting. It is non-nil only on the
	// completed event, and only when the provider reported it.
	Usage *ImageUsage
}
