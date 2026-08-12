package loop

import (
	"context"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/rsbin1178/pips/agent/continuation"
	"github.com/rsbin1178/pips/ai"
)

// ControllerOption configures a Loop Controller.
type ControllerOption func(*controllerConfig) error

type controllerConfig struct {
	clock continuation.Clock
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// WithClock replaces the Decision-time clock.
func WithClock(clock continuation.Clock) ControllerOption {
	return func(config *controllerConfig) error {
		if clock == nil {
			return fmt.Errorf("%w: nil clock", ErrInvalid)
		}

		config.clock = clock

		return nil
	}
}

// Controller maps activation plans onto durable continuation waits.
type Controller struct {
	planner Planner
	clock   continuation.Clock
}

// NewController constructs a Loop Controller.
func NewController(planner Planner, options ...ControllerOption) (*Controller, error) {
	if planner == nil {
		return nil, fmt.Errorf("%w: nil planner", ErrInvalid)
	}

	config := controllerConfig{clock: systemClock{}}

	for _, option := range options {
		if option == nil {
			return nil, fmt.Errorf("%w: nil controller option", ErrInvalid)
		}

		if err := option(&config); err != nil {
			return nil, err
		}
	}

	return &Controller{planner: planner, clock: config.clock}, nil
}

// Every constructs a fixed-interval Loop Controller.
func Every(interval time.Duration, options ...ControllerOption) (*Controller, error) {
	if interval <= 0 {
		return nil, fmt.Errorf("%w: interval must be positive", ErrInvalid)
	}

	return NewController(fixedPlanner{interval: interval}, options...)
}

type fixedPlanner struct {
	interval time.Duration
}

func (planner fixedPlanner) Plan(context.Context, PlanRequest) (Plan, error) {
	return Plan{After: planner.interval, Reason: "waiting for fixed interval"}, nil
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

	if state.Iterations == math.MaxInt {
		return continuation.Decision{}, fmt.Errorf("%w: iteration count overflow", ErrInvalid)
	}

	iteration := state.Iterations + 1

	plan, err := controller.planner.Plan(ctx, PlanRequest{
		Iteration: iteration, WorkInput: slices.Clone(state.WorkInput),
		Evidence: slices.Clone(request.Work.Value), PlannerState: slices.Clone(state.PlannerState),
		Attempt: request.Attempt, Activation: cloneActivation(request.Activation),
		Limits: request.Limits, Accounting: request.Accounting, Previous: cloneRecord(state.Last),
	})
	if err != nil {
		return continuation.Decision{}, fmt.Errorf("loop: plan next activation: %w", err)
	}

	if err := validatePlan(plan); err != nil {
		return continuation.Decision{}, err
	}

	state.Iterations = iteration
	state.Last = &PlanRecord{
		Stop: plan.Stop, After: plan.After, SignalKey: plan.SignalKey, Reason: plan.Reason,
	}

	if plan.PlannerState != nil {
		state.PlannerState = slices.Clone(plan.PlannerState)
	}

	if plan.NextWorkInput != nil {
		state.WorkInput = slices.Clone(plan.NextWorkInput)
	}

	stateData, err := encodeJSON("controller state", state)
	if err != nil {
		return continuation.Decision{}, err
	}

	decision := continuation.Decision{
		Reason: plan.Reason, State: stateData, Usage: plan.Usage,
	}

	if plan.Stop {
		decision.Action = continuation.ActionComplete
		decision.Output = slices.Clone(plan.Output)

		if decision.Output == nil {
			decision.Output, err = encodeSummary(state)
		}

		return decision, err
	}

	wait, err := controller.waitCondition(plan)
	if err != nil {
		return continuation.Decision{}, err
	}

	decision.Action = continuation.ActionWait
	decision.NextInput = slices.Clone(state.WorkInput)
	decision.Wait = wait

	return decision, nil
}

func (controller *Controller) waitCondition(plan Plan) (*continuation.WaitCondition, error) {
	wait := &continuation.WaitCondition{}

	if plan.After > 0 {
		now := controller.clock.Now().UTC()
		notBefore := now.Add(plan.After)

		if notBefore.Year() < 0 || notBefore.Year() > 9999 {
			return nil, fmt.Errorf("%w: next activation time is outside JSON range", ErrInvalid)
		}

		wait.NotBefore = &notBefore
	}

	if plan.SignalKey != "" {
		wait.Signal = &continuation.SignalSpec{Key: plan.SignalKey}
	}

	return wait, nil
}

type summary struct {
	Iterations int    `json:"iterations"`
	Reason     string `json:"reason"`
}

func encodeSummary(state State) (ai.JSON, error) {
	return encodeJSON("summary", summary{
		Iterations: state.Iterations, Reason: state.Last.Reason,
	})
}

var (
	_ continuation.Controller = (*Controller)(nil)
	_ Planner                 = fixedPlanner{}
)
