package acp

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeliverCancelOnDone pins the guarantee a cancelled turn relies on: the
// delivery has happened by the time the disarm returns, whichever goroutine the
// scheduler ran first, and exactly once.
func TestDeliverCancelOnDone(t *testing.T) {
	t.Parallel()

	t.Run("cancelled before the disarm", func(t *testing.T) {
		t.Parallel()

		// The loop is the point: the callback may have run before the disarm or be
		// prevented by it, and both orderings have to deliver exactly once.
		for range 100 {
			ctx, cancel := context.WithCancel(t.Context())

			var deliveries atomic.Int64

			stop := deliverCancelOnDone(ctx, func() { deliveries.Add(1) })

			cancel()
			stop()

			require.EqualValues(t, 1, deliveries.Load())
		}
	})

	t.Run("disarmed before the cancel delivers nothing", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())

		var deliveries atomic.Int64

		stop := deliverCancelOnDone(ctx, func() { deliveries.Add(1) })

		stop()
		cancel()

		assert.Zero(t, deliveries.Load(), "a turn that ends on its own must not cancel the controller")
	})

	t.Run("an open context delivers nothing", func(t *testing.T) {
		t.Parallel()

		var deliveries atomic.Int64

		stop := deliverCancelOnDone(t.Context(), func() { deliveries.Add(1) })

		stop()

		assert.Zero(t, deliveries.Load())
	})
}
