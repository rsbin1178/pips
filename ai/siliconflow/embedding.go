package siliconflow

import (
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
)

// NewEmbeddingModel returns an embedding model configured for SiliconFlow (e.g. "BAAI/bge-large-zh-v1.5").
// It defaults to reading the SILICONFLOW_API_KEY environment variable.
func NewEmbeddingModel(model string, opts ...Option) *openai.EmbeddingModel {
	defaults := []openai.Option{
		openai.WithProvider(ai.ProviderSiliconFlow),
		openai.WithBaseURL(DefaultBaseURL),
	}

	userOpts := toOpenAIOptions(opts)

	return openai.NewEmbeddingModel(model, append(defaults, userOpts...)...)
}
