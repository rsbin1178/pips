package workflow

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

type edgeState uint8

const (
	edgePending edgeState = iota
	edgeTaken
	edgeSkipped
)

type execution struct {
	runner *Runner
	plan   *Plan
	runID  string
	state  *runState
	scope  []ScopeFrame
	input  map[string]Value
	loop   *loopIterationState
	result RunResult

	edges       []edgeState
	nodes       []NodeRun
	outputs     []map[string]Value
	ready       []int
	running     int
	done        int
	paused      map[int]pausedNodeCheckpoint
	inflight    map[int]inflightNode
	forcedRerun map[int]struct{}

	beforePassed           map[int]struct{}
	pauseInfo              InterruptInfo
	pauseDynamic           []dynamicInterruptCheckpoint
	pauseRequested         bool
	resumed                bool
	hostInterruptRequested bool

	steps atomic.Int64
	emit  eventEmitter
}

type eventEmitter struct {
	runner *Runner
	plan   *Plan
	runID  string
	state  *runState
	scope  []ScopeFrame
}

type runState struct {
	runID         string
	maxSteps      int64
	steps         atomic.Int64
	leafTokens    chan struct{}
	eventMu       sync.Mutex
	resumeTargets map[string]ResumeTarget
	nodeDebug     *nodeDebugCollector
}

type nodeRuntime struct {
	execution *execution
	nodeID    NodeID
	resume    *pausedNodeCheckpoint
}

type nodeCompletion struct {
	index     int
	output    NodeOutput
	err       error
	attempts  int
	failure   FailureKind
	started   time.Time
	ended     time.Time
	inputs    map[string]Value
	pause     *executionPauseError
	dynamic   *dynamicInterruptError
	hostRerun bool
}

type inflightNode struct {
	inputs      map[string]Value
	cancel      context.CancelCauseFunc
	isComposite bool
}

type runInterruptMonitor struct {
	state     *runInterruptState
	signal    <-chan struct{}
	graceDone <-chan time.Time
	timer     *time.Timer
}

type executionPauseError struct {
	checkpoint executionCheckpoint
	hasChild   bool
	batch      *batchCheckpoint
	loop       *loopCheckpoint
	info       InterruptInfo
	dynamic    []dynamicInterruptCheckpoint
}

func (e *executionPauseError) Error() string {
	return ErrInterrupted.Error()
}

func (e *executionPauseError) Unwrap() error {
	return ErrInterrupted
}

var (
	errStepLimit = errors.New("workflow step limit exceeded")
	errHostRerun = errors.New("workflow host interruption rerun")
)

func newExecution(
	runner *Runner,
	plan *Plan,
	state *runState,
	startedAt time.Time,
	inputs map[string]Value,
	scope []ScopeFrame,
	loop *loopIterationState,
) *execution {
	nodes := make([]NodeRun, len(plan.nodes))
	for index, node := range plan.nodes {
		nodes[index] = NodeRun{ID: node.definition.ID, Type: node.definition.Type, Status: NodeStatusPending}
	}

	return &execution{
		runner:       runner,
		plan:         plan,
		runID:        state.runID,
		state:        state,
		scope:        slices.Clone(scope),
		input:        cloneValues(inputs),
		loop:         loop,
		edges:        make([]edgeState, len(plan.edges)),
		nodes:        nodes,
		outputs:      make([]map[string]Value, len(plan.nodes)),
		paused:       map[int]pausedNodeCheckpoint{},
		inflight:     map[int]inflightNode{},
		forcedRerun:  map[int]struct{}{},
		beforePassed: map[int]struct{}{},
		result: RunResult{
			RunID: state.runID, Status: RunStatusRunning, Outputs: map[string]Value{},
			Nodes: map[NodeID]NodeRun{}, StartedAt: startedAt,
		},
	}
}

func newRunState(runID string, limits Limits) *runState {
	return &runState{
		runID:         runID,
		maxSteps:      int64(limits.MaxSteps),
		leafTokens:    make(chan struct{}, limits.MaxConcurrency),
		resumeTargets: map[string]ResumeTarget{},
	}
}

