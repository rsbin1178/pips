//nolint:wsl_v5 // Publisher commits and broadcasts are one atomic protocol path.
package coding

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/rsbin/pips/agent"
)

// EventObservation atomically pairs the current Runtime State with a
// subscription that starts immediately after Cursor.
type EventObservation struct {
	State        State
	Children     map[string]State
	Cursor       uint64
	Subscription *EventSubscription
}

// eventPublisher is the single Runtime commit boundary for event sequence,
// reducer state, telemetry, and best-effort subscriber delivery.
type eventPublisher struct {
	mu      sync.Mutex
	runtime *Runtime
	hub     *eventHub
}

func newEventPublisher(runtime *Runtime) *eventPublisher {
	return &eventPublisher{
		runtime: runtime,
		hub:     newEventHub(defaultEventReplayCapacity, defaultEventSubscriberCapacity),
	}
}

func (p *eventPublisher) emit(
	ctx context.Context,
	emitter *eventEmitter,
	interactionID string,
	runID string,
	eventType EventType,
	payload EventPayload,
) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.emitLocked(ctx, emitter, interactionID, runID, eventType, payload)
}

func (p *eventPublisher) emitLocked(
	ctx context.Context,
	emitter *eventEmitter,
	interactionID string,
	runID string,
	eventType EventType,
	payload EventPayload,
) error {
	event, err := p.runtime.writer.prepare(interactionID, runID, eventType, payload)
	if err != nil {
		return err
	}

	return p.publishLocked(ctx, emitter, p.runtime.writer, event)
}

func (p *eventPublisher) publishAgent(
	ctx context.Context,
	emitter *eventEmitter,
	projector *agentProjector,
	event agent.Event,
) (Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	projected, err := projector.project(event)
	if err != nil {
		return Event{}, err
	}

	return projected, p.publishLocked(ctx, emitter, projector.writer, projected)
}

func (p *eventPublisher) publishLocked(
	ctx context.Context,
	emitter *eventEmitter,
	writer *eventWriter,
	event Event,
) error {
	if writer == nil {
		return invalidEvent("event writer is required")
	}
	p.runtime.mu.Lock()
	next, err := Reduce(p.runtime.state, event)
	if err == nil {
		err = writer.commit(event)
		if err == nil {
			p.runtime.state = next
		}
	}
	p.runtime.mu.Unlock()
	if err != nil {
		return err
	}

	diagnostics := p.runtime.observeEvent(ctx, event)
	p.hub.publish(event)

	consumerStopped := emitter != nil && !emitter.deliver(event)
	for _, diagnostic := range diagnostics {
		if err := p.emitLocked(
			ctx,
			emitter,
			"",
			"",
			EventIntegrationDiagnostic,
			diagnostic,
		); err != nil {
			if errors.Is(err, errConsumerStopped) {
				consumerStopped = true

				continue
			}

			return err
		}
	}

	if consumerStopped {
		return errConsumerStopped
	}

	return nil
}

func (p *eventPublisher) observe() (EventObservation, error) {
	if p == nil {
		return EventObservation{}, ErrRuntimeClosed
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.runtime.mu.Lock()
	state := p.runtime.state.Clone()
	p.runtime.mu.Unlock()
	children := make(map[string]State, len(p.runtime.children))
	for childSessionID, child := range p.runtime.children {
		children[childSessionID] = child.state.Clone()
	}

	cursor := p.hub.currentCursor()
	subscription, _, err := p.hub.subscribe(cursor)
	if err != nil {
		return EventObservation{}, err
	}

	return EventObservation{
		State: state, Children: children, Cursor: cursor, Subscription: subscription,
	}, nil
}

func (p *eventPublisher) publishChildGeneratedLocked(
	ctx context.Context,
	child *childProjection,
	eventTime time.Time,
	runID string,
	eventType EventType,
	payload EventPayload,
) error {
	interactionID := child.interactionID
	if eventType == EventSessionOpened {
		interactionID = ""
	}
	event, err := child.writer.prepareAt(
		eventTime,
		interactionID,
		runID,
		eventType,
		payload,
	)
	if err != nil {
		return err
	}

	return p.publishChildLocked(ctx, child, event)
}

func (p *eventPublisher) publishChildLocked(
	ctx context.Context,
	child *childProjection,
	event Event,
) error {
	next, err := Reduce(child.state, event)
	if err != nil {
		return err
	}
	if err := child.writer.commit(event); err != nil {
		return err
	}
	child.state = next

	diagnostics := p.runtime.observeEvent(ctx, event)
	p.hub.publish(event)
	for _, diagnostic := range diagnostics {
		if err := p.publishChildGeneratedLocked(
			ctx,
			child,
			event.Time,
			"",
			EventIntegrationDiagnostic,
			diagnostic,
		); err != nil {
			return err
		}
	}

	return nil
}

func (p *eventPublisher) close() {
	if p == nil {
		return
	}

	p.mu.Lock()
	p.hub.close()
	p.mu.Unlock()
}
