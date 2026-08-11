package workflow

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

func restoreExecution(
	runner *Runner,
	plan *Plan,
	state *runState,
	checkpoint *executionCheckpoint,
	loop *loopIterationState,
) *execution {
	execution := newExecution(
		runner,
		plan,
		state,
		checkpoint.StartedAt,
		checkpoint.Input,
		checkpoint.Scope,
		loop,
	)
	execution.edges = slices.Clone(checkpoint.Edges)
	execution.ready = slices.Clone(checkpoint.Ready)
	execution.done = checkpoint.Done
	execution.steps.Store(checkpoint.Steps)
	execution.nodes = make([]NodeRun, len(checkpoint.Nodes))
	execution.outputs = make([]map[string]Value, len(checkpoint.Outputs))
	execution.failures = make([]nodeFailureData, len(checkpoint.FailureData))
	execution.paused = make(map[int]pausedNodeCheckpoint, len(checkpoint.Paused))
	execution.beforePassed = make(map[int]struct{}, len(checkpoint.BeforePassed))

	for index, node := range checkpoint.Nodes {
		execution.nodes[index] = NodeRun(node)
		execution.outputs[index] = cloneValuesOrNil(checkpoint.Outputs[index])
		execution.failures[index] = cloneNodeFailureData(checkpoint.FailureData[index])
	}

	for _, index := range checkpoint.BeforePassed {
		execution.beforePassed[index] = struct{}{}
	}

	for _, paused := range checkpoint.Paused {
		cloned := clonePausedNodeCheckpoint(paused)
		execution.paused[paused.Index] = cloned
		execution.ready = append(execution.ready, paused.Index)
	}

	return execution
}

func (e *execution) finishInterrupted(ctx context.Context) (RunResult, error) {
	if interruptInfoEmpty(e.pauseInfo) {
		return e.finishFailed(&RunError{Err: errors.New("scheduler interruption has no context")})
	}

	e.synchronizeNodeDebug()
	checkpoint := e.checkpoint()
	info := cloneInterruptInfo(e.pauseInfo)

	if len(e.scope) > 0 {
		e.result.Status = RunStatusInterrupted
		e.result.Interruption = &info
		e.snapshotNodes()
		e.emit.run(RunInterrupted{})

		return cloneRunResult(e.result), &executionPauseError{
			checkpoint: checkpoint,
			hasChild:   true,
			info:       info,
			dynamic:    cloneDynamicInterrupts(e.pauseDynamic),
		}
	}

	if isNilInterface(e.runner.checkpointStore) {
		return e.finishFailed(fmt.Errorf("%w: checkpoint store is required", ErrRun))
	}

	stored := workflowCheckpoint{
		Version:               checkpointVersion,
		RunID:                 e.runID,
		DefinitionID:          e.plan.definition.ID,
		Revision:              e.plan.definition.Revision,
		DefinitionFingerprint: e.plan.definitionFingerprint,
		RegistryFingerprint:   e.plan.registryFingerprint,
		PlanFingerprint:       e.plan.fingerprint,
		StartedAt:             e.result.StartedAt,
		TotalSteps:            e.state.steps.Load(),
		HandledFailure:        e.state.hasHandledFailure.Load(),
		Execution:             checkpoint,
		Interruption:          info,
	}
	if e.state.nodeDebug != nil {
		stored.NodeDebug = e.state.nodeDebug.checkpoint(e.plan.nodeDebug)
	}

	if e.state.partialRun != nil {
		stored.PartialRun = e.state.partialRun.checkpoint(e.plan, &checkpoint)
	}

	data, err := encodeWorkflowCheckpoint(stored)
	if err != nil {
		return e.finishFailed(fmt.Errorf("%w: save checkpoint: %w", ErrRun, err))
	}

	if err := e.runner.checkpointStore.Set(ctx, e.runID, slices.Clone(data)); err != nil {
		return e.finishFailed(fmt.Errorf("%w: save checkpoint: %w", ErrRun, err))
	}

	e.result.Status = RunStatusInterrupted
	e.result.Interruption = &info
	e.snapshotNodes()
	e.emit.run(RunInterrupted{})

	return cloneRunResult(e.result), &InterruptError{RunID: e.runID, Info: cloneInterruptInfo(info)}
}

