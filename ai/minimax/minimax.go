package minimax

import (
	"strings"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/anthropic"
	"github.com/rsbin1178/pips/ai/openai"
)

// DefaultBaseURL is the international MiniMax OpenAI-compatible endpoint.
const DefaultBaseURL = "https://api.minimax.io/v1"

// DefaultChinaBaseURL is the mainland China MiniMax OpenAI-compatible endpoint.
const DefaultChinaBaseURL = "https://api.minimaxi.com/v1"

// DefaultAnthropicBaseURL is the international MiniMax Anthropic-compatible
// base. The Messages endpoint is served at /anthropic/v1/messages.
const DefaultAnthropicBaseURL = "https://api.minimax.io/anthropic/v1"

// DefaultChinaAnthropicBaseURL is the mainland China MiniMax
// Anthropic-compatible base.
const DefaultChinaAnthropicBaseURL = "https://api.minimaxi.com/anthropic/v1"

// New returns a language model configured for MiniMax over the
// OpenAI-compatible Chat Completions API (e.g. "MiniMax-M3").
//
// It defaults to reading the MINIMAX_API_KEY environment variable.
func New(model string, opts ...Option) *openai.Model {
	defaults := []openai.Option{
		openai.WithProvider(ai.ProviderMiniMax),
		openai.WithBaseURL(DefaultBaseURL),
		openai.WithAPI(openai.APIChatCompletions),
		openai.WithCompatibility(openai.Compatibility{
			// MiniMax documents max_tokens as legacy and recommends
			// max_completion_tokens, which is the adapter default. Thinking is
			// configured through extra_body, but a configured level is still
			// forwarded rather than withheld (compatibility policy R1).
			ChatReasoning:    openai.ChatReasoningEffort,
			ReasoningHistory: openai.ReasoningHistoryContent,
			BuiltinTools:     openai.BuiltinToolsStrip,
		}),
		openai.WithCapabilities(capabilitiesFor(model)),
	}

	userOpts := toOpenAIOptions(opts)

	return openai.New(model, append(defaults, userOpts...)...)
}

// NewAnthropic returns a language model configured for MiniMax over the
// Anthropic-compatible Messages API, which MiniMax documents as the
// recommended route for advanced model features.
//
// It defaults to reading the MINIMAX_API_KEY environment variable.
func NewAnthropic(model string, opts ...Option) *anthropic.Model {
	defaults := []anthropic.Option{
		anthropic.WithProvider(ai.ProviderMiniMax),
		anthropic.WithBaseURL(DefaultAnthropicBaseURL),
	}

	userOpts := toAnthropicOptions(opts)

	return anthropic.New(model, append(defaults, userOpts...)...)
}

// capabilitiesFor infers capabilities from the MiniMax model family name.
// The M2 and M3 families are multimodal reasoners; video input is specific to
// M3. Declare overrides in the coding configuration when a family changes.
func capabilitiesFor(model string) ai.Capabilities {
	caps := ai.Capabilities{Text: true, Tools: true}

	if containsAnyFold(model, "m3", "m2", "vision", "vl") {
		caps.Vision = true
	}

	if containsAnyFold(model, "m3") {
		caps.VideoInput = true
	}

	if containsAnyFold(model, "m3", "m2") {
		caps.Reasoning = true
	}

	return caps
}

func containsAnyFold(model string, needles ...string) bool {
	lower := strings.ToLower(model)
	for _, needle := range needles {
		if strings.Contains(lower, needle) {
			return true
		}
	}

	return false
}
