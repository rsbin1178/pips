package openai

import (
	"strings"

	"github.com/rsbin1178/pips/ai"
)

// Capabilities implements ai.LanguageModel with a static prefix table.
// Unknown models report text-only; calls are never blocked by this data.
func (m *Model) Capabilities() ai.Capabilities {
	if m.capabilities != nil {
		return *m.capabilities
	}

	return capabilitiesFor(m.model)
}

func capabilitiesFor(model string) ai.Capabilities {
	switch {
	case strings.HasPrefix(model, "gpt-image"), strings.HasPrefix(model, "dall-e"):
		return ai.Capabilities{ImageGeneration: true}
	case strings.HasPrefix(model, "text-embedding"):
		return ai.Capabilities{Embeddings: true}
	case isReasoningModel(model):
		return ai.Capabilities{
			Text:             true,
			Vision:           true,
			Documents:        true,
			Tools:            true,
			StructuredOutput: true,
			Reasoning:        true,
			PromptCaching:    true, // automatic caching on OpenAI's side
		}
	case strings.HasPrefix(model, "gpt-4o"), strings.HasPrefix(model, "gpt-4.1"),
		strings.HasPrefix(model, "chatgpt-4o"), strings.HasPrefix(model, "gpt-4-turbo"):
		return ai.Capabilities{
			Text:             true,
			Vision:           true,
			Documents:        true,
			Tools:            true,
			StructuredOutput: true,
			PromptCaching:    true,
		}
	case strings.HasPrefix(model, "gpt-4"), strings.HasPrefix(model, "gpt-3.5"):
		return ai.Capabilities{Text: true, Tools: true}
	default:
		// Unknown (including third-party compat models): assume plain chat
		// with tools, the least surprising baseline for compat endpoints.
		return ai.Capabilities{Text: true, Tools: true}
	}
}
