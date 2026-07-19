package goal

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/rsbin/pips/agent/continuation"
	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestControllerDecideMapsOutcomes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		result     EvaluationResult
		action     continuation.Action
		blockKind  string
		wantOutput string
	}{
		{
			name: "continue with feedback",
			result: EvaluationResult{
				Outcome: OutcomeContinue, Reason: "lint still fails",
				Feedback: ai.JSON(`{"command":"go vet ./..."}`),
				Usage:    ai.Usage{InputTokens: 2, OutputTokens: 1},
			},
			action: continuation.ActionContinue,
		},
		{
			name: "complete with default summary",
			result: EvaluationResult{
				Outcome: OutcomeComplete, Reason: "all checks pass",
			},
			action:     continuation.ActionComplete,
			wantOutput: `{"condition":"all tests pass","evaluations":1,"outcome":"complete","reason":"all checks pass"}`,
		},
		{
			name: "blocked with custom block",
			result: EvaluationResult{
				Outcome: OutcomeBlocked, Reason: "credentials required",
				Feedback: ai.JSON(`{"scope":"deploy"}`),
				Block:    &continuation.Block{Kind: "credentials", Data: ai.JSON(`{"provider":"ci"}`)},
			},
			action:    continuation.ActionBlock,
			blockKind: "credentials",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			initial, err := Prepare("all tests pass")
			require.NoError(t, err)

			controller, err := NewController(EvaluatorFunc(func(_ context.Context, evaluation Evaluation) (EvaluationResult, error) {
				assert.Equal(t, "all tests pass", evaluation.Condition)
				assert.JSONEq(t, `{"tests":"pending"}`, string(evaluation.Evidence))
				assert.Nil(t, evaluation.Previous)

				return test.result, nil
			}))
			require.NoError(t, err)

			decision, err := controller.Decide(t.Context(), continuation.DecisionRequest{
				Attempt: 1, Work: continuation.WorkResult{Value: ai.JSON(`{"tests":"pending"}`)},
				ControllerState: initial.ControllerState,
			})
			require.NoError(t, err)
			assert.Equal(t, test.action, decision.Action)
			assert.Equal(t, test.result.Reason, decision.Reason)
			assert.Equal(t, test.result.Usage, decision.Usage)

			state, err := DecodeState(decision.State)
			require.NoError(t, err)
			assert.Equal(t, 1, state.Evaluations)
			assert.Equal(t, test.result.Outcome, state.Last.Outcome)

			if test.action == continuation.ActionContinue {
				input, decodeErr := DecodeWorkInput(decision.NextInput)
				require.NoError(t, decodeErr)
				assert.Equal(t, 1, input.Evaluation)
				assert.Equal(t, test.result.Reason, input.Reason)
				assert.JSONEq(t, string(test.result.Feedback), string(input.Feedback))
			}

			if test.blockKind != "" {
				require.NotNil(t, decision.Block)
				assert.Equal(t, test.blockKind, decision.Block.Kind)
				assert.JSONEq(t, string(test.result.Block.Data), string(decision.Block.Data))

				input, decodeErr := DecodeWorkInput(decision.NextInput)
				require.NoError(t, decodeErr)
				assert.Equal(t, 1, input.Evaluation)
				assert.JSONEq(t, string(test.result.Feedback), string(input.Feedback))
			}

			if test.wantOutput != "" {
				assert.JSONEq(t, test.wantOutput, string(decision.Output))
			}
		})
	}
}

func TestControllerDefaultBlockCarriesAuditSummary(t *testing.T) {
	t.Parallel()

	initial, err := Prepare("deployment is healthy")
	require.NoError(t, err)
	controller, err := NewController(EvaluatorFunc(func(context.Context, Evaluation) (EvaluationResult, error) {
		return EvaluationResult{Outcome: OutcomeBlocked, Reason: "deployment API unavailable"}, nil
	}))
	require.NoError(t, err)

	decision, err := controller.Decide(t.Context(), continuation.DecisionRequest{
		Attempt: 1, Work: continuation.WorkResult{Value: ai.JSON(`{}`)},
		ControllerState: initial.ControllerState,
	})
	require.NoError(t, err)
	require.NotNil(t, decision.Block)
	assert.Equal(t, "goal", decision.Block.Kind)
	assert.JSONEq(t,
		`{"condition":"deployment is healthy","evaluations":1,"outcome":"blocked","reason":"deployment API unavailable"}`,
		string(decision.Block.Data),
	)

	input, err := DecodeWorkInput(decision.NextInput)
	require.NoError(t, err)
	assert.Equal(t, "deployment is healthy", input.Condition)
	assert.Equal(t, "deployment API unavailable", input.Reason)
}

