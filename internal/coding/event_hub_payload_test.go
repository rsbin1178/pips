//nolint:wsl_v5 // The shared-payload fixture and the old comparison stay adjacent.
package coding

import (
	"bytes"
	"runtime"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func largeMediaEvent(size int) Event {
	return Event{
		Schema: EventSchema, Sequence: 1, Type: EventMessageCommitted,
		Payload: MessageCommitted{Message: ai.User(ai.ImagePart{
			Source: ai.MediaSource{Data: bytes.Repeat([]byte{7}, size), MIMEType: "image/png"},
		})},
	}
}

func eventMediaData(event Event) []byte {
	committed, ok := event.Payload.(MessageCommitted)
	if !ok {
		return nil
	}
	user, ok := committed.Message.(ai.UserMessage)
	if !ok || len(user.Parts) == 0 {
		return nil
	}
	image, ok := user.Parts[0].(ai.ImagePart)
	if !ok {
		return nil
	}

	return image.Source.Data
}

func TestEventHubSharesOneImmutablePayload(t *testing.T) {
	t.Parallel()

	hub := newEventHub(8, 8)
	subscription, _, err := hub.subscribe(0)
	require.NoError(t, err)
	defer subscription.Close()

	event := largeMediaEvent(1 << 20)
	hub.publish(event)

	record, ok := awaitRecord(t, subscription)
	require.True(t, ok)
	require.Len(t, hub.replay, 1)
	assert.Same(t, &eventMediaData(hub.replay[0].Event)[0], &eventMediaData(record.Event)[0],
		"replay and the subscriber share the one accepted payload")
}

// cloneEventRecordOld is the pre-optimisation delivery shape: a second deep
// clone for every subscriber. The benchmark keeps it as the comparison point.
func cloneEventRecordOld(record EventRecord) EventRecord {
	record.Event = cloneEvent(record.Event)

	return record
}

// BenchmarkEventHubPublish measures one large event delivered to four
// subscribers. The shared path clones the event once; the old path cloned it
// once for replay and again for every subscriber.
func BenchmarkEventHubPublish(b *testing.B) {
	event := largeMediaEvent(1 << 20)

	run := func(b *testing.B, cloned bool) {
		hub := newEventHub(256, 256)
		subscriptions := make([]*EventSubscription, 4)
		for index := range subscriptions {
			subscription, _, err := hub.subscribe(0)
			if err != nil {
				b.Fatal(err)
			}
			subscriptions[index] = subscription
		}
		defer func() {
			for _, subscription := range subscriptions {
				subscription.Close()
			}
		}()

		b.ReportAllocs()

		for b.Loop() {
			if cloned {
				publishCloned(hub, event)
			} else {
				hub.publish(event)
			}
			for _, subscription := range subscriptions {
				if _, ok := subscription.TryReceive(); !ok {
					b.Fatal("the subscriber queue is empty")
				}
			}
		}
	}

	b.Run("shared", func(b *testing.B) { run(b, false) })
	b.Run("clone_per_subscriber", func(b *testing.B) { run(b, true) })
}

func publishCloned(hub *eventHub, event Event) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.closed {
		return
	}

	hub.cursor++
	record := EventRecord{Cursor: hub.cursor, Event: cloneEvent(event)}
	hub.replay = append(hub.replay, record)
	if overflow := len(hub.replay) - hub.replayCapacity; overflow > 0 {
		copy(hub.replay, hub.replay[overflow:])
		hub.replay = hub.replay[:hub.replayCapacity]
	}

	for subscriber := range hub.subscribers {
		cloned := cloneEventRecordOld(record)
		if !subscriber.push(cloned, eventRecordBytes(cloned)) {
			delete(hub.subscribers, subscriber)
			subscriber.finish(ErrEventGap)
		}
	}
}

var eventHubBenchmarkSink EventRecord

func BenchmarkEventHubRetained(b *testing.B) {
	const (
		events    = 64
		payload   = 64 << 10
		consumers = 4
	)

	build := func(cloned bool) *eventHub {
		// The budgets hold the whole burst, so the measurement covers four
		// consumers that are all still subscribed.
		hub := newEventHubWithBudget(events, events, 8<<20, 8<<20)
		for range consumers {
			if _, _, err := hub.subscribe(0); err != nil {
				b.Fatal(err)
			}
		}

		for index := 0; index < events; index++ {
			event := largeMediaEvent(payload)
			event.Sequence = uint64(index + 1)
			if cloned {
				publishCloned(hub, event)
			} else {
				hub.publish(event)
			}
		}

		return hub
	}

	b.Run("shared", func(b *testing.B) {
		b.StopTimer()
		runtime.GC()

		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		hub := build(false)
		runtime.GC()
		runtime.ReadMemStats(&after)

		retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
		b.ReportMetric(float64(retained), "retained-B")
		b.Logf("retained=%dKB", retained/1024)
		b.ResetTimer()

		for index := 0; index < b.N; index++ {
			runtime.KeepAlive(hub)
		}
	})

	b.Run("clone_per_subscriber", func(b *testing.B) {
		b.StopTimer()
		runtime.GC()

		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		hub := build(true)
		runtime.GC()
		runtime.ReadMemStats(&after)

		retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
		b.ReportMetric(float64(retained), "retained-B")
		b.Logf("retained=%dKB", retained/1024)
		b.ResetTimer()

		for index := 0; index < b.N; index++ {
			runtime.KeepAlive(hub)
		}
	})
}
