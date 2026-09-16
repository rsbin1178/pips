package openai

import "github.com/rsbin1178/pips/ai"

// ToolOption configures an OpenAI built-in tool.
type ToolOption func(*ai.Tool)

// WithEnabled controls whether the tool is enabled for the request. Passing
// false marks the tool as disabled so adapters omit it from the wire payload.
func WithEnabled(enabled bool) ToolOption {
	return func(t *ai.Tool) {
		t.Disabled = !enabled
	}
}

// WebSearch returns a provider-executed web search tool for OpenAI models.
func WebSearch(opts ...ToolOption) ai.Tool {
	t := ai.Tool{
		Kind:         ai.ToolKindProviderExecuted,
		Name:         "web_search",
		ProviderType: "web_search",
	}
	for _, opt := range opts {
		opt(&t)
	}

	return t
}

// WebSearchPreview returns a provider-executed web_search_preview tool for
// the OpenAI Responses API.
func WebSearchPreview(opts ...ToolOption) ai.Tool {
	t := ai.Tool{
		Kind:         ai.ToolKindProviderExecuted,
		Name:         "web_search_preview",
		ProviderType: "web_search_preview",
	}
	for _, opt := range opts {
		opt(&t)
	}

	return t
}

// CodeInterpreter returns a provider-executed Python code sandbox tool.
func CodeInterpreter(opts ...ToolOption) ai.Tool {
	t := ai.Tool{
		Kind:         ai.ToolKindProviderExecuted,
		Name:         "code_interpreter",
		ProviderType: "code_interpreter",
	}
	for _, opt := range opts {
		opt(&t)
	}

	return t
}

// FileSearchData holds configuration for the OpenAI file_search tool.
type FileSearchData struct {
	VectorStoreIDs []string `json:"vector_store_ids,omitempty"`
}

// FileSearch returns a provider-executed file search tool using the specified
// vector store IDs.
func FileSearch(vectorStoreIDs []string, opts ...ToolOption) ai.Tool {
	t := ai.Tool{
		Kind:         ai.ToolKindProviderExecuted,
		Name:         "file_search",
		ProviderType: "file_search",
		ProviderData: FileSearchData{
			VectorStoreIDs: vectorStoreIDs,
		},
	}
	for _, opt := range opts {
		opt(&t)
	}

	return t
}
