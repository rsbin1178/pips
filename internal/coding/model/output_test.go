package model

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type outputAttempt struct {
	events []ai.StreamEvent
	resp   *ai.Response
	err    error
}

type outputScript struct {
	attempts []outputAttempt
	calls    int
}

func (m *outputScript) next() outputAttempt {
	index := min(m.calls, len(m.attempts)-1)
	m.calls++

	return m.attempts[index]
}

func (m *outputScript) Generate(context.Context, ai.Request) (*ai.Response, error) {
	attempt := m.next()
	return attempt.resp, attempt.err
}

func (m *outputScript) Stream(context.Context, ai.Request) ai.Stream {
	attempt := m.next()

	return func(yield func(ai.StreamEvent, error) bool) {
		for _, event := range attempt.events {
			if !yield(event, nil) {
				return
			}
		}

		if attempt.err != nil {
			yield(ai.StreamEvent{}, attempt.err)
		}
	}
}

func (*outputScript) Provider() ai.Provider         { return ai.ProviderOpenAI }
func (*outputScript) ModelID() string               { return "output-test" }
func (*outputScript) Capabilities() ai.Capabilities { return ai.Capabilities{Text: true, Tools: true} }

func outputEvents(parts ...ai.StreamEvent) []ai.StreamEvent {
	events := []ai.StreamEvent{{Type: ai.StreamMessageStart, Provider: ai.ProviderOpenAI, ID: "response"}}
	events = append(events, parts...)

	return append(events, ai.StreamEvent{Type: ai.StreamMessageEnd, FinishReason: ai.FinishStop})
}

func TestCodingEmptyStreamRecoveryUsesSingleOwner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		events []ai.StreamEvent
		cause  error
	}{
		{name: "no events", cause: ai.ErrIncompleteStream},
		{name: "only start", events: []ai.StreamEvent{{Type: ai.StreamMessageStart}}, cause: ai.ErrIncompleteStream},
		{name: "empty completion", events: outputEvents(), cause: ai.ErrEmptyResponse},
		{name: "whitespace completion", events: outputEvents(ai.StreamEvent{Type: ai.StreamTextDelta, Text: " \n\t"}), cause: ai.ErrEmptyResponse},
		{name: "reasoning completion", events: outputEvents(ai.StreamEvent{Type: ai.StreamReasoningDelta, Text: "thinking"}), cause: ai.ErrEmptyResponse},
		{name: "reasoning signature", events: outputEvents(ai.StreamEvent{Type: ai.StreamReasoningDelta, Signature: "opaque"}), cause: ai.ErrEmptyResponse},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			base := &outputScript{attempts: []outputAttempt{{events: test.events}}}
			model := withCodingRetry(base, time.Hour, noRetrySleep())
			a, err := agent.New(model, agent.WithStreamRecovery(3, time.Nanosecond, time.Nanosecond), agent.WithStreamReplayAttempts(RetryBudget))
			require.NoError(t, err)

			session := agent.NewSession()

			var (
				terminal           error
				notices, committed int
			)

			for event, streamErr := range a.Stream(t.Context(), session, ai.UserText("answer")) {
				if streamErr != nil {
					terminal = streamErr
					break
				}

				switch payload := event.Payload().(type) {
				case agent.ModelStreamEvent:
					if payload.Event.Type == ai.StreamRetry {
						notices++
					}
				case agent.MessageCommitted:
					if _, ok := payload.Message.(ai.AssistantMessage); ok {
						committed++
					}
				}
			}

			require.ErrorIs(t, terminal, test.cause)
			assert.Equal(t, RetryBudget+1, base.calls)
			assert.Equal(t, RetryBudget, notices)
			assert.Zero(t, committed)
			assert.Len(t, session.Messages(), 1)
		})
	}
}

