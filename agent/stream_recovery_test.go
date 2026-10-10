package agent_test

import (
	"context"
	"errors"
	"fmt"
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

// collectStream drains one agent stream, returning the non-delta event types,
// the last retry notice seen, and how many notices the stream carried, so a
// test can pin the count rather than only the last one.
func collectStream(t *testing.T, a *agent.Agent, sess *agent.Session) ([]agent.EventType, *ai.RetryNotice, int, error) {
	t.Helper()

	var (
		types   []agent.EventType
		notice  *ai.RetryNotice
		notices int
	)

	for ev, err := range a.Stream(t.Context(), sess, ai.UserText("hi")) {
		if err != nil {
			return types, notice, notices, err
		}

		require.NoError(t, ev.Validate())

		switch payload := ev.Payload().(type) {
		case agent.ModelStreamEvent:
			if payload.Event.Type == ai.StreamRetry {
				notice = payload.Event.Retry
				notices++
			}

			continue
		case agent.CandidateDiscarded:
			types = append(types, ev.Type())

			continue
		}

		types = append(types, ev.Type())
	}

	return types, notice, notices, nil
}

func TestStreamRecoveryReissuesTurnAfterPartialOutput(t *testing.T) {
	t.Parallel()

	model := &interruptedStreamModel{failures: 1, err: io.ErrUnexpectedEOF}

	a, err := agent.New(model, agent.WithStreamRecovery(1, time.Millisecond, time.Millisecond))
	require.NoError(t, err)

	sess := agent.NewSession()

	types, notice, _, err := collectStream(t, a, sess)
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

// A stream that produced output and then went silent is aborted by the
// transport as ai.ErrStreamIdle. It has to recover exactly like any other
// post-output failure: this pins the design claim that the idle bound needs no
// mechanism of its own.
func TestStreamRecoveryReissuesAfterAnIdleAbort(t *testing.T) {
	t.Parallel()

	// Wrapped the way an adapter reports it, so the test covers the
	// errors.Is traversal the recovery gate actually performs.
	model := &interruptedStreamModel{
		failures: 1,
		err:      fmt.Errorf("openai: responses stream: %w", ai.ErrStreamIdle),
	}

	a, err := agent.New(model, agent.WithStreamRecovery(1, time.Millisecond, time.Millisecond))
	require.NoError(t, err)

	sess := agent.NewSession()

	types, notice, notices, err := collectStream(t, a, sess)
	require.NoError(t, err)

	assert.Equal(t, []agent.EventType{
		agent.EventRunStarted,
		agent.EventTurnStarted,
		agent.EventCandidateDiscarded,
		agent.EventMessageCommitted,
		agent.EventTurnCompleted,
		agent.EventRunCompleted,
	}, types)

	require.NotNil(t, notice)
	assert.Equal(t, 1, notices, "one re-issue means exactly one notice")
	assert.Equal(t, 1, notice.Attempt)
	assert.Equal(t, "stream idle", notice.Reason, "the notice names the silence, not a connection error")
	assert.Equal(t, int32(2), model.calls.Load())

	msgs := sess.Messages()
	require.Len(t, msgs, 2)

	assistant, ok := msgs[1].(ai.AssistantMessage)
	require.True(t, ok)
	text, ok := assistant.Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, "final", text.Text)
}

// A recovery episode is bounded by wall clock as well as by attempts: a
// provider that has stopped answering can spend a full idle window on every
// attempt, so the attempt budget alone does not bound the waiting.
func TestStreamRecoveryWindowStopsReissuing(t *testing.T) {
	t.Parallel()

	model := &interruptedStreamModel{failures: 5, err: io.ErrUnexpectedEOF}

	a, err := agent.New(
		model,
		agent.WithStreamRecovery(3, time.Millisecond, time.Millisecond),
		agent.WithStreamRecoveryWindow(time.Nanosecond),
	)
	require.NoError(t, err)

	types, notice, notices, err := collectStream(t, a, agent.NewSession())

	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.Equal(t, int32(2), model.calls.Load(),
		"the first re-issue fits before the window is spent, the second never starts")
	assert.Equal(t, 1, notices)
	require.NotNil(t, notice)
	assert.Equal(t, 1, notice.Attempt)
	assert.Contains(t, types, agent.EventCandidateIncomplete,
		"giving up on the window reports the fragment the same way an exhausted budget does")
}

func TestStreamRecoveryWindowLeavesARoomyEpisodeAlone(t *testing.T) {
	t.Parallel()

	model := &interruptedStreamModel{failures: 1, err: io.ErrUnexpectedEOF}

	a, err := agent.New(
		model,
		agent.WithStreamRecovery(1, time.Millisecond, time.Millisecond),
		agent.WithStreamRecoveryWindow(time.Hour),
	)
	require.NoError(t, err)

	sess := agent.NewSession()

	types, _, notices, err := collectStream(t, a, sess)

	require.NoError(t, err)
	assert.Equal(t, 1, notices)
	assert.Contains(t, types, agent.EventMessageCommitted)
	assert.Equal(t, int32(2), model.calls.Load())
}

// A frontend hands the loop one policy value rather than knowing which options
// make it up, so composition has to apply every part of it.
func TestComposeOptionsAppliesEveryRecoveryOption(t *testing.T) {
	t.Parallel()

	model := &interruptedStreamModel{failures: 5, err: io.ErrUnexpectedEOF}

	a, err := agent.New(
		model,
		agent.ComposeOptions(
			agent.WithStreamRecovery(1, time.Millisecond, time.Millisecond),
			agent.WithStreamRecoveryWindow(time.Nanosecond),
		),
	)
	require.NoError(t, err)

	_, _, notices, err := collectStream(t, a, agent.NewSession())

	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.Equal(t, 1, notices, "the re-issue budget was applied")
	assert.Equal(t, int32(2), model.calls.Load(), "the window was applied too")
}

func TestStreamRecoveryIsOffByDefault(t *testing.T) {
	t.Parallel()

	model := &interruptedStreamModel{failures: 1, err: io.ErrUnexpectedEOF}

	a, err := agent.New(model)
	require.NoError(t, err)

	_, notice, _, err := collectStream(t, a, agent.NewSession())
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.Nil(t, notice)
	assert.Equal(t, int32(1), model.calls.Load())
}

func TestStreamRecoveryGivesUpAfterBoundedAttempts(t *testing.T) {
	t.Parallel()

	model := &interruptedStreamModel{failures: 5, err: io.ErrUnexpectedEOF}

	a, err := agent.New(model, agent.WithStreamRecovery(2, time.Millisecond, time.Millisecond))
	require.NoError(t, err)

	_, notice, _, err := collectStream(t, a, agent.NewSession())
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
	_, notice, _, err := collectStream(t, a, agent.NewSession())
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

	_, notice, _, err := collectStream(t, a, agent.NewSession())
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

// TestStreamReplayAttemptsAloneEnableNothing pins that the allowance configures
// recovery rather than switching it on: with no attempt budget there is nothing
// to spend it against. Giving up still retracts the thinking the consumer
// rendered, because a failed run must not be left holding a live draft.
func TestStreamReplayAttemptsAloneEnableNothing(t *testing.T) {
	t.Parallel()

	model := &continuationModel{attempts: []continuationAttempt{
		{reasoning: []string{"first thought"}, err: io.ErrUnexpectedEOF},
		{reasoning: []string{"second thought"}, err: io.ErrUnexpectedEOF},
	}}

	a, err := agent.New(model, agent.WithStreamReplayAttempts(9))
	require.NoError(t, err)

	stream := collectContinuationStream(t, a, agent.NewSession())

	require.ErrorIs(t, stream.err, io.ErrUnexpectedEOF)
	assert.Empty(t, stream.notices)
	assert.Len(t, model.Requests(), 1)
	assert.Equal(t, 1, stream.discards,
		"giving up retracts the thinking the consumer rendered")
	assert.Empty(t, stream.incompletes)
}

// TestStreamReplayAttemptsCoverReasoningOnlyFailures pins the split budget: a
// failure whose only output was reasoning is equivalent to one that produced
// nothing, because dropping the thinking costs the consumer no answer. It draws
// the replay allowance the model middleware would have spent, while the
// re-issue budget stays reserved for answer content.
func TestStreamReplayAttemptsCoverReasoningOnlyFailures(t *testing.T) {
	t.Parallel()

	model := &continuationModel{attempts: []continuationAttempt{
		{reasoning: []string{"first thought"}, err: io.ErrUnexpectedEOF},
		{reasoning: []string{"second thought"}, err: io.ErrUnexpectedEOF},
		{reasoning: []string{"third thought"}, err: io.ErrUnexpectedEOF},
		{deltas: []string{"answer"}, finish: ai.FinishStop},
	}}

	a, err := agent.New(
		model,
		agent.WithStreamRecovery(1, time.Millisecond, time.Millisecond),
		agent.WithStreamReplayAttempts(3),
	)
	require.NoError(t, err)

	stream := collectContinuationStream(t, a, agent.NewSession())

	require.NoError(t, stream.err)
	assert.Equal(t, []string{"first thought", "second thought", "third thought"}, stream.reasoning)
	assert.Equal(t, "answer", stream.emitted())
	require.Len(t, stream.notices, 3, "the replay allowance covers three re-issues, not the re-issue budget")
	assert.Equal(t, 3, stream.notices[2].Attempt)
	assert.Equal(t, 3, stream.notices[2].MaxRetries, "the notice reports the replay budget")
	assert.Equal(t, 3, stream.discards, "every replay retracts the thinking the consumer rendered")
	assert.Empty(t, stream.incompletes, "reasoning is not an answer fragment to report")
	assert.Len(t, model.Requests(), 4)
}

// TestStreamReplayAttemptsLeaveTheOutputBudgetAlone is the other half of the
// split: a failure that produced answer content still spends the smaller
// re-issue budget, because each of its re-issues retracts what the consumer
// read.
func TestStreamReplayAttemptsLeaveTheOutputBudgetAlone(t *testing.T) {
	t.Parallel()

	model := &continuationModel{attempts: []continuationAttempt{
		{deltas: []string{"half"}, err: io.ErrUnexpectedEOF},
		{deltas: []string{"more"}, err: io.ErrUnexpectedEOF},
	}}

	a, err := agent.New(
		model,
		agent.WithStreamRecovery(1, time.Millisecond, time.Millisecond),
		agent.WithStreamReplayAttempts(9),
	)
	require.NoError(t, err)

	stream := collectContinuationStream(t, a, agent.NewSession())

	require.ErrorIs(t, stream.err, io.ErrUnexpectedEOF)
	require.Len(t, stream.notices, 1, "answer output does not reach the replay allowance")
	assert.Equal(t, 1, stream.notices[0].MaxRetries, "the notice reports the re-issue budget")
	assert.Equal(t, 1, stream.discards)
	require.Len(t, stream.incompletes, 1)
	assert.Equal(t, "more", stream.incompletes[0].Text)
	assert.Len(t, model.Requests(), 2)
}

// TestStreamRecoveryWithoutReplayAllowanceKeepsOneTier keeps the default
// honest: an agent that sets no replay allowance bounds both failure shapes with
// one budget, which is what every existing frontend already gets.
func TestStreamRecoveryWithoutReplayAllowanceKeepsOneTier(t *testing.T) {
	t.Parallel()

	model := &continuationModel{attempts: []continuationAttempt{
		{reasoning: []string{"first thought"}, err: io.ErrUnexpectedEOF},
		{reasoning: []string{"second thought"}, err: io.ErrUnexpectedEOF},
	}}

	a, err := agent.New(model, agent.WithStreamRecovery(1, time.Millisecond, time.Millisecond))
	require.NoError(t, err)

	stream := collectContinuationStream(t, a, agent.NewSession())

	require.ErrorIs(t, stream.err, io.ErrUnexpectedEOF)
	require.Len(t, stream.notices, 1)
	assert.Equal(t, 1, stream.notices[0].MaxRetries)
	assert.Equal(t, 2, stream.discards,
		"the re-issue retracts the first thinking and the give-up retracts the second")
	assert.Empty(t, stream.incompletes, "reasoning is not an answer fragment to report")
	assert.Len(t, model.Requests(), 2)
}
