package goal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/rsbin1178/pips/agent/continuation"
	"github.com/rsbin1178/pips/ai"
)

const (
	stateVersion   = 1
	maxJSONBytes   = 256 << 10
	maxReasonBytes = 4096
	policyKind     = "goal"

	// MaxConditionChars is the maximum number of Unicode characters in a Goal
	// condition.
	MaxConditionChars = 4000
)

// Goal validation errors.
var (
	ErrInvalid  = errors.New("goal: invalid value")
	ErrTooLarge = errors.New("goal: value too large")
)

// Outcome is an Evaluator's completion decision.
type Outcome string

// Goal evaluation outcomes.
const (
	OutcomeContinue Outcome = "continue"
	OutcomeComplete Outcome = "complete"
	OutcomeBlocked  Outcome = "blocked"
)

// EvaluationRecord is the bounded durable projection of one evaluation.
type EvaluationRecord struct {
	Outcome Outcome `json:"outcome"`
	Reason  string  `json:"reason"`
}

// State is the versioned Goal projection stored as continuation ControllerState.
type State struct {
	Version     int               `json:"version"`
	Condition   string            `json:"condition"`
	Evaluations int               `json:"evaluations"`
	Last        *EvaluationRecord `json:"last,omitempty"`
}

// WorkInput is the stable Goal envelope supplied to one Worker invocation.
type WorkInput struct {
	Kind       string  `json:"kind"`
	Condition  string  `json:"condition"`
	Evaluation int     `json:"evaluation"`
	Reason     string  `json:"reason,omitempty"`
	Feedback   ai.JSON `json:"feedback,omitempty"`
}

// Setup contains the two payloads required to create a Goal
// continuation execution.
type Setup struct {
	ControllerState ai.JSON
	WorkInput       ai.JSON
}

// Evaluation is the immutable input to an Evaluator.
type Evaluation struct {
	Condition  string
	Evidence   ai.JSON
	Attempt    int
	Activation *continuation.Activation
	Limits     continuation.Limits
	Accounting continuation.Accounting
	Previous   *EvaluationRecord
}

// EvaluationResult controls the Goal action after one Work result.
type EvaluationResult struct {
	Outcome  Outcome
	Reason   string
	Feedback ai.JSON
	Output   ai.JSON
	Block    *continuation.Block
	Usage    ai.Usage
}

// Evaluator decides whether durable Work evidence satisfies a Goal condition.
type Evaluator interface {
	Evaluate(context.Context, Evaluation) (EvaluationResult, error)
}

// EvaluatorFunc adapts a function to Evaluator.
type EvaluatorFunc func(context.Context, Evaluation) (EvaluationResult, error)

// Evaluate implements Evaluator.
func (function EvaluatorFunc) Evaluate(
	ctx context.Context,
	evaluation Evaluation,
) (EvaluationResult, error) {
	return function(ctx, evaluation)
}

// Prepare validates a condition and creates its initial durable state and
// Worker input.
func Prepare(condition string) (Setup, error) {
	condition, err := validateCondition(condition)
	if err != nil {
		return Setup{}, err
	}

	state, err := encodeJSON("controller state", State{
		Version: stateVersion, Condition: condition,
	})
	if err != nil {
		return Setup{}, err
	}

	input, err := encodeInput(WorkInput{Kind: policyKind, Condition: condition})
	if err != nil {
		return Setup{}, err
	}

	return Setup{ControllerState: state, WorkInput: input}, nil
}

// DecodeState validates and decodes Goal ControllerState.
func DecodeState(data ai.JSON) (State, error) {
	if err := validateJSON("controller state", data); err != nil {
		return State{}, err
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("%w: decode controller state: %w", ErrInvalid, err)
	}

	if state.Version != stateVersion {
		return State{}, fmt.Errorf("%w: unsupported state version %d", ErrInvalid, state.Version)
	}

	condition, err := validateCondition(state.Condition)
	if err != nil {
		return State{}, err
	}

	state.Condition = condition

	if state.Evaluations < 0 {
		return State{}, fmt.Errorf("%w: negative evaluation count", ErrInvalid)
	}

	if state.Last == nil && state.Evaluations != 0 || state.Last != nil && state.Evaluations == 0 {
		return State{}, fmt.Errorf("%w: evaluation history does not match count", ErrInvalid)
	}

	if state.Last != nil {
		if err := validateRecord(*state.Last); err != nil {
			return State{}, err
		}

		last := *state.Last
		state.Last = &last
	}

	return state, nil
}

