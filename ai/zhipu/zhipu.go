package zhipu

import (
	"strings"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
)

// DefaultBaseURL is the official Zhipu AI BigModel endpoint.
const DefaultBaseURL = "https://open.bigmodel.cn/api/paas/v4"

// New returns a language model configured for Zhipu AI (GLM series models).
// It defaults to reading the ZHIPU_API_KEY environment variable.
func New(model string, opts ...Option) *openai.Model {
	defaults := []openai.Option{
		openai.WithProvider(ai.ProviderZhipu),
		openai.WithBaseURL(DefaultBaseURL),
		openai.WithAPI(openai.APIChatCompletions),
		openai.WithCompatibility(openai.Compatibility{
			MaxTokensField: openai.MaxTokensFieldLegacy,
			// Zhipu documents response_format type text|json_object only;
			// json_schema is not part of the Chat Completions schema.
			StructuredOutput: openai.StructuredOutputJSONObject,
			// Zhipu returns reasoning_content, so continuation echoes that
			// field rather than a generic reasoning field.
			ReasoningHistory: openai.ReasoningHistoryContent,
			// GLM-5.2 and later read a top-level reasoning_effort and map
			// out-of-ladder values server-side; the 4.x family treats
			// reasoning_effort as inert. A configured level is forwarded
			// rather than withheld (compatibility policy R1). GLM-5.3 accepts
			// only low/high/max and cannot stop thinking, so declare exactly
			// those reasoning_levels for it; the catalog then rejects any
			// other selection locally (R2).
			ChatReasoning: openai.ChatReasoningEffort,
			BuiltinTools:  openai.BuiltinToolsStrip,
		}),
		openai.WithCapabilities(capabilitiesFor(model)),
	}

	userOpts := toOpenAIOptions(opts)

	return openai.New(model, append(defaults, userOpts...)...)
}

func capabilitiesFor(model string) ai.Capabilities {
	caps := ai.Capabilities{
		Text:             true,
		Tools:            true,
		StructuredOutput: true,
	}

	if strings.Contains(model, "vision") || strings.Contains(model, "v") {
		caps.Vision = true
	}

	if strings.Contains(model, "zero") || strings.Contains(model, "reasoning") {
		caps.Reasoning = true
	}

	return caps
}
