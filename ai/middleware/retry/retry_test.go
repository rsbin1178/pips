package retry_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/middleware/retry"
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

func TestStreamAnnouncesRetryBeforeReattempt(t *testing.T) {
	t.Parallel()

	base := &scriptedModel{
		responses: []*ai.Response{nil, {Message: ai.AssistantText("hello")}},
		errs:      []error{ai.NewError(ai.ProviderOpenAI, 503, "overloaded"), nil},
	}

	var delays []time.Duration

	model := retry.New(
		retry.WithMaxAttempts(3),
		retry.WithJitter(func() float64 { return 1 }),
		noSleep(&delays),
	)(base)

	var notices []ai.RetryNotice

	for ev, err := range model.Stream(t.Context(), ai.Request{}) {
		require.NoError(t, err)

		if ev.Type == ai.StreamRetry {
			require.NotNil(t, ev.Retry)

			notices = append(notices, *ev.Retry)
		}
	}

	require.Len(t, notices, 1)
	assert.Equal(t, 1, notices[0].Attempt, "the notice counts retries, not tries")
	assert.Equal(t, 2, notices[0].MaxRetries)
	assert.Equal(t, "provider overloaded", notices[0].Reason)
	require.Len(t, delays, 1)
	assert.Equal(t, delays[0], notices[0].Delay, "the advertised wait is the wait performed")
}

func TestStreamRetryReasonNamesEarlyStreamEnd(t *testing.T) {
	t.Parallel()

	base := &scriptedModel{
		responses: []*ai.Response{nil, {Message: ai.AssistantText("hello")}},
		errs:      []error{io.ErrUnexpectedEOF, nil},
	}

	var notices []ai.RetryNotice

	model := retry.New(retry.WithMaxAttempts(2), noSleep(new([]time.Duration)))(base)

	for ev, err := range model.Stream(t.Context(), ai.Request{}) {
		require.NoError(t, err)

		if ev.Retry != nil {
			notices = append(notices, *ev.Retry)
		}
	}

	require.Len(t, notices, 1)
	assert.Equal(t, "stream ended early", notices[0].Reason)
}