// DecodeWorkInput validates and decodes a Goal Worker input envelope.
func DecodeWorkInput(data ai.JSON) (WorkInput, error) {
	if err := validateJSON("input", data); err != nil {
		return WorkInput{}, err
	}

	var input WorkInput
	if err := json.Unmarshal(data, &input); err != nil {
		return WorkInput{}, fmt.Errorf("%w: decode input: %w", ErrInvalid, err)
	}

	if input.Kind != policyKind {
		return WorkInput{}, fmt.Errorf("%w: input kind must be goal", ErrInvalid)
	}

	condition, err := validateCondition(input.Condition)
	if err != nil {
		return WorkInput{}, err
	}

	input.Condition = condition

	if input.Evaluation < 0 {
		return WorkInput{}, fmt.Errorf("%w: negative input evaluation", ErrInvalid)
	}

	if err := validateReason(input.Reason, false); err != nil {
		return WorkInput{}, err
	}

	if err := validateJSON("input feedback", input.Feedback); err != nil {
		return WorkInput{}, err
	}

	input.Feedback = slices.Clone(input.Feedback)

	return input, nil
}

func validateCondition(condition string) (string, error) {
	condition = strings.TrimSpace(condition)
	if condition == "" {
		return "", fmt.Errorf("%w: empty condition", ErrInvalid)
	}

	if utf8.RuneCountInString(condition) > MaxConditionChars {
		return "", fmt.Errorf("%w: condition exceeds %d characters", ErrTooLarge, MaxConditionChars)
	}

	return condition, nil
}

func validateRecord(record EvaluationRecord) error {
	if !validOutcome(record.Outcome) {
		return fmt.Errorf("%w: invalid outcome %q", ErrInvalid, record.Outcome)
	}

	return validateReason(record.Reason, true)
}

func validateResult(result EvaluationResult) error {
	if err := validateResultFields(result); err != nil {
		return err
	}

	return validateResultOutcome(result)
}

func validateResultFields(result EvaluationResult) error {
	if !validOutcome(result.Outcome) {
		return fmt.Errorf("%w: invalid outcome %q", ErrInvalid, result.Outcome)
	}

	if err := validateReason(result.Reason, true); err != nil {
		return err
	}

	if !validUsage(result.Usage) {
		return fmt.Errorf("%w: negative evaluator usage", ErrInvalid)
	}

	if err := validateJSON("feedback", result.Feedback); err != nil {
		return err
	}

	if err := validateJSON("output", result.Output); err != nil {
		return err
	}

	return nil
}

func validateResultOutcome(result EvaluationResult) error {
	switch result.Outcome {
	case OutcomeContinue:
		if result.Block != nil || result.Output != nil {
			return fmt.Errorf("%w: continue cannot carry block or output", ErrInvalid)
		}
	case OutcomeComplete:
		if result.Block != nil || result.Feedback != nil {
			return fmt.Errorf("%w: complete cannot carry block or feedback", ErrInvalid)
		}
	case OutcomeBlocked:
		if result.Output != nil {
			return fmt.Errorf("%w: blocked cannot carry output", ErrInvalid)
		}

		if result.Block != nil {
			return validateBlock(result.Block)
		}
	}

	return nil
}

func validateBlock(block *continuation.Block) error {
	if strings.TrimSpace(block.Kind) == "" {
		return fmt.Errorf("%w: block requires a kind", ErrInvalid)
	}

	return validateJSON("block data", block.Data)
}

func validOutcome(outcome Outcome) bool {
	switch outcome {
	case OutcomeContinue, OutcomeComplete, OutcomeBlocked:
		return true
	default:
		return false
	}
}

func validateReason(reason string, required bool) error {
	if required && strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: reason is required", ErrInvalid)
	}

	if len(reason) > maxReasonBytes {
		return fmt.Errorf("%w: reason exceeds %d bytes", ErrTooLarge, maxReasonBytes)
	}

	return nil
}

func validateJSON(name string, value ai.JSON) error {
	if value == nil {
		return nil
	}

	if len(value) > maxJSONBytes {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrTooLarge, name, maxJSONBytes)
	}

	if !json.Valid(value) {
		return fmt.Errorf("%w: %s is not valid JSON", ErrInvalid, name)
	}

	return nil
}

func encodeJSON(name string, value any) (ai.JSON, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("goal: encode %s: %w", name, err)
	}

	if err := validateJSON(name, data); err != nil {
		return nil, err
	}

	return data, nil
}

func encodeInput(input WorkInput) (ai.JSON, error) {
	return encodeJSON("input", input)
}

func validUsage(usage ai.Usage) bool {
	return usage.InputTokens >= 0 && usage.OutputTokens >= 0 && usage.ReasoningTokens >= 0 &&
		usage.CachedInputTokens >= 0 && usage.CacheWriteTokens >= 0
}

func validAccounting(accounting continuation.Accounting) bool {
	return accounting.Attempts >= 0 && accounting.Turns >= 0 && accounting.ActiveDuration >= 0 &&
		validUsage(accounting.Usage)
}

func cloneActivation(activation *continuation.Activation) *continuation.Activation {
	if activation == nil {
		return nil
	}

	clone := *activation
	clone.Payload = slices.Clone(activation.Payload)

	return &clone
}
