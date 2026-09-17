package qwen

import (
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
)

// NewEmbeddingModel returns an embedding model configured for DashScope
// (for example "text-embedding-v4" or "text-embedding-v3"). It defaults to
// reading the DASHSCOPE_API_KEY environment variable.
//
// DashScope's OpenAI-compatible embedding endpoint only accepts
// encoding_format "float"; other encodings may be rejected.
func NewEmbeddingModel(model string, opts ...Option) *openai.EmbeddingModel {
	defaults := []openai.Option{
		openai.WithProvider(ai.ProviderQwen),
		openai.WithBaseURL(DefaultBaseURL),
	}

	userOpts := toOpenAIOptions(opts)

	return openai.NewEmbeddingModel(model, append(defaults, userOpts...)...)
}
