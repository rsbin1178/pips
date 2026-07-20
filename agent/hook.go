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

// TurnUpdate adjusts the run between turns, returned by a [WithPrepareTurn]
// hook. The zero value changes nothing.
type TurnUpdate struct {
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
}
