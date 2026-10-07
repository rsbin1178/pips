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
	assert.Equal(t, 10, codingModelMaxRetries)

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
		base := newRetryModel(append(transient(codingModelMaxRetries), nil)...)
		model := withCodingRetry(base, noRetrySleep())

		response, err := ai.Collect(model.Stream(t.Context(), ai.Request{}))

		require.NoError(t, err)
		assert.Equal(t, "ok", response.Text())
		assert.Equal(t, int32(codingModelMaxRetries+1), base.calls.Load())
	})

	t.Run("budget exhaustion surfaces the last failure", func(t *testing.T) {
		t.Parallel()

		finalErr := errors.New("final transport failure")
		base := newRetryModel(append(transient(codingModelMaxRetries), finalErr)...)
		model := withCodingRetry(base, noRetrySleep())

		_, err := ai.Collect(model.Stream(t.Context(), ai.Request{}))

		require.ErrorIs(t, err, finalErr)
		assert.Equal(t, int32(codingModelMaxRetries+1), base.calls.Load())
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
