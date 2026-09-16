package cohere

import (
	"os"

	"github.com/rsbin1178/pips/ai"
)

// Base URLs for popular Cohere-compatible reranking endpoints.
const (
	BaseURLSiliconFlow = "https://api.siliconflow.cn/v1"
	BaseURLJina        = "https://api.jina.ai/v1"
	BaseURLTogether    = "https://api.together.xyz/v1"
)

// SiliconFlowRerank returns a rerank model configured for SiliconFlow
// (https://api.siliconflow.cn/v1). API key defaults to the SILICONFLOW_API_KEY
// environment variable.
func SiliconFlowRerank(model string, opts ...Option) *RerankModel {
	defaults := []Option{
		WithProvider(ai.ProviderSiliconFlow),
		WithBaseURL(BaseURLSiliconFlow),
		WithAPIKey(firstEnv("SILICONFLOW_API_KEY")),
	}

	return NewRerankModel(model, append(defaults, opts...)...)
}

// JinaRerank returns a rerank model configured for Jina AI
// (https://api.jina.ai/v1). API key defaults to the JINA_API_KEY
// environment variable.
func JinaRerank(model string, opts ...Option) *RerankModel {
	defaults := []Option{
		WithProvider(ai.ProviderJina),
		WithBaseURL(BaseURLJina),
		WithAPIKey(firstEnv("JINA_API_KEY")),
	}

	return NewRerankModel(model, append(defaults, opts...)...)
}

// TogetherRerank returns a rerank model configured for Together AI
// (https://api.together.xyz/v1). API key defaults to the TOGETHER_API_KEY
// environment variable.
func TogetherRerank(model string, opts ...Option) *RerankModel {
	defaults := []Option{
		WithProvider(ai.ProviderTogether),
		WithBaseURL(BaseURLTogether),
		WithAPIKey(firstEnv("TOGETHER_API_KEY")),
	}

	return NewRerankModel(model, append(defaults, opts...)...)
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if val := os.Getenv(name); val != "" {
			return val
		}
	}

	return ""
}