func TestControllerFeedsEvaluationBackIntoContinuationWork(t *testing.T) {
	t.Parallel()

	store, err := continuation.NewMemoryStore()
	require.NoError(t, err)
	engine, err := continuation.New(store)
	require.NoError(t, err)
	initial, err := Prepare("all tests pass")
	require.NoError(t, err)

	var inputs []WorkInput

	worker := goalWorkerFunc(func(_ context.Context, request continuation.WorkRequest) (continuation.WorkResult, error) {
		input, decodeErr := DecodeWorkInput(request.Input)
		require.NoError(t, decodeErr)

		inputs = append(inputs, input)

		if request.Attempt == 1 {
			return continuation.WorkResult{
				Value: ai.JSON(`{"tests":"fail"}`), Progress: continuation.ProgressChanged,
			}, nil
		}

		return continuation.WorkResult{
			Value: ai.JSON(`{"tests":"pass"}`), Progress: continuation.ProgressChanged,
		}, nil
	})
	controller, err := NewController(EvaluatorFunc(func(_ context.Context, evaluation Evaluation) (EvaluationResult, error) {
		if string(evaluation.Evidence) == `{"tests":"pass"}` {
			return EvaluationResult{Outcome: OutcomeComplete, Reason: "passing evidence received"}, nil
		}

		return EvaluationResult{
			Outcome: OutcomeContinue, Reason: "tests still fail",
			Feedback: ai.JSON(`{"command":"go test ./..."}`),
		}, nil
	}))
	require.NoError(t, err)

	workerRef := continuation.HandlerRef{Kind: "goal-feedback-worker", Version: "v1"}
	controllerRef := continuation.HandlerRef{Kind: "goal", Version: "v1"}
	execution, err := engine.Create(t.Context(), continuation.CreateRequest{
		ID: "goal-feedback", Target: continuation.Target{Kind: "session", ID: "s1"},
		Worker: workerRef, Controller: controllerRef,
		ControllerState: initial.ControllerState, Input: initial.WorkInput,
	})
	require.NoError(t, err)

	result, err := engine.Drive(t.Context(), execution.ID, execution.Revision, continuation.Handlers{
		WorkerRef: workerRef, Worker: worker,
		ControllerRef: controllerRef, Controller: controller,
	}, continuation.DriveOptions{MaxAdvances: 2})
	require.NoError(t, err)
	assert.Equal(t, continuation.StatusCompleted, result.Execution.Status)
	assert.Equal(t, 2, result.Execution.Accounting.Attempts)
	require.Len(t, inputs, 2)
	assert.Zero(t, inputs[0].Evaluation)
	assert.Equal(t, 1, inputs[1].Evaluation)
	assert.Equal(t, "tests still fail", inputs[1].Reason)
	assert.JSONEq(t, `{"command":"go test ./..."}`, string(inputs[1].Feedback))

	state, err := DecodeState(result.Execution.ControllerState)
	require.NoError(t, err)
	assert.Equal(t, 2, state.Evaluations)
	assert.Equal(t, OutcomeComplete, state.Last.Outcome)
}

