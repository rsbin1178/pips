package agent_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The continuation instruction is unexported; the test pins a stable phrase
// from it so a request carrying it is provable without exporting the constant.
const continuationInstructionPhrase = "Continue it: output only the missing remainder"

// continuationAttempt scripts one stream attempt: the text and reasoning it
// emits, any tool calls, and either the error that breaks it or the finish
// reason that ends it.
type continuationAttempt struct {
	deltas    []string
	reasoning []string
	tools     []ai.ToolCallPart
	err       error
	finish    ai.FinishReason
}

// continuationModel replays one scripted attempt per call and records every
// request, so a continuation can be checked by its shape and not only by its
// call count.
type continuationModel struct {
	mu       sync.Mutex
	attempts []continuationAttempt
	pos      int
	requests []ai.Request
}

func (m *continuationModel) Generate(context.Context, ai.Request) (*ai.Response, error) {
	return nil, errors.New("continuationModel: Generate is not scripted")
}

func (m *continuationModel) Stream(_ context.Context, req ai.Request) ai.Stream {
	m.mu.Lock()

	m.requests = append(m.requests, req)

	index := m.pos
	m.pos++

	var attempt continuationAttempt
	if index < len(m.attempts) {
		attempt = m.attempts[index]
	}

	m.mu.Unlock()

	return func(yield func(ai.StreamEvent, error) bool) {
		if !yield(ai.StreamEvent{Type: ai.StreamMessageStart, ID: "resp"}, nil) {
			return
		}

		for _, text := range attempt.deltas {
			if !yield(ai.StreamEvent{Type: ai.StreamTextDelta, Text: text}, nil) {
				return
			}
		}

		for _, text := range attempt.reasoning {
			if !yield(ai.StreamEvent{Type: ai.StreamReasoningDelta, Text: text}, nil) {
				return
			}
		}

		for i, tool := range attempt.tools {
			if !yield(ai.StreamEvent{Type: ai.StreamToolCallStart, ToolCallIndex: i, ToolCallID: tool.ID, ToolCallName: tool.Name}, nil) ||
				!yield(ai.StreamEvent{Type: ai.StreamToolCallDelta, ToolCallIndex: i, ArgsDelta: string(tool.Args)}, nil) ||
				!yield(ai.StreamEvent{Type: ai.StreamToolCallEnd, ToolCallIndex: i}, nil) {
				return
			}
		}

		if attempt.err != nil {
			yield(ai.StreamEvent{}, attempt.err)

			return
		}

		yield(ai.StreamEvent{Type: ai.StreamMessageEnd, FinishReason: attempt.finish}, nil)
	}
}

func (m *continuationModel) Provider() ai.Provider { return ai.Provider("continuation") }
func (m *continuationModel) ModelID() string       { return "continuation-1" }
func (m *continuationModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

func (m *continuationModel) Requests() []ai.Request {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]ai.Request(nil), m.requests...)
}

// continuationStream is what one agent stream produced: the emitted deltas,
// the lifecycle events, and the give-up events.
type continuationStream struct {
	types       []agent.EventType
	deltas      []string
	reasoning   []string
	discards    int
	incompletes []agent.CandidateIncomplete
	notices     []*ai.RetryNotice
	err         error
}

// emitted joins every text delta the consumer saw.
func (c continuationStream) emitted() string { return strings.Join(c.deltas, "") }

func collectContinuationStream(t *testing.T, a *agent.Agent, sess *agent.Session) continuationStream {
	t.Helper()

	var out continuationStream

	for ev, err := range a.Stream(t.Context(), sess, ai.UserText("hi")) {
		if err != nil {
			out.err = err

			return out
		}

		require.NoError(t, ev.Validate())
		out.types = append(out.types, ev.Type())

		switch payload := ev.Payload().(type) {
		case agent.ModelStreamEvent:
			if payload.Event.Type == ai.StreamTextDelta {
				out.deltas = append(out.deltas, payload.Event.Text)
			}

			if payload.Event.Type == ai.StreamReasoningDelta {
				out.reasoning = append(out.reasoning, payload.Event.Text)
			}

			if payload.Event.Retry != nil {
				out.notices = append(out.notices, payload.Event.Retry)
			}
		case agent.CandidateDiscarded:
			out.discards++
		case agent.CandidateIncomplete:
			out.incompletes = append(out.incompletes, payload)
		}
	}

	return out
}