//nolint:gocyclo // The select branches are the explicit scheduler state machine.
func (e *execution) run(ctx context.Context) (RunResult, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	e.emit = eventEmitter{
		runner: e.runner,
		plan:   e.plan,
		runID:  e.runID,
		state:  e.state,
		scope:  slices.Clone(e.scope),
	}
	if e.resumed {
		e.emit.run(RunResumed{})
		e.advance()
	} else {
		e.emit.run(RunStarted{})
		e.markReady(e.plan.startIndex)
	}

	completions := make(chan nodeCompletion, len(e.plan.nodes))

	interrupt := newRunInterruptMonitor(ctx)
	defer interrupt.close()

	for {
		interrupt.poll(e)

		if err := ctx.Err(); err != nil {
			cancel()
			e.drain(completions)

			return e.finishCanceled(err)
		}

		if e.pauseRequested && e.running == 0 {
			return e.finishInterrupted(ctx)
		}

		if e.done == len(e.plan.nodes) {
			return e.finishSucceeded()
		}

		if !e.pauseRequested {
			e.requestBeforeInterrupt()
		}

		if e.pauseRequested && e.running == 0 {
			return e.finishInterrupted(ctx)
		}

		if !e.pauseRequested {
			e.launchReady(runCtx, completions)
		}

		if e.running == 0 {
			return e.finishFailed(&RunError{Err: errors.New("scheduler made no progress")})
		}

		select {
		case <-ctx.Done():
			cancel()
			e.drain(completions)

			return e.finishCanceled(ctx.Err())
		case <-interrupt.signal:
			interrupt.activate(e)
		case <-interrupt.graceDone:
			interrupt.expire(e)
		case completion := <-completions:
			e.running--
			e.finishInflight(completion.index)

			interrupt.poll(e)

			if err := e.completeNode(completion); err != nil {
				cancel()
				e.drain(completions)

				return e.finishFailed(err)
			}

			if !e.pauseRequested {
				e.advance()
			}
		}
	}
}

func newRunInterruptMonitor(ctx context.Context) *runInterruptMonitor {
	state := runInterruptFromContext(ctx)

	monitor := &runInterruptMonitor{state: state}
	if state != nil {
		monitor.signal = state.requested
	}

	return monitor
}

func (m *runInterruptMonitor) poll(execution *execution) {
	if m.signal == nil {
		return
	}

	select {
	case <-m.signal:
		m.activate(execution)
	default:
	}
}

func (m *runInterruptMonitor) activate(execution *execution) {
	m.signal = nil

	execution.requestHostInterrupt()

	options := m.state.snapshot()
	if !options.hasTimeout {
		return
	}

	if options.timeout <= 0 {
		execution.forceHostReruns()

		return
	}

	m.timer = time.NewTimer(options.timeout)
	m.graceDone = m.timer.C
}

func (m *runInterruptMonitor) expire(execution *execution) {
	m.graceDone = nil

	execution.forceHostReruns()
}

func (m *runInterruptMonitor) close() {
	if m.timer != nil {
		m.timer.Stop()
	}
}

func (e *execution) requestHostInterrupt() {
	if e.hostInterruptRequested {
		return
	}

	e.hostInterruptRequested = true

	e.pauseRequested = true
	for _, index := range e.ready {
		e.appendRerunAddress(index)
	}
}

func (e *execution) forceHostReruns() {
	for index, invocation := range e.inflight {
		if invocation.isComposite {
			continue
		}

		if _, forced := e.forcedRerun[index]; forced {
			continue
		}

		e.forcedRerun[index] = struct{}{}

		invocation.cancel(errHostRerun)
	}
}

func (e *execution) finishInflight(index int) {
	invocation, ok := e.inflight[index]
	if ok {
		invocation.cancel(nil)
		delete(e.inflight, index)
	}

	delete(e.forcedRerun, index)
}

