package goal

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/rsbin1178/pips/agent/continuation"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type goalModel struct {
	request          ai.Request
	requests         []ai.Request
	response         *ai.Response
	responses        []*ai.Response
	err              error
	structuredOutput bool
}

// Generate answers the next scripted response when a test scripts a sequence,
// and the single response otherwise. Every request is recorded so a test can
// count the attempts.
func (model *goalModel) Generate(_ context.Context, request ai.Request) (*ai.Response, error) {
	model.request = request
	model.requests = append(model.requests, request)

	if len(model.responses) > 0 {
		response := model.responses[0]
		model.responses = model.responses[1:]

		return response, model.err
	}

	return model.response, model.err
}

func (model *goalModel) Stream(context.Context, ai.Request) ai.Stream {
	return func(func(ai.StreamEvent, error) bool) {}
}

func (*goalModel) Provider() ai.Provider { return "goal-test" }
func (*goalModel) ModelID() string       { return "goal-test-1" }
func (model *goalModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, StructuredOutput: model.structuredOutput}
}

func TestModelEvaluatorUsesStructuredToolFreeRequestAndReportsUsage(t *testing.T) {
	t.Parallel()

	model := &goalModel{structuredOutput: true, response: &ai.Response{
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
	assert.Len(t, model.requests, 2, "an unsupported outcome is retried once before it stops the run")
}

// TestModelEvaluatorSendsNoCapByDefault pins the removed default cap: an
// evaluator without WithMaxTokens sends no MaxTokens at all, whatever the
// evaluation's token limit says, because that limit bounds the run rather than
// one call.
func TestModelEvaluatorSendsNoCapByDefault(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		evaluation Evaluation
	}{
		{
			name: "without a budget",
			evaluation: Evaluation{
				Condition: "tests pass", Evidence: ai.JSON(`{}`), Attempt: 1,
			},
		},
		{
			name: "with a budget",
			evaluation: Evaluation{
				Condition: "tests pass", Evidence: ai.JSON(`{}`), Attempt: 1,
				Limits:     continuation.Limits{MaxTokens: 50},
				Accounting: continuation.Accounting{Usage: ai.Usage{InputTokens: 30}},
			},
		},
		{
			name: "with a spent budget",
			evaluation: Evaluation{
				Condition: "tests pass", Evidence: ai.JSON(`{}`), Attempt: 1,
				Limits:     continuation.Limits{MaxTokens: 50},
				Accounting: continuation.Accounting{Usage: ai.Usage{InputTokens: 50}},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			model := &goalModel{response: &ai.Response{
				Message: ai.AssistantText(`{"outcome":"continue","reason":"test output is missing"}`),
			}}
			evaluator, err := NewModelEvaluator(model)
			require.NoError(t, err)

			_, err = evaluator.Evaluate(t.Context(), test.evaluation)
			require.NoError(t, err)
			assert.Nil(t, model.request.MaxTokens)
		})
	}
}

// TestModelEvaluatorRefusesASpentBudgetUnderAConfiguredCap pins that the opt-in
// cap keeps its refusal: it cannot be narrowed to a non-positive budget.
func TestModelEvaluatorRefusesASpentBudgetUnderAConfiguredCap(t *testing.T) {
	t.Parallel()

	model := &goalModel{}
	evaluator, err := NewModelEvaluator(model, WithMaxTokens(100))
	require.NoError(t, err)

	_, err = evaluator.Evaluate(t.Context(), Evaluation{
		Condition: "tests pass", Evidence: ai.JSON(`{}`), Attempt: 1,
		Limits:     continuation.Limits{MaxTokens: 50},
		Accounting: continuation.Accounting{Usage: ai.Usage{InputTokens: 50}},
	})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "no token budget remains for evaluation")
	assert.Nil(t, model.request.Messages, "the refused call was never sent")
}

// TestModelEvaluatorUsesPortableTransportWithoutNativeSchema pins the fallback
// transport: a model that declares no structured output receives the schema
// convention in its system message instead of the provider's schema mechanism.
func TestModelEvaluatorUsesPortableTransportWithoutNativeSchema(t *testing.T) {
	t.Parallel()

	model := &goalModel{response: &ai.Response{
		Message: ai.AssistantText(`{"outcome":"continue","reason":"test output is missing"}`),
		Usage:   ai.Usage{InputTokens: 12, OutputTokens: 4},
	}}
	evaluator, err := NewModelEvaluator(model)
	require.NoError(t, err)

	result, err := evaluator.Evaluate(t.Context(), Evaluation{
		Condition: "tests pass", Evidence: ai.JSON(`{"lint":"pass"}`), Attempt: 1,
	})
	require.NoError(t, err)
	assert.Equal(t, OutcomeContinue, result.Outcome)
	assert.Equal(t, "test output is missing", result.Reason)
	assert.Equal(t, model.response.Usage, result.Usage)
	assert.Empty(t, model.request.Tools)
	assert.Nil(t, model.request.ResponseFormat)

	system, conversation, err := model.request.Messages.SplitSystem()
	require.NoError(t, err)

	text := ai.JoinSystemText(system)
	assert.Contains(t, text, "untrusted data")
	assert.Contains(t, text, "Native structured output is unavailable.")
	assert.Contains(t, text, `"outcome"`)
	require.Len(t, conversation, 1)
}