// committedAssistant returns the single committed assistant message, asserting
// the session holds exactly the prompt and that answer.
func committedAssistant(t *testing.T, sess *agent.Session) ai.AssistantMessage {
	t.Helper()

	messages := sess.Messages()
	require.Len(t, messages, 2)

	assistant, ok := messages[1].(ai.AssistantMessage)
	require.True(t, ok)

	return assistant
}

func TestStreamContinuationReissuesWithRetainedPrefix(t *testing.T) {
	t.Parallel()

	const prefix = "The quick brown fox jumps over the lazy dog"

	model := &continuationModel{attempts: []continuationAttempt{
		{deltas: []string{prefix}, err: io.ErrUnexpectedEOF},
		{deltas: []string{"over the lazy dog and then"}, finish: ai.FinishStop},
	}}

	a, err := agent.New(model, agent.WithStreamContinuation(1, time.Millisecond, time.Millisecond))
	require.NoError(t, err)

	sess := agent.NewSession()
	stream := collectContinuationStream(t, a, sess)
	require.NoError(t, stream.err)

	assert.Zero(t, stream.discards, "a retained prefix is never retracted")
	assert.Empty(t, stream.incompletes)

	// The restated prefix tail is dropped from the continuation, so the live
	// draft and the committed message agree on the de-duplicated text.
	want := prefix + " and then"
	assert.Equal(t, want, stream.emitted(), "the emitted deltas agree with the committed message")

	assistant := committedAssistant(t, sess)
	require.Len(t, assistant.Parts, 1, "adjacent text parts are merged into one")

	text, ok := assistant.Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, want, text.Text)

	requests := model.Requests()
	require.Len(t, requests, 2)

	// The re-issue sends the partial back as a trailing assistant message and
	// asks for the remainder in the leading system block — it does not replay
	// the original request.
	last := requests[1].Messages[len(requests[1].Messages)-1]
	replayed, ok := last.(ai.AssistantMessage)
	require.True(t, ok, "the continuation request ends with an assistant message")
	require.Len(t, replayed.Parts, 1)

	replayedText, ok := replayed.Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, prefix, replayedText.Text)

	assert.Contains(t, requestSystem(t, requests[1]), continuationInstructionPhrase)
	assert.NotContains(t, requestSystem(t, requests[0]), continuationInstructionPhrase)

	require.Len(t, stream.notices, 1)
	assert.Equal(t, "stream ended early", stream.notices[0].Reason)
}

func TestStreamContinuationKeepsLegitimateContinuation(t *testing.T) {
	t.Parallel()

	const prefix = "Hello there, this is the start of a longer answer"

	model := &continuationModel{attempts: []continuationAttempt{
		{deltas: []string{prefix}, err: io.ErrUnexpectedEOF},
		{deltas: []string{"and this sentence carries on without repeating"}, finish: ai.FinishStop},
	}}

	a, err := agent.New(model, agent.WithStreamContinuation(1, time.Millisecond, time.Millisecond))
	require.NoError(t, err)

	sess := agent.NewSession()
	stream := collectContinuationStream(t, a, sess)
	require.NoError(t, stream.err)

	// No 16-byte overlap exists, so nothing is trimmed.
	want := prefix + "and this sentence carries on without repeating"
	assert.Equal(t, want, stream.emitted())

	text, ok := committedAssistant(t, sess).Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, want, text.Text)
}

