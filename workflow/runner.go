package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// RunStatus identifies one Run lifecycle state.
type RunStatus string

// Run states.
const (
	RunStatusPending          RunStatus = "pending"
	RunStatusRunning          RunStatus = "running"
	RunStatusSucceeded        RunStatus = "succeeded"
	RunStatusPartialSucceeded RunStatus = "partial-succeeded"
	RunStatusFailed           RunStatus = "failed"
	RunStatusCanceled         RunStatus = "canceled"
	RunStatusInterrupted      RunStatus = "interrupted"
)

// NodeStatus identifies one node lifecycle state.
type NodeStatus string

// Node states.
const (
	NodeStatusPending     NodeStatus = "pending"
	NodeStatusReady       NodeStatus = "ready"
	NodeStatusRunning     NodeStatus = "running"
	NodeStatusSucceeded   NodeStatus = "succeeded"
	NodeStatusException   NodeStatus = "exception"
	NodeStatusFailed      NodeStatus = "failed"
	NodeStatusSkipped     NodeStatus = "skipped"
	NodeStatusInterrupted NodeStatus = "interrupted"
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

// RunResult is a detached snapshot of one terminal or interrupted Run. An
// interrupted result keeps EndedAt zero and exposes its resumable frontier in
// Interruption. Callers may mutate its maps without affecting runtime state.
type RunResult struct {
	RunID        string
	Status       RunStatus
	Outputs      map[string]Value
	Nodes        map[NodeID]NodeRun
	StartedAt    time.Time
	EndedAt      time.Time
	Interruption *InterruptInfo
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
	eventSink             EventSink
	nodeExecutionRecorder NodeExecutionRecorder
	clock                 func() time.Time
	idSource              RunIDSource
	checkpointStore       CheckpointStore
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

// WithNodeExecutionRecorder observes sensitive, storage-neutral node
// execution snapshots from ordinary Run and Resume operations.
func WithNodeExecutionRecorder(recorder NodeExecutionRecorder) RunnerOption {
	return func(config *runnerConfig) error {
		if recorder == nil {
			return errors.New("workflow: nil node execution recorder")
		}

		config.nodeExecutionRecorder = recorder

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

// WithCheckpointStore configures opaque checkpoint persistence for interrupted
// Runs and later Resume calls.
func WithCheckpointStore(store CheckpointStore) RunnerOption {
	return func(config *runnerConfig) error {
		if isNilInterface(store) {
			return errors.New("workflow: nil checkpoint store")
		}

		config.checkpointStore = store

		return nil
	}
}

// Runner synchronously executes immutable Plans in process.
type Runner struct {
	eventSink             EventSink
	nodeExecutionRecorder NodeExecutionRecorder
	clock                 func() time.Time
	idSource              RunIDSource
	checkpointStore       CheckpointStore
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
		eventSink:             config.eventSink,
		nodeExecutionRecorder: config.nodeExecutionRecorder,
		clock:                 config.clock,
		idSource:              config.idSource,
		checkpointStore:       config.checkpointStore,
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

	normalizedInputs, err := normalizeWorkflowInputs(plan.definition.Inputs, inputs, nil)
	if err != nil {
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
	execution := newExecution(r, plan, state, startedAt, normalizedInputs, nil, nil)

	return execution.run(ctx)
}

func randomRunID(_ time.Time) (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}

	return hex.EncodeToString(bytes[:]), nil
}
