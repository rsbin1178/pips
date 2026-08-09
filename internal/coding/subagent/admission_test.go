//nolint:wsl_v5 // Admission fixtures keep reservation actions beside their budget assertions.
package subagent

import (
	"errors"
	"sync"
	"testing"

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
		{name: "zero concurrent", maxSpawned: 1},
		{name: "concurrent above hard limit", maxConcurrent: maximumMaxConcurrent + 1, maxSpawned: 1},
		{name: "zero spawned", maxConcurrent: 1},
		{name: "spawned above hard limit", maxConcurrent: 1, maxSpawned: maximumMaxSpawned + 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := newAdmissionBudget(test.maxConcurrent, test.maxSpawned)
			require.ErrorIs(t, err, ErrInvalid)
		})
	}
}

func TestAdmissionPermitOwnsActiveAndCumulativeSpawnBudgets(t *testing.T) {
	t.Parallel()

	budget, err := newAdmissionBudget(2, 2)
	require.NoError(t, err)
	foreground, err := budget.reserve(DeliveryForeground, "", false)
	require.NoError(t, err)
	background, err := budget.reserve(DeliveryBackground, "root-1", false)
	require.NoError(t, err)

	_, err = budget.reserve(DeliveryForeground, "", false)
	require.ErrorIs(t, err, ErrCapacity)

	foreground.release()
	foreground.release()
	background.commit()
	background.release()
	active, spawned := budget.snapshot()
	assert.Zero(t, active)
	assert.Equal(t, map[string]int{"root-1": 1}, spawned)

	second, err := budget.reserve(DeliveryBackground, "root-1", false)
	require.NoError(t, err)
	second.commit()
	second.release()
	_, err = budget.reserve(DeliveryBackground, "root-1", false)
	require.ErrorIs(t, err, ErrSpawnLimit)
}

func TestAdmissionPermitRollsBackUncommittedBackgroundSpawn(t *testing.T) {
	t.Parallel()

	budget, err := newAdmissionBudget(1, 1)
	require.NoError(t, err)
	permit, err := budget.reserve(DeliveryBackground, "root-1", false)
	require.NoError(t, err)
	permit.release()

	active, spawned := budget.snapshot()
	assert.Zero(t, active)
	assert.Empty(t, spawned)
	permit, err = budget.reserve(DeliveryBackground, "root-1", false)
	require.NoError(t, err)
	permit.commit()
	permit.release()
	_, err = budget.reserve(DeliveryBackground, "root-1", false)
	require.ErrorIs(t, err, ErrSpawnLimit)
}

func TestAdmissionBudgetPreservesSingleSlotBusyError(t *testing.T) {
	t.Parallel()

	budget, err := newAdmissionBudget(1, 1)
	require.NoError(t, err)
	permit, err := budget.reserve(DeliveryForeground, "", false)
	require.NoError(t, err)
	defer permit.release()

	_, err = budget.reserve(DeliveryForeground, "", false)
	require.ErrorIs(t, err, ErrBusy)
	_, err = budget.reserve(DeliveryBackground, "", false)
	require.ErrorIs(t, err, ErrBusy)
}

func TestAdmissionBudgetRejectsInvalidRequests(t *testing.T) {
	t.Parallel()

	budget, err := newAdmissionBudget(2, 1)
	require.NoError(t, err)
	_, err = budget.reserve(DeliveryBackground, "", false)
	require.ErrorIs(t, err, ErrSpawnLimit)
	_, err = budget.reserve(Delivery("later"), "root-1", false)
	require.ErrorIs(t, err, ErrInvalid)

	var nilBudget *admissionBudget
	_, err = nilBudget.reserve(DeliveryForeground, "", false)
	require.ErrorIs(t, err, ErrInvalid)
}

func TestAdmissionBudgetConcurrentReservationIsBoundedAndReusable(t *testing.T) {
	t.Parallel()

	const (
		maximum = 4
		callers = 64
	)
	budget, err := newAdmissionBudget(maximum, callers)
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
			permit, reserveErr := budget.reserve(DeliveryForeground, "", false)
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
		case errors.Is(reserveErr, ErrCapacity):
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
	reused, err := budget.reserve(DeliveryForeground, "", false)
	require.NoError(t, err)
	reused.release()
}

func TestAdmissionBudgetCountsNestedForegroundDescendants(t *testing.T) {
	t.Parallel()

	budget, err := newAdmissionBudget(2, 1)
	require.NoError(t, err)
	permit, err := budget.reserve(DeliveryForeground, "root-1", true)
	require.NoError(t, err)
	permit.commit()
	permit.release()

	_, err = budget.reserve(DeliveryForeground, "root-1", true)
	require.ErrorIs(t, err, ErrSpawnLimit)
}
