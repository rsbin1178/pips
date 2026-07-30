package agent

import (
	"github.com/rsbin/pips/ai"
)

// ToolResultInfo is the read-only view passed to a [WithAfterTool] hook: the
// call that ran and the result it produced, before any override.
type ToolResultInfo struct {
	ToolCall
	// Turn is the turn (1-based) that produced the call.
	Turn int
	// Result is the executed outcome (IsError reflects execution failures;
	// terminate hints are visible via [ToolResultOverride], not here).
	Result ai.ToolResultPart
}

// ToolResultOverride replaces fields of an executed tool result from a
// [WithAfterTool] hook. Each field is a full replacement; nil (or a nil
// pointer) keeps the executed value. There is no deep merge.
type ToolResultOverride struct {
	// Content replaces the result content.
	Content []ai.Part
	// IsError replaces the error flag.
	IsError *bool
	// Terminate replaces the tool's termination hint (see [ErrTerminate]).
	Terminate *bool
}

// ModelRequestUpdate constrains exactly one subsequent model request. It is
// run-local: applying it never mutates the Agent or the persistent Session.
// A non-nil Tools slice replaces the declaration and execution snapshot for
// that request only.
type ModelRequestUpdate struct {
	Tools        []Tool
	ToolChoice   ai.ToolChoice
	SystemSuffix string
}

// CandidateAnswerInfo is the read-only view passed to a
// [WithCandidateAnswer] hook before a no-Tool assistant answer is committed.
type CandidateAnswerInfo struct {
	RunInfo
	Session []ai.Message
	Message ai.Message
}

// CandidateAnswerDecision accepts the candidate when both fields are zero.
// Err rejects it and aborts the run. Retry rejects it without committing the
// assistant message and constrains exactly the next model request.
type CandidateAnswerDecision struct {
	Retry *ModelRequestUpdate
	Err   error
}

// TurnUpdate adjusts the run between turns, returned by a [WithPrepareTurn]
// hook. The zero value changes nothing.
type TurnUpdate struct {
	// Err aborts the run before another model call. Hook composition stops at
	// the first error so an application can make a failed context rewrite an
	// explicit run failure instead of continuing with stale messages.
	Err error
	// Model, when non-nil, serves all subsequent model calls of this run.
	Model ai.LanguageModel
	// ReplaceMessages, when non-nil, replaces the session history via
	// [Session.Replace] — the commit point for context compaction.
	ReplaceMessages []ai.Message
	// Tools, when non-nil, replaces the complete tool snapshot used for all
	// subsequent model calls and executions in this run. The replacement is
	// validated before it takes effect, so a declaration can never be shown to
	// the model without the matching executable implementation. This enables
	// deferred tool discovery without mutating the Agent shared by other runs.
	Tools []Tool
	// NextRequest, when non-nil, constrains exactly the next model request.
	// Unlike Tools, it does not replace the run's persistent tool snapshot.
	NextRequest *ModelRequestUpdate
}
