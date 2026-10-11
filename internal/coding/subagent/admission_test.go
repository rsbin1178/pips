//nolint:wsl_v5 // Admission fixtures keep reservation actions beside their budget assertions.
package subagent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAdmissionBudgetValidatesBounds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		maxConcurrent int
		maxSpawned    int
	}{
		{name: "concurrent above hard limit", maxConcurrent: maximumMaxConcurrent + 1, maxSpawned: 1},
		{name: "zero spawned", maxConcurrent: 1},
		{name: "spawned above hard limit", maxConcurrent: 1, maxSpawned: maximumMaxSpawned + 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := newAdmissionBudget(test.maxConcurrent, test.maxSpawned, LimitBehaviorQueue)
			require.ErrorIs(t, err, ErrInvalid)
		})
	}
}

func TestAdmissionBudgetClampsConcurrentToOne(t *testing.T) {
	t.Parallel()

	budget, err := newAdmissionBudget(0, 1, LimitBehaviorQueue)
	require.NoError(t, err)
	assert.Equal(t, 1, budget.maxConcurrent)
}

func TestAdmissionBudgetPreservesSingleSlotBusyError(t *testing.T) {
	t.Parallel()

	budget, err := newAdmissionBudget(1, 1, LimitBehaviorFail)
	require.NoError(t, err)
	permit, err := budget.reserve(context.Background(), DeliveryForeground, "", false, 0, nil)
	require.NoError(t, err)
	defer permit.release()

	_, err = budget.reserve(context.Background(), DeliveryForeground, "", false, 0, nil)
	require.ErrorIs(t, err, ErrConcurrentLimit)
	assert.Equal(t,
		"Concurrent subagent limit reached: 1 subagents are already running for this session. "+
			"Do not retry; spawning succeeds again when a running subagent finishes.",
		err.Error(),
	)
}

func TestAdmissionPermitOwnsActiveAndCumulativeSpawnBudgets(t *testing.T) {
	t.Parallel()

	budget, err := newAdmissionBudget(2, 2, LimitBehaviorFail)
	require.NoError(t, err)
	foreground, err := budget.reserve(context.Background(), DeliveryForeground, "", false, 0, nil)
	require.NoError(t, err)
	background, err := budget.reserve(context.Background(), DeliveryBackground, "root-1", false, 0, nil)
	require.NoError(t, err)

	_, err = budget.reserve(context.Background(), DeliveryForeground, "", false, 0, nil)
	require.ErrorIs(t, err, ErrConcurrentLimit)

	foreground.release()
	foreground.release()
	background.commit()
	background.release()
	active, spawned := budget.snapshot()
	assert.Zero(t, active)
	assert.Equal(t, map[string]int{"root-1": 1}, spawned)

	second, err := budget.reserve(context.Background(), DeliveryBackground, "root-1", false, 0, nil)
	require.NoError(t, err)
	second.commit()
	second.release()
	_, err = budget.reserve(context.Background(), DeliveryBackground, "root-1", false, 0, nil)
	require.ErrorIs(t, err, ErrSpawnLimit)
}

func TestAdmissionPermitRollsBackUncommittedBackgroundSpawn(t *testing.T) {
	t.Parallel()

	budget, err := newAdmissionBudget(1, 1, LimitBehaviorQueue)
	require.NoError(t, err)
	permit, err := budget.reserve(context.Background(), DeliveryBackground, "root-1", false, 0, nil)
	require.NoError(t, err)
	permit.release()

	active, spawned := budget.snapshot()
	assert.Zero(t, active)
	assert.Empty(t, spawned)
	permit, err = budget.reserve(context.Background(), DeliveryBackground, "root-1", false, 0, nil)
	require.NoError(t, err)
	permit.commit()
	permit.release()
	_, err = budget.reserve(context.Background(), DeliveryBackground, "root-1", false, 0, nil)
	require.ErrorIs(t, err, ErrSpawnLimit)
}

func TestAdmissionBudgetQueueStartsOnReleaseInArrivalOrder(t *testing.T) {
	t.Parallel()

	budget, err := newAdmissionBudget(1, 8, LimitBehaviorQueue)
	require.NoError(t, err)
	first, err := budget.reserve(context.Background(), DeliveryForeground, "", false, 0, nil)
	require.NoError(t, err)

	type result struct {
		permit *admissionPermit
		err    error
	}
	queued := make(chan result, 2)
	var queuedReports int
	var mu sync.Mutex
	onQueued := func() {
		mu.Lock()
		queuedReports++
		mu.Unlock()
	}
	for range 2 {
		go func() {
			permit, reserveErr := budget.reserve(
				context.Background(), DeliveryForeground, "", false, 0, onQueued,
			)
			queued <- result{permit: permit, err: reserveErr}
		}()
	}
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return queuedReports == 2
	}, 30*time.Second, 10*time.Millisecond)

	first.release()
	firstQueued := <-queued
	require.NoError(t, firstQueued.err)
	firstQueued.permit.release()
	secondQueued := <-queued
	require.NoError(t, secondQueued.err)
	secondQueued.permit.release()
}

