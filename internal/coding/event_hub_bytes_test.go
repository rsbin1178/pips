package coding

import (
	"strings"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func hubPayloadEvent(sequence uint64, size int) Event {
	event := hubTestEvent(sequence)
	event.Type = EventMessageCommitted
	event.Payload = MessageCommitted{Message: ai.UserText(strings.Repeat("m", size))}

	return event
}

// TestEventHubReplayByteBudgetDropsOldest pins the replay bound: the buffer
// drops its oldest records once the content budget is reached, and a cursor into
// the dropped range is a gap rather than a silent hole.
func TestEventHubReplayByteBudgetDropsOldest(t *testing.T) {
	t.Parallel()

	hub := newEventHubWithBudget(64, 8, 4<<20, 1<<20)
	for sequence := uint64(1); sequence <= 16; sequence++ {
		hub.publish(hubPayloadEvent(sequence, 512<<10))
	}

	assert.LessOrEqual(t, hub.replayBytes, hub.replayByteCapacity)
	assert.Less(t, len(hub.replay), 16, "the byte budget trims before the count cap")
	require.NotEmpty(t, hub.replay)
	assert.Equal(t, uint64(16), hub.replay[len(hub.replay)-1].Cursor, "the newest record is kept")

	_, _, err := hub.subscribe(0)
	assert.ErrorIs(t, err, ErrEventGap, "a cursor into the dropped range is a gap")
}

// TestEventHubSubscriberByteBudgetClosesOversizedEvent pins the single-event
// case: an event larger than a subscriber's whole budget cannot be delivered,
// so the consumer is told to resubscribe.
func TestEventHubSubscriberByteBudgetClosesOversizedEvent(t *testing.T) {
	t.Parallel()

	hub := newEventHubWithBudget(8, 8, 1<<20, 1<<20)
	subscription, _, err := hub.subscribe(0)
	require.NoError(t, err)
	t.Cleanup(subscription.Close)

	hub.publish(hubPayloadEvent(1, 2<<20))

	_, ok := awaitRecord(t, subscription)
	assert.False(t, ok)
	assert.ErrorIs(t, subscription.Err(), ErrEventGap)
}

// TestEventHubSubscriberByteBudgetClosesSlowConsumer pins the slow-consumer
// bound by content rather than by count: a burst of large events closes a
// subscriber that does not read, and its queued bytes never exceed the budget it
// was given.
func TestEventHubSubscriberByteBudgetClosesSlowConsumer(t *testing.T) {
	t.Parallel()

	hub := newEventHubWithBudget(64, 64, 1<<20, 4<<20)
	// One published record teaches the replay buffer the event size, so the
	// subscriber is handed a budget measured against real content.
	hub.publish(hubPayloadEvent(1, 512<<10))
	subscription, _, err := hub.subscribe(0)
	require.NoError(t, err)
	t.Cleanup(subscription.Close)

	for sequence := uint64(2); sequence <= 32; sequence++ {
		hub.publish(hubPayloadEvent(sequence, 512<<10))
	}

	require.ErrorIs(t, subscription.Err(), ErrEventGap,
		"the byte budget closes a consumer that never reads")
	assert.LessOrEqual(t, subscription.subscriber.queuedBytes, hub.subscriberBytes,
		"a subscriber's queued bytes stay inside its budget")

	delivered := 0

	for {
		_, ok := awaitRecord(t, subscription)
		if !ok {
			break
		}

		delivered++
	}

	assert.Positive(t, delivered, "the records that fit the budget are still delivered")
	assert.Less(t, delivered, 31, "the budget closes the slow subscriber early")
}

// TestEventHubSubscriberByteBudgetIsExact pins that the budget counts each
// record's own content. An average-sized channel capacity could accept a burst
// larger than the budget, or close a consumer that was still inside it.
func TestEventHubSubscriberByteBudgetIsExact(t *testing.T) {
	t.Parallel()

	const size = 1 << 20

	hub := newEventHubWithBudget(64, 64, 64<<20, 4<<20)
	subscription, _, err := hub.subscribe(0)
	require.NoError(t, err)
	t.Cleanup(subscription.Close)

	accepted := 0

	for sequence := uint64(1); sequence <= 8; sequence++ {
		hub.publish(hubPayloadEvent(sequence, size))

		if subscription.Err() != nil {
			break
		}

		accepted++
	}

	require.ErrorIs(t, subscription.Err(), ErrEventGap)

	queued := subscription.subscriber.queuedBytes

	assert.Positive(t, accepted)
	assert.Less(t, accepted, 8)
	assert.LessOrEqual(t, queued, hub.subscriberBytes)
	assert.Greater(t, queued+size, hub.subscriberBytes,
		"one more record of at least this size would not have fit")
}

// TestEventHubSubscriberBytesAreReleasedOnReceive pins the exact accounting in
// the other direction: a pop releases the record's own bytes, so a consumer that
// keeps reading is never closed for content it already consumed.
func TestEventHubSubscriberBytesAreReleasedOnReceive(t *testing.T) {
	t.Parallel()

	const size = 512 << 10

	hub := newEventHubWithBudget(64, 64, 1<<20, 4<<20)
	subscription, _, err := hub.subscribe(0)
	require.NoError(t, err)
	t.Cleanup(subscription.Close)

	// Sixty-four records of half a megabyte are far more than one budget; each
	// one is popped before the next arrives.
	for sequence := uint64(1); sequence <= 64; sequence++ {
		hub.publish(hubPayloadEvent(sequence, size))
		require.NoError(t, subscription.Err(), "record %d closed a reading consumer", sequence)

		record, ok := awaitRecord(t, subscription)
		require.True(t, ok)
		assert.Equal(t, sequence, record.Event.Sequence)
		assert.Zero(t, subscription.subscriber.queuedBytes, "a pop releases the record's bytes")
	}
}

// BenchmarkEventHubSubscriberQueue measures the per-event cost of the exact byte
// accounting: one publish into one reading subscriber, so the number is the queue
// append plus the pop that releases the record's bytes. A drained queue keeps its
// capacity, so the steady state does not allocate per event.
func BenchmarkEventHubSubscriberQueue(b *testing.B) {
	event := hubTestEvent(1)
	hub := newEventHubWithBudget(256, 256, 8<<20, 4<<20)

	subscription, _, err := hub.subscribe(0)
	if err != nil {
		b.Fatal(err)
	}
	defer subscription.Close()

	b.ReportAllocs()

	for b.Loop() {
		hub.publish(event)

		if _, ok := subscription.TryReceive(); !ok {
			b.Fatal("the record was not queued")
		}

		if subscription.subscriber.queuedBytes != 0 {
			b.Fatal("a pop did not release the record's bytes")
		}
	}
}
