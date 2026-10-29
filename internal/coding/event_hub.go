//nolint:wsl_v5 // Hub lock transitions keep queue ownership visibly adjacent.
package coding

import (
	"errors"
	"sync"
)

const (
	defaultEventReplayCapacity     = 256
	defaultEventSubscriberCapacity = 256
	// defaultEventReplayBytes bounds the replay buffer by content, so 256 very
	// large events cannot pin hundreds of megabytes for a reconnecting client.
	defaultEventReplayBytes = 8 << 20
	// defaultEventSubscriberBytes bounds one subscriber's queued content. A
	// subscriber whose queue would exceed it is closed with ErrEventGap rather
	// than allowed to grow without limit.
	defaultEventSubscriberBytes = 4 << 20
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
//
// The hub clones an event once when it accepts it. Replay and every subscriber
// then share that immutable value; a consumer must not modify the event or any
// slice reachable from it.
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

// Receive returns the next record, blocking until one is queued or the
// subscription ends. ok is false once the subscription has ended; [Err] then
// reports why.
//
// The consumer pops through this method rather than reading a channel, because
// the pop is what releases the record's bytes from the subscriber's budget. A
// consumer that reads the queue without popping would be closed for content it
// already consumed.
func (s *EventSubscription) Receive() (EventRecord, bool) {
	if s == nil || s.subscriber == nil {
		return EventRecord{}, false
	}

	return s.subscriber.receive()
}

// TryReceive returns the next queued record without blocking. ok is false when
// nothing is queued, including on a subscription that has already ended.
func (s *EventSubscription) TryReceive() (EventRecord, bool) {
	if s == nil || s.subscriber == nil {
		return EventRecord{}, false
	}

	return s.subscriber.tryReceive()
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

// eventSubscriber is one consumer's queue. The hub appends under its own lock;
// the consumer pops through Receive. The queued byte total is therefore exact
// rather than a channel capacity estimated from an average event size, and a
// pop is what releases the bytes.
type eventSubscriber struct {
	mu          sync.Mutex
	queue       []EventRecord
	queuedBytes int
	// countCap and byteCap bound this consumer's queue. Both start at the hub's
	// configured caps and are raised to fit the replay backlog the consumer is
	// handed, because a reconnect is never closed for content the hub itself
	// chose to send.
	countCap int
	byteCap  int
	err      error
	closed   bool
	// signal wakes a blocked Receive. It is buffered, so a push never waits for
	// a consumer that is not reading.
	signal chan struct{}
}

// push appends one record and reports whether it fit the consumer's budget. A
// record that does not fit closes the consumer instead: it can never be
// delivered within the budget, and dropping it silently would leave a hole.
func (s *eventSubscriber) push(record EventRecord, size int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return true
	}
	if len(s.queue) >= s.countCap || s.queuedBytes+size > s.byteCap {
		return false
	}

	s.queue = append(s.queue, record)
	s.queuedBytes += size
	s.wake()

	return true
}

func (s *eventSubscriber) receive() (EventRecord, bool) {
	for {
		s.mu.Lock()
		if record, ok := s.popLocked(); ok {
			s.mu.Unlock()

			return record, true
		}
		if s.closed {
			s.mu.Unlock()

			return EventRecord{}, false
		}

		signal := s.signal
		s.mu.Unlock()

		<-signal
	}
}

func (s *eventSubscriber) tryReceive() (EventRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.popLocked()
}

// popLocked removes the oldest record and releases its bytes. The slot is
// cleared so a drained queue retains no event it has already delivered.
func (s *eventSubscriber) popLocked() (EventRecord, bool) {
	if len(s.queue) == 0 {
		return EventRecord{}, false
	}

	record := s.queue[0]
	s.queuedBytes -= eventRecordBytes(record)
	s.queue[0] = EventRecord{}
	s.queue = s.queue[1:]
	if len(s.queue) == 0 {
		s.queue = s.queue[:0]
	}

	return record, true
}

// wake releases a Receive blocked on an empty queue. A lost wakeup cannot hang
// a consumer: push and finish both hold the lock that Receive re-checks under.
func (s *eventSubscriber) wake() {
	select {
	case s.signal <- struct{}{}:
	default:
	}
}

func (s *eventSubscriber) finish(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}

	s.err = err
	s.closed = true
	s.wake()
}

type eventHub struct {
	mu sync.Mutex

	replayCapacity     int
	subscriberCapacity int
	replayByteCapacity int
	subscriberBytes    int
	cursor             uint64
	replay             []EventRecord
	replayBytes        int
	subscribers        map[*eventSubscriber]struct{}
	closed             bool
}

func newEventHub(
	replayCapacity int,
	subscriberCapacity int,
) *eventHub {
	return newEventHubWithBudget(replayCapacity, subscriberCapacity, 0, 0)
}

func newEventHubWithBudget(
	replayCapacity int,
	subscriberCapacity int,
	replayBytes int,
	subscriberBytes int,
) *eventHub {
	if replayCapacity <= 0 {
		replayCapacity = defaultEventReplayCapacity
	}
	if subscriberCapacity <= 0 {
		subscriberCapacity = defaultEventSubscriberCapacity
	}
	if replayBytes <= 0 {
		replayBytes = defaultEventReplayBytes
	}
	if subscriberBytes <= 0 {
		subscriberBytes = defaultEventSubscriberBytes
	}

	return &eventHub{
		replayCapacity:     replayCapacity,
		subscriberCapacity: subscriberCapacity,
		replayByteCapacity: replayBytes,
		subscriberBytes:    subscriberBytes,
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
	recordBytes := eventRecordBytes(record)
	h.replay = append(h.replay, record)
	h.replayBytes += recordBytes
	for len(h.replay) > h.replayCapacity || h.replayBytes > h.replayByteCapacity {
		if len(h.replay) <= 1 {
			break
		}
		h.replayBytes -= eventRecordBytes(h.replay[0])
		h.replay[0] = EventRecord{}
		h.replay = h.replay[1:]
	}

	for subscriber := range h.subscribers {
		if subscriber.push(record, recordBytes) {
			continue
		}

		// The record does not fit this consumer's budget, so it cannot be
		// delivered within it: the consumer is told to resubscribe.
		delete(h.subscribers, subscriber)
		subscriber.finish(ErrEventGap)
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
	backlogBytes := 0
	for _, record := range pending {
		backlogBytes += eventRecordBytes(record)
	}

	countCap := max(h.subscriberCapacity, len(pending))
	subscriber := &eventSubscriber{
		queue:    make([]EventRecord, len(pending), countCap),
		countCap: countCap,
		byteCap:  max(h.subscriberBytes, backlogBytes),
		signal:   make(chan struct{}, 1),
	}
	copy(subscriber.queue, pending)
	subscriber.queuedBytes = backlogBytes
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