func TestStreamContinuationGrowsPrefixAcrossAttempts(t *testing.T) {
	t.Parallel()

	first := "alpha beta gamma delta epsilon"
	second := "zeta eta theta iota kappa"

	model := &continuationModel{attempts: []continuationAttempt{
		{deltas: []string{first}, err: io.ErrUnexpectedEOF},
		{deltas: []string{second}, err: io.ErrUnexpectedEOF},
		{deltas: []string{" lambda"}, finish: ai.FinishStop},
	}}

	a, err := agent.New(model, agent.WithStreamContinuation(2, time.Millisecond, time.Millisecond))
	require.NoError(t, err)

	sess := agent.NewSession()
	stream := collectContinuationStream(t, a, sess)
	require.NoError(t, stream.err)

	want := first + second + " lambda"
	assert.Equal(t, want, stream.emitted())

	text, ok := committedAssistant(t, sess).Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, want, text.Text)

	requests := model.Requests()
	require.Len(t, requests, 3)

	grown := requests[2].Messages[len(requests[2].Messages)-1]
	replayed, ok := grown.(ai.AssistantMessage)
	require.True(t, ok)

	replayedText, ok := replayed.Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, first+second, replayedText.Text, "the second continuation carries the grown prefix")
}

func TestStreamContinuationWithoutTextFallsBackToDiscard(t *testing.T) {
	t.Parallel()

	toolCall := ai.ToolCallPart{ID: "call", Name: "add", Args: ai.JSON(`{"a":1,"b":2}`)}

	tests := []struct {
		name    string
		attempt continuationAttempt
	}{
		{
			name:    "tool call",
			attempt: continuationAttempt{tools: []ai.ToolCallPart{toolCall}, err: io.ErrUnexpectedEOF},
		},
		{
			name:    "no text",
			attempt: continuationAttempt{reasoning: []string{"thinking"}, err: io.ErrUnexpectedEOF},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			model := &continuationModel{attempts: []continuationAttempt{
				test.attempt,
				{deltas: []string{"final"}, finish: ai.FinishStop},
			}}

			a, err := agent.New(
				model,
				agent.WithTools(addTool()),
				agent.WithStreamContinuation(1, time.Millisecond, time.Millisecond),
			)
			require.NoError(t, err)

			sess := agent.NewSession()
			stream := collectContinuationStream(t, a, sess)
			require.NoError(t, stream.err)

			assert.Equal(t, 1, stream.discards)
			assert.Empty(t, stream.incompletes)

			requests := model.Requests()
			require.Len(t, requests, 2)

			last := requests[1].Messages[len(requests[1].Messages)-1]
			_, isAssistant := last.(ai.AssistantMessage)
			assert.False(t, isAssistant, "the fallback re-issues the original request")
			assert.NotContains(t, requestSystem(t, requests[1]), continuationInstructionPhrase)

			text, ok := committedAssistant(t, sess).Parts[0].(ai.TextPart)
			require.True(t, ok)
			assert.Equal(t, "final", text.Text)
		})
	}
}

func TestStreamContinuationBudgetExhaustedReportsIncomplete(t *testing.T) {
	t.Parallel()

	const prefix = "the retained partial answer"

	model := &continuationModel{attempts: []continuationAttempt{
		{deltas: []string{prefix}, err: io.ErrUnexpectedEOF},
		{reasoning: []string{"still thinking"}, err: io.ErrUnexpectedEOF},
	}}

	a, err := agent.New(model, agent.WithStreamContinuation(1, time.Millisecond, time.Millisecond))
	require.NoError(t, err)

	stream := collectContinuationStream(t, a, agent.NewSession())
	require.ErrorIs(t, stream.err, io.ErrUnexpectedEOF)

	assert.Zero(t, stream.discards, "the retained prefix is not reported as discarded")
	require.Len(t, stream.incompletes, 1)
	assert.Equal(t, 1, stream.incompletes[0].Turn)
	assert.Equal(t, prefix, stream.incompletes[0].Text)
	assert.Equal(t, "stream ended early", stream.incompletes[0].Reason)
	assert.Len(t, model.Requests(), 2, "one try plus one continuation")
}

