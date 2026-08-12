package agent

import (
	"fmt"
	"time"

	"github.com/rsbin1178/pips/ai"
)

// Validate verifies the event envelope and payload invariants. All failures
// match [ErrInvalidEvent]. It does not replace independent validation of a
// complete model message or model stream state machine.
func (e Event) Validate() error {
	if err := validateEventEnvelope(e); err != nil {
		return err
	}

	return validateEventPayload(e.payload)
}

func validateEventEnvelope(event Event) error {
	switch {
	case event.RunID == "":
		return invalidEvent("RunID is empty")
	case event.Time.IsZero():
		return invalidEvent("Time is zero")
	case event.Time.Location() != time.UTC:
		return invalidEvent("Time is not UTC")
	default:
		return nil
	}
}

func validateEventPayload(payload EventPayload) error {
	if _, ok := eventPayloadType(payload); !ok {
		return invalidEvent("payload is not a supported concrete value")
	}

	var turn int

	switch value := payload.(type) {
	case RunStarted:
		return nil
	case TurnStarted:
		turn = value.Turn
	case ModelStreamEvent:
		return validateModelStreamEvent(value)
	case MessageCommitted:
		return validateMessageCommitted(value)
	case CandidateDiscarded:
		turn = value.Turn
	case ToolStarted:
		turn = value.Turn
	case ToolUpdated:
		turn = value.Turn
	case ToolCompleted:
		turn = value.Turn
	case TurnCompleted:
		turn = value.Turn
	case RunCompleted:
		return validateRunCompleted(value)
	}

	return validateEventTurn(turn)
}

func validateMessageCommitted(event MessageCommitted) error {
	if err := validateEventTurn(event.Turn); err != nil {
		return err
	}

	if err := ai.ValidateMessage(event.Message); err != nil {
		return invalidEvent("committed message is invalid: %v", err)
	}

	return nil
}

func validateModelStreamEvent(event ModelStreamEvent) error {
	if err := validateEventTurn(event.Turn); err != nil {
		return err
	}

	if !validModelStreamEventType(event.Event.Type) {
		return invalidEvent("model stream event type %q is invalid", event.Event.Type)
	}

	return nil
}

func validateRunCompleted(event RunCompleted) error {
	if event.Turns < 1 {
		return invalidEvent("run completion turns must be positive")
	}

	if !validStopReason(event.Stop) {
		return invalidEvent("run completion stop reason %q is invalid", event.Stop)
	}

	return nil
}

func eventPayloadType(payload EventPayload) (EventType, bool) {
	switch payload.(type) {
	case RunStarted:
		return EventRunStarted, true
	case TurnStarted:
		return EventTurnStarted, true
	case ModelStreamEvent:
		return EventModelStream, true
	case MessageCommitted:
		return EventMessageCommitted, true
	case CandidateDiscarded:
		return EventCandidateDiscarded, true
	case ToolStarted:
		return EventToolStarted, true
	case ToolUpdated:
		return EventToolUpdated, true
	case ToolCompleted:
		return EventToolCompleted, true
	case TurnCompleted:
		return EventTurnCompleted, true
	case RunCompleted:
		return EventRunCompleted, true
	default:
		return "", false
	}
}

func validateEventTurn(turn int) error {
	if turn < 1 {
		return invalidEvent("turn must be positive")
	}

	return nil
}

func validModelStreamEventType(eventType ai.StreamEventType) bool {
	switch eventType {
	case ai.StreamMessageStart,
		ai.StreamTextDelta,
		ai.StreamReasoningDelta,
		ai.StreamToolCallStart,
		ai.StreamToolCallDelta,
		ai.StreamToolCallEnd,
		ai.StreamMessageEnd:
		return true
	default:
		return false
	}
}

func validStopReason(reason StopReason) bool {
	switch reason {
	case StopEndTurn, StopMaxTurns, StopBudget, StopPaused, StopWhen, StopTerminated:
		return true
	default:
		return false
	}
}

func invalidEvent(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidEvent, fmt.Sprintf(format, args...))
}
