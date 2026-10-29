package ai

import (
	"context"
	"sync"
	"time"
)

type retryAllowanceKey struct{}

// RetryAllowance bounds request replays shared by cooperating recovery layers.
// An allowance belongs to one model turn; visible-output reissues have their
// own budget but share this allowance's scheduling window.
type RetryAllowance struct {
	mu      sync.Mutex
	limit   int
	used    int
	started time.Time
	window  time.Duration
}

// WithRetryAllowance creates a request-local replay allowance. Its scheduling
// window starts with the first replay reservation; a non-positive window leaves
// only the retry count bound.
func WithRetryAllowance(ctx context.Context, retries int, window time.Duration) (context.Context, *RetryAllowance) {
	allowance := &RetryAllowance{limit: max(0, retries), window: window}
	return context.WithValue(ctx, retryAllowanceKey{}, allowance), allowance
}

// RetryAllowanceFromContext reports a cooperating owner's replay allowance.
func RetryAllowanceFromContext(ctx context.Context) (*RetryAllowance, bool) {
	allowance, ok := ctx.Value(retryAllowanceKey{}).(*RetryAllowance)
	return allowance, ok
}

// Used reports the number of replays already reserved.
func (a *RetryAllowance) Used() int {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.used
}

// Limit is the total number of replays available to all owners together.
func (a *RetryAllowance) Limit() int { return a.limit }

// Begin starts the common window when an owner schedules a visible-output
// reissue that does not consume the no-answer replay count.
func (a *RetryAllowance) Begin() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.started.IsZero() {
		a.started = time.Now()
	}
}

// Expired reports whether the common scheduling window has elapsed. It does
// not interrupt an attempt already in flight.
func (a *RetryAllowance) Expired() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.expired()
}

func (a *RetryAllowance) expired() bool {
	return !a.started.IsZero() && a.window > 0 && time.Since(a.started) >= a.window
}

// Claim reserves one replay and returns its one-based ordinal.
func (a *RetryAllowance) Claim() (int, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.used >= a.limit || a.expired() {
		return a.used, false
	}

	if a.started.IsZero() {
		a.started = time.Now()
	}

	a.used++

	return a.used, true
}
