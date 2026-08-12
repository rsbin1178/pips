package workflow

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

func (e *execution) launchReady(ctx context.Context, completions chan<- nodeCompletion) {
	for len(e.ready) > 0 && e.running < e.plan.definition.Limits.MaxConcurrency {
		index := e.ready[0]
		e.ready = e.ready[1:]

		if ctx.Err() != nil {
			return
		}

		paused, resuming := e.paused[index]

		var (
			inputs map[string]Value
			err    error
		)
		if resuming {
			inputs = cloneValues(paused.Inputs)
		} else {
			inputs, err = e.resolveInputs(index)
		}

		if err != nil {
			completions <- nodeCompletion{
				index: index, err: err, failure: FailureError, ended: e.runner.clock().UTC(),
			}

			e.running++

			continue
		}

		if resuming && len(paused.Dynamic) > 0 && !e.pausedNodeHasTarget(paused) {
			e.resurfacePausedNode(paused)

			continue
		}

		e.nodes[index].Status = NodeStatusRunning
		delete(e.paused, index)

		invokeCtx, cancel := context.WithCancelCause(ctx)
		e.inflight[index] = inflightNode{
			inputs: cloneValues(inputs), cancel: cancel,
			isComposite: e.plan.nodes[index].isComposite,
		}

		e.running++

		go func() {
			var resume *pausedNodeCheckpoint

			if resuming {
				cloned := clonePausedNodeCheckpoint(paused)
				resume = &cloned
			}

			completions <- e.invokeNode(invokeCtx, index, inputs, resume)
		}()
	}
}

func (e *execution) resurfacePausedNode(paused pausedNodeCheckpoint) {
	e.paused[paused.Index] = clonePausedNodeCheckpoint(paused)
	e.nodes[paused.Index].Status = NodeStatusInterrupted

	e.pauseDynamic = append(e.pauseDynamic, cloneDynamicInterrupts(paused.Dynamic)...)
	for _, point := range paused.Dynamic {
		e.pauseInfo.Contexts = append(e.pauseInfo.Contexts, InterruptContext{
			ID: point.ID, Address: cloneNodeAddress(point.Address), Info: point.Info,
		})
	}

	e.pauseRequested = true
}

func (e *execution) pausedNodeHasTarget(paused pausedNodeCheckpoint) bool {
	for _, interruption := range paused.Dynamic {
		if _, ok := e.state.resumeTargets[interruption.ID]; ok {
			return true
		}
	}

	return false
}

func (e *execution) invokeNode(
	ctx context.Context,
	index int,
	inputs map[string]Value,
	resume *pausedNodeCheckpoint,
) nodeCompletion {
	node := e.plan.nodes[index]
	policy := node.definition.Policy
	maximumAttempts := max(policy.Retry.MaxAttempts, 1)

	baseAttempts := 0
	if resume != nil {
		baseAttempts = resume.Attempts
	}

	completion := nodeCompletion{index: index, inputs: cloneValues(inputs)}

	for retryAttempt := 1; retryAttempt <= maximumAttempts; retryAttempt++ {
		if e.invokeNodeAttempt(
			ctx,
			node,
			inputs,
			baseAttempts+retryAttempt,
			retryAttempt,
			maximumAttempts,
			resume,
			&completion,
		) {
			return completion
		}
	}

	return completion
}

func (e *execution) invokeNodeAttempt(
	ctx context.Context,
	node planNode,
	inputs map[string]Value,
	attempt int,
	retryAttempt int,
	maximumAttempts int,
	resume *pausedNodeCheckpoint,
	completion *nodeCompletion,
) bool {
	if err := ctx.Err(); err != nil {
		e.cancelOrRerunCompletion(ctx, completion, err, attempt-1)

		return true
	}

	acquired, err := e.acquireLeaf(ctx, node)
	if err != nil {
		e.cancelOrRerunCompletion(ctx, completion, err, attempt-1)

		return true
	}

	if e.stepLimitExceeded() {
		e.releaseLeaf(acquired)
		e.limitCompletion(completion, attempt)

		return true
	}

	if err := ctx.Err(); err != nil {
		e.releaseLeaf(acquired)
		e.cancelOrRerunCompletion(ctx, completion, err, attempt-1)

		return true
	}

	started := e.runner.clock().UTC()
	if completion.started.IsZero() {
		completion.started = started
	}

	e.emit.node(completion.index, attempt, NodeStarted{})

	runtime := &nodeRuntime{execution: e, nodeID: node.definition.ID, resume: resume}
	output, failure, err := invokeAttempt(
		ctx,
		node,
		inputs,
		node.definition.Policy.TimeoutMilli,
		runtime,
		attempt,
	)

	e.releaseLeaf(acquired)

	completion.attempts = attempt
	completion.ended = e.runner.clock().UTC()

	return e.finishNodeAttempt(
		ctx,
		node,
		attempt,
		retryAttempt,
		maximumAttempts,
		resume,
		completion,
		output,
		failure,
		err,
	)
}

