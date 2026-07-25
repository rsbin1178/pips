//nolint:wsl_v5 // Hub lock transitions keep channel ownership visibly adjacent.
package coding

import (
	"errors"
	"sync"
)

const (
	defaultEventReplayCapacity     = 256
	defaultEventSubscriberCapacity = 256
)

var (
	// ErrEventGap means a subscriber can no longer continue from its cursor.
	// Callers must obtain a fresh snapshot and subscription.
	ErrEventGap = errors.New("coding runtime: event subscription gap")
	// ErrEventStreamClosed means the Runtime no longer accepts subscriptions.
	ErrEventStreamClosed = errors.New("coding runtime: event stream closed")
)

// EventRecord pairs a Runtime-local broadcast cursor with one Coding event.
// Cursor is intentionally independent from Event.Sequence so a future hub can
// also carry child Session events without changing the per-Session protocol.
type EventRecord struct {
	Cursor uint64
	Event  Event
}

// EventSubscription is one bounded, independently cancelable Runtime event
// consumer. A slow consumer is closed with [ErrEventGap]; it never applies
// backpressure to the Runtime or its Agent execution.
type EventSubscription struct {
	hub        *eventHub
	subscriber *eventSubscriber
	once       sync.Once
}

// Events returns the ordered event channel owned by the subscription.
func (s *EventSubscription) Events() <-chan EventRecord {
	if s == nil || s.subscriber == nil {
		closed := make(chan EventRecord)
		close(closed)

		return closed
	}

	return s.subscriber.events
}

// Err reports why the subscription ended. It returns nil for an explicit
// Close or a normal Runtime event-stream close.
func (s *EventSubscription) Err() error {
	if s == nil || s.subscriber == nil {
		return nil
	}

	s.subscriber.mu.Lock()
	defer s.subscriber.mu.Unlock()

	return s.subscriber.err
}

// Close stops this consumer without affecting the Runtime or other consumers.
func (s *EventSubscription) Close() {
	if s == nil {
		return
	}

	s.once.Do(func() {
		if s.hub != nil && s.subscriber != nil {
			s.hub.unsubscribe(s.subscriber)
		}
	})
}

type eventSubscriber struct {
	mu     sync.Mutex
	events chan EventRecord
	err    error
	closed bool
}

func (s *eventSubscriber) finish(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}

	s.err = err
	s.closed = true
	close(s.events)
}

type eventHub struct {
	mu sync.Mutex

	replayCapacity     int
	subscriberCapacity int
	cursor             uint64
	replay             []EventRecord
	subscribers        map[*eventSubscriber]struct{}
	closed             bool
}

func newEventHub(
	replayCapacity int,
	subscriberCapacity int,
) *eventHub {
	if replayCapacity <= 0 {
		replayCapacity = defaultEventReplayCapacity
	}
	if subscriberCapacity <= 0 {
		subscriberCapacity = defaultEventSubscriberCapacity
	}

	return &eventHub{
		replayCapacity:     replayCapacity,
		subscriberCapacity: subscriberCapacity,
		replay:             make([]EventRecord, 0, replayCapacity),
		subscribers:        make(map[*eventSubscriber]struct{}),
	}
}

func (h *eventHub) publish(event Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}

	h.cursor++
	record := EventRecord{Cursor: h.cursor, Event: cloneEvent(event)}
	h.replay = append(h.replay, record)
	if overflow := len(h.replay) - h.replayCapacity; overflow > 0 {
		copy(h.replay, h.replay[overflow:])
		h.replay = h.replay[:h.replayCapacity]
	}

	for subscriber := range h.subscribers {
		select {
		case subscriber.events <- cloneEventRecord(record):
		default:
			delete(h.subscribers, subscriber)
			subscriber.finish(ErrEventGap)
		}
	}
}

func (h *eventHub) subscribe(afterCursor uint64) (*EventSubscription, uint64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, h.cursor, ErrEventStreamClosed
	}
	if afterCursor > h.cursor || h.hasGap(afterCursor) {
		return nil, h.cursor, ErrEventGap
	}

	pending := h.recordsAfter(afterCursor)
	capacity := max(h.subscriberCapacity, len(pending))
	subscriber := &eventSubscriber{events: make(chan EventRecord, capacity)}
	for _, record := range pending {
		subscriber.events <- cloneEventRecord(record)
	}
	h.subscribers[subscriber] = struct{}{}

	return &EventSubscription{hub: h, subscriber: subscriber}, h.cursor, nil
}

func (h *eventHub) currentCursor() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.cursor
}

func (h *eventHub) hasGap(afterCursor uint64) bool {
	if afterCursor == h.cursor || len(h.replay) == 0 {
		return false
	}

	oldest := h.replay[0].Cursor

	return afterCursor+1 < oldest
}

func (h *eventHub) recordsAfter(afterCursor uint64) []EventRecord {
	start := len(h.replay)
	for index, record := range h.replay {
		if record.Cursor > afterCursor {
			start = index
			break
		}
	}

	return h.replay[start:]
}

func (h *eventHub) unsubscribe(subscriber *eventSubscriber) {
	if h == nil || subscriber == nil {
		return
	}

	h.mu.Lock()
	if _, ok := h.subscribers[subscriber]; ok {
		delete(h.subscribers, subscriber)
		subscriber.finish(nil)
	}
	h.mu.Unlock()
}

func (h *eventHub) close() {
	if h == nil {
		return
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}

	h.closed = true
	for subscriber := range h.subscribers {
		delete(h.subscribers, subscriber)
		subscriber.finish(nil)
	}
	h.mu.Unlock()
}

func cloneEventRecord(record EventRecord) EventRecord {
	record.Event = cloneEvent(record.Event)

	return record
}
