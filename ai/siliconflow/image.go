package siliconflow

import (
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
)

// NewImageModel returns an image generation model configured for SiliconFlow
// (for example "black-forest-labs/FLUX.1-schnell", "stabilityai/stable-diffusion-3-medium").
// It defaults to reading the SILICONFLOW_API_KEY environment variable and calls the POST /images/generations endpoint.
func NewImageModel(model string, opts ...Option) *openai.ImageModel {
	defaults := []openai.Option{
		openai.WithProvider(ai.ProviderSiliconFlow),
		openai.WithBaseURL(DefaultBaseURL),
	}

	userOpts := toOpenAIOptions(opts)

	return openai.NewImageModel(model, append(defaults, userOpts...)...)
}
