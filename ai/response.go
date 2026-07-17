package ai

import "strings"

// FinishReason is why generation stopped, normalized across providers.
type FinishReason string

// Normalized finish reasons.
const (
	// FinishStop means the model completed its turn normally (including by
	// hitting a stop sequence).
	FinishStop FinishReason = "stop"
	// FinishLength means generation hit the max-token limit.
	FinishLength FinishReason = "length"
	// FinishToolCalls means the model stopped to request tool invocations.
	FinishToolCalls FinishReason = "tool_calls"
	// FinishContentFilter means the provider suppressed output for safety
	// reasons.
	FinishContentFilter FinishReason = "content_filter"
	// FinishOther covers provider-specific reasons with no portable meaning;
	// consult [Response.Raw] for details.
	FinishOther FinishReason = "other"
)

// Usage is token accounting for a request, normalized across providers.
// Fields a provider does not report stay zero.
type Usage struct {
	// InputTokens is the prompt size, including cached tokens.
	InputTokens int
	// OutputTokens is the generated size, including reasoning tokens where
	// the provider counts them there.
	OutputTokens int
	// ReasoningTokens is the portion of output spent on reasoning, when
	// reported separately.
	ReasoningTokens int
	// CachedInputTokens is the portion of input served from a prompt cache,
	// when reported.
	CachedInputTokens int
	// CacheWriteTokens is the portion of input written to a prompt cache
	// (Anthropic cache_creation), when reported.
	CacheWriteTokens int
}

// Add accumulates counts from another Usage. Streaming adapters use it to
// fold incremental usage reports into a running total.
func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.ReasoningTokens += o.ReasoningTokens
	u.CachedInputTokens += o.CachedInputTokens
	u.CacheWriteTokens += o.CacheWriteTokens
}

// Response is a completed generation.
type Response struct {
	// ID is the provider's response identifier, when supplied.
	ID string
	// Model is the model that actually served the request, as reported by the
	// provider (it may be more specific than the requested alias).
	Model string
	// Provider identifies which adapter produced this response.
	Provider Provider

	// Message is the assistant turn: text, tool calls, and reasoning parts in
	// model output order. Append it to the conversation to continue the turn.
	Message Message

	// FinishReason is why generation stopped.
	FinishReason FinishReason
	// Usage is the token accounting for this request.
	Usage Usage

	// Raw is the provider's response body, untouched. It is an escape hatch
	// for provider-specific fields; do not parse it in portable code.
	Raw JSON
}

// Text concatenates the response's text parts.
func (r *Response) Text() string {
	var b strings.Builder

	for _, p := range r.Message.Parts {
		if t, ok := p.(TextPart); ok {
			b.WriteString(t.Text)
		}
	}

	return b.String()
}

// Reasoning concatenates the response's reasoning parts.
func (r *Response) Reasoning() string {
	var b strings.Builder

	for _, p := range r.Message.Parts {
		if t, ok := p.(ReasoningPart); ok {
			b.WriteString(t.Text)
		}
	}

	return b.String()
}

// ToolCalls returns the tool invocations requested by the model, in order.
func (r *Response) ToolCalls() []ToolCallPart {
	var calls []ToolCallPart

	for _, p := range r.Message.Parts {
		if c, ok := p.(ToolCallPart); ok {
			calls = append(calls, c)
		}
	}

	return calls
}
