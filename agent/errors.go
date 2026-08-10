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
	// was abandoned mid-turn). Resolve them with [Session.ResolveToolCalls] or
	// [Session.ResolvePending] before starting another run.
	ErrPendingToolCalls = errors.New("agent: session has unresolved tool calls")
	// ErrToolCallNotPending means a supplied tool resolution references a call
	// that is not currently awaiting a result.
	ErrToolCallNotPending = errors.New("agent: tool call is not pending")
	// ErrInvalidToolResolution means a supplied resolution has an empty or
	// duplicate tool-call ID.
	ErrInvalidToolResolution = errors.New("agent: invalid tool resolution")
	// ErrInvalidEvent classifies invalid process-local event metadata or
	// payloads. Construct events with [NewEvent] to validate and snapshot them.
	ErrInvalidEvent = errors.New("agent: invalid event")
	// ErrEventWireFormat reports an attempt to marshal or unmarshal [Event].
	// Agent events are process-local; use an application-owned, versioned
	// projection for durable or remote transport.
	ErrEventWireFormat = errors.New("agent: event has no wire format")
	// ErrGuardrail classifies input and output validation failures. Extract a
	// [GuardrailError] with [errors.As] for the phase, name, and cause.
	ErrGuardrail = errors.New("agent: guardrail rejected the run")
	// ErrSubagentPaused means an [AsTool] child requested durable approval,
	// which the tool result contract cannot preserve across invocations.
	ErrSubagentPaused = errors.New("agent: subagent paused with pending tool calls")
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
