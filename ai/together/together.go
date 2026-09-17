package together

import (
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
)

// DefaultBaseURL is the official Together AI API endpoint.
const DefaultBaseURL = "https://api.together.ai/v1"

// New returns a language model configured for Together AI (e.g. "meta-llama/Meta-Llama-3.1-8B-Instruct-Turbo").
// It defaults to reading the TOGETHER_API_KEY environment variable.
func New(model string, opts ...Option) *openai.Model {
	defaults := []openai.Option{
		openai.WithProvider(ai.ProviderTogether),
		openai.WithBaseURL(DefaultBaseURL),
		openai.WithAPI(openai.APIChatCompletions),
		openai.WithCompatibility(openai.Compatibility{
			MaxTokensField: openai.MaxTokensFieldLegacy,
			// Together documents reasoning_effort (the adapter default) and
			// maps unsupported levels automatically, so reasoning levels are
			// forwarded rather than refused.
			ReasoningHistory: openai.ReasoningHistoryReasoning,
			BuiltinTools:     openai.BuiltinToolsStrip,
		}),
		openai.WithCapabilities(ai.Capabilities{Text: true, Tools: true}),
	}

	userOpts := toOpenAIOptions(opts)

	return openai.New(model, append(defaults, userOpts...)...)
}
