package model

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/middleware/retry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithCodingRetryUsesTenRetryBudget(t *testing.T) {
	t.Parallel()

	// The retry budget follows the default the other agent frontends ship; pin
	// the number so a change is deliberate.
	assert.Equal(t, 10, RetryBudget)

	transient := func(failures int) []error {
		errs := make([]error, 0, failures)
		for index := 1; index <= failures; index++ {
			errs = append(errs, fmt.Errorf("transient %d", index))
		}

		return errs
	}

	t.Run("tenth retry succeeds", func(t *testing.T) {
		t.Parallel()

		// One initial attempt plus ten replays: the eleventh call answers.
		base := newRetryModel(append(transient(RetryBudget), nil)...)
		model := withCodingRetry(base, RetryWindowFor(0), noRetrySleep())

		response, err := ai.Collect(model.Stream(t.Context(), ai.Request{}))

		require.NoError(t, err)
		assert.Equal(t, "ok", response.Text())
		assert.Equal(t, int32(RetryBudget+1), base.calls.Load())
	})

	t.Run("budget exhaustion surfaces the last failure", func(t *testing.T) {
		t.Parallel()

		finalErr := errors.New("final transport failure")
		base := newRetryModel(append(transient(RetryBudget), finalErr)...)
		model := withCodingRetry(base, RetryWindowFor(0), noRetrySleep())

		_, err := ai.Collect(model.Stream(t.Context(), ai.Request{}))

		require.ErrorIs(t, err, finalErr)
		assert.Equal(t, int32(RetryBudget+1), base.calls.Load())
	})
}

// The attempt budget bounds the work, but a provider that has gone silent can
// spend a full stream idle window on every attempt, so the episode is bounded by
// wall clock too — from the same policy value the loop's recovery uses.
func TestWithCodingRetryBoundsTheEpisodeOnTheWallClock(t *testing.T) {
	t.Parallel()

	// An unset idle bound selects the transport default, so the episode stays at
	// the twenty minutes Coding has always shipped. Pinned so a change is
	// deliberate; the derivation itself is covered by TestRetryWindowFor.
	assert.Equal(t, 20*time.Minute, RetryWindowFor(0))

	finalErr := errors.New("provider stopped answering")
	base := newRetryModel(finalErr)

	// A clock that jumps an hour per reading: the first reading starts the
	// episode and the second is already past the window, so the ceiling can be
	// observed without waiting it out. Without the wiring the whole
	// eleven-attempt budget would run.
	now := time.Unix(0, 0)
	clock := retry.ClockFunc(func() time.Time {
		current := now
		now = now.Add(time.Hour)

		return current
	})

	model := withCodingRetry(base, RetryWindowFor(0), noRetrySleep(), retry.WithClock(clock))

	_, err := ai.Collect(model.Stream(t.Context(), ai.Request{}))

	require.ErrorIs(t, err, finalErr)
	assert.Equal(t, int32(1), base.calls.Load(),
		"the coding chain stops as soon as the episode is spent")
}

// TestRetryWindowFor pins the formula the episode follows: max(floor, 2×idle),
// with an unset bound standing in the transport default. The window has to stay
// at least twice the idle bound, because one attempt that goes silent can spend
// a whole idle window and the episode still has to hold a second attempt.
func TestRetryWindowFor(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 10*time.Minute, RetryWindowFloor)

	tests := []struct {
		name string
		idle time.Duration
		want time.Duration
	}{
		{name: "unset selects the transport default", idle: 0, want: 20 * time.Minute},
		{name: "the transport default", idle: ai.DefaultStreamIdleTimeout, want: 20 * time.Minute},
		{name: "a raised bound doubles", idle: 30 * time.Minute, want: time.Hour},
		{name: "a lowered bound keeps the floor", idle: 30 * time.Second, want: 10 * time.Minute},
		{name: "just below the floor is not doubled", idle: 4 * time.Minute, want: 10 * time.Minute},
		{name: "just above the floor is doubled", idle: 6 * time.Minute, want: 12 * time.Minute},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, RetryWindowFor(test.idle))
		})
	}
}

