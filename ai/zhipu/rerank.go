package zhipu

import (
	"cmp"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/cohere"
)

// DefaultRerankModel is the official model identifier for Zhipu's rerank service.
const DefaultRerankModel = "rerank"

// NewRerankModel returns a rerank model configured for Zhipu AI.
// If model is empty, it defaults to [DefaultRerankModel] ("rerank").
// It defaults to reading the ZHIPU_API_KEY environment variable.
func NewRerankModel(model string, opts ...Option) *cohere.RerankModel {
	modelID := cmp.Or(model, DefaultRerankModel)

	cohereOpts := append([]cohere.Option{cohere.WithProvider(ai.ProviderZhipu)}, toCohereOptions(opts)...)

	return cohere.NewRerankModel(modelID, cohereOpts...)
}
