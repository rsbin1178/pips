//nolint:wsl_v5 // Event projection keeps payload construction adjacent to validation.
package coding

import (
	"fmt"
	"sync"
	"time"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
)

type eventClock func() time.Time

type eventWriter struct {
	mu        sync.Mutex
	sessionID string
	clock     eventClock
	sequence  uint64
}

func newEventWriter(sessionID string, clock eventClock) (*eventWriter, error) {
	if err := validateEventID("session id", sessionID, true); err != nil {
		return nil, err
	}

	if clock == nil {
		return nil, invalidEvent("clock is required")
	}

	return &eventWriter{sessionID: sessionID, clock: clock}, nil
}

func (w *eventWriter) write(
	interactionID string,
	runID string,
	eventType EventType,
	payload EventPayload,
) (Event, error) {
	return w.writeAt(w.clock().UTC(), interactionID, runID, eventType, payload)
}

func (w *eventWriter) writeAt(
	eventTime time.Time,
	interactionID string,
	runID string,
	eventType EventType,
	payload EventPayload,
) (Event, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	event := Event{
		Schema:        EventSchema,
		Sequence:      w.sequence + 1,
		Time:          eventTime.UTC(),
		SessionID:     w.sessionID,
		InteractionID: interactionID,
		RunID:         runID,
		Type:          eventType,
		Payload:       cloneEventPayload(payload),
	}
	if err := ValidateEvent(event); err != nil {
		return Event{}, err
	}

	w.sequence = event.Sequence

	return cloneEvent(event), nil
}

type agentProjector struct {
	writer        *eventWriter
	interactionID string
	synthetic     func(ai.Message) bool
}

func newAgentProjector(writer *eventWriter, interactionID string) (*agentProjector, error) {
	if writer == nil {
		return nil, invalidEvent("event writer is required")
	}

	if err := validateEventID("interaction id", interactionID, true); err != nil {
		return nil, err
	}

	return &agentProjector{writer: writer, interactionID: interactionID}, nil
}

//nolint:gocyclo,cyclop // Projection intentionally mirrors every Agent lifecycle event.
func (p *agentProjector) project(event agent.Event) (Event, error) {
	if !validAgentEventType(event.Type) {
		return Event{}, fmt.Errorf("%w: unknown agent event %q", ErrInvalidEvent, event.Type)
	}

	var (
		eventType EventType
		payload   EventPayload
	)

	switch event.Type {
	case agent.EventRunStart:
		eventType = EventRunStarted
		payload = RunStarted{ParentRunID: event.ParentRunID, Agent: event.Agent}
	case agent.EventTurnStart:
		eventType = EventTurnStarted
		payload = TurnStarted{Turn: event.Turn}
	case agent.EventDelta:
		eventType = EventMessageDelta
		payload = messageDeltaFromAI(event.Delta)
	case agent.EventMessage:
		if event.Message == nil {
			return Event{}, invalidEvent("agent message event has no message")
		}

		eventType = EventMessageCommitted
		payload = MessageCommitted{Message: cloneMessage(*event.Message)}
		if p.synthetic != nil {
			payload = MessageCommitted{
				Message: cloneMessage(*event.Message), Synthetic: p.synthetic(*event.Message),
			}
		}
	case agent.EventToolStart:
		if event.Call == nil {
			return Event{}, invalidEvent("agent tool-start event has no call")
		}

		eventType = EventToolStarted
		payload = ToolStarted{Turn: event.Turn, Call: toolCallFromAI(*event.Call)}
	case agent.EventToolUpdate:
		if event.Call == nil {
			return Event{}, invalidEvent("agent tool-update event has no call")
		}

		eventType = EventToolUpdated
		payload = ToolUpdated{
			Turn:   event.Turn,
			Call:   toolCallFromAI(*event.Call),
			Update: toolUpdateMessage(event.Update),
		}
	case agent.EventToolEnd:
		if event.Call == nil || event.Result == nil {
			return Event{}, invalidEvent("agent tool-end event is incomplete")
		}

		eventType = EventToolCompleted
		payload = ToolCompleted{
			Turn:   event.Turn,
			Call:   toolCallFromAI(*event.Call),
			Result: toolResultMessage(*event.Result),
		}
	case agent.EventTurnEnd:
		eventType = EventTurnCompleted
		payload = TurnCompleted{Turn: event.Turn, Usage: tokenUsageFromAI(event.Usage)}
	case agent.EventRunEnd:
		eventType = EventRunCompleted
		payload = RunCompleted{
			Stop:  event.Stop,
			Turns: event.Turn,
			Usage: tokenUsageFromAI(event.Usage),
		}
	}

	return p.writer.writeAt(
		event.Time,
		p.interactionID,
		event.RunID,
		eventType,
		payload,
	)
}
