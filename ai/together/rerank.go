package together

import (
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/cohere"
)

// NewRerankModel returns a rerank model configured for Together AI (e.g. "Salesforce/Llama-Rank-v1").
// It defaults to reading the TOGETHER_API_KEY environment variable.
func NewRerankModel(model string, opts ...Option) *cohere.RerankModel {
	cohereOpts := append([]cohere.Option{cohere.WithProvider(ai.ProviderTogether)}, toCohereOptions(opts)...)

	return cohere.NewRerankModel(model, cohereOpts...)
}
