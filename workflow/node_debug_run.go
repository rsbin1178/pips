package workflow

import (
	"context"
	"errors"
	"fmt"
)

// DebugNode executes only the selected node boundary through the ordinary
// Workflow scheduler. Registered Actions are real and may have side effects.
func (r *Runner) DebugNode(
	ctx context.Context,
	debugPlan *NodeDebugPlan,
	inputs map[string]Value,
) (NodeDebugResult, error) {
	if r == nil || r.clock == nil || r.idSource == nil {
		return NodeDebugResult{}, errors.New("workflow: nil or invalid runner")
	}

	if err := debugPlan.validate(); err != nil {
		return NodeDebugResult{}, fmt.Errorf("workflow: %w", err)
	}

	resolvedInputs, err := materializeNodeDebugInputs(debugPlan, inputs)
	if err != nil {
		return NodeDebugResult{}, fmt.Errorf("%w: node debug inputs: %w", ErrRun, err)
	}

	if err := ctx.Err(); err != nil {
		return NodeDebugResult{}, err
	}

	startedAt := r.clock().UTC()

	runID, err := r.idSource(startedAt)
	if err != nil {
		return NodeDebugResult{}, fmt.Errorf("%w: create run id: %w", ErrRun, err)
	}

	if runID == "" {
		return NodeDebugResult{}, fmt.Errorf("%w: create run id: empty id", ErrRun)
	}

	state := newRunState(runID, debugPlan.globalLimits)
	state.nodeDebug = newNodeDebugCollector(debugPlan.plan.nodes[0], resolvedInputs)
	execution := newExecution(r, debugPlan.plan, state, startedAt, inputs, nil, nil)

	runResult, runErr := execution.run(ctx)
	result := projectNodeDebugResult(debugPlan, runResult, state.nodeDebug)

	return result, runErr
}

// ResumeNodeDebug resumes one interrupted isolated node trial. The exact same
// NodeDebugPlan and Run ID are required before any node can be invoked.
func (r *Runner) ResumeNodeDebug(
	ctx context.Context,
	debugPlan *NodeDebugPlan,
	runID string,
	targets []ResumeTarget,
) (NodeDebugResult, error) {
	if r == nil || r.clock == nil || r.idSource == nil {
		return NodeDebugResult{}, errors.New("workflow: nil or invalid runner")
	}

	if err := debugPlan.validate(); err != nil {
		return NodeDebugResult{}, fmt.Errorf("workflow: %w", err)
	}

	runResult, collector, runErr := r.resumeExecution(
		ctx,
		debugPlan.plan,
		runID,
		targets,
		debugPlan.globalLimits,
	)
	if runResult.RunID == "" {
		return NodeDebugResult{}, runErr
	}

	return projectNodeDebugResult(debugPlan, runResult, collector), runErr
}

func projectNodeDebugResult(
	plan *NodeDebugPlan,
	runResult RunResult,
	collector *nodeDebugCollector,
) NodeDebugResult {
	records := collector.snapshot()
	rootID := plan.plan.nodes[0].definition.ID
	root := NodeDebugExecution{
		Address: NodeAddress{NodeID: rootID, Scope: []ScopeFrame{}},
		NodeRun: NodeRun{ID: rootID, Type: plan.plan.nodes[0].definition.Type},
		Inputs:  map[string]Value{},
		Outputs: map[string]Value{},
	}
	inner := make([]NodeDebugExecution, 0, max(len(records)-1, 0))

	for _, record := range records {
		if len(record.Address.Scope) == 0 && record.Address.NodeID == rootID {
			root = cloneNodeDebugExecution(record)
			continue
		}

		inner = append(inner, cloneNodeDebugExecution(record))
	}

	result := NodeDebugResult{
		RunID:           runResult.RunID,
		Target:          plan.Target(),
		Status:          runResult.Status,
		Execution:       root,
		InnerExecutions: inner,
		StartedAt:       runResult.StartedAt,
		EndedAt:         runResult.EndedAt,
	}
	if runResult.Interruption != nil {
		info := cloneInterruptInfo(*runResult.Interruption)
		result.Interruption = &info
	}

	return cloneNodeDebugResult(result)
}
