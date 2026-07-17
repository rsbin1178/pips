package ai

// Request is a provider-agnostic generation request. The zero value of every
// optional field means "let the provider decide"; numeric knobs use pointers
// so that zero can be sent deliberately (see [Ptr]).
type Request struct {
	// Messages is the conversation so far, oldest first.
	Messages []Message
	// System is the system prompt. Adapters place it wherever the provider
	// expects (top-level field, system message, or systemInstruction).
	System string

	// Tools the model may call.
	Tools []Tool
	// ToolChoice constrains tool usage. The zero value is provider default.
	ToolChoice ToolChoice

	// Temperature controls randomness. Nil sends nothing.
	Temperature *float64
	// TopP is the nucleus-sampling cutoff. Nil sends nothing.
	TopP *float64
	// MaxTokens caps generated tokens. Nil lets the adapter choose: providers
	// that require the field (Anthropic) get a sensible default; others get
	// nothing.
	MaxTokens *int
	// Stop lists sequences that end generation.
	Stop []string

	// ResponseFormat requests schema-constrained JSON output. See
	// [GenerateTyped] for the typed convenience wrapper.
	ResponseFormat *ResponseFormat
	// Reasoning requests and tunes model reasoning.
	Reasoning *ReasoningConfig

	// ProviderOptions passes provider-specific extensions that have no
	// portable representation. Each adapter looks up its own [Provider] key
	// and type-asserts the value to its documented options type; unknown keys
	// are ignored.
	ProviderOptions map[Provider]any
}

// Ptr returns a pointer to v. It keeps request literals terse:
//
//	ai.Request{Temperature: ai.Ptr(0.2), MaxTokens: ai.Ptr(1024)}
//
//go:fix inline
func Ptr[T any](v T) *T {
	return new(v)
}
