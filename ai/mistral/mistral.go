package mistral

import (
	"strings"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
)

// DefaultBaseURL is the official Mistral API endpoint.
const DefaultBaseURL = "https://api.mistral.ai/v1"

// New returns a language model configured for Mistral (e.g. "mistral-large-latest", "pixtral-12b-2409").
// It defaults to reading the MISTRAL_API_KEY environment variable.
func New(model string, opts ...Option) *openai.Model {
	defaults := []openai.Option{
		openai.WithProvider(ai.ProviderMistral),
		openai.WithBaseURL(DefaultBaseURL),
		openai.WithAPI(openai.APIChatCompletions),
		openai.WithCompatibility(openai.Compatibility{
			MaxTokensField:   openai.MaxTokensFieldLegacy,
			ReasoningHistory: openai.ReasoningHistoryContentChunks,
			BuiltinTools:     openai.BuiltinToolsStrip,
		}),
		openai.WithCapabilities(capabilitiesFor(model)),
	}

	userOpts := toOpenAIOptions(opts)

	return openai.New(model, append(defaults, userOpts...)...)
}

func capabilitiesFor(model string) ai.Capabilities {
	caps := ai.Capabilities{Text: true, Tools: true, StructuredOutput: true}

	if strings.Contains(model, "pixtral") || strings.Contains(model, "vision") {
		caps.Vision = true
	}

	if strings.Contains(model, "magistral") || strings.Contains(model, "reasoning") {
		caps.Reasoning = true
	}

	return caps
}