// TestWithCodingRetryUsesTheDerivedWindow covers the wiring rather than the
// formula: a model whose idle bound is raised earns a proportionally longer
// replay episode, which is what keeps a legitimately slow provider from being
// replayed against an episode sized for the default bound.
func TestWithCodingRetryUsesTheDerivedWindow(t *testing.T) {
	t.Parallel()

	// Every reading advances fifteen minutes, so the episode's ceiling is what
	// decides how many attempts fit before it is spent.
	stepping := func() retry.Option {
		now := time.Unix(0, 0)

		return retry.WithClock(retry.ClockFunc(func() time.Time {
			current := now
			now = now.Add(15 * time.Minute)

			return current
		}))
	}

	tests := []struct {
		name  string
		idle  time.Duration
		calls int32
	}{
		{name: "transport default", idle: 0, calls: 2},
		{name: "raised idle bound", idle: 30 * time.Minute, calls: 4},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			base := newRetryModel(errors.New("provider stopped answering"))
			model := withCodingRetry(base, RetryWindowFor(test.idle), noRetrySleep(), stepping())

			_, err := ai.Collect(model.Stream(t.Context(), ai.Request{}))

			require.Error(t, err)
			assert.Equal(t, test.calls, base.calls.Load())
		})
	}
}

func TestWithCodingRetryPreservesRetrySafetyAndIdentity(t *testing.T) {
	t.Parallel()

	t.Run("non-retryable error", func(t *testing.T) {
		t.Parallel()

		base := newRetryModel(ai.NewError(ai.ProviderOpenAI, 400, "bad request"))
		model := withCodingRetry(base, RetryWindowFor(0), noRetrySleep())

		_, err := ai.Collect(model.Stream(t.Context(), ai.Request{}))

		require.ErrorIs(t, err, ai.ErrInvalidRequest)
		assert.Equal(t, int32(1), base.calls.Load())
	})

	t.Run("failure after first event", func(t *testing.T) {
		t.Parallel()

		base := newRetryModel(errors.New("stream interrupted"))
		base.emitBeforeError = true
		model := withCodingRetry(base, RetryWindowFor(0), noRetrySleep())

		var (
			texts  []string
			gotErr error
		)

		for event, err := range model.Stream(t.Context(), ai.Request{}) {
			if err != nil {
				gotErr = err
				break
			}

			texts = append(texts, event.Text)
		}

		require.Error(t, gotErr)
		assert.Equal(t, []string{"partial"}, texts)
		assert.Equal(t, int32(1), base.calls.Load())
	})

	t.Run("model identity", func(t *testing.T) {
		t.Parallel()

		base := newRetryModel(nil)
		model := withCodingRetry(base, RetryWindowFor(0), noRetrySleep())

		assert.Equal(t, base.Provider(), model.Provider())
		assert.Equal(t, base.ModelID(), model.ModelID())
		assert.Equal(t, base.Capabilities(), model.Capabilities())
	})
}

type retryModel struct {
	errors          []error
	calls           atomic.Int32
	emitBeforeError bool
}

func newRetryModel(errs ...error) *retryModel {
	return &retryModel{errors: errs}
}

func (m *retryModel) Generate(context.Context, ai.Request) (*ai.Response, error) {
	return nil, errors.New("generate is not used")
}

func (m *retryModel) Stream(context.Context, ai.Request) ai.Stream {
	index := int(m.calls.Add(1)) - 1
	err := m.errors[min(index, len(m.errors)-1)]

	return func(yield func(ai.StreamEvent, error) bool) {
		if m.emitBeforeError && !yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: "partial"}, nil) {
			return
		}

		if err != nil {
			yield(ai.StreamEvent{}, err)
			return
		}

		if !yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: "ok"}, nil) {
			return
		}

		yield(ai.StreamEvent{Type: ai.StreamMessageEnd, FinishReason: ai.FinishStop}, nil)
	}
}

func (*retryModel) Provider() ai.Provider { return ai.ProviderOpenAI }

func (*retryModel) ModelID() string { return "retry-test" }

func (*retryModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

func noRetrySleep() retry.Option {
	return retry.WithSleep(func(context.Context, time.Duration) error { return nil })
}

var _ ai.LanguageModel = (*retryModel)(nil)
