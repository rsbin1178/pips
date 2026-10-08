// Package retry provides a retrying [ai.LanguageModel] middleware with
// exponential backoff and full jitter.
//
// Only retryable failures are retried (see [ai.IsRetryable]); a provider's
// Retry-After delay is honored when present. Streaming is retried only before
// the first event is produced — once output has been observed, replaying the
// request could duplicate content, so the original error is surfaced instead.
// Each streaming re-attempt is announced with one [ai.StreamRetry] event.
//
// [WithMaxElapsed] additionally bounds how long one request's replay episode may
// last on the wall clock. The attempt budget bounds the work; for a provider that
// has stopped answering, only time bounds the waiting, because each attempt can
// spend a full idle window before it fails.
package retry

import (
	"context"
	"errors"
	"math"
	rand "math/rand/v2"
	"time"

	"github.com/rsbin1178/pips/ai"
)

// Default backoff parameters.
const (
	defaultMaxAttempts = 3
	defaultBaseDelay   = 500 * time.Millisecond
	defaultMaxDelay    = 30 * time.Second
)

// Clock reports the current time.
type Clock interface {
	Now() time.Time
}

// ClockFunc adapts a function to Clock.
type ClockFunc func() time.Time

// Now implements Clock.
func (function ClockFunc) Now() time.Time { return function() }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

type config struct {
	maxAttempts int
	baseDelay   time.Duration
	maxDelay    time.Duration
	// maxElapsed bounds one request's replay episode; zero leaves the attempt
	// budget as the only bound.
	maxElapsed time.Duration
	// clock is the time source the episode is measured against.
	clock Clock
	// sleep is the delay function, overridable in tests.
	sleep func(context.Context, time.Duration) error
	// jitter returns a fraction in [0,1) used for full jitter; overridable in
	// tests for determinism.
	jitter func() float64
}

// Option configures the retry middleware.
type Option func(*config)

// WithMaxAttempts sets the total number of attempts, including the first
// (default 3). Values below 1 are treated as 1.
func WithMaxAttempts(n int) Option {
	return func(c *config) { c.maxAttempts = n }
}

// WithBaseDelay sets the first backoff delay (default 500ms). Subsequent
// delays double up to the maximum.
func WithBaseDelay(d time.Duration) Option {
	return func(c *config) { c.baseDelay = d }
}

// WithMaxDelay caps the backoff delay (default 30s).
func WithMaxDelay(d time.Duration) Option {
	return func(c *config) { c.maxDelay = d }
}

// WithMaxElapsed bounds the wall clock one request's replay episode may spend,
// including the time its attempts take. Once the episode has lasted d, no further
// attempt is scheduled and the last failure is surfaced; nothing in flight is
// interrupted, so the worst case is d plus that one attempt. Zero — the default —
// leaves the attempt budget as the only bound.
func WithMaxElapsed(d time.Duration) Option {
	return func(c *config) { c.maxElapsed = d }
}

// WithClock replaces the time source the replay episode is measured against. It
// exists mainly for tests that need an exact ceiling; a nil clock is ignored and
// the system clock stays in place.
func WithClock(clock Clock) Option {
	return func(c *config) {
		if clock != nil {
			c.clock = clock
		}
	}
}

// WithSleep overrides the delay function. It exists mainly for tests, letting
// them collapse backoff and record the delays; production code should not
// need it.
func WithSleep(fn func(context.Context, time.Duration) error) Option {
	return func(c *config) { c.sleep = fn }
}

// WithJitter overrides the jitter source, which returns a fraction in [0,1).
// It exists mainly for deterministic tests.
func WithJitter(fn func() float64) Option {
	return func(c *config) { c.jitter = fn }
}

