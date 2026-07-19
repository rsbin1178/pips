package goal

import (
	"context"
	"fmt"
	"math"
	"slices"

	"github.com/rsbin/pips/agent/continuation"
	"github.com/rsbin/pips/ai"
)

// Controller maps evidence evaluations onto continuation lifecycle actions.
type Controller struct {
	evaluator Evaluator
}

// NewController constructs a Goal Controller.
func NewController(evaluator Evaluator) (*Controller, error) {
	if evaluator == nil {
		return nil, fmt.Errorf("%w: nil evaluator", ErrInvalid)
	}

	return &Controller{evaluator: evaluator}, nil
}

// Decide implements continuation.Controller.
func (controller *Controller) Decide(
	ctx context.Context,
	request continuation.DecisionRequest,
) (continuation.Decision, error) {
	state, err := DecodeState(request.ControllerState)
	if err != nil {
		return continuation.Decision{}, err
	}

	result, err := controller.evaluator.Evaluate(ctx, Evaluation{
		Condition: state.Condition, Evidence: slices.Clone(request.Work.Value),
		Attempt: request.Attempt, Activation: cloneActivation(request.Activation),
		Limits: request.Limits, Accounting: request.Accounting,
		Previous: cloneRecord(state.Last),
	})
	if err != nil {
		return continuation.Decision{}, fmt.Errorf("goal: evaluate condition: %w", err)
	}

	if err := validateResult(result); err != nil {
		return continuation.Decision{}, err
	}

	if state.Evaluations == math.MaxInt {
		return continuation.Decision{}, fmt.Errorf("%w: evaluation count overflow", ErrInvalid)
	}

	state.Evaluations++
	state.Last = &EvaluationRecord{Outcome: result.Outcome, Reason: result.Reason}

	stateData, err := encodeJSON("controller state", state)
	if err != nil {
		return continuation.Decision{}, err
	}

	decision := continuation.Decision{
		Reason: result.Reason, State: stateData, Usage: result.Usage,
	}

	switch result.Outcome {
	case OutcomeContinue:
		decision.Action = continuation.ActionContinue
		decision.NextInput, err = encodeWorkInput(state, result)
	case OutcomeComplete:
		decision.Action = continuation.ActionComplete
		decision.Output = slices.Clone(result.Output)

		if decision.Output == nil {
			decision.Output, err = encodeSummary(state)
		}
	case OutcomeBlocked:
		decision.Action = continuation.ActionBlock
		decision.NextInput, err = encodeWorkInput(state, result)
		decision.Block = cloneBlock(result.Block)

		if err == nil && decision.Block == nil {
			var data ai.JSON

			data, err = encodeSummary(state)
			decision.Block = &continuation.Block{Kind: policyKind, Data: data}
		}
	}

	if err != nil {
		return continuation.Decision{}, err
	}

	return decision, nil
}

func encodeWorkInput(state State, result EvaluationResult) (ai.JSON, error) {
	return encodeInput(WorkInput{
		Kind: policyKind, Condition: state.Condition, Evaluation: state.Evaluations,
		Reason: result.Reason, Feedback: slices.Clone(result.Feedback),
	})
}

type summary struct {
	Condition   string  `json:"condition"`
	Evaluations int     `json:"evaluations"`
	Outcome     Outcome `json:"outcome"`
	Reason      string  `json:"reason"`
}

func encodeSummary(state State) (ai.JSON, error) {
	return encodeJSON("summary", summary{
		Condition: state.Condition, Evaluations: state.Evaluations,
		Outcome: state.Last.Outcome, Reason: state.Last.Reason,
	})
}

func cloneRecord(record *EvaluationRecord) *EvaluationRecord {
	if record == nil {
		return nil
	}

	clone := *record

	return &clone
}

func cloneBlock(block *continuation.Block) *continuation.Block {
	if block == nil {
		return nil
	}

	clone := *block
	clone.Data = slices.Clone(block.Data)

	return &clone
}

var _ continuation.Controller = (*Controller)(nil)
