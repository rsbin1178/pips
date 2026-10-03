package compaction

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type sampleStep struct {
	response *ai.Response
	err      error
}

type scriptedModel struct {
	steps    []sampleStep
	requests []ai.Request
	generate func(context.Context, ai.Request) (*ai.Response, error)
}

func (m *scriptedModel) Generate(ctx context.Context, request ai.Request) (*ai.Response, error) {
	m.requests = append(m.requests, request)
	if m.generate != nil {
		return m.generate(ctx, request)
	}

	if len(m.steps) == 0 {
		return nil, errors.New("script exhausted")
	}

	step := m.steps[0]
	m.steps = m.steps[1:]

	return step.response, step.err
}

func (*scriptedModel) Stream(context.Context, ai.Request) ai.Stream { return nil }
func (*scriptedModel) Provider() ai.Provider                        { return "test" }
func (*scriptedModel) ModelID() string                              { return "test" }
func (*scriptedModel) Capabilities() ai.Capabilities                { return ai.Capabilities{Text: true} }

func goodResponse() *ai.Response {
	return &ai.Response{
		Message:      ai.AssistantText("<summary>\n" + strings.Repeat("Verified changes and remaining work. ", 24) + "\n</summary>"),
		FinishReason: ai.FinishStop,
		Usage:        ai.Usage{InputTokens: 1000, OutputTokens: 200, CachedInputTokens: 800, ReasoningTokens: 50},
	}
}

func normalInput() Input {
	return Input{Messages: ai.Messages{ai.UserText("Implement the requested change."), ai.AssistantText("Inspected the relevant source.")}, SeedTokens: 100}
}

func testSummarizer(t *testing.T, model *scriptedModel) *Summarizer {
	t.Helper()

	s, err := NewSummarizer(model, Policy{ContextWindow: 12000, ReserveTokens: 1024}, nil)
	require.NoError(t, err)

	s.wait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }

	return s
}

func TestSummarizeAcceptsCleanedTextIncludingTruncation(t *testing.T) {
	t.Parallel()

	for _, finish := range []ai.FinishReason{ai.FinishStop, ai.FinishLength} {
		t.Run(string(finish), func(t *testing.T) {
			t.Parallel()

			response := goodResponse()
			response.FinishReason = finish
			model := &scriptedModel{steps: []sampleStep{{response: response}}}
			result, err := testSummarizer(t, model).Summarize(t.Context(), normalInput())
			require.NoError(t, err)
			assert.Contains(t, result.Summary, "Verified changes")
			assert.NotContains(t, result.Summary, "<summary>")
			assert.Equal(t, finish == ai.FinishLength, result.Truncated)
			assert.Equal(t, 1, result.Attempts)
			assert.Equal(t, response.Usage, result.Usage)
		})
	}
}

func TestSummarizeRetriesUnusableResponses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		bad  *ai.Response
	}{
		{name: "nil"},
		{name: "empty", bad: &ai.Response{Message: ai.AssistantText("")}},
		{name: "whitespace", bad: &ai.Response{Message: ai.AssistantText(" \n ")}},
		{name: "wrapper", bad: &ai.Response{Message: ai.AssistantText("\n\n---\n\n**Turn Context (split turn):**\n\n")}},
		{name: "reasoning", bad: &ai.Response{Message: ai.Assistant(ai.ReasoningPart{Text: strings.Repeat("thinking", 100)})}},
		{name: "tool_call", bad: &ai.Response{Message: ai.Assistant(ai.ToolCallPart{ID: "x", Name: "shell", Args: ai.JSON(`{}`)})}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			model := &scriptedModel{steps: []sampleStep{{response: tt.bad}, {response: goodResponse()}}}
			result, err := testSummarizer(t, model).Summarize(t.Context(), normalInput())
			require.NoError(t, err)
			assert.Equal(t, 2, result.Attempts)
			require.Len(t, model.requests, 2)
		})
	}
}

func TestSummarizeExhaustsQualityAttemptsWithoutChangingStage(t *testing.T) {
	t.Parallel()

	bad := &ai.Response{Message: ai.AssistantText("too short"), FinishReason: ai.FinishLength}
	model := &scriptedModel{steps: []sampleStep{{response: bad}, {response: bad}, {response: bad}}}
	result, err := testSummarizer(t, model).Summarize(t.Context(), normalInput())
	require.ErrorIs(t, err, ErrSummary)
	assert.Empty(t, result.Summary)
	assert.Equal(t, 3, result.Attempts)
	assert.Equal(t, StageFaithful, result.Stage)
}

func TestSummarizeStopsDeterministicErrors(t *testing.T) {
	t.Parallel()

	for _, code := range []int{401, 403, 400} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			t.Parallel()

			failure := ai.NewError("test", code, "invalid credentials or configuration")
			model := &scriptedModel{steps: []sampleStep{{err: failure}}}
			result, err := testSummarizer(t, model).Summarize(t.Context(), normalInput())
			require.ErrorIs(t, err, failure)
			assert.Equal(t, 1, result.Attempts)
			assert.NotContains(t, err.Error(), failure.Message)
		})
	}
}