func TestStreamContinuationNonRetryableFailureReportsIncomplete(t *testing.T) {
	t.Parallel()

	const prefix = "partial before the error"

	model := &continuationModel{attempts: []continuationAttempt{
		{deltas: []string{prefix}, err: ai.NewError(ai.ProviderOpenAI, 400, "invalid request")},
	}}

	a, err := agent.New(model, agent.WithStreamContinuation(2, time.Millisecond, time.Millisecond))
	require.NoError(t, err)

	stream := collectContinuationStream(t, a, agent.NewSession())
	require.Error(t, stream.err)

	assert.Len(t, model.Requests(), 1, "a non-retryable failure is not re-issued")
	assert.Zero(t, stream.discards)
	require.Len(t, stream.incompletes, 1)
	assert.Equal(t, prefix, stream.incompletes[0].Text)
}

func TestStreamContinuationBoundsIncompleteText(t *testing.T) {
	t.Parallel()

	oversized := strings.Repeat("x", 64<<10) + "tail"

	model := &continuationModel{attempts: []continuationAttempt{
		{deltas: []string{oversized}, err: io.ErrUnexpectedEOF},
		{err: io.ErrUnexpectedEOF},
	}}

	a, err := agent.New(model, agent.WithStreamContinuation(1, time.Millisecond, time.Millisecond))
	require.NoError(t, err)

	stream := collectContinuationStream(t, a, agent.NewSession())
	require.ErrorIs(t, stream.err, io.ErrUnexpectedEOF)

	require.Len(t, stream.incompletes, 1)
	assert.Len(t, stream.incompletes[0].Text, 64<<10, "the retained text is bounded")
	assert.Equal(t, strings.Repeat("x", 64<<10), stream.incompletes[0].Text)
	assert.Equal(t, len(oversized), stream.incompletes[0].Bytes,
		"the record still reports the size before the bound")
}

func TestCandidateIncompleteEventValidates(t *testing.T) {
	t.Parallel()

	meta := agent.RunMetadata{RunID: "run"}

	event, err := agent.NewEvent(meta, time.Now().UTC(), agent.CandidateIncomplete{
		Turn: 1, Text: "partial", Bytes: len("partial"), Reason: "stream ended early",
	})
	require.NoError(t, err)
	assert.Equal(t, agent.EventCandidateIncomplete, event.Type())
	require.NoError(t, event.Validate())

	_, err = agent.NewEvent(meta, time.Now().UTC(), agent.CandidateIncomplete{})
	require.ErrorIs(t, err, agent.ErrInvalidEvent)

	_, err = agent.NewEvent(meta, time.Now().UTC(), agent.CandidateIncomplete{
		Turn: 1, Text: "partial", Bytes: len("partial") - 1,
	})
	require.ErrorIs(t, err, agent.ErrInvalidEvent,
		"a record cannot claim fewer original bytes than it retains")
}

// The restart instruction is unexported; the test pins a stable phrase from it
// so a request carrying it is provable without exporting the constant.
const restartInstructionPhrase = "Write the complete answer again from the start"

// lastRequestMessage returns the message a request ends with, which is where a
// continuation's retained prefix would appear.
func lastRequestMessage(t *testing.T, req ai.Request) ai.Message {
	t.Helper()

	require.NotEmpty(t, req.Messages)

	return req.Messages[len(req.Messages)-1]
}

