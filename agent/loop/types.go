package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rsbin1178/pips/agent/continuation"
	"github.com/rsbin1178/pips/ai"
)

const (
	stateVersion      = 1
	maxJSONBytes      = 256 << 10
	maxReasonBytes    = 4096
	maxSignalKeyBytes = 256
)

// Loop validation errors.
var (
	ErrInvalid  = errors.New("loop: invalid value")
	ErrTooLarge = errors.New("loop: value too large")
)

// PlanRecord is the bounded durable projection of one activation plan.
type PlanRecord struct {
	Stop      bool          `json:"stop"`
	After     time.Duration `json:"after,omitempty"`
	SignalKey string        `json:"signal_key,omitempty"`
	Reason    string        `json:"reason"`
}

// State is the versioned Loop projection stored as continuation ControllerState.
type State struct {
	Version      int         `json:"version"`
	Iterations   int         `json:"iterations"`
	WorkInput    ai.JSON     `json:"input,omitempty"`
	PlannerState ai.JSON     `json:"planner_state,omitempty"`
	Last         *PlanRecord `json:"last,omitempty"`
}

// Setup contains the durable state and first Worker input for a Loop
// execution.
type Setup struct {
	ControllerState ai.JSON
	WorkInput       ai.JSON
}

// PlanRequest is the immutable input for planning after one Work iteration.
type PlanRequest struct {
	Iteration    int
	WorkInput    ai.JSON
	Evidence     ai.JSON
	PlannerState ai.JSON
	Attempt      int
	Activation   *continuation.Activation
	Limits       continuation.Limits
	Accounting   continuation.Accounting
	Previous     *PlanRecord
}

// Plan selects the next activation or stops the Loop.
type Plan struct {
	Stop          bool
	After         time.Duration
	SignalKey     string
	Reason        string
	PlannerState  ai.JSON
	NextWorkInput ai.JSON
	Output        ai.JSON
	Usage         ai.Usage
}

// Planner selects when another Loop iteration should become runnable.
type Planner interface {
	Plan(context.Context, PlanRequest) (Plan, error)
}

// PlannerFunc adapts a function to Planner.
type PlannerFunc func(context.Context, PlanRequest) (Plan, error)

// Plan implements Planner.
func (function PlannerFunc) Plan(ctx context.Context, request PlanRequest) (Plan, error) {
	return function(ctx, request)
}

// Prepare validates and persists the stable input for a new Loop.
func Prepare(input ai.JSON) (Setup, error) {
	if err := validateJSON("input", input); err != nil {
		return Setup{}, err
	}

	state, err := encodeJSON("controller state", State{
		Version: stateVersion, WorkInput: slices.Clone(input),
	})
	if err != nil {
		return Setup{}, err
	}

	return Setup{
		ControllerState: state,
		WorkInput:       slices.Clone(input),
	}, nil
}

// DecodeState validates and decodes Loop ControllerState.
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

	if state.Iterations < 0 {
		return State{}, fmt.Errorf("%w: negative iteration count", ErrInvalid)
	}

	if state.Last == nil && state.Iterations != 0 || state.Last != nil && state.Iterations == 0 {
		return State{}, fmt.Errorf("%w: plan history does not match iteration count", ErrInvalid)
	}

	if err := validateJSON("input", state.WorkInput); err != nil {
		return State{}, err
	}

	if err := validateJSON("planner state", state.PlannerState); err != nil {
		return State{}, err
	}

	if state.Last != nil {
		if err := validateRecord(*state.Last); err != nil {
			return State{}, err
		}

		last := *state.Last
		state.Last = &last
	}

	state.WorkInput = slices.Clone(state.WorkInput)
	state.PlannerState = slices.Clone(state.PlannerState)

	return state, nil
}

func validatePlan(plan Plan) error {
	if err := validatePlanFields(plan); err != nil {
		return err
	}

	return validatePlanAction(plan)
}

func validatePlanFields(plan Plan) error {
	if err := validateReason(plan.Reason, true); err != nil {
		return err
	}

	if !validUsage(plan.Usage) {
		return fmt.Errorf("%w: negative planner usage", ErrInvalid)
	}

	for name, value := range map[string]ai.JSON{
		"planner state": plan.PlannerState,
		"next input":    plan.NextWorkInput,
		"output":        plan.Output,
	} {
		if err := validateJSON(name, value); err != nil {
			return err
		}
	}

	return validateSignalKey(plan.SignalKey)
}

func validatePlanAction(plan Plan) error {
	if plan.Stop {
		if plan.After != 0 || plan.SignalKey != "" || plan.NextWorkInput != nil {
			return fmt.Errorf("%w: stop cannot carry activation or next input", ErrInvalid)
		}

		return nil
	}

	if plan.Output != nil {
		return fmt.Errorf("%w: continuing plan cannot carry output", ErrInvalid)
	}

	if plan.After < 0 {
		return fmt.Errorf("%w: negative delay", ErrInvalid)
	}

	if plan.After == 0 && plan.SignalKey == "" {
		return fmt.Errorf("%w: plan requires a positive delay or signal", ErrInvalid)
	}

	return nil
}

func validateRecord(record PlanRecord) error {
	if err := validateReason(record.Reason, true); err != nil {
		return err
	}

	if err := validateSignalKey(record.SignalKey); err != nil {
		return err
	}

	return validatePlanAction(Plan{
		Stop: record.Stop, After: record.After, SignalKey: record.SignalKey,
		Reason: record.Reason,
	})
}

func validateSignalKey(key string) error {
	if len(key) > maxSignalKeyBytes {
		return fmt.Errorf("%w: signal key exceeds %d bytes", ErrTooLarge, maxSignalKeyBytes)
	}

	if key != "" && strings.TrimSpace(key) == "" {
		return fmt.Errorf("%w: empty signal key", ErrInvalid)
	}

	return nil
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
		return nil, fmt.Errorf("loop: encode %s: %w", name, err)
	}

	if err := validateJSON(name, data); err != nil {
		return nil, err
	}

	return data, nil
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

func cloneRecord(record *PlanRecord) *PlanRecord {
	if record == nil {
		return nil
	}

	clone := *record

	return &clone
}