func (e *execution) checkpoint() executionCheckpoint {
	checkpoint := executionCheckpoint{
		PlanFingerprint: e.plan.fingerprint,
		StartedAt:       e.result.StartedAt,
		Scope:           slices.Clone(e.scope),
		Input:           cloneValues(e.input),
		Edges:           slices.Clone(e.edges),
		Nodes:           make([]checkpointNodeRun, len(e.nodes)),
		Outputs:         make([]map[string]Value, len(e.outputs)),
		FailureData:     make([]nodeFailureData, len(e.failures)),
		Ready:           make([]int, 0, len(e.ready)),
		Done:            e.done,
		Steps:           e.steps.Load(),
		BeforePassed:    make([]int, 0, len(e.beforePassed)),
		Paused:          make([]pausedNodeCheckpoint, 0, len(e.paused)),
	}

	for index, node := range e.nodes {
		checkpoint.Nodes[index] = checkpointNodeRun(node)
		checkpoint.Outputs[index] = cloneValuesOrNil(e.outputs[index])
		checkpoint.FailureData[index] = cloneNodeFailureData(e.failures[index])
	}

	for _, index := range e.ready {
		if _, paused := e.paused[index]; !paused {
			checkpoint.Ready = append(checkpoint.Ready, index)
		}
	}

	for index := range e.beforePassed {
		checkpoint.BeforePassed = append(checkpoint.BeforePassed, index)
	}

	slices.Sort(checkpoint.BeforePassed)

	pausedIndexes := make([]int, 0, len(e.paused))
	for index := range e.paused {
		pausedIndexes = append(pausedIndexes, index)
	}

	slices.Sort(pausedIndexes)

	for _, index := range pausedIndexes {
		checkpoint.Paused = append(checkpoint.Paused, clonePausedNodeCheckpoint(e.paused[index]))
	}

	return checkpoint
}

func clonePausedNodeCheckpoint(checkpoint pausedNodeCheckpoint) pausedNodeCheckpoint {
	checkpoint.Inputs = cloneValues(checkpoint.Inputs)
	checkpoint.Dynamic = cloneDynamicInterrupts(checkpoint.Dynamic)

	checkpoint.Local = cloneDynamicLocal(checkpoint.Local)
	if checkpoint.Child != nil {
		child := cloneExecutionCheckpoint(*checkpoint.Child)
		checkpoint.Child = &child
	}

	checkpoint.Batch = cloneBatchCheckpoint(checkpoint.Batch)
	checkpoint.Loop = cloneLoopCheckpoint(checkpoint.Loop)

	return checkpoint
}

func cloneExecutionCheckpoint(checkpoint executionCheckpoint) executionCheckpoint {
	checkpoint.Scope = slices.Clone(checkpoint.Scope)
	checkpoint.Input = cloneValues(checkpoint.Input)
	checkpoint.Edges = slices.Clone(checkpoint.Edges)
	checkpoint.Nodes = slices.Clone(checkpoint.Nodes)
	checkpoint.Ready = slices.Clone(checkpoint.Ready)
	checkpoint.BeforePassed = slices.Clone(checkpoint.BeforePassed)

	outputs := make([]map[string]Value, len(checkpoint.Outputs))
	for index, values := range checkpoint.Outputs {
		outputs[index] = cloneValuesOrNil(values)
	}

	checkpoint.Outputs = outputs

	failures := make([]nodeFailureData, len(checkpoint.FailureData))
	for index, data := range checkpoint.FailureData {
		failures[index] = cloneNodeFailureData(data)
	}

	checkpoint.FailureData = failures

	paused := make([]pausedNodeCheckpoint, len(checkpoint.Paused))
	for index, node := range checkpoint.Paused {
		paused[index] = clonePausedNodeCheckpoint(node)
	}

	checkpoint.Paused = paused

	return checkpoint
}

func cloneValuesOrNil(values map[string]Value) map[string]Value {
	if values == nil {
		return nil
	}

	return cloneValues(values)
}
