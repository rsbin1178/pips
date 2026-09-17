package kimi

import (
	"strings"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
)

// DefaultBaseURL is the international Kimi API endpoint.
const DefaultBaseURL = "https://api.moonshot.ai/v1"

// DefaultChinaBaseURL is the mainland China Kimi API endpoint. API keys from
// the two platforms are not interchangeable.
const DefaultChinaBaseURL = "https://api.moonshot.cn/v1"

// New returns a language model configured for Kimi (e.g. "kimi-k3",
// "kimi-k2.6", "moonshot-v1-128k").
//
// It defaults to reading the MOONSHOT_API_KEY environment variable.
func New(model string, opts ...Option) *openai.Model {
	defaults := []openai.Option{
		openai.WithProvider(ai.ProviderKimi),
		openai.WithBaseURL(DefaultBaseURL),
		openai.WithAPI(openai.APIChatCompletions),
		openai.WithCompatibility(openai.Compatibility{
			// Kimi deprecates max_tokens in favour of max_completion_tokens,
			// which is the adapter default, so MaxTokensField stays unset.
			//
			// kimi-k3 reads a top-level reasoning_effort. The k2.x families
			// read a thinking object this adapter does not emit; a level is
			// still forwarded, because declining to encode one must not
			// withhold a user's explicit selection (compatibility policy R1).
			ChatReasoning:    openai.ChatReasoningEffort,
			ReasoningHistory: openai.ReasoningHistoryContent,
			BuiltinTools:     openai.BuiltinToolsStrip,
		}),
		openai.WithCapabilities(capabilitiesFor(model)),
	}

	userOpts := toOpenAIOptions(opts)

	return openai.New(model, append(defaults, userOpts...)...)
}

// capabilitiesFor mirrors the per-model flags Kimi reports from GET /v1/models
// (supports_image_in, supports_reasoning). The platform list is authoritative;
// declare overrides in the coding configuration when it changes.
func capabilitiesFor(model string) ai.Capabilities {
	caps := ai.Capabilities{
		Text:             true,
		Tools:            true,
		StructuredOutput: true,
		PromptCaching:    true,
	}

	if containsAny(model, "vision", "kimi-k3", "kimi-k2.6") {
		caps.Vision = true
	}

	if containsAny(model, "kimi-k3", "kimi-k2.7", "kimi-k2.6", "kimi-k2.5", "thinking", "reasoning") {
		caps.Reasoning = true
	}

	return caps
}

func containsAny(model string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(model, needle) {
			return true
		}
	}

	return false
}