func (e *execution) completeNode(completion nodeCompletion) error {
	if completion.hostRerun {
		e.recordHostRerun(completion)

		return nil
	}

	if completion.dynamic != nil {
		e.recordDynamicInterruption(completion)

		return nil //nolint:nilerr // Dynamic interruption is resumable control flow.
	}

	if completion.pause != nil {
		e.recordNodeInterruption(completion)

		return nil //nolint:nilerr // Descendant interruption is resumable control flow.
	}

	e.recordNodeCompletion(completion)

	if completion.err == nil {
		e.resolveOutgoing(completion.index, completion.output.Route)
		e.done++
		delete(e.beforePassed, completion.index)
		e.recordNodeDebug(
			completion.index,
			completion.inputs,
			completion.output.Values,
			completion.output.Route,
			"",
		)

		_, staticAfter := e.plan.interruptAfter[completion.index]
		if staticAfter || e.hostInterruptRequested {
			e.pauseInfo.AfterNodes = append(
				e.pauseInfo.AfterNodes,
				e.nodeAddress(completion.index),
			)
			e.pauseRequested = true
		}

		return nil
	}

	nodeDefinition := e.plan.nodes[completion.index].definition
	switch nodeDefinition.Policy.Error {
	case ErrorRoute:
		e.resolveOutgoing(completion.index, RouteError)
		e.done++
		e.recordNodeDebug(
			completion.index,
			completion.inputs,
			nil,
			RouteError,
			completion.err.Error(),
		)

		return nil
	case ErrorContinueWithDefault:
		e.outputs[completion.index] = cloneValues(nodeDefinition.Policy.DefaultOutputs)
		e.resolveOutgoing(completion.index, RouteSuccess)
		e.done++
		e.recordNodeDebug(
			completion.index,
			completion.inputs,
			nodeDefinition.Policy.DefaultOutputs,
			RouteSuccess,
			completion.err.Error(),
		)

		return nil
	default:
		e.recordNodeDebug(
			completion.index,
			completion.inputs,
			nil,
			"",
			completion.err.Error(),
		)

		return &RunError{
			NodeID: nodeDefinition.ID, Attempt: completion.attempts, Err: completion.err,
		}
	}
}

func (e *execution) recordHostRerun(completion nodeCompletion) {
	node := &e.nodes[completion.index]
	node.Status = NodeStatusInterrupted
	node.Attempts = completion.attempts
	node.Failure = ""
	node.StartedAt = completion.started
	node.EndedAt = time.Time{}
	e.paused[completion.index] = pausedNodeCheckpoint{
		Index: completion.index, Inputs: cloneValues(completion.inputs),
		Attempts: completion.attempts, StartedAt: completion.started, Rerun: true,
	}
	e.appendRerunAddress(completion.index)
	e.pauseRequested = true
	e.recordNodeDebug(completion.index, completion.inputs, nil, "", "")
}

func (e *execution) appendRerunAddress(index int) {
	address := e.nodeAddress(index)
	for _, existing := range e.pauseInfo.RerunNodes {
		if nodeAddressEqual(existing, address) {
			return
		}
	}

	e.pauseInfo.RerunNodes = append(e.pauseInfo.RerunNodes, address)
}

func (e *execution) recordDynamicInterruption(completion nodeCompletion) {
	node := &e.nodes[completion.index]
	node.Status = NodeStatusInterrupted
	node.Attempts = completion.attempts
	node.Failure = ""
	node.StartedAt = completion.started
	node.EndedAt = time.Time{}

	dynamic := cloneDynamicInterrupts(completion.dynamic.points)
	e.paused[completion.index] = pausedNodeCheckpoint{
		Index: completion.index, Inputs: cloneValues(completion.inputs),
		Attempts: completion.attempts, StartedAt: completion.started,
		Dynamic: dynamic, Local: cloneDynamicLocal(completion.dynamic.local),
	}

	e.pauseDynamic = append(e.pauseDynamic, cloneDynamicInterrupts(dynamic)...)
	for _, point := range dynamic {
		e.pauseInfo.Contexts = append(e.pauseInfo.Contexts, InterruptContext{
			ID: point.ID, Address: cloneNodeAddress(point.Address), Info: point.Info,
		})
	}

	e.pauseRequested = true
	e.recordNodeDebug(completion.index, completion.inputs, nil, "", "")
}

