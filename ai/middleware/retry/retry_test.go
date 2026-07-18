package retry_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/middleware/retry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedModel returns a canned result per attempt, counting calls.
type scriptedModel struct {
	responses []*ai.Response
	errs      []error
	calls     atomic.Int32
}

func (m *scriptedModel) Generate(_ context.Context, _ ai.Request) (*ai.Response, error) {
	i := int(m.calls.Add(1)) - 1
	if i >= len(m.errs) {
		i = len(m.errs) - 1
	}

	return m.responses[i], m.errs[i]
}

func (m *scriptedModel) Stream(_ context.Context, _ ai.Request) ai.Stream {
	i := int(m.calls.Add(1)) - 1
	if i >= len(m.errs) {
		i = len(m.errs) - 1
	}

	resp, err := m.responses[i], m.errs[i]

	return func(yield func(ai.StreamEvent, error) bool) {
		if err != nil {
			yield(ai.StreamEvent{}, err)
			return
		}

		yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: resp.Text()}, nil)
		yield(ai.StreamEvent{Type: ai.StreamMessageEnd, FinishReason: ai.FinishStop}, nil)
	}
}

func (m *scriptedModel) Provider() ai.Provider         { return ai.ProviderOpenAI }
func (m *scriptedModel) ModelID() string               { return "test" }
func (m *scriptedModel) Capabilities() ai.Capabilities { return ai.Capabilities{Text: true} }

// noSleep collapses backoff so tests run instantly while recording delays.
func noSleep(delays *[]time.Duration) retry.Option {
	return retry.WithSleep(func(_ context.Context, d time.Duration) error {
		*delays = append(*delays, d)
		return nil
	})
}

func TestRetrySucceedsAfterTransientErrors(t *testing.T) {
	t.Parallel()

	ok := &ai.Response{Message: ai.AssistantText("ok")}
	base := &scriptedModel{
		responses: []*ai.Response{nil, nil, ok},
		errs: []error{
			ai.NewError(ai.ProviderOpenAI, 503, "overloaded"),
			ai.NewError(ai.ProviderOpenAI, 429, "slow down"),
			nil,
		},
	}

	var delays []time.Duration

	model := retry.New(retry.WithMaxAttempts(3), retry.WithJitter(func() float64 { return 1 }), noSleep(&delays))(base)

	resp, err := model.Generate(t.Context(), ai.Request{})
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Text())
	assert.Equal(t, int32(3), base.calls.Load())
	require.Len(t, delays, 2)
	// Full jitter at fraction 1 gives base, then 2×base.
	assert.Equal(t, 500*time.Millisecond, delays[0])
	assert.Equal(t, time.Second, delays[1])
}

func TestRetryHonorsRetryAfter(t *testing.T) {
	t.Parallel()

	rlErr := ai.NewError(ai.ProviderOpenAI, 429, "slow down")
	rlErr.RetryAfter = 7 * time.Second
	base := &scriptedModel{
		responses: []*ai.Response{nil, {Message: ai.AssistantText("ok")}},
		errs:      []error{rlErr, nil},
	}

	var delays []time.Duration
	// Jitter 0 would give a 0 backoff; Retry-After must still dominate.
	model := retry.New(retry.WithJitter(func() float64 { return 0 }), noSleep(&delays))(base)

	_, err := model.Generate(t.Context(), ai.Request{})
	require.NoError(t, err)
	require.Len(t, delays, 1)
	assert.Equal(t, 7*time.Second, delays[0])
}

func TestRetryStopsOnNonRetryable(t *testing.T) {
	t.Parallel()

	base := &scriptedModel{
		responses: []*ai.Response{nil},
		errs:      []error{ai.NewError(ai.ProviderOpenAI, 400, "bad request")},
	}

	var delays []time.Duration

	model := retry.New(retry.WithMaxAttempts(5), noSleep(&delays))(base)

	_, err := model.Generate(t.Context(), ai.Request{})
	require.ErrorIs(t, err, ai.ErrInvalidRequest)
	assert.Equal(t, int32(1), base.calls.Load(), "400 must not be retried")
	assert.Empty(t, delays)
}

func TestRetryExhaustsAttempts(t *testing.T) {
	t.Parallel()

	base := &scriptedModel{
		responses: []*ai.Response{nil},
		errs:      []error{ai.NewError(ai.ProviderOpenAI, 503, "overloaded")},
	}

	var delays []time.Duration

	model := retry.New(retry.WithMaxAttempts(3), noSleep(&delays))(base)

	_, err := model.Generate(t.Context(), ai.Request{})
	require.ErrorIs(t, err, ai.ErrOverloaded)
	assert.Equal(t, int32(3), base.calls.Load())
	assert.Len(t, delays, 2, "N attempts sleep N-1 times")
}

func TestStreamRetriesBeforeFirstEvent(t *testing.T) {
	t.Parallel()

	base := &scriptedModel{
		responses: []*ai.Response{nil, {Message: ai.AssistantText("hello")}},
		errs:      []error{ai.NewError(ai.ProviderOpenAI, 503, "overloaded"), nil},
	}

	var delays []time.Duration

	model := retry.New(noSleep(&delays))(base)

	resp, err := ai.Collect(model.Stream(t.Context(), ai.Request{}))
	require.NoError(t, err)
	assert.Equal(t, "hello", resp.Text())
	assert.Equal(t, int32(2), base.calls.Load())
}

// midStreamModel yields one event then fails, to prove no replay after output.
type midStreamModel struct {
	calls atomic.Int32
}

func (m *midStreamModel) Generate(context.Context, ai.Request) (*ai.Response, error) {
	return nil, errors.New("unused")
}

func (m *midStreamModel) Stream(context.Context, ai.Request) ai.Stream {
	m.calls.Add(1)

	return func(yield func(ai.StreamEvent, error) bool) {
		if !yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: "partial"}, nil) {
			return
		}

		yield(ai.StreamEvent{}, ai.NewError(ai.ProviderOpenAI, 503, "dropped mid-stream"))
	}
}

func (m *midStreamModel) Provider() ai.Provider         { return ai.ProviderOpenAI }
func (m *midStreamModel) ModelID() string               { return "test" }
func (m *midStreamModel) Capabilities() ai.Capabilities { return ai.Capabilities{} }

func TestStreamDoesNotRetryAfterFirstEvent(t *testing.T) {
	t.Parallel()

	base := &midStreamModel{}

	var delays []time.Duration

	model := retry.New(retry.WithMaxAttempts(3), noSleep(&delays))(base)

	var (
		texts  []string
		gotErr error
	)

	for ev, err := range model.Stream(t.Context(), ai.Request{}) {
		if err != nil {
			gotErr = err
			break
		}

		texts = append(texts, ev.Text)
	}

	require.Error(t, gotErr)
	assert.Equal(t, []string{"partial"}, texts)
	assert.Equal(t, int32(1), base.calls.Load(), "must not replay after emitting output")
}

func TestRetryPassesThroughIdentity(t *testing.T) {
	t.Parallel()

	base := &scriptedModel{responses: []*ai.Response{{}}, errs: []error{nil}}
	model := retry.New()(base)
	assert.Equal(t, ai.ProviderOpenAI, model.Provider())
	assert.Equal(t, "test", model.ModelID())
	assert.True(t, model.Capabilities().Text)
}
