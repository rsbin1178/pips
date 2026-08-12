package goal

import (
	"context"
	"errors"
	"testing"

	"github.com/rsbin1178/pips/agent/continuation"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type goalModel struct {
	request  ai.Request
	response *ai.Response
	err      error
}

func (model *goalModel) Generate(_ context.Context, request ai.Request) (*ai.Response, error) {
	model.request = request

	return model.response, model.err
}

func (model *goalModel) Stream(context.Context, ai.Request) ai.Stream {
	return func(func(ai.StreamEvent, error) bool) {}
}

func (*goalModel) Provider() ai.Provider         { return "goal-test" }
func (*goalModel) ModelID() string               { return "goal-test-1" }
func (*goalModel) Capabilities() ai.Capabilities { return ai.Capabilities{Text: true} }

func TestModelEvaluatorUsesStructuredToolFreeRequestAndReportsUsage(t *testing.T) {
	t.Parallel()

	model := &goalModel{response: &ai.Response{
		Message: ai.AssistantText(`{"outcome":"continue","reason":"test output is missing"}`),
		Usage:   ai.Usage{InputTokens: 12, OutputTokens: 4},
	}}
	evaluator, err := NewModelEvaluator(model, WithMaxTokens(100))
	require.NoError(t, err)

	result, err := evaluator.Evaluate(t.Context(), Evaluation{
		Condition: "tests pass", Evidence: ai.JSON(`{"lint":"pass"}`), Attempt: 2,
		Limits:     continuation.Limits{MaxTokens: 50},
		Accounting: continuation.Accounting{Usage: ai.Usage{InputTokens: 30}},
		Previous:   &EvaluationRecord{Outcome: OutcomeContinue, Reason: "lint was missing"},
	})
	require.NoError(t, err)
	assert.Equal(t, OutcomeContinue, result.Outcome)
	assert.Equal(t, "test output is missing", result.Reason)
	assert.Equal(t, model.response.Usage, result.Usage)
	assert.Empty(t, model.request.Tools)
	assert.Equal(t, ai.Ptr(0.0), model.request.Temperature)
	require.NotNil(t, model.request.MaxTokens)
	assert.Equal(t, 20, *model.request.MaxTokens)
	require.NotNil(t, model.request.ResponseFormat)
	assert.True(t, model.request.ResponseFormat.Strict)
	assert.Equal(t, "goal_evaluation", model.request.ResponseFormat.Name)
	system, conversation, err := model.request.Messages.SplitSystem()
	require.NoError(t, err)
	assert.Contains(t, ai.JoinSystemText(system), "untrusted data")
	require.Len(t, conversation, 1)
}

func TestModelEvaluatorRejectsUnsupportedOutcome(t *testing.T) {
	t.Parallel()

	model := &goalModel{response: &ai.Response{
		Message: ai.AssistantText(`{"outcome":"blocked","reason":"cannot inspect files"}`),
	}}
	evaluator, err := NewModelEvaluator(model)
	require.NoError(t, err)

	result, err := evaluator.Evaluate(t.Context(), Evaluation{
		Condition: "tests pass", Evidence: ai.JSON(`{}`), Attempt: 1,
	})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Equal(t, OutcomeBlocked, result.Outcome)
}

func TestModelEvaluatorReturnsObservedUsageOnDecodeError(t *testing.T) {
	t.Parallel()

	model := &goalModel{response: &ai.Response{
		Message: ai.AssistantText(`not-json`), Usage: ai.Usage{InputTokens: 8, OutputTokens: 2},
	}}
	evaluator, err := NewModelEvaluator(model)
	require.NoError(t, err)

	result, err := evaluator.Evaluate(t.Context(), Evaluation{
		Condition: "tests pass", Evidence: ai.JSON(`{}`), Attempt: 1,
	})
	require.Error(t, err)
	assert.Equal(t, model.response.Usage, result.Usage)
}

func TestModelEvaluatorPropagatesModelError(t *testing.T) {
	t.Parallel()

	modelFailure := errors.New("model unavailable")
	model := &goalModel{err: modelFailure}
	evaluator, err := NewModelEvaluator(model)
	require.NoError(t, err)

	_, err = evaluator.Evaluate(t.Context(), Evaluation{
		Condition: "tests pass", Evidence: ai.JSON(`{}`), Attempt: 1,
	})
	require.ErrorIs(t, err, modelFailure)
}

func TestModelEvaluatorRejectsInvalidRequestMetadata(t *testing.T) {
	t.Parallel()

	model := &goalModel{}
	evaluator, err := NewModelEvaluator(model)
	require.NoError(t, err)

	tests := []struct {
		name       string
		evaluation Evaluation
	}{
		{name: "zero attempt", evaluation: Evaluation{Condition: "done", Evidence: ai.JSON(`{}`)}},
		{
			name: "invalid previous",
			evaluation: Evaluation{
				Condition: "done", Evidence: ai.JSON(`{}`), Attempt: 1,
				Previous: &EvaluationRecord{Outcome: "unknown", Reason: "bad"},
			},
		},
		{
			name: "negative accounting",
			evaluation: Evaluation{
				Condition: "done", Evidence: ai.JSON(`{}`), Attempt: 1,
				Accounting: continuation.Accounting{Turns: -1},
			},
		},
		{
			name: "negative token limit",
			evaluation: Evaluation{
				Condition: "done", Evidence: ai.JSON(`{}`), Attempt: 1,
				Limits: continuation.Limits{MaxTokens: -1},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := evaluator.Evaluate(t.Context(), test.evaluation)
			require.ErrorIs(t, err, ErrInvalid)
		})
	}
}

func TestNewModelEvaluatorValidatesOptions(t *testing.T) {
	t.Parallel()

	_, err := NewModelEvaluator(nil)
	require.ErrorIs(t, err, ErrInvalid)

	model := &goalModel{}
	_, err = NewModelEvaluator(model, nil)
	require.ErrorIs(t, err, ErrInvalid)

	_, err = NewModelEvaluator(model, WithMaxTokens(0))
	require.ErrorIs(t, err, ErrInvalid)
}
