package workflow

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

func (r *nodeRuntime) runChild(
	ctx context.Context,
	plan *Plan,
	inputs map[string]Value,
	frame ScopeFrame,
) (map[string]Value, error) {
	var checkpoint *executionCheckpoint
	if r != nil && r.resume != nil {
		checkpoint = r.resume.Child
	}

	return r.runChildWithCheckpoint(ctx, plan, inputs, frame, checkpoint)
}

func (r *nodeRuntime) runChildWithCheckpoint(
	ctx context.Context,
	plan *Plan,
	inputs map[string]Value,
	frame ScopeFrame,
	checkpoint *executionCheckpoint,
) (map[string]Value, error) {
	if r == nil || r.execution == nil || plan == nil {
		return nil, errors.New("workflow child runtime is unavailable")
	}

	if err := validateScopeFrame(frame); err != nil {
		return nil, err
	}

	scope := append(slices.Clone(r.execution.scope), frame)

	var execution *execution
	if checkpoint != nil {
		execution = restoreExecution(
			r.execution.runner,
			plan,
			r.execution.state,
			checkpoint,
			nil,
		)
		execution.resumed = true
	} else {
		normalizedInputs, err := normalizeWorkflowInputs(plan.definition.Inputs, inputs, nil)
		if err != nil {
			return nil, fmt.Errorf("child workflow inputs: %w", err)
		}

		startedAt := r.execution.runner.clock().UTC()
		execution = newExecution(
			r.execution.runner,
			plan,
			r.execution.state,
			startedAt,
			normalizedInputs,
			scope,
			nil,
		)
	}

	result, err := execution.run(ctx)
	if err != nil {
		return nil, err
	}

	return cloneValues(result.Outputs), nil
}

func (r *nodeRuntime) runLoopChild(
	ctx context.Context,
	plan *Plan,
	inputs map[string]Value,
	frame ScopeFrame,
	state *loopIterationState,
	checkpoint *executionCheckpoint,
) (map[string]Value, error) {
	if r == nil || r.execution == nil || plan == nil || state == nil {
		return nil, errors.New("workflow loop child runtime is unavailable")
	}

	if err := validateScopeFrame(frame); err != nil {
		return nil, err
	}

	scope := append(slices.Clone(r.execution.scope), frame)

	var execution *execution
	if checkpoint != nil {
		execution = restoreExecution(
			r.execution.runner,
			plan,
			r.execution.state,
			checkpoint,
			state,
		)
		execution.resumed = true
	} else {
		normalizedInputs, err := normalizeWorkflowInputs(plan.definition.Inputs, inputs, nil)
		if err != nil {
			return nil, fmt.Errorf("loop body inputs: %w", err)
		}

		startedAt := r.execution.runner.clock().UTC()
		execution = newExecution(
			r.execution.runner,
			plan,
			r.execution.state,
			startedAt,
			normalizedInputs,
			scope,
			state,
		)
	}

	result, err := execution.run(ctx)
	if err != nil {
		return nil, err
	}

	return cloneValues(result.Outputs), nil
}

func (r *nodeRuntime) loopState() *loopIterationState {
	if r == nil || r.execution == nil {
		return nil
	}

	return r.execution.loop
}

func (r *nodeRuntime) breakLoop() error {
	state := r.loopState()
	if state == nil {
		return errors.New("workflow loop state is unavailable")
	}

	return state.requestBreak()
}

func (r *nodeRuntime) setLoopVariables(updates map[string]Value) error {
	state := r.loopState()
	if state == nil {
		return errors.New("workflow loop state is unavailable")
	}

	return state.assign(updates)
}

func validateScopeFrame(frame ScopeFrame) error {
	if !validIdentifier(string(frame.NodeID)) {
		return errors.New("workflow child scope has invalid node id")
	}

	switch frame.Kind {
	case ScopeSubWorkflow:
		if frame.Index != -1 {
			return errors.New("workflow sub-workflow scope has invalid index")
		}
	case ScopeBatchItem, ScopeLoopIteration:
		if frame.Index < 0 {
			return errors.New("workflow indexed child scope has invalid index")
		}
	default:
		return errors.New("workflow child scope has invalid kind")
	}

	return nil
}
