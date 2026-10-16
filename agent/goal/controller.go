package goal

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/rsbin1178/pips/agent/continuation"
	"github.com/rsbin1178/pips/ai"
)

// Controller maps evidence evaluations onto continuation lifecycle actions.
type Controller struct {
	evaluator Evaluator
}

// blockerStreak is how many consecutive blocked evaluations must name the same
// blocker before the Goal is blocked on it. The reference counts the same three,
// so a single complaint keeps the round going instead of stopping the run.
const blockerStreak = 3

// blockedKind is the block kind a Goal blocked on a repeated blocker carries. It
// is deliberately not no_progress: the reference pauses this case as a
// verification pause whose status is blocked, and the coding side projects every
// other kind as blocked, so the Goal is reported as blocked on a user action
// rather than as paused on a lack of progress.
const blockedKind = "gate"

// blockedActionPrefix introduces the exact user action the reference's own block
// message names after the evidence.
const blockedActionPrefix = "\nNext user action: "

// blockedMessage is the reference's block message: the evidence the assessment
// gave, then the action it named. It becomes the Decision's reason, so it is
// bounded like one. An evaluation that names no action has nothing to hand the
// user, so the message is the evidence alone; the assessment always names one,
// because its document requires a next step.
func blockedMessage(result EvaluationResult) string {
	if strings.TrimSpace(result.NextStep) == "" {
		return result.Reason
	}

	return result.Reason + blockedActionPrefix + result.NextStep
}

// recordBlocker counts one evaluation in the state's keyed blocker streak, as the
// reference's own record_evaluator_blocker and reset_evaluator_blocker do: the
// same blocker increments, a different one restarts at one, and a decision that is
// not blocked ends the run. A fresh activation — the Goal's creation, a wake from
// a wait, or the resolution of a block — starts the run over too, which is where
// the reference's own resume lands: a Goal this streak blocked is StatusBlocked,
// so a user resume resolves the block.
func recordBlocker(state *State, result EvaluationResult, activation *continuation.Activation) {
	if result.Outcome != OutcomeBlocked {
		state.BlockerKey, state.BlockerStreak = "", 0

		return
	}

	if activation != nil || state.BlockerKey != result.BlockerKey {
		state.BlockerKey, state.BlockerStreak = result.BlockerKey, 1

		return
	}

	if state.BlockerStreak < math.MaxInt {
		state.BlockerStreak++
	}
}

// blockOnRepeatedBlocker maps a blocked evaluation onto the Decision. Below the
// streak the round continues with the evidence as its reason, exactly as the
// reference does; only a repeated declaration of the same blocker blocks the Goal,
// with the reference's own message.
func blockOnRepeatedBlocker(
	state State, result EvaluationResult, decision *continuation.Decision,
) error {
	nextInput, err := encodeWorkInput(state, result)
	if err != nil {
		return err
	}

	decision.NextInput = nextInput

	if state.BlockerStreak < blockerStreak {
		decision.Action = continuation.ActionContinue

		return nil
	}

	decision.Action = continuation.ActionBlock
	decision.Reason = blockedMessage(result)
	decision.Block = cloneBlock(result.Block)

	if decision.Block != nil {
		return nil
	}

	data, err := encodeSummary(state)
	if err != nil {
		return err
	}

	decision.Block = &continuation.Block{Kind: blockedKind, Data: data}

	return nil
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
	state.Last = &EvaluationRecord{
		Outcome: result.Outcome, Reason: result.Reason,
		NextStep: result.NextStep, BlockerKey: result.BlockerKey,
	}
	recordBlocker(&state, result, request.Activation)

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
		err = blockOnRepeatedBlocker(state, result, &decision)
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
