package together

import (
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
)

// NewImageModel returns an image generation model configured for Together AI
// (for example "black-forest-labs/FLUX.1-schnell", "stabilityai/stable-diffusion-xl-base-1.0").
// It defaults to reading the TOGETHER_API_KEY environment variable and calls the POST /images/generations endpoint.
func NewImageModel(model string, opts ...Option) *openai.ImageModel {
	defaults := []openai.Option{
		openai.WithProvider(ai.ProviderTogether),
		openai.WithBaseURL(DefaultBaseURL),
	}

	userOpts := toOpenAIOptions(opts)

	return openai.NewImageModel(model, append(defaults, userOpts...)...)
}