// TestStreamContinuationFallsBackWhenTheProviderRefusesTheShape covers a
// provider that will not serve a request ending in an assistant message: the
// loop drops the fragment and answers from the start rather than losing a turn
// its own request could still complete.
func TestStreamContinuationFallsBackWhenTheProviderRefusesTheShape(t *testing.T) {
	t.Parallel()

	model := &continuationModel{attempts: []continuationAttempt{
		{deltas: []string{"half"}, err: io.ErrUnexpectedEOF},
		{err: ai.NewError(ai.ProviderOpenAI, 400, "invalid request")},
		{deltas: []string{"whole answer"}, finish: ai.FinishStop},
	}}

	a, err := agent.New(model, agent.WithStreamContinuation(3, time.Millisecond, time.Millisecond))
	require.NoError(t, err)

	sess := agent.NewSession()
	stream := collectContinuationStream(t, a, sess)
	require.NoError(t, stream.err)
	assert.Empty(t, stream.incompletes, "the turn completed, so nothing is abandoned")
	assert.Equal(t, 1, stream.discards, "the fragment is dropped before the restart")

	requests := model.Requests()
	require.Len(t, requests, 3)

	// The continuation really was tried first and really did carry the
	// fragment, so the fallback is not passing by accident.
	replayed, ok := lastRequestMessage(t, requests[1]).(ai.AssistantMessage)
	require.True(t, ok, "the first re-issue is the continuation")
	require.Len(t, replayed.Parts, 1)

	replayedText, ok := replayed.Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, "half", replayedText.Text)
	assert.Contains(t, requestSystem(t, requests[1]), continuationInstructionPhrase)

	// The restart goes back to the turn's own request shape and says why.
	assert.NotContains(t, requestSystem(t, requests[2]), continuationInstructionPhrase)
	assert.Contains(t, requestSystem(t, requests[2]), restartInstructionPhrase)

	_, tailIsAssistant := lastRequestMessage(t, requests[2]).(ai.AssistantMessage)
	assert.False(t, tailIsAssistant, "the restart sends the turn's own request")

	assistant := committedAssistant(t, sess)
	text, ok := assistant.Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, "whole answer", text.Text,
		"the answer comes from the restart, not from the abandoned fragment")
}

// TestStreamContinuationFallsBackWhenTheContinuationAddsNothing covers the
// other way a continuation can be unusable: the provider accepts it and returns
// no answer text. Committing the fragment alone would present it as the whole
// reply, so the turn answers from the start instead.
func TestStreamContinuationFallsBackWhenTheContinuationAddsNothing(t *testing.T) {
	t.Parallel()

	model := &continuationModel{attempts: []continuationAttempt{
		{deltas: []string{"half"}, err: io.ErrUnexpectedEOF},
		{finish: ai.FinishStop},
		{deltas: []string{"whole answer"}, finish: ai.FinishStop},
	}}

	a, err := agent.New(model, agent.WithStreamContinuation(3, time.Millisecond, time.Millisecond))
	require.NoError(t, err)

	sess := agent.NewSession()
	stream := collectContinuationStream(t, a, sess)
	require.NoError(t, stream.err)
	assert.Empty(t, stream.incompletes)
	assert.Equal(t, 1, stream.discards)

	requests := model.Requests()
	require.Len(t, requests, 3)
	assert.Contains(t, requestSystem(t, requests[2]), restartInstructionPhrase)

	assistant := committedAssistant(t, sess)
	text, ok := assistant.Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, "whole answer", text.Text)
}

// TestStreamContinuationFallsBackOnlyOnce pins the bound: a restart that also
// fails gives up instead of restarting again, so the escape cannot loop.
func TestStreamContinuationFallsBackOnlyOnce(t *testing.T) {
	t.Parallel()

	invalid := ai.NewError(ai.ProviderOpenAI, 400, "invalid request")

	model := &continuationModel{attempts: []continuationAttempt{
		{deltas: []string{"half"}, err: io.ErrUnexpectedEOF},
		{err: invalid},
		{err: invalid},
	}}

	a, err := agent.New(model, agent.WithStreamContinuation(5, time.Millisecond, time.Millisecond))
	require.NoError(t, err)

	stream := collectContinuationStream(t, a, agent.NewSession())
	require.Error(t, stream.err)
	assert.Equal(t, 1, stream.discards, "the fallback happens once")
	assert.Len(t, model.Requests(), 3, "and it does not restart a second time")
}
