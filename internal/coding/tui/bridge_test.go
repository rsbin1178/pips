//nolint:wsl_v5 // Backpressure tests keep synchronization next to assertions.
package tui

import (
	"context"
	"errors"
	"iter"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEventBridgeIsBoundedAndCancelable(t *testing.T) {
	t.Parallel()

	var produced atomic.Int32
	bridge := startBridge(t.Context(), func(context.Context) iter.Seq2[coding.Event, error] {
		return func(yield func(coding.Event, error) bool) {
			for range 1_000 {
				produced.Add(1)
				if !yield(coding.Event{}, nil) {
					return
				}
			}
		}
	})

	require.Eventually(t, func() bool {
		return produced.Load() >= bridgeCapacity
	}, time.Second, time.Millisecond)
	assert.LessOrEqual(t, produced.Load(), int32(bridgeCapacity+1))
	select {
	case <-bridge.stopped():
		t.Fatal("producer must remain backpressured")
	default:
	}

	stopCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, bridge.stop(stopCtx))
	select {
	case <-bridge.stopped():
	case <-time.After(time.Second):
		t.Fatal("producer did not stop")
	}
}

func TestEventBridgePreservesOrderAndError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("stream failed")
	bridge := startBridge(t.Context(), func(context.Context) iter.Seq2[coding.Event, error] {
		return func(yield func(coding.Event, error) bool) {
			for sequence := uint64(1); sequence <= 3; sequence++ {
				if !yield(coding.Event{Sequence: sequence}, nil) {
					return
				}
			}
			yield(coding.Event{}, wantErr)
		}
	})

	for sequence := uint64(1); sequence <= 3; sequence++ {
		message := waitBridgeItem(t, bridge)
		require.True(t, message.ok)
		assert.Equal(t, sequence, message.item.event.Sequence)
		require.NoError(t, message.item.err)
	}

	message := waitBridgeItem(t, bridge)
	require.True(t, message.ok)
	require.ErrorIs(t, message.item.err, wantErr)
	message = waitBridgeItem(t, bridge)
	assert.False(t, message.ok)

	stopCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, bridge.stop(stopCtx))
}

func waitBridgeItem(t *testing.T, bridge *eventBridge) streamItemMsg {
	t.Helper()

	message, ok := bridge.wait()().(streamItemMsg)
	require.True(t, ok)

	return message
}

func TestEventBridgeStopHonorsDeadline(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	bridge := startBridge(t.Context(), func(context.Context) iter.Seq2[coding.Event, error] {
		return func(func(coding.Event, error) bool) {
			<-release
		}
	})
	t.Cleanup(func() { close(release) })

	stopCtx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	err := bridge.stop(stopCtx)
	require.ErrorIs(t, err, errBridgeStopped)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestEventBridgeRepeatedCancellation(t *testing.T) {
	t.Parallel()

	for range 100 {
		bridge := startBridge(t.Context(), func(ctx context.Context) iter.Seq2[coding.Event, error] {
			return func(func(coding.Event, error) bool) { <-ctx.Done() }
		})
		stopCtx, cancel := context.WithTimeout(t.Context(), time.Second)
		require.NoError(t, bridge.stop(stopCtx))
		cancel()
	}
}
