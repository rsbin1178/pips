package model

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/middleware/retry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithCodingRetryUsesFiveRetryBudget(t *testing.T) {
	t.Parallel()

	t.Run("sixth attempt succeeds", func(t *testing.T) {
		t.Parallel()

		base := newRetryModel(
			errors.New("transient 1"),
			errors.New("transient 2"),
			errors.New("transient 3"),
			errors.New("transient 4"),
			errors.New("transient 5"),
			nil,
		)
		model := withCodingRetry(base, noRetrySleep())

		response, err := ai.Collect(model.Stream(t.Context(), ai.Request{}))

		require.NoError(t, err)
		assert.Equal(t, "ok", response.Text())
		assert.Equal(t, int32(6), base.calls.Load())
	})

	t.Run("six failures exhaust budget", func(t *testing.T) {
		t.Parallel()

		finalErr := errors.New("final transport failure")
		base := newRetryModel(
			errors.New("transient 1"),
			errors.New("transient 2"),
			errors.New("transient 3"),
			errors.New("transient 4"),
			errors.New("transient 5"),
			finalErr,
		)
		model := withCodingRetry(base, noRetrySleep())

		_, err := ai.Collect(model.Stream(t.Context(), ai.Request{}))

		require.ErrorIs(t, err, finalErr)
		assert.Equal(t, int32(6), base.calls.Load())
	})
}

func TestWithCodingRetryPreservesRetrySafetyAndIdentity(t *testing.T) {
	t.Parallel()

	t.Run("non-retryable error", func(t *testing.T) {
		t.Parallel()

		base := newRetryModel(ai.NewError(ai.ProviderOpenAI, 400, "bad request"))
		model := withCodingRetry(base, noRetrySleep())

		_, err := ai.Collect(model.Stream(t.Context(), ai.Request{}))

		require.ErrorIs(t, err, ai.ErrInvalidRequest)
		assert.Equal(t, int32(1), base.calls.Load())
	})

	t.Run("failure after first event", func(t *testing.T) {
		t.Parallel()

		base := newRetryModel(errors.New("stream interrupted"))
		base.emitBeforeError = true
		model := withCodingRetry(base, noRetrySleep())

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
		model := withCodingRetry(base, noRetrySleep())

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