func TestCodingGenerateValidatesBeforeRetry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		response *ai.Response
		cause    error
	}{
		{name: "nil", cause: ai.ErrEmptyResponse},
		{name: "whitespace", response: &ai.Response{Message: ai.AssistantText(" \n")}, cause: ai.ErrEmptyResponse},
		{name: "reasoning", response: &ai.Response{Message: ai.Assistant(ai.ReasoningPart{Text: "thought"})}, cause: ai.ErrEmptyResponse},
		{name: "length", response: &ai.Response{FinishReason: ai.FinishLength}, cause: ai.ErrResponseTruncated},
		{name: "filter", response: &ai.Response{FinishReason: ai.FinishContentFilter}, cause: ai.ErrContentFiltered},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			base := &outputScript{attempts: []outputAttempt{{resp: test.response}}}
			model := withCodingRetry(base, time.Hour, noRetrySleep())
			_, err := model.Generate(t.Context(), ai.Request{})
			require.ErrorIs(t, err, test.cause)

			if ai.IsRetryable(err) {
				assert.Equal(t, RetryBudget+1, base.calls)
			} else {
				assert.Equal(t, 1, base.calls)
			}
		})
	}
}

func TestCodingStreamRejectsTerminalBeforePublishing(t *testing.T) {
	t.Parallel()

	base := &outputScript{attempts: []outputAttempt{
		{events: outputEvents(ai.StreamEvent{Type: ai.StreamReasoningDelta, Text: "thought"})},
	}}

	var (
		terminal error
		ended    bool
	)

	for event, err := range (&outputModel{LanguageModel: base}).Stream(t.Context(), ai.Request{}) {
		if err != nil {
			terminal = err
			break
		}

		ended = ended || event.Type == ai.StreamMessageEnd
	}

	require.ErrorIs(t, terminal, ai.ErrEmptyResponse)
	assert.False(t, ended)

	var summary *ai.ResponseError
	require.ErrorAs(t, terminal, &summary)
	assert.True(t, summary.TerminalObserved)
	assert.True(t, summary.HasReasoning)
}

func TestCodingOutputPreservesSuccessfulFormattingAndTools(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		response *ai.Response
	}{
		{name: "formatted text", response: &ai.Response{Message: ai.AssistantText("  answer\n")}},
		{name: "tool", response: &ai.Response{Message: ai.Assistant(ai.ToolCallPart{ID: "call", Name: "read", Args: ai.JSON(`{}`)})}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			base := &outputScript{attempts: []outputAttempt{{resp: test.response}}}
			result, err := withCodingRetry(base, time.Hour, noRetrySleep()).Generate(t.Context(), ai.Request{})
			require.NoError(t, err)
			assert.Same(t, test.response, result)
			assert.Equal(t, 1, base.calls)
		})
	}
}

func TestCodingValidationHonorsCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	base := &outputScript{attempts: []outputAttempt{{}}}
	model := withCodingRetry(base, time.Hour, noRetrySleep())
	_, err := model.Generate(ctx, ai.Request{})
	require.ErrorIs(t, err, context.Canceled)
	_, err = ai.Collect(model.Stream(ctx, ai.Request{}))
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, base.calls)
}

func TestCodingEmptyResponseRecoveryCommitsOnlyAnswer(t *testing.T) {
	t.Parallel()

	base := &outputScript{attempts: []outputAttempt{
		{events: outputEvents()},
		{events: outputEvents(ai.StreamEvent{Type: ai.StreamTextDelta, Text: "answer"})},
	}}
	model := withCodingRetry(base, time.Hour, noRetrySleep())
	a, err := agent.New(model)
	require.NoError(t, err)

	session := agent.NewSession()
	result, err := a.RunStream(t.Context(), session, func(agent.Event) bool { return true }, ai.UserText("question"))
	require.NoError(t, err)
	assert.Equal(t, agent.StopEndTurn, result.Stop)
	assert.Equal(t, "answer", result.Response.Text())
	assert.Len(t, session.Messages(), 2)
	assert.Equal(t, 2, base.calls)
}

func TestCodingExplicitFailuresBypassEmptyRetries(t *testing.T) {
	t.Parallel()

	for _, cause := range []error{ai.ErrRefused, ai.ErrContentFiltered} {
		t.Run(fmt.Sprint(cause), func(t *testing.T) {
			t.Parallel()

			base := &outputScript{attempts: []outputAttempt{{err: ai.NewResponseError(cause, nil, true)}}}
			_, err := ai.Collect(withCodingRetry(base, time.Hour, noRetrySleep()).Stream(t.Context(), ai.Request{}))
			require.ErrorIs(t, err, cause)
			require.NotErrorIs(t, err, ai.ErrEmptyResponse)
			assert.Equal(t, 1, base.calls)
		})
	}
}

