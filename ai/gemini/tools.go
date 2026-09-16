package gemini

import "github.com/rsbin1178/pips/ai"

// ToolOption configures a Gemini built-in tool.
type ToolOption func(*ai.Tool)

// WithEnabled controls whether the tool is enabled for the request. Passing
// false marks the tool as disabled so adapters omit it from the wire payload.
func WithEnabled(enabled bool) ToolOption {
	return func(t *ai.Tool) {
		t.Disabled = !enabled
	}
}

// GoogleSearch returns a provider-executed Google Search Grounding tool.
func GoogleSearch(opts ...ToolOption) ai.Tool {
	t := ai.Tool{
		Kind:         ai.ToolKindProviderExecuted,
		Name:         "google_search",
		ProviderType: "google_search",
	}
	for _, opt := range opts {
		opt(&t)
	}

	return t
}

// CodeExecution returns a provider-executed Python code execution sandbox tool.
func CodeExecution(opts ...ToolOption) ai.Tool {
	t := ai.Tool{
		Kind:         ai.ToolKindProviderExecuted,
		Name:         "code_execution",
		ProviderType: "code_execution",
	}
	for _, opt := range opts {
		opt(&t)
	}

	return t
}
