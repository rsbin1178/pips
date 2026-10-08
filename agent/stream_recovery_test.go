package agent_test

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// interruptedStreamModel streams partial output and then fails for its first
// failures attempts, so a re-issue can be observed to succeed on a later one.
type interruptedStreamModel struct {
	failures int32
	silent   bool // fail before any delta, leaving produced false
	err      error
	calls    atomic.Int32
}

func (m *interruptedStreamModel) Generate(context.Context, ai.Request) (*ai.Response, error) {
	return nil, errors.New("interruptedStreamModel: Generate is not scripted")
}

func (m *interruptedStreamModel) Stream(_ context.Context, _ ai.Request) ai.Stream {
	attempt := m.calls.Add(1)

	return func(yield func(ai.StreamEvent, error) bool) {
		if !yield(ai.StreamEvent{Type: ai.StreamMessageStart, ID: "resp"}, nil) {
			return
		}

		if attempt <= m.failures {
			if !m.silent && !yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: "partial"}, nil) {
				return
			}

			yield(ai.StreamEvent{}, m.err)

			return
		}

		yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: "final"}, nil)
		yield(ai.StreamEvent{Type: ai.StreamMessageEnd, FinishReason: ai.FinishStop}, nil)
	}
}

func (m *interruptedStreamModel) Provider() ai.Provider         { return ai.Provider("interrupted") }
func (m *interruptedStreamModel) ModelID() string               { return "interrupted-1" }
func (m *interruptedStreamModel) Capabilities() ai.Capabilities { return ai.Capabilities{Text: true} }

// collectStream drains one agent stream, returning the non-delta event types and
// the last retry notice seen.
func collectStream(t *testing.T, a *agent.Agent, sess *agent.Session) ([]agent.EventType, *ai.RetryNotice, error) {
	t.Helper()

	var (
		types  []agent.EventType
		notice *ai.RetryNotice
	)

	for ev, err := range a.Stream(t.Context(), sess, ai.UserText("hi")) {
		if err != nil {
			return types, notice, err
		}

		require.NoError(t, ev.Validate())

		switch payload := ev.Payload().(type) {
		case agent.ModelStreamEvent:
			if payload.Event.Type == ai.StreamRetry {
				notice = payload.Event.Retry
			}

			continue
		case agent.CandidateDiscarded:
			types = append(types, ev.Type())

			continue
		}

		types = append(types, ev.Type())
	}

	return types, notice, nil
}

func TestStreamRecoveryReissuesTurnAfterPartialOutput(t *testing.T) {
	t.Parallel()

	model := &interruptedStreamModel{failures: 1, err: io.ErrUnexpectedEOF}

	a, err := agent.New(model, agent.WithStreamRecovery(1, time.Millisecond, time.Millisecond))
	require.NoError(t, err)

	sess := agent.NewSession()

	types, notice, err := collectStream(t, a, sess)
	require.NoError(t, err)

	// The partial candidate is retracted before the re-issue, and only the
	// successful attempt reaches the session.
	assert.Equal(t, []agent.EventType{
		agent.EventRunStarted,
		agent.EventTurnStarted,
		agent.EventCandidateDiscarded,
		agent.EventMessageCommitted,
		agent.EventTurnCompleted,
		agent.EventRunCompleted,
	}, types)

	require.NotNil(t, notice)
	assert.Equal(t, 1, notice.Attempt, "the notice counts retries, not tries")
	assert.Equal(t, 1, notice.MaxRetries)
	assert.Equal(t, time.Millisecond, notice.Delay)
	assert.Equal(t, "stream ended early", notice.Reason)

	assert.Equal(t, int32(2), model.calls.Load())

	msgs := sess.Messages()
	require.Len(t, msgs, 2)

	assistant, ok := msgs[1].(ai.AssistantMessage)
	require.True(t, ok)
	text, ok := assistant.Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, "final", text.Text)
}

func TestStreamRecoveryIsOffByDefault(t *testing.T) {
	t.Parallel()

	model := &interruptedStreamModel{failures: 1, err: io.ErrUnexpectedEOF}

	a, err := agent.New(model)
	require.NoError(t, err)

	_, notice, err := collectStream(t, a, agent.NewSession())
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.Nil(t, notice)
	assert.Equal(t, int32(1), model.calls.Load())
}