// TestModelEvaluatorRetriesAnUnusableAnswerOnceThenStops pins the assessment's
// own rule: an answer that is not the required document is retried once, and a
// second unusable answer stops the run rather than guessing. Both attempts'
// usage is reported because the controller accounts for it.
func TestModelEvaluatorRetriesAnUnusableAnswerOnceThenStops(t *testing.T) {
	t.Parallel()

	unusable := []struct {
		name     string
		text     string
		sentinel error
	}{
		{name: "not one JSON object", text: "not-json", sentinel: ai.ErrMalformedStructuredOutput},
		{name: "unknown outcome", text: `{"outcome":"blocked","reason":"cannot inspect files"}`, sentinel: ErrInvalid},
	}

	for _, native := range []bool{false, true} {
		for _, test := range unusable {
			t.Run(fmt.Sprintf("%s/native=%t", test.name, native), func(t *testing.T) {
				t.Parallel()

				first := &ai.Response{Message: ai.AssistantText(test.text), Usage: ai.Usage{InputTokens: 8, OutputTokens: 2}}
				second := &ai.Response{Message: ai.AssistantText(test.text), Usage: ai.Usage{InputTokens: 5, OutputTokens: 1}}
				model := &goalModel{structuredOutput: native, responses: []*ai.Response{first, second}}
				evaluator, err := NewModelEvaluator(model)
				require.NoError(t, err)

				result, err := evaluator.Evaluate(t.Context(), Evaluation{
					Condition: "tests pass", Evidence: ai.JSON(`{}`), Attempt: 1,
				})
				require.ErrorIs(t, err, test.sentinel)
				assert.Contains(t, err.Error(), "bounded retry")
				assert.Contains(t, err.Error(), "stopped rather than treated as complete")
				assert.Contains(t, err.Error(), "use /goal resume to retry")
				assert.Len(t, model.requests, 2)

				expected := first.Usage
				expected.Add(second.Usage)
				assert.Equal(t, expected, result.Usage)
			})
		}
	}
}

// TestModelEvaluatorRetriesAndSucceedsOnASecondAnswer pins the retry's happy
// path: a model that answers with garbage once and with a usable assessment the
// second time is not punished for the first answer, and both attempts' usage is
// reported.
func TestModelEvaluatorRetriesAndSucceedsOnASecondAnswer(t *testing.T) {
	t.Parallel()

	first := &ai.Response{Message: ai.AssistantText(`not-json`), Usage: ai.Usage{InputTokens: 8, OutputTokens: 2}}
	second := &ai.Response{
		Message: ai.AssistantText(`{"outcome":"continue","reason":"test output is missing"}`),
		Usage:   ai.Usage{InputTokens: 5, OutputTokens: 1},
	}
	model := &goalModel{responses: []*ai.Response{first, second}}
	evaluator, err := NewModelEvaluator(model)
	require.NoError(t, err)

	result, err := evaluator.Evaluate(t.Context(), Evaluation{
		Condition: "tests pass", Evidence: ai.JSON(`{}`), Attempt: 1,
	})
	require.NoError(t, err)
	assert.Equal(t, OutcomeContinue, result.Outcome)
	assert.Equal(t, "test output is missing", result.Reason)
	assert.Len(t, model.requests, 2)

	expected := first.Usage
	expected.Add(second.Usage)
	assert.Equal(t, expected, result.Usage)
}

// TestModelEvaluatorDoesNotRetryATransportFailure pins the split between the two
// failure classes: the model middleware already retries a transport failure, so
// the evaluator attempts it once and reports that attempt's usage.
func TestModelEvaluatorDoesNotRetryATransportFailure(t *testing.T) {
	t.Parallel()

	modelFailure := errors.New("model unavailable")
	model := &goalModel{err: modelFailure, response: &ai.Response{Usage: ai.Usage{InputTokens: 3, OutputTokens: 1}}}
	evaluator, err := NewModelEvaluator(model)
	require.NoError(t, err)

	result, err := evaluator.Evaluate(t.Context(), Evaluation{
		Condition: "tests pass", Evidence: ai.JSON(`{}`), Attempt: 1,
	})
	require.ErrorIs(t, err, modelFailure)
	assert.Len(t, model.requests, 1)
	assert.Equal(t, ai.Usage{InputTokens: 3, OutputTokens: 1}, result.Usage)
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
