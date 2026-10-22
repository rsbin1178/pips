//nolint:wsl_v5 // Hub fixtures keep publish/subscribe assertions adjacent.
package coding

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEventHubReplaysAndBroadcastsInOrder(t *testing.T) {
	t.Parallel()

	hub := newEventHub(8, 4)
	hub.publish(hubTestEvent(1))
	hub.publish(hubTestEvent(2))

	first, cursor, err := hub.subscribe(0)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), cursor)
	second, _, err := hub.subscribe(1)
	require.NoError(t, err)
	t.Cleanup(first.Close)
	t.Cleanup(second.Close)

	hub.publish(hubTestEvent(3))

	assert.Equal(t, []uint64{1, 2, 3}, receiveEventCursors(t, first, 3))
	assert.Equal(t, []uint64{2, 3}, receiveEventCursors(t, second, 2))
}

func TestEventHubIsolatesSlowSubscriber(t *testing.T) {
	t.Parallel()

	hub := newEventHub(8, 1)
	slow, _, err := hub.subscribe(0)
	require.NoError(t, err)
	fast, _, err := hub.subscribe(0)
	require.NoError(t, err)
	t.Cleanup(slow.Close)
	t.Cleanup(fast.Close)

	hub.publish(hubTestEvent(1))
	assert.Equal(t, []uint64{1}, receiveEventCursors(t, fast, 1))
	hub.publish(hubTestEvent(2))
	assert.Equal(t, []uint64{2}, receiveEventCursors(t, fast, 1))

	assert.Equal(t, []uint64{1}, receiveEventCursors(t, slow, 1))
	_, ok := awaitRecord(t, slow)
	assert.False(t, ok)
	assert.ErrorIs(t, slow.Err(), ErrEventGap)
}

func TestEventHubRejectsExpiredCursor(t *testing.T) {
	t.Parallel()

	hub := newEventHub(2, 2)
	for sequence := uint64(1); sequence <= 3; sequence++ {
		hub.publish(hubTestEvent(sequence))
	}

	_, cursor, err := hub.subscribe(0)
	assert.Equal(t, uint64(3), cursor)
	assert.ErrorIs(t, err, ErrEventGap)
}

func TestEventHubCloseEndsSubscribers(t *testing.T) {
	t.Parallel()

	hub := newEventHub(2, 2)
	subscription, _, err := hub.subscribe(0)
	require.NoError(t, err)

	hub.close()

	_, ok := awaitRecord(t, subscription)
	assert.False(t, ok)
	require.NoError(t, subscription.Err())
	_, _, err = hub.subscribe(0)
	assert.ErrorIs(t, err, ErrEventStreamClosed)
}

func hubTestEvent(sequence uint64) Event {
	return Event{
		Schema: EventSchema, Sequence: sequence, Time: time.Unix(0, 0).UTC(),
		SessionID: "session", Type: EventIntegrationDiagnostic,
		Payload: IntegrationDiagnostic{Component: "test", Code: "event"},
	}
}

func receiveEventCursors(
	t *testing.T,
	subscription *EventSubscription,
	count int,
) []uint64 {
	t.Helper()

	values := make([]uint64, 0, count)
	for range count {
		record, ok := awaitRecord(t, subscription)
		require.True(t, ok)
		values = append(values, record.Cursor)
	}

	return values
}

// awaitRecord reads one record, failing the test rather than blocking forever
// when the hub was already supposed to have queued it.
func awaitRecord(t *testing.T, subscription *EventSubscription) (EventRecord, bool) {
	t.Helper()

	type result struct {
		record EventRecord
		ok     bool
	}

	received := make(chan result, 1)
	go func() {
		record, ok := subscription.Receive()
		received <- result{record: record, ok: ok}
	}()

	select {
	case value := <-received:
		return value.record, value.ok
	case <-time.After(30 * time.Second):
		require.FailNow(t, "timed out waiting for event")

		return EventRecord{}, false
	}
}
