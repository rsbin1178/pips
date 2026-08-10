package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// RunStatus identifies one Run lifecycle state.
type RunStatus string

// Run states.
const (
	RunStatusPending   RunStatus = "pending"
	RunStatusRunning   RunStatus = "running"
	RunStatusSucceeded RunStatus = "succeeded"
	RunStatusFailed    RunStatus = "failed"
	RunStatusCanceled  RunStatus = "canceled"
)

// NodeStatus identifies one node lifecycle state.
type NodeStatus string

// Node states.
const (
	NodeStatusPending   NodeStatus = "pending"
	NodeStatusReady     NodeStatus = "ready"
	NodeStatusRunning   NodeStatus = "running"
	NodeStatusSucceeded NodeStatus = "succeeded"
	NodeStatusFailed    NodeStatus = "failed"
	NodeStatusSkipped   NodeStatus = "skipped"
)

// NodeRun is an immutable node execution summary.
type NodeRun struct {
	ID        NodeID
	Type      NodeTypeKey
	Status    NodeStatus
	Attempts  int
	Failure   FailureKind
	StartedAt time.Time
	EndedAt   time.Time
}

// RunResult is a detached snapshot of one completed, failed, or canceled Run.
// Callers may mutate its maps without affecting a Runner or later Runs.
type RunResult struct {
	RunID     string
	Status    RunStatus
	Outputs   map[string]Value
	Nodes     map[NodeID]NodeRun
	StartedAt time.Time
	EndedAt   time.Time
}

// RunError identifies the node and attempt that terminated a Run.
type RunError struct {
	NodeID  NodeID
	Attempt int
	Err     error
}

// Error implements error.
func (e *RunError) Error() string {
	return fmt.Sprintf("%v: node %q attempt %d: %v", ErrRun, e.NodeID, e.Attempt, e.Err)
}

// Unwrap exposes both ErrRun and the invocation cause.
func (e *RunError) Unwrap() []error {
	return []error{ErrRun, e.Err}
}

// RunIDSource creates one Run ID from the Run start time.
type RunIDSource func(time.Time) (string, error)

type runnerConfig struct {
	eventSink EventSink
	clock     func() time.Time
	idSource  RunIDSource
}

// RunnerOption configures a [Runner].
type RunnerOption func(*runnerConfig) error

// WithEventSink observes lifecycle metadata without input, output, config, or
// Action error payloads.
func WithEventSink(sink EventSink) RunnerOption {
	return func(config *runnerConfig) error {
		if sink == nil {
			return errors.New("workflow: nil event sink")
		}

		config.eventSink = sink

		return nil
	}
}

// WithClock replaces the Runner clock, primarily for deterministic tests.
func WithClock(clock func() time.Time) RunnerOption {
	return func(config *runnerConfig) error {
		if clock == nil {
			return errors.New("workflow: nil clock")
		}

		config.clock = clock

		return nil
	}
}

// WithRunIDSource replaces Run ID generation, primarily for deterministic
// tests and host correlation policies.
func WithRunIDSource(source RunIDSource) RunnerOption {
	return func(config *runnerConfig) error {
		if source == nil {
			return errors.New("workflow: nil run id source")
		}

		config.idSource = source

		return nil
	}
}

// Runner synchronously executes immutable Plans in process.
type Runner struct {
	eventSink EventSink
	clock     func() time.Time
	idSource  RunIDSource
}

// NewRunner creates a Runner with cryptographically random Run IDs.
func NewRunner(options ...RunnerOption) (*Runner, error) {
	config := runnerConfig{clock: time.Now, idSource: randomRunID}

	for _, option := range options {
		if option == nil {
			return nil, errors.New("workflow: nil runner option")
		}

		if err := option(&config); err != nil {
			return nil, err
		}
	}

	return &Runner{
		eventSink: config.eventSink,
		clock:     config.clock,
		idSource:  config.idSource,
	}, nil
}