func (e *execution) recordNodeInterruption(completion nodeCompletion) {
	node := &e.nodes[completion.index]
	node.Status = NodeStatusInterrupted
	node.Attempts = completion.attempts
	node.Failure = ""
	node.StartedAt = completion.started
	node.EndedAt = time.Time{}

	paused := pausedNodeCheckpoint{
		Index: completion.index, Inputs: cloneValues(completion.inputs),
		Attempts: completion.attempts, StartedAt: completion.started,
	}

	checkpoint := completion.pause.checkpoint
	if completion.pause.hasChild {
		paused.Child = &checkpoint
	}

	paused.Batch = cloneBatchCheckpoint(completion.pause.batch)
	paused.Loop = cloneLoopCheckpoint(completion.pause.loop)
	paused.Dynamic = cloneDynamicInterrupts(completion.pause.dynamic)
	e.paused[completion.index] = paused
	e.pauseDynamic = append(
		e.pauseDynamic,
		cloneDynamicInterrupts(completion.pause.dynamic)...,
	)
	e.mergeInterruptInfo(completion.pause.info)
	e.pauseRequested = true
	e.recordNodeDebug(completion.index, completion.inputs, nil, "", "")
}

func (e *execution) requestBeforeInterrupt() {
	matched := false

	for _, index := range e.ready {
		if _, configured := e.plan.interruptBefore[index]; !configured {
			continue
		}

		if _, passed := e.beforePassed[index]; passed {
			continue
		}

		e.beforePassed[index] = struct{}{}
		e.pauseInfo.BeforeNodes = append(e.pauseInfo.BeforeNodes, e.nodeAddress(index))
		matched = true
	}

	if matched {
		e.pauseRequested = true
	}
}

func (e *execution) mergeInterruptInfo(info InterruptInfo) {
	e.pauseInfo.Contexts = append(e.pauseInfo.Contexts, info.Contexts...)
	e.pauseInfo.BeforeNodes = append(e.pauseInfo.BeforeNodes, info.BeforeNodes...)
	e.pauseInfo.AfterNodes = append(e.pauseInfo.AfterNodes, info.AfterNodes...)
	e.pauseInfo.RerunNodes = append(e.pauseInfo.RerunNodes, info.RerunNodes...)
}

func (e *execution) nodeAddress(index int) NodeAddress {
	return NodeAddress{NodeID: e.plan.nodes[index].definition.ID, Scope: slices.Clone(e.scope)}
}

func (e *execution) recordNodeCompletion(completion nodeCompletion) {
	node := &e.nodes[completion.index]
	node.Attempts = completion.attempts
	node.StartedAt = completion.started
	node.EndedAt = completion.ended

	if completion.err == nil {
		node.Status = NodeStatusSucceeded
		e.outputs[completion.index] = cloneValues(completion.output.Values)

		return
	}

	node.Status = NodeStatusFailed
	node.Failure = completion.failure
}

func (e *execution) resolveOutgoing(nodeIndex int, selectedRoute string) {
	for _, edgeIndex := range e.plan.outgoing[nodeIndex] {
		state := edgeSkipped
		if e.plan.edges[edgeIndex].route == selectedRoute {
			state = edgeTaken
		}

		e.edges[edgeIndex] = state
	}
}

func (e *execution) advance() {
	for {
		progressed := false

		for index := range e.nodes {
			if e.nodes[index].Status != NodeStatusPending || !e.incomingResolved(index) {
				continue
			}

			if e.anyIncomingTaken(index) {
				e.markReady(index)
			} else {
				e.skipNode(index)
			}

			progressed = true
		}

		if !progressed {
			return
		}
	}
}

