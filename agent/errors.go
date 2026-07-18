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
	// ErrTerminate is a control sentinel (in the spirit of [io.EOF]) a tool
	// returns alongside its parts to ask the run to stop after the current
	// tool batch:
	//
	//	return agent.TextResult("final answer"), agent.ErrTerminate
	//
	// The parts are recorded as a successful result. The run stops with
	// [StopTerminated] only when every result in the batch carries the hint
	// (failed or denied calls never do); queued follow-ups still run first.
	ErrTerminate = errors.New("agent: tool requested run termination")
)
