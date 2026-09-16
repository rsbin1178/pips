package compat

import (
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
)

// GrokWebSearch returns a provider-executed web search tool for xAI Grok models.
func GrokWebSearch(opts ...openai.ToolOption) ai.Tool {
	t := ai.Tool{
		Kind:         ai.ToolKindProviderExecuted,
		Name:         "web_search",
		ProviderType: "web_search",
	}
	for _, opt := range opts {
		opt(&t)
	}

	return t
}

// GrokXSearch returns a provider-executed X (formerly Twitter) search tool
// for xAI Grok models.
func GrokXSearch(opts ...openai.ToolOption) ai.Tool {
	t := ai.Tool{
		Kind:         ai.ToolKindProviderExecuted,
		Name:         "x_search",
		ProviderType: "x_search",
	}
	for _, opt := range opts {
		opt(&t)
	}

	return t
}

// GrokCodeExecution returns a provider-executed Python code execution tool
// for xAI Grok models.
func GrokCodeExecution(opts ...openai.ToolOption) ai.Tool {
	t := ai.Tool{
		Kind:         ai.ToolKindProviderExecuted,
		Name:         "code_execution",
		ProviderType: "code_execution",
	}
	for _, opt := range opts {
		opt(&t)
	}

	return t
}
