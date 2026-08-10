package workflow

import (
	"slices"
	"time"
)

// EventType identifies one process-local Workflow event variant.
type EventType string

// Event types.
const (
	EventRunStarted    EventType = "run_started"
	EventRunCompleted  EventType = "run_completed"
	EventRunFailed     EventType = "run_failed"
	EventRunCanceled   EventType = "run_canceled"
	EventNodeReady     EventType = "node_ready"
	EventNodeStarted   EventType = "node_started"
	EventNodeCompleted EventType = "node_completed"
	EventNodeFailed    EventType = "node_failed"
	EventNodeSkipped   EventType = "node_skipped"
	EventNodeRetrying  EventType = "node_retrying"
)

// ScopeKind identifies one nested execution boundary.
type ScopeKind string

// Nested execution scope kinds.
const (
	ScopeSubWorkflow ScopeKind = "sub_workflow"
	ScopeBatchItem   ScopeKind = "batch_item"
)

// ScopeFrame identifies the composite node that owns one child execution.
// Index is -1 for SubWorkflow and the input index for a Batch item.
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
}

// RunStarted opens one Run.
type RunStarted struct{}

// RunCompleted closes a successful Run.
type RunCompleted struct{}

// RunFailed closes a failed Run.
type RunFailed struct{}

// RunCanceled closes a canceled Run.
type RunCanceled struct{}

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

func (RunStarted) isEventPayload()    {}
func (RunCompleted) isEventPayload()  {}
func (RunFailed) isEventPayload()     {}
func (RunCanceled) isEventPayload()   {}
func (NodeReady) isEventPayload()     {}
func (NodeStarted) isEventPayload()   {}
func (NodeCompleted) isEventPayload() {}
func (NodeFailed) isEventPayload()    {}
func (NodeSkipped) isEventPayload()   {}
func (NodeRetrying) isEventPayload()  {}

// Type returns the discriminator for e's payload.
func (e Event) Type() EventType {
	switch e.payload.(type) {
	case RunStarted:
		return EventRunStarted
	case RunCompleted:
		return EventRunCompleted
	case RunFailed:
		return EventRunFailed
	case RunCanceled:
		return EventRunCanceled
	case NodeReady:
		return EventNodeReady
	case NodeStarted:
		return EventNodeStarted
	case NodeCompleted:
		return EventNodeCompleted
	case NodeFailed:
		return EventNodeFailed
	case NodeSkipped:
		return EventNodeSkipped
	case NodeRetrying:
		return EventNodeRetrying
	default:
		return ""
	}
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