func TestAdmissionBudgetQueueTimesOutForForegroundCaller(t *testing.T) {
	t.Parallel()

	budget, err := newAdmissionBudget(1, 8, LimitBehaviorQueue)
	require.NoError(t, err)
	permit, err := budget.reserve(context.Background(), DeliveryForeground, "", false, 0, nil)
	require.NoError(t, err)
	defer permit.release()

	_, err = budget.reserve(context.Background(), DeliveryForeground, "", false, 20*time.Millisecond, nil)
	require.ErrorIs(t, err, ErrQueueTimeout)

	// The abandoned waiter never consumes the freed slot.
	permit.release()
	active, _ := budget.snapshot()
	assert.Zero(t, active)
}

func TestAdmissionBudgetQueueHonorsCancellation(t *testing.T) {
	t.Parallel()

	budget, err := newAdmissionBudget(1, 8, LimitBehaviorQueue)
	require.NoError(t, err)
	permit, err := budget.reserve(context.Background(), DeliveryForeground, "", false, 0, nil)
	require.NoError(t, err)
	defer permit.release()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	_, err = budget.reserve(ctx, DeliveryForeground, "", false, 0, nil)
	require.ErrorIs(t, err, context.Canceled)
}

func TestAdmissionBudgetRejectsInvalidRequests(t *testing.T) {
	t.Parallel()

	budget, err := newAdmissionBudget(2, 1, LimitBehaviorQueue)
	require.NoError(t, err)
	_, err = budget.reserve(context.Background(), DeliveryBackground, "", false, 0, nil)
	require.ErrorIs(t, err, ErrSpawnLimit)
	_, err = budget.reserve(context.Background(), Delivery("later"), "root-1", false, 0, nil)
	require.ErrorIs(t, err, ErrInvalid)

	var nilBudget *admissionBudget
	_, err = nilBudget.reserve(context.Background(), DeliveryForeground, "", false, 0, nil)
	require.ErrorIs(t, err, ErrInvalid)
}

func TestAdmissionBudgetConcurrentReservationIsBoundedAndReusable(t *testing.T) {
	t.Parallel()

	const (
		maximum = 4
		callers = 64
	)
	budget, err := newAdmissionBudget(maximum, callers, LimitBehaviorFail)
	require.NoError(t, err)
	start := make(chan struct{})
	release := make(chan struct{})
	results := make(chan error, callers)
	var group sync.WaitGroup
	group.Add(callers)
	for range callers {
		go func() {
			defer group.Done()
			<-start
			permit, reserveErr := budget.reserve(
				context.Background(), DeliveryForeground, "", false, 0, nil,
			)
			results <- reserveErr
			if reserveErr != nil {
				return
			}
			<-release
			permit.commit()
			permit.release()
		}()
	}
	close(start)

	succeeded := 0
	exhausted := 0
	for range callers {
		reserveErr := <-results
		switch {
		case reserveErr == nil:
			succeeded++
		case errors.Is(reserveErr, ErrConcurrentLimit):
			exhausted++
		default:
			require.NoError(t, reserveErr)
		}
	}
	assert.Equal(t, maximum, succeeded)
	assert.Equal(t, callers-maximum, exhausted)
	active, _ := budget.snapshot()
	assert.Equal(t, maximum, active)

	close(release)
	group.Wait()
	active, _ = budget.snapshot()
	assert.Zero(t, active)
	reused, err := budget.reserve(context.Background(), DeliveryForeground, "", false, 0, nil)
	require.NoError(t, err)
	reused.release()
}

func TestAdmissionBudgetCountsNestedForegroundDescendants(t *testing.T) {
	t.Parallel()

	budget, err := newAdmissionBudget(2, 1, LimitBehaviorQueue)
	require.NoError(t, err)
	permit, err := budget.reserve(context.Background(), DeliveryForeground, "root-1", true, 0, nil)
	require.NoError(t, err)
	permit.commit()
	permit.release()

	_, err = budget.reserve(context.Background(), DeliveryForeground, "root-1", true, 0, nil)
	require.ErrorIs(t, err, ErrSpawnLimit)
}