func TestStreamRetryReasonNamesStreamIdle(t *testing.T) {
	t.Parallel()

	// A stream that produced nothing and then went silent is retryable, so the
	// middleware replays it and the notice has to name the silence rather than
	// falling back to the generic connection wording.
	base := &scriptedModel{
		responses: []*ai.Response{nil, {Message: ai.AssistantText("hello")}},
		errs:      []error{ai.ErrStreamIdle, nil},
	}

	var notices []ai.RetryNotice

	model := retry.New(retry.WithMaxAttempts(2), noSleep(new([]time.Duration)))(base)

	for ev, err := range model.Stream(t.Context(), ai.Request{}) {
		require.NoError(t, err)

		if ev.Retry != nil {
			notices = append(notices, *ev.Retry)
		}
	}

	require.Len(t, notices, 1)
	assert.Equal(t, "stream idle", notices[0].Reason)
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

// steppingClock is a clock the test advances, so a wall-clock ceiling can be
// asserted exactly instead of timed.
type steppingClock struct {
	mu  sync.Mutex
	now time.Time
}

func newSteppingClock() *steppingClock {
	return &steppingClock{now: time.Unix(0, 0)}
}

func (c *steppingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

// total reports how far the clock has moved from its start.
func (c *steppingClock) total() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now.Sub(time.Unix(0, 0))
}

func (c *steppingClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
}

// drainingSleep records each delay and advances the clock, so the waits count
// against the ceiling the way they do in production.
func drainingSleep(clock *steppingClock, delays *[]time.Duration) retry.Option {
	return retry.WithSleep(func(_ context.Context, d time.Duration) error {
		*delays = append(*delays, d)
		clock.advance(d)

		return nil
	})
}

func TestStreamMaxElapsedStopsReplayingBeforeTheBudget(t *testing.T) {
	t.Parallel()

	clock := newSteppingClock()
	base := &scriptedModel{
		responses: []*ai.Response{nil},
		errs:      []error{io.ErrUnexpectedEOF},
	}

	var (
		notices []ai.RetryNotice
		delays  []time.Duration
		lastErr error
	)

	model := retry.New(
		retry.WithMaxAttempts(11),
		retry.WithBaseDelay(500*time.Millisecond),
		retry.WithMaxElapsed(5*time.Second),
		retry.WithJitter(func() float64 { return 1 }),
		retry.WithClock(clock),
		drainingSleep(clock, &delays),
	)(base)

	for ev, err := range model.Stream(t.Context(), ai.Request{}) {
		if err != nil {
			lastErr = err

			break
		}

		if ev.Retry != nil {
			notices = append(notices, *ev.Retry)
		}
	}

	// 500ms + 1s + 2s + 4s = 7.5s of waiting, so the ceiling stops the episode
	// well before the eleven-attempt budget is spent.
	assert.Equal(t, int32(5), base.calls.Load(), "the ceiling stops the episode early")
	require.ErrorIs(t, lastErr, io.ErrUnexpectedEOF, "the last failure is surfaced")

	require.Len(t, notices, 4)
	assert.Equal(t, 4, notices[len(notices)-1].Attempt)
	assert.Equal(t, 10, notices[len(notices)-1].MaxRetries)
	assert.Equal(t, 7*time.Second+500*time.Millisecond, clock.total(), "the waits are what consumed the window")
}

func TestStreamMaxElapsedLeavesARoomyEpisodeAlone(t *testing.T) {
	t.Parallel()

	clock := newSteppingClock()
	base := &scriptedModel{
		responses: []*ai.Response{nil, nil, nil, {Message: ai.AssistantText("ok")}},
		errs:      []error{io.ErrUnexpectedEOF, io.ErrUnexpectedEOF, io.ErrUnexpectedEOF, nil},
	}

	var notices []ai.RetryNotice

	model := retry.New(
		retry.WithMaxAttempts(11),
		retry.WithMaxElapsed(time.Hour),
		retry.WithJitter(func() float64 { return 1 }),
		retry.WithClock(clock),
		drainingSleep(clock, new([]time.Duration)),
	)(base)

	for ev, err := range model.Stream(t.Context(), ai.Request{}) {
		require.NoError(t, err)

		if ev.Retry != nil {
			notices = append(notices, *ev.Retry)
		}
	}

	assert.Len(t, notices, 3)
	assert.Equal(t, int32(4), base.calls.Load(), "a roomy ceiling does not interfere")
}

func TestStreamWithoutMaxElapsedKeepsTheWholeBudget(t *testing.T) {
	t.Parallel()

	clock := newSteppingClock()
	base := &scriptedModel{
		responses: []*ai.Response{nil},
		errs:      []error{io.ErrUnexpectedEOF},
	}

	model := retry.New(
		retry.WithMaxAttempts(4),
		retry.WithJitter(func() float64 { return 1 }),
		retry.WithClock(clock),
		drainingSleep(clock, new([]time.Duration)),
	)(base)

	for _, err := range model.Stream(t.Context(), ai.Request{}) {
		if err == nil {
			continue // a re-attempt announcement is progress, not a result
		}

		require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	}

	assert.Equal(t, int32(4), base.calls.Load(), "without a ceiling the budget is the only bound")
	assert.Equal(t, 3500*time.Millisecond, clock.total(),
		"the waits alone already exceeded a one-minute ceiling the episode did not have")
}

func TestGenerateMaxElapsedStopsReplaying(t *testing.T) {
	t.Parallel()

	clock := newSteppingClock()
	base := &scriptedModel{
		responses: []*ai.Response{nil},
		errs:      []error{io.ErrUnexpectedEOF},
	}

	var delays []time.Duration

	model := retry.New(
		retry.WithMaxAttempts(5),
		retry.WithBaseDelay(500*time.Millisecond),
		retry.WithMaxElapsed(250*time.Millisecond),
		retry.WithJitter(func() float64 { return 1 }),
		retry.WithClock(clock),
		drainingSleep(clock, &delays),
	)(base)

	_, err := model.Generate(t.Context(), ai.Request{})

	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.Equal(t, int32(2), base.calls.Load(),
		"one replay fits before the window is spent, and the next is not started")
	assert.Len(t, delays, 1)
}
