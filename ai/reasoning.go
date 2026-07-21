package ai

// ReasoningEffort selects how much reasoning ("thinking") a model does before
// answering. It maps to each provider's native control: OpenAI Responses
// reasoning.effort, Anthropic adaptive effort or enabled-thinking budget, and
// Gemini thinkingConfig.thinkingLevel.
type ReasoningEffort string

// Reasoning effort levels. The empty value leaves reasoning at the provider
// default.
const (
	ReasoningNone    ReasoningEffort = "none"
	ReasoningMinimal ReasoningEffort = "minimal"
	ReasoningLow     ReasoningEffort = "low"
	ReasoningMedium  ReasoningEffort = "medium"
	ReasoningHigh    ReasoningEffort = "high"
	ReasoningXHigh   ReasoningEffort = "xhigh"
	ReasoningMax     ReasoningEffort = "max"
)

// ReasoningMode selects a provider strategy independently from the effort or
// explicit token budget.
type ReasoningMode string

// Portable reasoning modes.
const (
	ReasoningModeAuto     ReasoningMode = "auto"
	ReasoningModeEnabled  ReasoningMode = "enabled"
	ReasoningModeAdaptive ReasoningMode = "adaptive"
	ReasoningModeDisabled ReasoningMode = "disabled"
)

// ReasoningConfig requests and tunes model reasoning. BudgetTokens is explicit;
// adapters may translate Effort to their native level or documented default
// budget when the protocol has no effort field.
type ReasoningConfig struct {
	// Mode controls whether and how the provider enables reasoning.
	Mode ReasoningMode
	// Effort is the portable knob; prefer it for cross-provider code.
	Effort ReasoningEffort
	// BudgetTokens optionally pins an explicit thinking-token budget for
	// providers that accept one (Anthropic, Gemini). Zero leaves budget
	// selection to the effort mapping or provider default.
	BudgetTokens int
	// IncludeSummary asks providers that can return a reasoning summary
	// (OpenAI Responses) to include it. Providers that always stream reasoning
	// content ignore this.
	IncludeSummary bool
}
