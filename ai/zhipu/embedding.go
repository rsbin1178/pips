package zhipu

import (
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
)

// NewEmbeddingModel returns an embedding model configured for Zhipu AI
// (for example "embedding-3" or "embedding-2"). It defaults to reading the
// ZHIPU_API_KEY environment variable.
func NewEmbeddingModel(model string, opts ...Option) *openai.EmbeddingModel {
	defaults := []openai.Option{
		openai.WithProvider(ai.ProviderZhipu),
		openai.WithBaseURL(DefaultBaseURL),
	}

	userOpts := toOpenAIOptions(opts)

	return openai.NewEmbeddingModel(model, append(defaults, userOpts...)...)
}