func (e *execution) finishNodeAttempt(
	ctx context.Context,
	node planNode,
	attempt int,
	retryAttempt int,
	maximumAttempts int,
	resume *pausedNodeCheckpoint,
	completion *nodeCompletion,
	output NodeOutput,
	failure FailureKind,
	err error,
) bool {
	var pause *executionPauseError
	if errors.As(err, &pause) {
		completion.pause = pause
		completion.err = nil
		completion.failure = ""
		e.emit.node(completion.index, attempt, NodeInterrupted{})

		return true
	}

	if err != nil && errors.Is(context.Cause(ctx), errHostRerun) {
		completion.hostRerun = true
		completion.err = nil
		completion.failure = ""
		e.emit.node(completion.index, attempt, NodeInterrupted{})

		return true
	}

	var dynamic *dynamicInterruptError
	if errors.As(err, &dynamic) {
		dynamic.points = mergeDynamicInterrupts(
			dynamic.points,
			remainingDynamicInterrupts(resume, e.state.resumeTargets),
		)
		if dynamic.local == nil && resume != nil {
			dynamic.local = cloneDynamicLocal(resume.Local)
		}

		completion.dynamic = dynamic
		completion.err = nil
		completion.failure = ""
		e.emit.node(completion.index, attempt, NodeInterrupted{})

		return true
	}

	if err == nil {
		remaining := remainingDynamicInterrupts(resume, e.state.resumeTargets)
		if len(remaining) > 0 {
			completion.dynamic = &dynamicInterruptError{
				points: remaining,
				local:  cloneDynamicLocal(resume.Local),
			}
			completion.err = nil
			completion.failure = ""
			e.emit.node(completion.index, attempt, NodeInterrupted{})

			return true
		}

		completion.output = output
		completion.err = nil
		completion.failure = ""

		e.emit.node(completion.index, attempt, NodeCompleted{})

		return true
	}

	completion.err = err
	completion.failure = failure
	e.emit.node(completion.index, attempt, NodeFailed{Kind: failure})

	if terminalAttempt(retryAttempt, maximumAttempts, failure) {
		return true
	}

	e.emit.node(completion.index, attempt, NodeRetrying{NextAttempt: attempt + 1})

	if err := waitBackoff(ctx, node.definition.Policy.Retry.BackoffMilli); err != nil {
		e.cancelCompletion(completion, err, attempt)

		return true
	}

	return false
}

func (e *execution) stepLimitExceeded() bool {
	localSteps := e.steps.Add(1)
	totalSteps := e.state.steps.Add(1)
	localExceeded := localSteps > int64(e.plan.definition.Limits.MaxSteps)
	totalExceeded := totalSteps > e.state.maxSteps

	return localExceeded || totalExceeded
}

func (e *execution) limitCompletion(
	completion *nodeCompletion,
	attempt int,
) {
	completion.err = errStepLimit
	completion.failure = FailureLimit
	completion.attempts = attempt
	completion.ended = e.runner.clock().UTC()
	e.emit.node(completion.index, attempt, NodeFailed{Kind: FailureLimit})
}

func (e *execution) cancelCompletion(
	completion *nodeCompletion,
	err error,
	attempts int,
) {
	completion.err = err
	completion.failure = FailureCanceled
	completion.attempts = attempts
	completion.ended = e.runner.clock().UTC()
}

func (e *execution) cancelOrRerunCompletion(
	ctx context.Context,
	completion *nodeCompletion,
	err error,
	attempts int,
) {
	if errors.Is(context.Cause(ctx), errHostRerun) {
		completion.hostRerun = true
		completion.attempts = attempts
		completion.ended = e.runner.clock().UTC()

		return
	}

	e.cancelCompletion(completion, err, attempts)
}

func terminalAttempt(attempt, maximumAttempts int, failure FailureKind) bool {
	if attempt == maximumAttempts {
		return true
	}

	switch failure {
	case FailureCanceled, FailureTimeout, FailureLimit:
		return true
	default:
		return false
	}
}

func (e *execution) acquireLeaf(ctx context.Context, node planNode) (bool, error) {
	if node.isComposite {
		return false, nil
	}

	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case e.state.leafTokens <- struct{}{}:
		return true, nil
	}
}

func (e *execution) releaseLeaf(acquired bool) {
	if acquired {
		<-e.state.leafTokens
	}
}

func invokeAttempt(
	ctx context.Context,
	node planNode,
	inputs map[string]Value,
	timeoutMilli int64,
	runtime *nodeRuntime,
	attempt int,
) (output NodeOutput, failure FailureKind, returnErr error) {
	if timeoutMilli > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutMilli)*time.Millisecond)
		defer cancel()
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			output = NodeOutput{}
			failure = FailurePanic
			returnErr = fmt.Errorf("node panicked: %v", recovered)
		}
	}()

	ctx = context.WithValue(
		ctx,
		executionContextKey{},
		invocationExecutionContext(runtime, attempt),
	)
	ctx = context.WithValue(ctx, invocationInterruptKey{}, newInvocationInterruptContext(runtime))

	output, err := node.executor.Invoke(ctx, NodeInput{
		Values:  cloneValues(inputs),
		runtime: runtime,
	})
	if err != nil {
		return NodeOutput{}, classifyFailure(ctx, err), err
	}

	if err := validatePortValues(output.Values, node.spec.Outputs); err != nil {
		return NodeOutput{}, FailureError, fmt.Errorf("node output: %w", err)
	}

	if !slices.Contains(node.spec.Routes, output.Route) {
		return NodeOutput{}, FailureError, fmt.Errorf("node selected unknown route %q", output.Route)
	}

	return NodeOutput{Values: cloneValues(output.Values), Route: output.Route}, "", nil
}

func classifyFailure(ctx context.Context, err error) FailureKind {
	switch {
	case errors.Is(err, errStepLimit), errors.Is(err, errLoopLimit):
		return FailureLimit
	case errors.Is(err, context.Canceled):
		return FailureCanceled
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		return FailureTimeout
	default:
		return FailureError
	}
}

func waitBackoff(ctx context.Context, milliseconds int64) error {
	if milliseconds == 0 {
		return ctx.Err()
	}

	timer := time.NewTimer(time.Duration(milliseconds) * time.Millisecond)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