func TestStreamRecoveryGivesUpAfterBoundedAttempts(t *testing.T) {
	t.Parallel()

	model := &interruptedStreamModel{failures: 5, err: io.ErrUnexpectedEOF}

	a, err := agent.New(model, agent.WithStreamRecovery(2, time.Millisecond, time.Millisecond))
	require.NoError(t, err)

	_, notice, err := collectStream(t, a, agent.NewSession())
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.NotNil(t, notice)
	assert.Equal(t, 2, notice.Attempt)
	assert.Equal(t, 2, notice.MaxRetries)
	assert.Equal(t, int32(3), model.calls.Load(), "one try plus two re-issues")
}

// TestStreamRecoveryBackoffGrowsThenStops pins the wait schedule: every
// re-issue waits longer than the one before it, up to the ceiling.
func TestStreamRecoveryBackoffGrowsThenStops(t *testing.T) {
	t.Parallel()

	model := &interruptedStreamModel{failures: 4, err: io.ErrUnexpectedEOF}

	a, err := agent.New(model, agent.WithStreamRecovery(3, time.Millisecond, 3*time.Millisecond))
	require.NoError(t, err)

	var delays []time.Duration

	for ev, streamErr := range a.Stream(t.Context(), agent.NewSession(), ai.UserText("hi")) {
		if streamErr != nil {
			break
		}

		if payload, ok := ev.Payload().(agent.ModelStreamEvent); ok && payload.Event.Retry != nil {
			delays = append(delays, payload.Event.Retry.Delay)
		}
	}

	assert.Equal(t,
		[]time.Duration{time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond},
		delays,
		"the wait doubles per re-issue and stops at the ceiling",
	)
}

func TestStreamRecoveryRequiresObservedOutput(t *testing.T) {
	t.Parallel()

	model := &interruptedStreamModel{failures: 5, silent: true, err: io.ErrUnexpectedEOF}

	a, err := agent.New(model, agent.WithStreamRecovery(2, time.Millisecond, time.Millisecond))
	require.NoError(t, err)

	// A request that produced nothing belongs to the model middleware, which
	// has its own budget; the loop must not stack a second budget on top.
	_, notice, err := collectStream(t, a, agent.NewSession())
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.Nil(t, notice)
	assert.Equal(t, int32(1), model.calls.Load())
}

func TestStreamRecoverySkipsNonRetryableFailure(t *testing.T) {
	t.Parallel()

	model := &interruptedStreamModel{
		failures: 5,
		err:      ai.NewError(ai.ProviderOpenAI, 400, "invalid request"),
	}

	a, err := agent.New(model, agent.WithStreamRecovery(2, time.Millisecond, time.Millisecond))
	require.NoError(t, err)

	_, notice, err := collectStream(t, a, agent.NewSession())
	require.Error(t, err)
	assert.Nil(t, notice)
	assert.Equal(t, int32(1), model.calls.Load())
}

// TestStreamRecoveryReportsTheFragmentItGivesUpOn keeps the discard mode
// honest. Regenerating replaces the fragment on every attempt, so the one to
// report is the last the consumer was looking at — and it must be reported
// rather than erased, which is what the silent retraction used to do.
func TestStreamRecoveryReportsTheFragmentItGivesUpOn(t *testing.T) {
	t.Parallel()

	model := &continuationModel{attempts: []continuationAttempt{
		{deltas: []string{"half"}, err: io.ErrUnexpectedEOF},
		{deltas: []string{"more"}, err: io.ErrUnexpectedEOF},
	}}

	a, err := agent.New(model, agent.WithStreamRecovery(1, time.Millisecond, time.Millisecond))
	require.NoError(t, err)

	stream := collectContinuationStream(t, a, agent.NewSession())
	require.ErrorIs(t, stream.err, io.ErrUnexpectedEOF)
	assert.Equal(t, 1, stream.discards, "the regeneration retracts the draft it replaced")

	require.Len(t, stream.incompletes, 1)
	assert.Equal(t, "more", stream.incompletes[0].Text,
		"the reported fragment is the one the consumer was looking at")
	assert.Equal(t, "stream ended early", stream.incompletes[0].Reason)
}
