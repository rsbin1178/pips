package zhipu

import (
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
)

// NewImageModel returns an image generation model configured for Zhipu AI
// (for example "cogview-4" or "cogview-3-plus"). It defaults to reading the
// ZHIPU_API_KEY environment variable and calls the POST /images/generations endpoint.
func NewImageModel(model string, opts ...Option) *openai.ImageModel {
	defaults := []openai.Option{
		openai.WithProvider(ai.ProviderZhipu),
		openai.WithBaseURL(DefaultBaseURL),
	}

	userOpts := toOpenAIOptions(opts)

	return openai.NewImageModel(model, append(defaults, userOpts...)...)
}
