package deepseek

import (
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/anthropic"
	"github.com/rsbin1178/pips/ai/openai"
)

// DefaultBaseURL is the official DeepSeek OpenAI-compatible API endpoint.
const DefaultBaseURL = "https://api.deepseek.com"

// DefaultAnthropicBaseURL is the official DeepSeek Anthropic-compatible Messages endpoint.
// Designed for Anthropic SDK clients and Claude ecosystem tools.
const DefaultAnthropicBaseURL = "https://api.deepseek.com/anthropic"

// New returns a language model configured for DeepSeek using the OpenAI wire format
// (e.g. "deepseek-flash", "deepseek-v4-pro").
// It defaults to reading the DEEPSEEK_API_KEY environment variable.
func New(model string, opts ...Option) *openai.Model {
	defaults := []openai.Option{
		openai.WithProvider(ai.ProviderDeepSeek),
		openai.WithBaseURL(DefaultBaseURL),
		openai.WithAPI(openai.APIChatCompletions),
		openai.WithCompatibility(openai.Compatibility{
			MaxTokensField:   openai.MaxTokensFieldLegacy,
			StructuredOutput: openai.StructuredOutputJSONObject,
			ChatReasoning:    openai.ChatReasoningDeepSeek,
			ReasoningHistory: openai.ReasoningHistoryContent,
			BuiltinTools:     openai.BuiltinToolsStrip,
		}),
		openai.WithCapabilities(ai.Capabilities{
			Text:             true,
			Tools:            true,
			StructuredOutput: true,
			Reasoning:        true,
			PromptCaching:    true,
		}),
	}

	userOpts := toOpenAIOptions(opts)

	return openai.New(model, append(defaults, userOpts...)...)
}

// NewAnthropic returns a language model configured for DeepSeek using the Anthropic Messages wire format
// (e.g. "deepseek-flash", "deepseek-v4-pro").
// It points to https://api.deepseek.com/anthropic and defaults to reading the DEEPSEEK_API_KEY environment variable.
func NewAnthropic(model string, opts ...Option) *anthropic.Model {
	defaults := []anthropic.Option{
		anthropic.WithProvider(ai.ProviderDeepSeek),
		anthropic.WithBaseURL(DefaultAnthropicBaseURL),
	}

	userOpts := toAnthropicOptions(opts)

	return anthropic.New(model, append(defaults, userOpts...)...)
}