func toolHistory() ai.Messages {
	return ai.Messages{
		ai.UserText(strings.Repeat("old context ", 400)),
		ai.AssistantText("Previous work finished."),
		ai.UserText("Inspect the next source file."),
		ai.Assistant(ai.ToolCallPart{ID: "c1", Name: "read", Args: ai.JSON(`{"path":"file.go"}`)}),
		ai.ToolResultText("c1", "read", strings.Repeat("large-unique-output ", 400)),
	}
}

func TestSummarizeOverflowUsesDifferentLossyInput(t *testing.T) {
	t.Parallel()

	overflow := ai.NewError("test", 400, "maximum context length exceeded")
	overflow.Code = "context_length_exceeded"
	model := &scriptedModel{steps: []sampleStep{{err: overflow}, {response: goodResponse()}}}
	result, err := testSummarizer(t, model).Summarize(t.Context(), Input{Messages: toolHistory(), SeedTokens: 100})
	require.NoError(t, err)
	assert.Equal(t, StageLossy, result.Stage)
	require.Len(t, model.requests, 2)
	parts, err := ai.MessageParts(model.requests[1].Messages[1])
	require.NoError(t, err)
	require.Len(t, parts, 1)
	body, ok := parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Contains(t, body.Text, "body archived")
	assert.NotContains(t, body.Text, "large-unique-output")
}

func TestSummarizeFitsLargeTrailingToolResult(t *testing.T) {
	t.Parallel()

	model := &scriptedModel{steps: []sampleStep{{response: goodResponse()}}}
	s := testSummarizer(t, model)
	s.policy.ContextWindow = 3000
	s.policy.ReserveTokens = 500
	result, err := s.Summarize(t.Context(), Input{Messages: toolHistory(), SeedTokens: 100})
	require.NoError(t, err)
	assert.Equal(t, StageFitted, result.Stage)
	require.Len(t, model.requests, 1)
	assert.LessOrEqual(t, estimateMessages(model.requests[0].Messages), 2500)
}

func TestSummarizeRejectsPendingBeforeModelIO(t *testing.T) {
	t.Parallel()

	model := &scriptedModel{}
	input := normalInput()
	input.Messages = append(input.Messages, ai.Assistant(ai.ToolCallPart{ID: "c1", Name: "read", Args: ai.JSON(`{}`)}))
	_, err := testSummarizer(t, model).Summarize(t.Context(), input)
	require.ErrorIs(t, err, ErrPending)
	assert.Empty(t, model.requests)
}

func TestSummaryRequestDoesNotInheritInteractiveOutputContract(t *testing.T) {
	t.Parallel()

	model := &scriptedModel{steps: []sampleStep{{response: goodResponse()}}}
	s := testSummarizer(t, model)
	s.apply = func(request *ai.Request) {
		request.MaxTokens = ai.Ptr(2048)
		request.Tools = []ai.Tool{{Name: "shell"}}
		request.ToolChoice = ai.ToolChoice{Mode: ai.ToolChoiceRequired}
		request.ResponseFormat = &ai.ResponseFormat{Name: "child_final"}
		request.Stop = []string{"</summary>"}
	}
	_, err := s.Summarize(t.Context(), normalInput())
	require.NoError(t, err)

	request := model.requests[0]
	assert.Empty(t, request.Tools)
	assert.Empty(t, request.ToolChoice.Mode)
	assert.Nil(t, request.ResponseFormat)
	assert.Empty(t, request.Stop)
	require.NotNil(t, request.MaxTokens)
	assert.Equal(t, 2048, *request.MaxTokens)
}

func TestSummarizeDeadlineAndRetryCancellation(t *testing.T) {
	t.Parallel()
	t.Run("deadline", func(t *testing.T) {
		t.Parallel()

		model := &scriptedModel{generate: func(ctx context.Context, _ ai.Request) (*ai.Response, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		s := testSummarizer(t, model)
		s.policy.Timeout = 10 * time.Millisecond
		_, err := s.Summarize(t.Context(), normalInput())
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})
	t.Run("retry_cancel", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		model := &scriptedModel{steps: []sampleStep{{response: nil}}}
		s := testSummarizer(t, model)
		s.wait = func(ctx context.Context, _ time.Duration) error {
			cancel()
			return ctx.Err()
		}
		result, err := s.Summarize(ctx, normalInput())
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, 1, result.Attempts)
	})
}

func TestContextOverflowRequiresProviderEvidence(t *testing.T) {
	t.Parallel()
	assert.False(t, IsContextOverflow(errors.New("maximum context length exceeded")))
	assert.False(t, IsContextOverflow(ai.NewError("test", 429, "maximum context length exceeded")))
	assert.True(t, IsContextOverflow(ai.NewError("test", 400, "maximum context length exceeded")))
}

func TestPolicyUsesEarlierThreshold(t *testing.T) {
	t.Parallel()

	p := Policy{ContextWindow: 10000, ReserveTokens: 1000}
	threshold, err := p.Threshold()
	require.NoError(t, err)
	assert.Equal(t, 8500, threshold)
	assert.False(t, p.ShouldCompact(8499))
	assert.True(t, p.ShouldCompact(8500))
	p.ReserveTokens = 3000
	threshold, err = p.Threshold()
	require.NoError(t, err)
	assert.Equal(t, 7000, threshold)
}
