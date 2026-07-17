package ai

// Provider identifies an LLM API vendor. Provider values appear in errors,
// capability lookups, and the ProviderOptions escape hatch on [Request].
type Provider string

// Known providers.
const (
	ProviderOpenAI    Provider = "openai"
	ProviderAnthropic Provider = "anthropic"
	ProviderGemini    Provider = "gemini"
)

// Capabilities reports, best effort, what a specific model supports. It is
// derived from static per-provider model tables; unknown models report
// pessimistic values but calls are never blocked based on capabilities.
type Capabilities struct {
	// Text reports whether the model generates text.
	Text bool
	// Vision reports whether the model accepts image input.
	Vision bool
	// Tools reports whether the model supports tool / function calling.
	Tools bool
	// StructuredOutput reports whether the model supports schema-constrained
	// JSON output.
	StructuredOutput bool
	// Reasoning reports whether the model exposes reasoning ("thinking")
	// controls and content.
	Reasoning bool
	// ImageGeneration reports whether the model can generate images.
	ImageGeneration bool
	// Embeddings reports whether the model produces embeddings.
	Embeddings bool
	// PromptCaching reports whether the provider supports explicit prompt
	// caching for this model.
	PromptCaching bool
}
