package ai

// ReasoningEffort selects how much reasoning ("thinking") a model does before
// answering. It maps to each provider's native control: OpenAI Responses
// reasoning.effort, Anthropic thinking.budget_tokens, and Gemini
// thinkingConfig.thinkingBudget.
type ReasoningEffort string

// Reasoning effort levels. The empty value leaves reasoning at the provider
// default.
const (
	ReasoningLow    ReasoningEffort = "low"
	ReasoningMedium ReasoningEffort = "medium"
	ReasoningHigh   ReasoningEffort = "high"
)

// ReasoningConfig requests and tunes model reasoning. Providers that map
// effort to a token budget use BudgetTokens when set, otherwise they derive a
// budget from Effort.
type ReasoningConfig struct {
	// Effort is the portable knob; prefer it for cross-provider code.
	Effort ReasoningEffort
	// BudgetTokens optionally pins an explicit thinking-token budget for
	// providers that accept one (Anthropic, Gemini). Zero means "derive from
	// Effort".
	BudgetTokens int
	// IncludeSummary asks providers that can return a reasoning summary
	// (OpenAI Responses) to include it. Providers that always stream reasoning
	// content ignore this.
	IncludeSummary bool
}