// Run validates inputs, executes plan, and waits for every Runner-owned
// goroutine before returning. Timeouts and cancellation are cooperative:
// Actions must return when their context is done.
func (r *Runner) Run(
	ctx context.Context,
	plan *Plan,
	inputs map[string]Value,
) (RunResult, error) {
	if r == nil || r.clock == nil || r.idSource == nil {
		return RunResult{}, errors.New("workflow: nil or invalid runner")
	}

	if plan == nil || plan.Fingerprint() == "" {
		return RunResult{}, errors.New("workflow: nil or invalid plan")
	}

	if err := validatePortValues(inputs, plan.definition.Inputs); err != nil {
		return RunResult{}, fmt.Errorf("%w: inputs: %w", ErrRun, err)
	}

	if err := ctx.Err(); err != nil {
		return RunResult{}, err
	}

	startedAt := r.clock().UTC()

	runID, err := r.idSource(startedAt)
	if err != nil {
		return RunResult{}, fmt.Errorf("%w: create run id: %w", ErrRun, err)
	}

	if runID == "" {
		return RunResult{}, fmt.Errorf("%w: create run id: empty id", ErrRun)
	}

	state := newRunState(runID, plan.definition.Limits)
	execution := newExecution(r, plan, state, startedAt, inputs, nil)

	return execution.run(ctx)
}

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
	result RunResult

	edges   []edgeState
	nodes   []NodeRun
	outputs []map[string]Value
	ready   []int
	running int
	done    int

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
	runID      string
	maxSteps   int64
	steps      atomic.Int64
	leafTokens chan struct{}
	eventMu    sync.Mutex
}

type nodeRuntime struct {
	execution *execution
	nodeID    NodeID
}

type nodeCompletion struct {
	index    int
	output   NodeOutput
	err      error
	attempts int
	failure  FailureKind
	started  time.Time
	ended    time.Time
}

var errStepLimit = errors.New("workflow step limit exceeded")

func newExecution(
	runner *Runner,
	plan *Plan,
	state *runState,
	startedAt time.Time,
	inputs map[string]Value,
	scope []ScopeFrame,
) *execution {
	nodes := make([]NodeRun, len(plan.nodes))
	for index, node := range plan.nodes {
		nodes[index] = NodeRun{ID: node.definition.ID, Type: node.definition.Type, Status: NodeStatusPending}
	}

	return &execution{
		runner:  runner,
		plan:    plan,
		runID:   state.runID,
		state:   state,
		scope:   slices.Clone(scope),
		input:   cloneValues(inputs),
		edges:   make([]edgeState, len(plan.edges)),
		nodes:   nodes,
		outputs: make([]map[string]Value, len(plan.nodes)),
		result: RunResult{
			RunID: state.runID, Status: RunStatusRunning, Outputs: map[string]Value{},
			Nodes: map[NodeID]NodeRun{}, StartedAt: startedAt,
		},
	}
}

func newRunState(runID string, limits Limits) *runState {
	return &runState{
		runID:      runID,
		maxSteps:   int64(limits.MaxSteps),
		leafTokens: make(chan struct{}, limits.MaxConcurrency),
	}
}

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
	e.emit.run(RunStarted{})

	completions := make(chan nodeCompletion, len(e.plan.nodes))
	e.markReady(e.plan.startIndex)

	for e.done < len(e.plan.nodes) {
		if err := ctx.Err(); err != nil {
			cancel()
			e.drain(completions)

			return e.finishCanceled(err)
		}

		e.launchReady(runCtx, completions)

		if e.running == 0 {
			return e.finishFailed(&RunError{Err: errors.New("scheduler made no progress")})
		}

		select {
		case <-ctx.Done():
			cancel()
			e.drain(completions)

			return e.finishCanceled(ctx.Err())
		case completion := <-completions:
			e.running--
			if err := e.completeNode(completion); err != nil {
				cancel()
				e.drain(completions)

				return e.finishFailed(err)
			}

			e.advance()
		}
	}

	return e.finishSucceeded()
}