func (e *execution) incomingResolved(nodeIndex int) bool {
	if nodeIndex == e.plan.startIndex {
		return false
	}

	for _, edgeIndex := range e.plan.incoming[nodeIndex] {
		if e.edges[edgeIndex] == edgePending {
			return false
		}
	}

	return true
}

func (e *execution) anyIncomingTaken(nodeIndex int) bool {
	for _, edgeIndex := range e.plan.incoming[nodeIndex] {
		if e.edges[edgeIndex] == edgeTaken {
			return true
		}
	}

	return false
}

func (e *execution) markReady(nodeIndex int) {
	e.nodes[nodeIndex].Status = NodeStatusReady
	e.ready = append(e.ready, nodeIndex)
	e.emit.node(nodeIndex, 0, NodeReady{})
}

func (e *execution) skipNode(nodeIndex int) {
	now := e.runner.clock().UTC()
	e.nodes[nodeIndex].Status = NodeStatusSkipped

	e.nodes[nodeIndex].EndedAt = now
	for _, edgeIndex := range e.plan.outgoing[nodeIndex] {
		e.edges[edgeIndex] = edgeSkipped
	}

	e.done++
	e.emit.node(nodeIndex, 0, NodeSkipped{})
	e.recordNodeDebug(nodeIndex, map[string]Value{}, map[string]Value{}, "", "")
}

func (e *execution) drain(completions <-chan nodeCompletion) {
	for e.running > 0 {
		completion := <-completions
		e.finishInflight(completion.index)
		e.recordNodeCompletion(completion)

		errorMessage := ""
		if completion.err != nil {
			errorMessage = completion.err.Error()
		}

		e.recordNodeDebug(
			completion.index,
			completion.inputs,
			completion.output.Values,
			completion.output.Route,
			errorMessage,
		)

		e.running--
	}
}

func (e *execution) finishSucceeded() (RunResult, error) {
	e.synchronizeNodeDebug()
	e.result.Status = RunStatusSucceeded
	e.result.Outputs = cloneValues(e.outputs[e.plan.endIndex])
	e.result.EndedAt = e.runner.clock().UTC()
	e.snapshotNodes()
	e.emit.run(RunCompleted{})

	return cloneRunResult(e.result), nil
}

func (e *execution) finishFailed(err error) (RunResult, error) {
	e.synchronizeNodeDebug()
	e.result.Status = RunStatusFailed
	e.result.EndedAt = e.runner.clock().UTC()
	e.snapshotNodes()
	e.emit.run(RunFailed{})

	return cloneRunResult(e.result), err
}

func (e *execution) finishCanceled(err error) (RunResult, error) {
	e.synchronizeNodeDebug()
	e.result.Status = RunStatusCanceled
	e.result.EndedAt = e.runner.clock().UTC()
	e.snapshotNodes()
	e.emit.run(RunCanceled{})

	return cloneRunResult(e.result), err
}

func (e *execution) recordNodeDebug(
	index int,
	inputs map[string]Value,
	outputs map[string]Value,
	route string,
	errorMessage string,
) {
	if e.state.nodeDebug != nil {
		e.state.nodeDebug.record(e, index, inputs, outputs, route, errorMessage)
	}
}

func (e *execution) synchronizeNodeDebug() {
	if e.state.nodeDebug != nil {
		e.state.nodeDebug.synchronize(e)
	}
}

func (e *execution) snapshotNodes() {
	e.result.Nodes = make(map[NodeID]NodeRun, len(e.nodes))
	for _, node := range e.nodes {
		e.result.Nodes[node.ID] = node
	}
}

func cloneRunResult(result RunResult) RunResult {
	result.Outputs = cloneValues(result.Outputs)
	nodes := result.Nodes
	result.Nodes = make(map[NodeID]NodeRun, len(result.Nodes))
	maps.Copy(result.Nodes, nodes)

	if result.Interruption != nil {
		info := cloneInterruptInfo(*result.Interruption)
		result.Interruption = &info
	}

	return result
}
