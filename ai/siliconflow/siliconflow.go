package siliconflow

import (
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
)

// DefaultBaseURL is the mainland China SiliconFlow API endpoint.
const DefaultBaseURL = "https://api.siliconflow.cn/v1"

// DefaultGlobalBaseURL is the international SiliconFlow API endpoint.
const DefaultGlobalBaseURL = "https://api.siliconflow.com/v1"

// New returns a language model configured for SiliconFlow (e.g. "deepseek-ai/DeepSeek-V3.2", "Qwen/Qwen2.5-72B-Instruct").
// It defaults to reading the SILICONFLOW_API_KEY environment variable.
func New(model string, opts ...Option) *openai.Model {
	defaults := []openai.Option{
		openai.WithProvider(ai.ProviderSiliconFlow),
		openai.WithBaseURL(DefaultBaseURL),
		openai.WithAPI(openai.APIChatCompletions),
		openai.WithCompatibility(openai.Compatibility{
			// SiliconFlow documents max_tokens only; max_completion_tokens is
			// not part of its Chat Completions schema.
			MaxTokensField:   openai.MaxTokensFieldLegacy,
			ReasoningHistory: openai.ReasoningHistoryContent,
			BuiltinTools:     openai.BuiltinToolsStrip,
		}),
		openai.WithCapabilities(ai.Capabilities{Text: true, Tools: true}),
	}

	userOpts := toOpenAIOptions(opts)

	return openai.New(model, append(defaults, userOpts...)...)
}
