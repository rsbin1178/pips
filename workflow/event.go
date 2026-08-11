package workflow

import (
	"slices"
	"time"
)

// EventType identifies one process-local Workflow event variant.
type EventType string

// Event types.
const (
	EventRunStarted      EventType = "run_started"
	EventRunCompleted    EventType = "run_completed"
	EventRunFailed       EventType = "run_failed"
	EventRunCanceled     EventType = "run_canceled"
	EventRunInterrupted  EventType = "run_interrupted"
	EventRunResumed      EventType = "run_resumed"
	EventNodeReady       EventType = "node_ready"
	EventNodeStarted     EventType = "node_started"
	EventNodeCompleted   EventType = "node_completed"
	EventNodeFailed      EventType = "node_failed"
	EventNodeSkipped     EventType = "node_skipped"
	EventNodeRetrying    EventType = "node_retrying"
	EventNodeInterrupted EventType = "node_interrupted"
)

// ScopeKind identifies one nested execution boundary.
type ScopeKind string

// Nested execution scope kinds.
const (
	ScopeSubWorkflow   ScopeKind = "sub_workflow"
	ScopeBatchItem     ScopeKind = "batch_item"
	ScopeLoopIteration ScopeKind = "loop_iteration"
)

// ScopeFrame identifies the composite node that owns one child execution.
// Index is -1 for SubWorkflow and the input index for Batch or Loop.
type ScopeFrame struct {
	Kind   ScopeKind
	NodeID NodeID
	Index  int
}

// FailureKind is a bounded, payload-free node failure classification.
type FailureKind string

// Failure kinds. Action error text is deliberately absent from Events.
const (
	FailureError    FailureKind = "error"
	FailureTimeout  FailureKind = "timeout"
	FailurePanic    FailureKind = "panic"
	FailureCanceled FailureKind = "canceled"
	FailureLimit    FailureKind = "limit"
)

// Event is one immutable, process-local Workflow lifecycle increment. It has
// no JSON wire format; durable consumers must define a versioned projection.
type Event struct {
	DefinitionID          DefinitionID
	Revision              Revision
	DefinitionFingerprint string
	PlanFingerprint       string
	RunID                 string
	NodeID                NodeID
	NodeType              NodeTypeKey
	Attempt               int
	Time                  time.Time

	payload EventPayload
	scope   []ScopeFrame
	_       [0]func()
}

// EventPayload is the sealed union of Workflow Event variants.
type EventPayload interface {
	isEventPayload()
	eventType() EventType
}

// RunStarted opens one Run.
type RunStarted struct{}

// RunCompleted closes a succeeded or partial-succeeded Run.
type RunCompleted struct{}

// RunFailed closes a failed Run.
type RunFailed struct{}

// RunCanceled closes a canceled Run.
type RunCanceled struct{}

// RunInterrupted reports that a resumable checkpoint was saved.
type RunInterrupted struct{}

// RunResumed reports that a checkpoint was validated and execution continued.
type RunResumed struct{}

// NodeReady reports that every incoming control edge has resolved and at
// least one was taken.
type NodeReady struct{}

// NodeStarted opens one invocation attempt.
type NodeStarted struct{}

// NodeCompleted closes a successful invocation attempt.
type NodeCompleted struct{}

// NodeFailed closes a failed invocation attempt without retaining error text.
type NodeFailed struct {
	Kind FailureKind
}

// NodeSkipped reports that every incoming control edge was skipped.
type NodeSkipped struct{}

// NodeRetrying reports that another bounded attempt will start.
type NodeRetrying struct {
	NextAttempt int
}

// NodeInterrupted reports an invocation suspended by a dynamic or descendant
// interruption.
type NodeInterrupted struct{}

func (RunStarted) isEventPayload()      {}
func (RunCompleted) isEventPayload()    {}
func (RunFailed) isEventPayload()       {}
func (RunCanceled) isEventPayload()     {}
func (RunInterrupted) isEventPayload()  {}
func (RunResumed) isEventPayload()      {}
func (NodeReady) isEventPayload()       {}
func (NodeStarted) isEventPayload()     {}
func (NodeCompleted) isEventPayload()   {}
func (NodeFailed) isEventPayload()      {}
func (NodeSkipped) isEventPayload()     {}
func (NodeRetrying) isEventPayload()    {}
func (NodeInterrupted) isEventPayload() {}

func (RunStarted) eventType() EventType     { return EventRunStarted }
func (RunCompleted) eventType() EventType   { return EventRunCompleted }
func (RunFailed) eventType() EventType      { return EventRunFailed }
func (RunCanceled) eventType() EventType    { return EventRunCanceled }
func (RunInterrupted) eventType() EventType { return EventRunInterrupted }
func (RunResumed) eventType() EventType     { return EventRunResumed }
func (NodeReady) eventType() EventType      { return EventNodeReady }
func (NodeStarted) eventType() EventType    { return EventNodeStarted }
func (NodeCompleted) eventType() EventType  { return EventNodeCompleted }
func (NodeFailed) eventType() EventType     { return EventNodeFailed }
func (NodeSkipped) eventType() EventType    { return EventNodeSkipped }
func (NodeRetrying) eventType() EventType   { return EventNodeRetrying }
func (NodeInterrupted) eventType() EventType {
	return EventNodeInterrupted
}

// Type returns the discriminator for e's payload.
func (e Event) Type() EventType {
	if e.payload == nil {
		return ""
	}

	return e.payload.eventType()
}

// Payload returns e's sealed semantic payload.
func (e Event) Payload() EventPayload {
	return e.payload
}

// Scope returns an independent snapshot of nested execution frames. Root
// events have an empty scope.
func (e Event) Scope() []ScopeFrame {
	return slices.Clone(e.scope)
}

// MarshalJSON prevents Event from being mistaken for a stable wire protocol.
func (Event) MarshalJSON() ([]byte, error) {
	return nil, ErrEventWireFormat
}

// UnmarshalJSON rejects an unspecified Event wire representation.
func (*Event) UnmarshalJSON([]byte) error {
	return ErrEventWireFormat
}

// EventSink synchronously observes process-local events. A Runner serializes
// calls to one sink for each Run and applies backpressure. The same sink may be
// called concurrently by concurrent Runs and must synchronize shared state.
type EventSink func(Event)
