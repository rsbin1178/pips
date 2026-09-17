package qwen

import (
	"strings"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
)

// DefaultBaseURL is the international (Singapore) DashScope
// OpenAI-compatible endpoint.
const DefaultBaseURL = "https://dashscope-intl.aliyuncs.com/compatible-mode/v1"

// DefaultChinaBaseURL is the mainland China (Beijing) DashScope
// OpenAI-compatible endpoint.
const DefaultChinaBaseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"

// DefaultUSBaseURL is the US (Virginia) DashScope OpenAI-compatible endpoint.
const DefaultUSBaseURL = "https://dashscope-us.aliyuncs.com/compatible-mode/v1"

// New returns a language model configured for Qwen (e.g. "qwen3.7-max",
// "qwen-plus", "qwen3-vl-plus").
//
// It defaults to reading the DASHSCOPE_API_KEY environment variable.
func New(model string, opts ...Option) *openai.Model {
	defaults := []openai.Option{
		openai.WithProvider(ai.ProviderQwen),
		openai.WithBaseURL(DefaultBaseURL),
		openai.WithAPI(openai.APIChatCompletions),
		openai.WithCompatibility(openai.Compatibility{
			MaxTokensField: openai.MaxTokensFieldLegacy,
			// DashScope reads reasoning_effort on its tier-native families and
			// enable_thinking/thinking_budget on the legacy hybrids. A level is
			// forwarded as reasoning_effort either way: it is inert on the
			// families that do not read it, and withholding it would reject a
			// user's explicit selection (compatibility policy R1).
			ChatReasoning:    openai.ChatReasoningEffort,
			ReasoningHistory: openai.ReasoningHistoryContent,
			BuiltinTools:     openai.BuiltinToolsStrip,
		}),
		openai.WithCapabilities(capabilitiesFor(model)),
	}

	userOpts := toOpenAIOptions(opts)

	return openai.New(model, append(defaults, userOpts...)...)
}

// capabilitiesFor infers capabilities from the Qwen model family name.
// DashScope does not expose a per-model capability endpoint on the
// OpenAI-compatible surface; declare overrides in the coding configuration
// when a family changes.
func capabilitiesFor(model string) ai.Capabilities {
	caps := ai.Capabilities{Text: true, Tools: true}

	if containsAny(model, "vl", "omni", "qvq") {
		caps.Vision = true
	}

	if containsAny(model, "qwq", "thinking", "qwen3") {
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
