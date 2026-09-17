package together

import (
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
)

// NewEmbeddingModel returns an embedding model configured for Together AI (e.g. "togethercomputer/m2-bert-80M-8k-retrieval").
// It defaults to reading the TOGETHER_API_KEY environment variable.
func NewEmbeddingModel(model string, opts ...Option) *openai.EmbeddingModel {
	defaults := []openai.Option{
		openai.WithProvider(ai.ProviderTogether),
		openai.WithBaseURL(DefaultBaseURL),
	}

	userOpts := toOpenAIOptions(opts)

	return openai.NewEmbeddingModel(model, append(defaults, userOpts...)...)
}
