package siliconflow

import (
	"cmp"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/cohere"
)

// DefaultRerankModel is a recommended rerank model on SiliconFlow.
const DefaultRerankModel = "BAAI/bge-reranker-v2-m3"

// NewRerankModel returns a rerank model configured for SiliconFlow.
// If model is empty, it defaults to [DefaultRerankModel] ("BAAI/bge-reranker-v2-m3").
// It defaults to reading the SILICONFLOW_API_KEY environment variable.
func NewRerankModel(model string, opts ...Option) *cohere.RerankModel {
	modelID := cmp.Or(model, DefaultRerankModel)

	cohereOpts := append([]cohere.Option{cohere.WithProvider(ai.ProviderSiliconFlow)}, toCohereOptions(opts)...)

	return cohere.NewRerankModel(modelID, cohereOpts...)
}
