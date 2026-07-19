package agent

import "context"

// ToolDecisionAction is what a [WithBeforeTool] gate tells the runtime to do with
// a tool call.
type ToolDecisionAction int

// Gate actions. The zero value allows the call, so a gate only needs to
// return non-zero decisions for the calls it wants to intercept.
const (
	// ToolDecisionAllow lets the call execute.
	ToolDecisionAllow ToolDecisionAction = iota
	// ToolDecisionDeny blocks the call; [ToolDecision.Reason] is fed back to
	// the model as an error tool result and the run continues.
	ToolDecisionDeny
	// ToolDecisionPause stops the run before executing this call. The call and every
	// later call in the same turn become [RunResult.Pending]; resolve them
	// with [Session.ResolvePending] and run again to continue.
	ToolDecisionPause
)

// ToolDecision is a gate's verdict on one tool call.
type ToolDecision struct {
	Action ToolDecisionAction
	// Reason is sent to the model when Action is [ToolDecisionDeny]. Empty
	// falls back to a generic denial message.
	Reason string
}

// DenyTool returns a [ToolDecisionDeny] decision carrying the given reason.
func DenyTool(reason string) ToolDecision {
	return ToolDecision{Action: ToolDecisionDeny, Reason: reason}
}

// ToolCallInfo is the read-only view of a tool call passed to gates.
type ToolCallInfo struct {
	ToolCall
	// Turn is the turn (1-based) that produced the call.
	Turn int
}

// gate is the [WithBeforeTool] callback type.
type gate func(ctx context.Context, info ToolCallInfo) ToolDecision