func TestControllerRejectsInvalidEvaluatorResults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		result EvaluationResult
		target error
	}{
		{name: "unknown outcome", result: EvaluationResult{Outcome: "unknown", Reason: "why"}, target: ErrInvalid},
		{name: "missing reason", result: EvaluationResult{Outcome: OutcomeContinue}, target: ErrInvalid},
		{name: "negative usage", result: EvaluationResult{Outcome: OutcomeComplete, Reason: "done", Usage: ai.Usage{OutputTokens: -1}}, target: ErrInvalid},
		{name: "invalid feedback", result: EvaluationResult{Outcome: OutcomeContinue, Reason: "more", Feedback: ai.JSON(`{`)}, target: ErrInvalid},
		{name: "continue output", result: EvaluationResult{Outcome: OutcomeContinue, Reason: "more", Output: ai.JSON(`{}`)}, target: ErrInvalid},
		{name: "complete feedback", result: EvaluationResult{Outcome: OutcomeComplete, Reason: "done", Feedback: ai.JSON(`{}`)}, target: ErrInvalid},
		{name: "blocked output", result: EvaluationResult{Outcome: OutcomeBlocked, Reason: "blocked", Output: ai.JSON(`{}`)}, target: ErrInvalid},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			initial, err := Prepare("done")
			require.NoError(t, err)
			controller, err := NewController(EvaluatorFunc(func(context.Context, Evaluation) (EvaluationResult, error) {
				return test.result, nil
			}))
			require.NoError(t, err)

			_, err = controller.Decide(t.Context(), continuation.DecisionRequest{
				Attempt: 1, Work: continuation.WorkResult{Value: ai.JSON(`{}`)},
				ControllerState: initial.ControllerState,
			})
			require.ErrorIs(t, err, test.target)
		})
	}
}

func TestControllerEvaluationErrorIsRetryableWithoutWorkReplay(t *testing.T) {
	t.Parallel()

	store, err := continuation.NewMemoryStore()
	require.NoError(t, err)
	engine, err := continuation.New(store)
	require.NoError(t, err)
	initial, err := Prepare("evidence is complete")
	require.NoError(t, err)

	var workCalls atomic.Int64

	worker := goalWorkerFunc(func(context.Context, continuation.WorkRequest) (continuation.WorkResult, error) {
		workCalls.Add(1)

		return continuation.WorkResult{
			Value: ai.JSON(`{"complete":true}`), Progress: continuation.ProgressChanged,
		}, nil
	})

	evaluatorFailure := errors.New("evaluator unavailable")

	var evaluationCalls atomic.Int64

	controller, err := NewController(EvaluatorFunc(func(context.Context, Evaluation) (EvaluationResult, error) {
		if evaluationCalls.Add(1) == 1 {
			return EvaluationResult{}, evaluatorFailure
		}

		return EvaluationResult{Outcome: OutcomeComplete, Reason: "evidence proves completion"}, nil
	}))
	require.NoError(t, err)

	workerRef := continuation.HandlerRef{Kind: "goal-test-worker", Version: "v1"}
	controllerRef := continuation.HandlerRef{Kind: "goal", Version: "v1"}
	execution, err := engine.Create(t.Context(), continuation.CreateRequest{
		ID: "goal-retry", Target: continuation.Target{Kind: "session", ID: "s1"},
		Worker: workerRef, Controller: controllerRef,
		ControllerState: initial.ControllerState, Input: initial.WorkInput,
	})
	require.NoError(t, err)

	handlers := continuation.Handlers{
		WorkerRef: workerRef, Worker: worker,
		ControllerRef: controllerRef, Controller: controller,
	}

	interrupted, err := engine.Advance(t.Context(), execution.ID, execution.Revision, handlers)
	require.ErrorIs(t, err, evaluatorFailure)
	require.Equal(t, continuation.StatusInterrupted, interrupted.Status)
	require.Equal(t, continuation.PhaseDecision, interrupted.Phase)
	attemptID := interrupted.CurrentAttempt.ID

	retry, err := engine.RetryDecision(t.Context(), interrupted.ID, interrupted.Revision, "retry evaluator")
	require.NoError(t, err)
	finished, err := engine.Advance(t.Context(), retry.ID, retry.Revision, handlers)
	require.NoError(t, err)
	assert.Equal(t, continuation.StatusCompleted, finished.Status)
	assert.Equal(t, attemptID, finished.LastAttempt.ID)
	assert.Equal(t, int64(1), workCalls.Load())
	assert.Equal(t, int64(2), evaluationCalls.Load())
}

type goalWorkerFunc func(context.Context, continuation.WorkRequest) (continuation.WorkResult, error)

func (function goalWorkerFunc) Run(
	ctx context.Context,
	request continuation.WorkRequest,
) (continuation.WorkResult, error) {
	return function(ctx, request)
}
