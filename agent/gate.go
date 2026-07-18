package agent

import "context"

// DecisionAction is what a [WithBeforeTool] gate tells the runtime to do with
// a tool call.
type DecisionAction int

// Gate actions. The zero value allows the call, so a gate only needs to
// return non-zero decisions for the calls it wants to intercept.
const (
	// Allow lets the call execute.
	Allow DecisionAction = iota
	// Deny blocks the call; [Decision.Reason] is fed back to the model as an
	// error tool result and the run continues.
	Deny
	// Pause stops the run before executing this call. The call and every
	// later call in the same turn become [RunResult.Pending]; resolve them
	// with [Session.ResolvePending] and run again to continue.
	Pause
)

// Decision is a gate's verdict on one tool call.
type Decision struct {
	Action DecisionAction
	// Reason is sent to the model when Action is [Deny]. Empty falls back to
	// a generic denial message.
	Reason string
}

// Denied returns a [Deny] decision carrying the given reason.
func Denied(reason string) Decision {
	return Decision{Action: Deny, Reason: reason}
}

// ToolCallInfo is the read-only view of a tool call passed to gates.
type ToolCallInfo struct {
	ToolCall
	// Turn is the turn (1-based) that produced the call.
	Turn int
}

// gate is the [WithBeforeTool] callback type.
type gate func(ctx context.Context, info ToolCallInfo) Decision
