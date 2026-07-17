package ai

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

	// ProviderOptions passes provider-specific extensions, keyed like
	// [Request.ProviderOptions].
	ProviderOptions map[Provider]any
}

// GeneratedImage is one produced image, decoded to raw bytes.
type GeneratedImage struct {
	Data     []byte
	MIMEType string
}

// ImageResponse is the result of image generation.
type ImageResponse struct {
	Images []GeneratedImage
	Usage  Usage
	// Raw is the provider's response body, untouched (minus inline image
	// payloads for providers that return them elsewhere).
	Raw JSON
}
