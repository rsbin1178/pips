package groq

import (
	"strings"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
)

// DefaultBaseURL is the official Groq OpenAI-compatible API endpoint.
const DefaultBaseURL = "https://api.groq.com/openai/v1"

// New returns a language model configured for Groq (e.g. "llama-3.3-70b-versatile", "qwen/qwen3.6-27b").
// It defaults to reading the GROQ_API_KEY environment variable.
func New(model string, opts ...Option) *openai.Model {
	defaults := []openai.Option{
		openai.WithProvider(ai.ProviderGroq),
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

	if strings.Contains(model, "vision") || strings.Contains(model, "scout") {
		caps.Vision = true
	}

	if strings.Contains(model, "qwen") || strings.Contains(model, "deepseek") || strings.Contains(model, "gpt-oss") {
		caps.Reasoning = true
	}

	return caps
}
