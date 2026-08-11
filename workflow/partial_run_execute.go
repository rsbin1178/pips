package workflow

import (
	"context"
	"errors"
	"fmt"
)

// RunPartial executes only the root dependency slice needed to settle
// destination. Previous results and Pins are development data boundaries;
// registered Actions that do execute are real and may have side effects.
func (r *Runner) RunPartial(
	ctx context.Context,
	source *Plan,
	destination NodeID,
	input PartialRunInput,
) (PartialRunResult, error) {
	if r == nil || r.clock == nil || r.idSource == nil {
		return PartialRunResult{}, errors.New("workflow: nil or invalid runner")
	}

	plan, err := derivePartialRunPlan(source, destination)
	if err != nil {
		return PartialRunResult{}, fmt.Errorf("workflow: %w", err)
	}

	inputs, partial, err := normalizePartialRunInput(source, plan, input)
	if err != nil {
		return PartialRunResult{}, fmt.Errorf("%w: partial run input: %w", ErrRun, err)
	}

	if err := ctx.Err(); err != nil {
		return PartialRunResult{}, err
	}

	startedAt := r.clock().UTC()

	runID, err := r.idSource(startedAt)
	if err != nil {
		return PartialRunResult{}, fmt.Errorf("%w: create run id: %w", ErrRun, err)
	}

	if runID == "" {
		return PartialRunResult{}, fmt.Errorf("%w: create run id: empty id", ErrRun)
	}

	state := newRunState(runID, source.definition.Limits)
	state.partialRun = partial
	execution := newExecution(r, plan, state, startedAt, inputs, nil, nil)

	runResult, runErr := execution.run(ctx)
	result := projectPartialRunResult(source, plan, runResult, state.partialRun)

	return result, runErr
}

// ResumePartial resumes one interrupted Partial Run. The exact same source
// Plan, destination, Run ID, and outstanding dynamic targets are required
// before any node can be invoked.
func (r *Runner) ResumePartial(
	ctx context.Context,
	source *Plan,
	destination NodeID,
	runID string,
	targets []ResumeTarget,
) (PartialRunResult, error) {
	if r == nil || r.clock == nil || r.idSource == nil {
		return PartialRunResult{}, errors.New("workflow: nil or invalid runner")
	}

	plan, err := derivePartialRunPlan(source, destination)
	if err != nil {
		return PartialRunResult{}, fmt.Errorf("workflow: %w", err)
	}

	runResult, _, partial, runErr := r.resumeExecution(
		ctx,
		plan,
		runID,
		targets,
		source.definition.Limits,
	)
	if runResult.RunID == "" {
		return PartialRunResult{}, runErr
	}

	return projectPartialRunResult(source, plan, runResult, partial), runErr
}

func (e *execution) partialState() *partialRunState {
	if e == nil || e.plan == nil || e.plan.partialRun == nil || len(e.scope) != 0 ||
		e.state == nil {
		return nil
	}

	return e.state.partialRun
}

func (e *execution) hasPartialData(index int) bool {
	state := e.partialState()
	if state == nil {
		return false
	}

	_, ok := state.materialized(index)

	return ok
}

func (e *execution) completePartialReady() bool {
	state := e.partialState()
	if state == nil || len(e.ready) == 0 {
		return false
	}

	ready := e.ready
	e.ready = make([]int, 0, len(ready))
	progressed := false

	for _, index := range ready {
		data, ok := state.materialized(index)
		if !ok {
			e.ready = append(e.ready, index)
			continue
		}

		node := &e.nodes[index]
		node.Status = NodeStatusSucceeded
		node.Attempts = 0
		node.Failure = ""

		e.outputs[index] = cloneValues(data.outputs)
		e.resolveOutgoing(index, data.route)
		e.done++
		delete(e.beforePassed, index)
		state.recordMaterialized(index, data)

		progressed = true

		_, staticAfter := e.plan.interruptAfter[index]
		if staticAfter || e.hostInterruptRequested {
			e.pauseInfo.AfterNodes = append(e.pauseInfo.AfterNodes, e.nodeAddress(index))
			e.pauseRequested = true
		}
	}

	if progressed {
		e.advance()
	}

	return progressed
}

func (e *execution) recordPartialExecuted(
	index int,
	attempts int,
	outputs map[string]Value,
	route string,
	isReusable bool,
) {
	state := e.partialState()
	if state != nil {
		state.recordExecuted(index, attempts, outputs, route, isReusable)
	}
}

func projectPartialRunResult(
	source *Plan,
	plan *Plan,
	runResult RunResult,
	state *partialRunState,
) PartialRunResult {
	result := PartialRunResult{
		RunID:                 runResult.RunID,
		Destination:           plan.partialRun.destination,
		SourcePlanFingerprint: source.fingerprint,
		PlanFingerprint:       plan.fingerprint,
		Status:                runResult.Status,
		Outputs:               cloneValues(runResult.Outputs),
		Nodes:                 make(map[NodeID]PartialNodeRun, len(plan.nodes)),
		Data: PartialRunData{
			RunID:                 runResult.RunID,
			DefinitionID:          source.definition.ID,
			Revision:              source.definition.Revision,
			SourcePlanFingerprint: source.fingerprint,
			Nodes:                 make(map[NodeID]PartialNodeData),
		},
		StartedAt: runResult.StartedAt,
		EndedAt:   runResult.EndedAt,
	}

	if runResult.Interruption != nil {
		info := cloneInterruptInfo(*runResult.Interruption)
		result.Interruption = &info
	}

	if state == nil {
		return clonePartialRunResult(result)
	}

	for index, node := range plan.nodes {
		nodeID := node.definition.ID
		summary := runResult.Nodes[nodeID]
		result.Nodes[nodeID] = PartialNodeRun{
			NodeRun: summary,
			Origin:  state.origins[index],
		}

		if summary.Status != NodeStatusSucceeded || state.outputs[index] == nil ||
			state.routes[index] == "" {
			continue
		}

		result.Data.Nodes[nodeID] = PartialNodeData{
			Outputs: cloneValues(state.outputs[index]),
			Route:   state.routes[index],
		}
	}

	return clonePartialRunResult(result)
}
