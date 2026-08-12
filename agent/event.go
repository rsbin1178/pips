package agent

import (
	"time"

	"github.com/rsbin1178/pips/ai"
)

// EventType identifies the semantic payload carried by an [Event].
type EventType string

// Event types, in the order a run can produce them. Failed runs return an
// iterator error instead of producing EventRunCompleted.
const (
	// EventRunStarted opens a run.
	EventRunStarted EventType = "run_started"
	// EventTurnStarted opens one model turn.
	EventTurnStarted EventType = "turn_started"
	// EventModelStream carries one normalized model stream event. It is only
	// produced by [Agent.Stream], never [Agent.Run].
	EventModelStream EventType = "model_stream"
	// EventMessageCommitted reports a message after it has been appended to
	// the session.
	EventMessageCommitted EventType = "message_committed"
	// EventCandidateDiscarded reports a provisional answer rejected before
	// session commit. Candidate content is deliberately absent.
	EventCandidateDiscarded EventType = "candidate_discarded"
	// EventToolStarted opens one tool-call lifecycle.
	EventToolStarted EventType = "tool_started"
	// EventToolUpdated carries a best-effort progress update from a running
	// tool.
	EventToolUpdated EventType = "tool_updated"
	// EventToolCompleted closes one tool-call lifecycle, including denials and
	// synthesized failures.
	EventToolCompleted EventType = "tool_completed"
	// EventTurnCompleted closes one model turn.
	EventTurnCompleted EventType = "turn_completed"
	// EventRunCompleted closes a cleanly terminated run.
	EventRunCompleted EventType = "run_completed"
)

// Event is one immutable, process-local increment of an agent run. The
// envelope carries correlation metadata shared by every variant; Payload
// carries exactly one semantic variant. Events are safe to retain. Consumers
// must treat the returned payload as read-only.
//
// Event intentionally has no JSON wire format. Durable or remote consumers
// must define a versioned projection appropriate to their disclosure and
// compatibility requirements.
type Event struct {
	// RunID correlates every event in one invocation. ParentRunID links a
	// nested invocation to the run whose tool or callback started it. Agent is
	// the configured [Agent.Name], and Time is the UTC emission time.
	RunID       string
	ParentRunID string
	Agent       string
	Time        time.Time

	payload EventPayload
	// noCompare prevents interface-backed payloads with slices from making an
	// apparently valid Event comparison panic at runtime.
	_ [0]func()
}

// EventPayload is the sealed union of semantic [Event] variants. Only the
// concrete value types declared in this package are valid payloads.
type EventPayload interface {
	isEventPayload()
}

// emitFunc delivers one semantic payload and reports whether the run should
// keep going. False means the stream consumer stopped iterating.
type emitFunc func(EventPayload) bool

// RunStarted opens a run and carries no variant-specific data.
type RunStarted struct{}

// TurnStarted opens one model turn. Turn is one-based.
type TurnStarted struct {
	Turn int
}

// ModelStreamEvent carries one normalized model stream increment. Turn is
// one-based. The embedded stream event remains provisional until a
// [MessageCommitted] payload is emitted.
type ModelStreamEvent struct {
	Turn  int
	Event ai.StreamEvent
}

// MessageCommitted reports a message after it has been appended to the
// session. Turn is one-based.
type MessageCommitted struct {
	Turn    int
	Message ai.Message
}

// CandidateDiscarded reports a provisional no-tool answer rejected before
// session commit. Turn is one-based; rejected content is never retained.
type CandidateDiscarded struct {
	Turn int
}

// ToolStarted opens the lifecycle of Call in the one-based Turn.
type ToolStarted struct {
	Turn int
	Call ai.ToolCallPart
}

// ToolUpdated carries a best-effort progress Update for Call in the
// one-based Turn.
type ToolUpdated struct {
	Turn   int
	Call   ai.ToolCallPart
	Update []ai.Part
}

// ToolCompleted closes the lifecycle of Call in the one-based Turn. Result
// includes denials and synthesized failures through Result.IsError.
type ToolCompleted struct {
	Turn   int
	Call   ai.ToolCallPart
	Result ai.ToolResultPart
}

// TurnCompleted closes the one-based Turn. Usage is cumulative for the run.
type TurnCompleted struct {
	Turn  int
	Usage ai.Usage
}

// RunCompleted closes a cleanly terminated run. Turns is the number of model
// calls made, Stop is the clean termination reason, and Usage is cumulative.
type RunCompleted struct {
	Turns int
	Stop  StopReason
	Usage ai.Usage
}

func (RunStarted) isEventPayload()         {}
func (TurnStarted) isEventPayload()        {}
func (ModelStreamEvent) isEventPayload()   {}
func (MessageCommitted) isEventPayload()   {}
func (CandidateDiscarded) isEventPayload() {}
func (ToolStarted) isEventPayload()        {}
func (ToolUpdated) isEventPayload()        {}
func (ToolCompleted) isEventPayload()      {}
func (TurnCompleted) isEventPayload()      {}
func (RunCompleted) isEventPayload()       {}

// NewEvent constructs a validated event and snapshots all mutable payload
// data. occurredAt is normalized to UTC. It returns an error matching
// [ErrInvalidEvent] when metadata or payload invariants are invalid.
func NewEvent(meta RunMetadata, occurredAt time.Time, payload EventPayload) (Event, error) {
	event := newEvent(meta, occurredAt, payload)
	if err := event.Validate(); err != nil {
		return Event{}, err
	}

	return event, nil
}

// Type returns the discriminator for the event's concrete payload. It
// returns the zero EventType for an invalid zero Event.
func (e Event) Type() EventType {
	typeOf, _ := eventPayloadType(e.payload)

	return typeOf
}

// Payload returns the event's sealed semantic payload. The returned value is
// owned by this event snapshot and must be treated as read-only.
func (e Event) Payload() EventPayload {
	return e.payload
}

func newEvent(meta RunMetadata, occurredAt time.Time, payload EventPayload) Event {
	return Event{
		RunID:       meta.RunID,
		ParentRunID: meta.ParentRunID,
		Agent:       meta.Agent,
		Time:        occurredAt.UTC(),
		payload:     cloneEventPayload(payload),
	}
}
