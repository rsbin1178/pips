package xai

import (
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
)

// DefaultBaseURL is the official xAI (Grok) API endpoint.
const DefaultBaseURL = "https://api.x.ai/v1"

// New returns a language model configured for xAI (e.g. "grok-2-1212", "grok-vision-beta").
// It defaults to reading the XAI_API_KEY environment variable.
func New(model string, opts ...Option) *openai.Model {
	defaults := []openai.Option{
		openai.WithProvider(ai.ProviderXAI),
		openai.WithBaseURL(DefaultBaseURL),
		openai.WithAPI(openai.APIResponses),
		openai.WithCompatibility(openai.Compatibility{
			// Responses uses max_output_tokens and ignores this; the legacy
			// Chat Completions surface this facade can switch to uses
			// max_tokens.
			MaxTokensField:            openai.MaxTokensFieldLegacy,
			IncludeEncryptedReasoning: true,
		}),
		openai.WithCapabilities(ai.Capabilities{
			Text:             true,
			Vision:           true,
			Tools:            true,
			StructuredOutput: true,
			Reasoning:        true,
			PromptCaching:    true,
			WebSearch:        true,
			CodeExecution:    true,
		}),
	}

	userOpts := toOpenAIOptions(opts)

	return openai.New(model, append(defaults, userOpts...)...)
}