func TestCodingMixedEmptyAndReasoningShareReplayBudget(t *testing.T) {
	t.Parallel()

	var attempts []outputAttempt

	for index := range RetryBudget + 2 {
		events := outputEvents()
		if index%2 == 1 {
			events = outputEvents(ai.StreamEvent{Type: ai.StreamReasoningDelta, Text: "thought"})
		}

		attempts = append(attempts, outputAttempt{events: events})
	}

	base := &outputScript{attempts: attempts}
	model := withCodingRetry(base, time.Hour, noRetrySleep())
	a, err := agent.New(model,
		agent.WithStreamRecovery(3, time.Nanosecond, time.Nanosecond),
		agent.WithStreamReplayAttempts(RetryBudget),
	)
	require.NoError(t, err)

	var (
		ordinals []int
		terminal error
	)

	for event, streamErr := range a.Stream(t.Context(), agent.NewSession(), ai.UserText("answer")) {
		if streamErr != nil {
			terminal = streamErr
			break
		}

		if payload, ok := event.Payload().(agent.ModelStreamEvent); ok && payload.Event.Retry != nil {
			ordinals = append(ordinals, payload.Event.Retry.Attempt)
			assert.Equal(t, RetryBudget, payload.Event.Retry.MaxRetries)
		}
	}

	require.ErrorIs(t, terminal, ai.ErrEmptyResponse)
	assert.Equal(t, RetryBudget+1, base.calls)
	assert.Equal(t, []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, ordinals)
}

func TestCodingPartialToolsCannotExecuteBeforeRecovery(t *testing.T) {
	t.Parallel()

	base := &outputScript{attempts: []outputAttempt{
		{events: []ai.StreamEvent{
			{Type: ai.StreamMessageStart},
			{Type: ai.StreamToolCallStart, ToolCallID: "call", ToolCallName: "write"},
			{Type: ai.StreamToolCallDelta, ArgsDelta: `{"path":`},
		}},
		{events: outputEvents(ai.StreamEvent{Type: ai.StreamTextDelta, Text: "recovered"})},
	}}

	var executions int

	tool := agent.NewTool("write", "write", func(context.Context, struct{}) (string, error) {
		executions++
		return "written", nil
	})
	a, err := agent.New(withCodingRetry(base, time.Hour, noRetrySleep()),
		agent.WithTools(tool), agent.WithStreamRecovery(3, time.Nanosecond, time.Nanosecond),
		agent.WithStreamReplayAttempts(RetryBudget))
	require.NoError(t, err)

	session := agent.NewSession()
	result, err := a.RunStream(t.Context(), session, func(agent.Event) bool { return true }, ai.UserText("write"))
	require.NoError(t, err)
	assert.Equal(t, "recovered", result.Response.Text())
	assert.Zero(t, executions)
	assert.Len(t, session.Messages(), 2)
}

func TestCodingEmptyRecoveryPreservesCompletedToolResults(t *testing.T) {
	t.Parallel()

	base := &outputScript{attempts: []outputAttempt{
		{events: outputEvents(
			ai.StreamEvent{Type: ai.StreamToolCallStart, ToolCallID: "call", ToolCallName: "write"},
			ai.StreamEvent{Type: ai.StreamToolCallDelta, ArgsDelta: `{}`},
			ai.StreamEvent{Type: ai.StreamToolCallEnd},
		)},
		{events: outputEvents()},
		{events: outputEvents(ai.StreamEvent{Type: ai.StreamTextDelta, Text: "done"})},
	}}

	var executions int

	tool := agent.NewTool("write", "write", func(context.Context, struct{}) (string, error) {
		executions++
		return "written", nil
	})
	a, err := agent.New(withCodingRetry(base, time.Hour, noRetrySleep()),
		agent.WithTools(tool), agent.WithStreamRecovery(3, time.Nanosecond, time.Nanosecond),
		agent.WithStreamReplayAttempts(RetryBudget))
	require.NoError(t, err)

	session := agent.NewSession()
	result, err := a.RunStream(t.Context(), session, func(agent.Event) bool { return true }, ai.UserText("write"))
	require.NoError(t, err)
	assert.Equal(t, "done", result.Response.Text())
	assert.Equal(t, 1, executions)
	assert.Len(t, session.Messages(), 4)
	assert.Equal(t, 3, base.calls)
}
