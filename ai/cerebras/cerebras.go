package cerebras

import (
	"strings"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
)

// DefaultBaseURL is the official Cerebras API endpoint.
const DefaultBaseURL = "https://api.cerebras.ai/v1"

// New returns a language model configured for Cerebras (e.g. "llama3.1-8b", "llama-3.3-70b").
// It defaults to reading the CEREBRAS_API_KEY environment variable.
func New(model string, opts ...Option) *openai.Model {
	defaults := []openai.Option{
		openai.WithProvider(ai.ProviderCerebras),
		openai.WithBaseURL(DefaultBaseURL),
		openai.WithAPI(openai.APIChatCompletions),
		openai.WithCompatibility(openai.Compatibility{
			ReasoningHistory: openai.ReasoningHistoryReasoning,
			BuiltinTools:     openai.BuiltinToolsStrip,
		}),
		openai.WithCapabilities(capabilitiesFor(model)),
	}

	userOpts := toOpenAIOptions(opts)

	return openai.New(model, append(defaults, userOpts...)...)
}

func capabilitiesFor(model string) ai.Capabilities {
	caps := ai.Capabilities{Text: true, Tools: true}

	if strings.Contains(model, "gpt-oss") || strings.Contains(model, "qwen") {
		caps.Reasoning = true
	}

	if strings.Contains(model, "gemma") {
		caps.Vision = true
	}

	return caps
}