func (e *execution) launchReady(ctx context.Context, completions chan<- nodeCompletion) {
	for len(e.ready) > 0 && e.running < e.plan.definition.Limits.MaxConcurrency {
		index := e.ready[0]
		e.ready = e.ready[1:]

		if ctx.Err() != nil {
			return
		}

		inputs, err := e.resolveInputs(index)
		if err != nil {
			completions <- nodeCompletion{
				index: index, err: err, failure: FailureError, ended: e.runner.clock().UTC(),
			}

			e.running++

			continue
		}

		e.nodes[index].Status = NodeStatusRunning

		e.running++
		go func() {
			completions <- e.invokeNode(ctx, index, inputs)
		}()
	}
}

func (e *execution) invokeNode(ctx context.Context, index int, inputs map[string]Value) nodeCompletion {
	node := e.plan.nodes[index]
	policy := node.definition.Policy
	maximumAttempts := max(policy.Retry.MaxAttempts, 1)
	completion := nodeCompletion{index: index}

	for attempt := 1; attempt <= maximumAttempts; attempt++ {
		if e.invokeNodeAttempt(
			ctx,
			node,
			inputs,
			attempt,
			maximumAttempts,
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
	maximumAttempts int,
	completion *nodeCompletion,
) bool {
	if err := ctx.Err(); err != nil {
		e.cancelCompletion(completion, err, attempt-1)

		return true
	}

	acquired, err := e.acquireLeaf(ctx, node)
	if err != nil {
		e.cancelCompletion(completion, err, attempt-1)

		return true
	}

	if e.stepLimitExceeded() {
		e.releaseLeaf(acquired)
		e.limitCompletion(completion, attempt)

		return true
	}

	if err := ctx.Err(); err != nil {
		e.releaseLeaf(acquired)
		e.cancelCompletion(completion, err, attempt-1)

		return true
	}

	started := e.runner.clock().UTC()
	if completion.started.IsZero() {
		completion.started = started
	}

	e.emit.node(completion.index, attempt, NodeStarted{})

	runtime := &nodeRuntime{execution: e, nodeID: node.definition.ID}
	output, failure, err := invokeAttempt(
		ctx,
		node,
		inputs,
		node.definition.Policy.TimeoutMilli,
		runtime,
	)

	e.releaseLeaf(acquired)

	completion.attempts = attempt
	completion.ended = e.runner.clock().UTC()

	if err == nil {
		completion.output = output
		completion.err = nil
		completion.failure = ""

		e.emit.node(completion.index, attempt, NodeCompleted{})

		return true
	}

	completion.err = err
	completion.failure = failure
	e.emit.node(completion.index, attempt, NodeFailed{Kind: failure})

	if terminalAttempt(attempt, maximumAttempts, failure) {
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
	case errors.Is(err, errStepLimit):
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

func (e *execution) resolveInputs(index int) (map[string]Value, error) {
	node := e.plan.nodes[index]

	values := make(map[string]Value, len(node.bindings))
	for name, binding := range node.bindings {
		value, present := e.resolveBinding(binding)
		if !present {
			if node.isMerge {
				continue
			}

			return nil, fmt.Errorf("input %q is unavailable", name)
		}

		if err := node.spec.Inputs[name].Validate(value); err != nil {
			return nil, fmt.Errorf("input %q: %w", name, err)
		}

		values[name] = value
	}

	return values, nil
}

func (e *execution) resolveBinding(binding Binding) (Value, bool) {
	var value Value

	switch binding.Source {
	case BindingLiteral:
		value = *binding.Value
	case BindingWorkflowInput:
		var ok bool

		value, ok = e.input[binding.Port]
		if !ok {
			return Value{}, false
		}
	case BindingNodeOutput:
		sourceIndex := e.plan.nodeIndex[binding.Node]

		var ok bool

		value, ok = e.outputs[sourceIndex][binding.Port]
		if !ok {
			return Value{}, false
		}
	default:
		return Value{}, false
	}

	if len(binding.Path) == 0 {
		return value, true
	}

	return value.Lookup(binding.Path...)
}

func (e *execution) completeNode(completion nodeCompletion) error {
	e.recordNodeCompletion(completion)

	if completion.err == nil {
		e.resolveOutgoing(completion.index, completion.output.Route)
		e.done++

		return nil
	}

	nodeDefinition := e.plan.nodes[completion.index].definition
	switch nodeDefinition.Policy.Error {
	case ErrorRoute:
		e.resolveOutgoing(completion.index, RouteError)
		e.done++

		return nil
	case ErrorContinueWithDefault:
		e.outputs[completion.index] = cloneValues(nodeDefinition.Policy.DefaultOutputs)
		e.resolveOutgoing(completion.index, RouteSuccess)
		e.done++

		return nil
	default:
		return &RunError{
			NodeID: nodeDefinition.ID, Attempt: completion.attempts, Err: completion.err,
		}
	}
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
}

func (e *execution) drain(completions <-chan nodeCompletion) {
	for e.running > 0 {
		completion := <-completions
		e.recordNodeCompletion(completion)

		e.running--
	}
}

func (e *execution) finishSucceeded() (RunResult, error) {
	e.result.Status = RunStatusSucceeded
	e.result.Outputs = cloneValues(e.outputs[e.plan.endIndex])
	e.result.EndedAt = e.runner.clock().UTC()
	e.snapshotNodes()
	e.emit.run(RunCompleted{})

	return cloneRunResult(e.result), nil
}

func (e *execution) finishFailed(err error) (RunResult, error) {
	e.result.Status = RunStatusFailed
	e.result.EndedAt = e.runner.clock().UTC()
	e.snapshotNodes()
	e.emit.run(RunFailed{})

	return cloneRunResult(e.result), err
}

func (e *execution) finishCanceled(err error) (RunResult, error) {
	e.result.Status = RunStatusCanceled
	e.result.EndedAt = e.runner.clock().UTC()
	e.snapshotNodes()
	e.emit.run(RunCanceled{})

	return cloneRunResult(e.result), err
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

	return result
}

func (e *eventEmitter) run(payload EventPayload) {
	e.emit(-1, "", 0, payload)
}

func (e *eventEmitter) node(nodeIndex, attempt int, payload EventPayload) {
	node := e.plan.nodes[nodeIndex].definition
	e.emit(nodeIndex, node.Type, attempt, payload)
}

func (e *eventEmitter) emit(
	nodeIndex int,
	nodeType NodeTypeKey,
	attempt int,
	payload EventPayload,
) {
	if e.runner.eventSink == nil {
		return
	}

	e.state.eventMu.Lock()
	defer e.state.eventMu.Unlock()

	var nodeID NodeID
	if nodeIndex >= 0 {
		nodeID = e.plan.nodes[nodeIndex].definition.ID
	}

	e.runner.eventSink(Event{
		DefinitionID:          e.plan.definition.ID,
		Revision:              e.plan.definition.Revision,
		DefinitionFingerprint: e.plan.definitionFingerprint,
		PlanFingerprint:       e.plan.fingerprint,
		RunID:                 e.runID,
		NodeID:                nodeID,
		NodeType:              nodeType,
		Attempt:               attempt,
		Time:                  e.runner.clock().UTC(),
		payload:               payload,
		scope:                 slices.Clone(e.scope),
	})
}

func (r *nodeRuntime) runChild(
	ctx context.Context,
	plan *Plan,
	inputs map[string]Value,
	frame ScopeFrame,
) (map[string]Value, error) {
	if r == nil || r.execution == nil || plan == nil {
		return nil, errors.New("workflow child runtime is unavailable")
	}

	if err := validateScopeFrame(frame); err != nil {
		return nil, err
	}

	if err := validatePortValues(inputs, plan.definition.Inputs); err != nil {
		return nil, fmt.Errorf("child workflow inputs: %w", err)
	}

	scope := append(slices.Clone(r.execution.scope), frame)
	startedAt := r.execution.runner.clock().UTC()
	execution := newExecution(
		r.execution.runner,
		plan,
		r.execution.state,
		startedAt,
		inputs,
		scope,
	)

	result, err := execution.run(ctx)
	if err != nil {
		return nil, err
	}

	return cloneValues(result.Outputs), nil
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
	case ScopeBatchItem:
		if frame.Index < 0 {
			return errors.New("workflow batch scope has invalid index")
		}
	default:
		return errors.New("workflow child scope has invalid kind")
	}

	return nil
}

func randomRunID(_ time.Time) (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}

	return hex.EncodeToString(bytes[:]), nil
}
