package agent

import "errors"

// Sentinel errors returned by [Agent.Run], [Agent.Stream], and [Session]
// methods. Match them with [errors.Is].
var (
	// ErrRunActive means the session already has a run in progress. A
	// [Session] serializes runs; wait for the active one to finish.
	ErrRunActive = errors.New("agent: session already has an active run")
	// ErrPendingToolCalls means the session tail contains tool calls that have
	// no results yet (a previous run stopped with [StopPaused], or a stream
	// was abandoned mid-turn). Resolve them with [Session.ResolvePending]
	// before starting another run.
	ErrPendingToolCalls = errors.New("agent: session has unresolved tool calls")
)