// New returns a [ai.Middleware] that retries failed calls.
func New(opts ...Option) ai.Middleware {
	cfg := config{
		maxAttempts: defaultMaxAttempts,
		baseDelay:   defaultBaseDelay,
		maxDelay:    defaultMaxDelay,
		sleep:       sleepCtx,
		jitter:      rand.Float64,
		clock:       systemClock{},
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	if cfg.maxAttempts < 1 {
		cfg.maxAttempts = 1
	}

	return func(next ai.LanguageModel) ai.LanguageModel {
		return &model{LanguageModel: next, cfg: cfg}
	}
}

// model wraps a LanguageModel with retry behavior. It embeds the wrapped model
// so Provider/ModelID/Capabilities pass through unchanged.
type model struct {
	ai.LanguageModel
	cfg config
}

// Generate retries the wrapped call on retryable errors.
func (m *model) Generate(ctx context.Context, req ai.Request) (*ai.Response, error) {
	started := m.cfg.clock.Now()

	var lastErr error

	for attempt := range m.cfg.maxAttempts {
		resp, err := m.LanguageModel.Generate(ctx, req)
		if err == nil {
			return resp, nil
		}

		lastErr = err
		if !ai.IsRetryable(err) || attempt == m.cfg.maxAttempts-1 {
			return resp, err
		}

		if m.spent(started) {
			return resp, err
		}

		if err := m.cfg.sleep(ctx, m.backoff(attempt, err)); err != nil {
			return nil, err
		}
	}

	return nil, lastErr
}

// Stream retries only before any event is produced. Once the first event
// arrives the stream is passed through verbatim.
//
// Every re-attempt is announced with one [ai.StreamRetry] event carrying the
// attempt that is about to start and the backoff before it, so a consumer can
// report the wait instead of looking stalled.
func (m *model) Stream(ctx context.Context, req ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		started := m.cfg.clock.Now()

		for attempt := range m.cfg.maxAttempts {
			retriable, err := m.streamAttempt(ctx, req, yield)
			if err == nil {
				return // stream finished (successfully or consumer stopped)
			}

			// A pre-first-event error may be retryable; anything after the
			// first event is terminal and was already yielded.
			if !retriable || !ai.IsRetryable(err) || attempt == m.cfg.maxAttempts-1 {
				yield(ai.StreamEvent{}, err)
				return
			}

			// The episode has spent its window, so an attempt that would exceed
			// it is never announced or started: the failure stands as it is.
			if m.spent(started) {
				yield(ai.StreamEvent{}, err)
				return
			}

			delay := m.backoff(attempt, err)
			// attempt is zero-based, so this is retry attempt+1 of the
			// maxAttempts-1 replays the configuration allows.
			if !yield(ai.StreamEvent{
				Type:  ai.StreamRetry,
				Retry: ai.NewRetryNotice(attempt+1, m.cfg.maxAttempts-1, delay, err),
			}, nil) {
				return
			}

			if sleepErr := m.cfg.sleep(ctx, delay); sleepErr != nil {
				yield(ai.StreamEvent{}, sleepErr)
				return
			}
		}
	}
}

// streamAttempt runs one streaming attempt. It returns retriable=true with a
// non-nil error only when the failure happened before any event was yielded;
// in that case the caller may retry without having emitted anything.
func (m *model) streamAttempt(ctx context.Context, req ai.Request, yield func(ai.StreamEvent, error) bool) (retriable bool, err error) {
	produced := false

	for ev, streamErr := range m.LanguageModel.Stream(ctx, req) {
		if streamErr != nil {
			if !produced {
				return true, streamErr
			}
			// Already streaming: surface the error to the consumer as-is.
			yield(ai.StreamEvent{}, streamErr)

			return false, nil
		}

		produced = true

		if !yield(ev, nil) {
			return false, nil // consumer stopped
		}
	}

	return false, nil
}

// spent reports whether the replay episode that began at started has used up its
// ceiling. An unset ceiling never expires.
func (m *model) spent(started time.Time) bool {
	return m.cfg.maxElapsed > 0 && m.cfg.clock.Now().Sub(started) >= m.cfg.maxElapsed
}

// backoff computes the delay before the next attempt: full-jittered
// exponential backoff, or the provider's Retry-After when it is longer.
func (m *model) backoff(attempt int, err error) time.Duration {
	exp := float64(m.cfg.baseDelay) * math.Pow(2, float64(attempt))
	if exp > float64(m.cfg.maxDelay) {
		exp = float64(m.cfg.maxDelay)
	}

	delay := time.Duration(m.cfg.jitter() * exp)

	if after := retryAfter(err); after > delay {
		delay = after
	}

	return delay
}

// retryAfter extracts a provider Retry-After hint from the error chain.
func retryAfter(err error) time.Duration {
	var apiErr *ai.Error
	if errors.As(err, &apiErr) {
		return apiErr.RetryAfter
	}

	return 0
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
