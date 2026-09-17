package openrouter

import (
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
)

// DefaultBaseURL is the official OpenRouter API endpoint.
const DefaultBaseURL = "https://openrouter.ai/api/v1"

// New returns a language model configured for OpenRouter (e.g.
// "openai/gpt-5.2", "anthropic/claude-sonnet-4.6").
//
// It defaults to reading the OPENROUTER_API_KEY environment variable.
func New(model string, opts ...Option) *openai.Model {
	defaults := []openai.Option{
		openai.WithProvider(ai.ProviderOpenRouter),
		openai.WithBaseURL(DefaultBaseURL),
		openai.WithAPI(openai.APIChatCompletions),
		openai.WithCompatibility(openai.Compatibility{
			MaxTokensField:   openai.MaxTokensFieldLegacy,
			ChatReasoning:    openai.ChatReasoningObject,
			ReasoningHistory: openai.ReasoningHistoryDetails,
		}),
		// OpenRouter routes to models from many vendors, so the reviewed
		// profile stays conservative. Declare more in the coding
		// configuration when the selected slug is known to support it.
		openai.WithCapabilities(ai.Capabilities{Text: true, Tools: true}),
	}

	userOpts := toOpenAIOptions(opts)

	return openai.New(model, append(defaults, userOpts...)...)
}
