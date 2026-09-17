package mistral

import (
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
)

// NewEmbeddingModel returns an embedding model configured for Mistral AI (e.g. "mistral-embed").
// It defaults to reading the MISTRAL_API_KEY environment variable.
func NewEmbeddingModel(model string, opts ...Option) *openai.EmbeddingModel {
	defaults := []openai.Option{
		openai.WithProvider(ai.ProviderMistral),
		openai.WithBaseURL(DefaultBaseURL),
	}

	userOpts := toOpenAIOptions(opts)

	return openai.NewEmbeddingModel(model, append(defaults, userOpts...)...)
}
