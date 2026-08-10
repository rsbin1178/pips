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

func (w *eventWriter) prepare(
	interactionID string,
	runID string,
	eventType EventType,
	payload EventPayload,
) (Event, error) {
	return w.prepareAt(w.clock().UTC(), interactionID, runID, eventType, payload)
}

func (w *eventWriter) prepareAt(
	eventTime time.Time,
	interactionID string,
	runID string,
	eventType EventType,
	payload EventPayload,
) (Event, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.prepareAtLocked(eventTime, interactionID, runID, eventType, payload)
}

func (w *eventWriter) prepareAtLocked(
	eventTime time.Time,
	interactionID string,
	runID string,
	eventType EventType,
	payload EventPayload,
) (Event, error) {
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

	return cloneEvent(event), nil
}

func (w *eventWriter) commit(event Event) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if event.SessionID != w.sessionID {
		return fmt.Errorf("%w: event writer Session changed", ErrEventProtocol)
	}
	if event.Sequence != w.sequence+1 {
		return fmt.Errorf(
			"%w: event writer sequence %d follows %d",
			ErrEventProtocol,
			event.Sequence,
			w.sequence,
		)
	}
	if err := ValidateEvent(event); err != nil {
		return err
	}

	w.sequence = event.Sequence

	return nil
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
	if err := event.Validate(); err != nil {
		return Event{}, fmt.Errorf("%w: invalid agent event: %w", ErrInvalidEvent, err)
	}

	var (
		eventType EventType
		payload   EventPayload
	)

	switch source := event.Payload().(type) {
	case agent.RunStarted:
		eventType = EventRunStarted
		payload = RunStarted{ParentRunID: event.ParentRunID, Agent: event.Agent}
	case agent.TurnStarted:
		eventType = EventTurnStarted
		payload = TurnStarted{Turn: source.Turn}
	case agent.ModelStreamEvent:
		eventType = EventMessageDelta
		payload = messageDeltaFromAI(source.Event)
	case agent.MessageCommitted:
		eventType = EventMessageCommitted
		payload = MessageCommitted{Message: cloneMessage(source.Message)}
		if p.synthetic != nil {
			payload = MessageCommitted{
				Message: cloneMessage(source.Message), Synthetic: p.synthetic(source.Message),
			}
		}
	case agent.CandidateDiscarded:
		eventType = EventMessageDiscarded
		payload = MessageDiscarded{Turn: source.Turn}
	case agent.ToolStarted:
		eventType = EventToolStarted
		payload = ToolStarted{Turn: source.Turn, Call: toolCallFromAI(source.Call)}
	case agent.ToolUpdated:
		eventType = EventToolUpdated
		payload = ToolUpdated{
			Turn:   source.Turn,
			Call:   toolCallFromAI(source.Call),
			Update: toolUpdateMessage(source.Update),
		}
	case agent.ToolCompleted:
		eventType = EventToolCompleted
		payload = ToolCompleted{
			Turn:   source.Turn,
			Call:   toolCallFromAI(source.Call),
			Result: toolResultMessage(source.Result),
		}
	case agent.TurnCompleted:
		eventType = EventTurnCompleted
		payload = TurnCompleted{Turn: source.Turn, Usage: tokenUsageFromAI(source.Usage)}
	case agent.RunCompleted:
		eventType = EventRunCompleted
		payload = RunCompleted{
			Stop:  source.Stop,
			Turns: source.Turns,
			Usage: tokenUsageFromAI(source.Usage),
		}
	}

	return p.writer.prepareAt(
		event.Time,
		p.interactionID,
		event.RunID,
		eventType,
		payload,
	)
}
