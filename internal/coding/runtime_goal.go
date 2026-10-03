//nolint:wsl_v5 // Lifecycle guards and durable boundary checks stay adjacent.
package coding

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"

	"github.com/rsbin1178/pips/agent/continuation"
	"github.com/rsbin1178/pips/agent/goal"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/goalflow"
)

type goalContextKey struct{}

type runtimeGoalDriver struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type goalWorkOperation struct {
	kind         runtimeOperationKind
	resolution   runtimeResolution
	messages     []ai.Message
	notification *notificationOperation
}

// GoalSnapshot returns the latest immutable Goal projection.
func (r *Runtime) GoalSnapshot() GoalState {
	if r == nil {
		return GoalState{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state.Goal.Clone()
}

// StartGoal admits one session-bound Goal and drives bounded Coding Work. The
// iterator is the owner of cancellation, including assessment and verification.
func (r *Runtime) StartGoal(ctx context.Context, request GoalRequest) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		if err := request.Validate(); err != nil {
			yield(Event{}, err)
			return
		}
		r.driveGoal(ctx, &request, goalWorkOperation{}, yield)
	}
}

// ResumeGoal is the explicit activation boundary after pause or recovery.
// Interrupted Decisions reuse persisted Work; they never replay its tools.
func (r *Runtime) ResumeGoal(ctx context.Context) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) { r.driveGoal(ctx, nil, goalWorkOperation{}, yield) }
}

// PauseGoal revokes automatic activation and cancels an admitted stage.
func (r *Runtime) PauseGoal(ctx context.Context) error { return r.controlGoal(ctx, false) }

// ClearGoal cancels the current Goal without deleting its durable audit trail.
func (r *Runtime) ClearGoal(ctx context.Context) error { return r.controlGoal(ctx, true) }

//nolint:gocyclo // Control races retry CAS, never work or model calls.
func (r *Runtime) controlGoal(ctx context.Context, clearGoal bool) error {
	if r == nil || r.goalControl == nil {
		return ErrGoalMissing
	}
	c := r.goalControl
	c.mu.Lock()
	if c.id == "" || c.ledger.Cleared {
		c.mu.Unlock()
		return ErrGoalMissing
	}
	var err error
	for range 32 {
		var execution continuation.Execution
		execution, err = c.engine.Get(ctx, c.id)
		if err != nil {
			break
		}
		if execution.Status.Terminal() {
			break
		}
		if clearGoal {
			_, err = c.engine.Cancel(ctx, c.id, execution.Revision, "Goal cleared by user")
		} else {
			_, err = c.engine.Pause(ctx, c.id, execution.Revision, "Goal paused by user")
		}
		if !errors.Is(err, continuation.ErrConflict) {
			break
		}
	}
	if err == nil && clearGoal {
		c.ledger.Cleared = true
		err = c.saveLedger()
	}
	roots := slices.Clone(c.ledger.Roots)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	r.mu.Lock()
	driver := r.goalDriver
	r.mu.Unlock()
	if driver != nil {
		driver.cancel()
	}
	if r.subagents != nil && (driver != nil || clearGoal) {
		for _, root := range roots {
			r.subagents.CancelRoot(root)
		}
	}
	return r.publishGoal(ctx, nil, "")
}

func (r *Runtime) beginGoalDriver(parent context.Context) (context.Context, *runtimeGoalDriver, error) {
	if r == nil {
		return nil, nil, ErrRuntimeClosed
	}
	if err := parent.Err(); err != nil {
		return nil, nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.closing {
		return nil, nil, ErrRuntimeClosed
	}
	if r.goalControl == nil || r.profile == profileTeamWorker {
		return nil, nil, ErrRuntimeInvalid
	}
	if r.goalDriver != nil || r.active != nil {
		return nil, nil, ErrRuntimeBusy
	}
	ctx, cancel := context.WithCancel(parent)
	driver := &runtimeGoalDriver{cancel: cancel, done: make(chan struct{})}
	r.goalDriver = driver
	return context.WithValue(ctx, goalContextKey{}, driver), driver, nil
}

func (r *Runtime) endGoalDriver(driver *runtimeGoalDriver) {
	driver.cancel()
	r.mu.Lock()
	if r.goalDriver == driver {
		r.goalDriver = nil
	}
	close(driver.done)
	r.mu.Unlock()
	r.signalNotifications()
}

//nolint:gocyclo // One owner spans all durable Work/Decision transitions.
func (r *Runtime) driveGoal(
	parent context.Context, request *GoalRequest, operation goalWorkOperation, yield func(Event, error) bool,
) {
	ctx, driver, err := r.beginGoalDriver(parent)
	if err != nil {
		yield(Event{}, err)
		return
	}
	defer r.endGoalDriver(driver)
	emitter := newEventEmitter(ctx, r, yield, true)
	defer emitter.detachConsumer()
	c := r.goalControl
	c.mu.Lock()
	var execution continuation.Execution
	if request != nil {
		execution, err = r.createGoal(ctx, *request)
	} else {
		execution, err = r.activateGoal(ctx, operation)
	}
	c.mu.Unlock()
	if err != nil {
		emitter.fail(err)
		return
	}
	if err := r.publishGoal(ctx, emitter, ""); err != nil {
		return
	}
	verifier, err := goalflow.NewVerifier(ctx, r.model, r.tree)
	if err != nil {
		emitter.fail(err)
		return
	}
	evaluator, err := goal.NewModelEvaluator(r.model)
	if err != nil {
		emitter.fail(err)
		return
	}
	controller := &goalflow.Controller{
		Evaluator: evaluator, Verifier: verifier, FailedUsage: c.recordFailedUsage, KnownFailed: c.failedUsage,
		Invalidated: func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.ledger.Invalidated },
		Phase:       func(phase string) error { return r.publishGoal(ctx, emitter, GoalStatus(phase)) },
	}
	worker := &runtimeGoalWorker{runtime: r, operation: operation, emitter: emitter}
	handlers := continuation.Handlers{
		WorkerRef: goalWorkerRef, Worker: worker, ControllerRef: goalControllerRef, Controller: controller,
	}
	for execution.Status == continuation.StatusReady {
		if err := ctx.Err(); err != nil {
			break
		}
		if err = r.observeGoalChildUsage(ctx); err != nil {
			break
		}
		failed := c.failedUsage()
		if execution.Limits.MaxTokens > 0 && execution.Accounting.Tokens()+failed.InputTokens+failed.OutputTokens >= execution.Limits.MaxTokens {
			_, err = c.engine.Fail(context.WithoutCancel(ctx), execution.ID, execution.Revision, "Observed Goal token budget exhausted")
			break
		}
		execution, err = c.engine.Advance(ctx, execution.ID, execution.Revision, handlers)
		publishErr := r.publishGoal(ctx, emitter, "")
		if err != nil || publishErr != nil {
			err = errors.Join(err, publishErr)
			break
		}
	}
	if ctx.Err() != nil {
		// A consumer or host cancellation is never permission to launch another
		// segment. Explicit Pause/Clear already persisted their stronger intent.
		c.mu.Lock()
		current, loadErr := c.engine.Get(context.WithoutCancel(ctx), c.id)
		if loadErr == nil && current.Status == continuation.StatusReady {
			_, loadErr = c.engine.Pause(context.WithoutCancel(ctx), current.ID, current.Revision, "Goal driver stopped; explicit resume required")
		}
		c.mu.Unlock()
		err = errors.Join(err, loadErr)
	}
	_ = r.publishGoal(context.WithoutCancel(ctx), emitter, "")
	if err != nil && !errors.Is(err, errConsumerStopped) {
		emitter.fail(err)
	}
}

// createGoal runs under the control mutex but never performs model/tool I/O.
func (r *Runtime) createGoal(ctx context.Context, request GoalRequest) (continuation.Execution, error) {
	c := r.goalControl
	r.mu.Lock()
	idle := r.state.Phase == PhaseIdle && r.interaction == nil && r.recovery.PendingID == ""
	r.mu.Unlock()
	if !idle {
		return continuation.Execution{}, ErrRuntimePending
	}
	if c.id != "" && !c.ledger.Cleared {
		existing, err := c.engine.Get(ctx, c.id)
		if err != nil {
			return continuation.Execution{}, err
		}
		if !existing.Status.Terminal() {
			return continuation.Execution{}, ErrGoalExists
		}
	}
	if !r.model.Capabilities().StructuredOutput {
		return continuation.Execution{}, fmt.Errorf("%w: Goal requires structured output", ErrRuntimeInvalid)
	}
	state, input, err := goalflow.Prepare(request.Condition)
	if err != nil {
		return continuation.Execution{}, err
	}
	encoded, err := goalflow.Encode(state)
	if err != nil {
		return continuation.Execution{}, err
	}
	// The non-context binding materializes a provisional session, but carries no
	// lifecycle authority. Forks may copy it; only this session's sidecar is read.
	if _, err := r.session.AppendCustom("pips.coding.goal.binding/v1", ai.JSON(`{"version":1}`)); err != nil {
		return continuation.Execution{}, err
	}
	execution, err := c.engine.Create(ctx, continuation.CreateRequest{
		Target: continuation.Target{Kind: "coding.session", ID: r.handle.Metadata().ID},
		Worker: goalWorkerRef, Controller: goalControllerRef, ControllerState: encoded, Input: input,
		Limits: continuation.Limits{MaxTokens: request.MaxTokens},
	})
	if err != nil {
		return continuation.Execution{}, err
	}
	c.id = execution.ID
	revoked := append(slices.Clone(c.ledger.RevokedRoots), c.ledger.Roots...)
	c.ledger = goalLedger{Version: 1, ID: execution.ID, RevokedRoots: revoked}
	return execution, c.saveLedger()
}

//nolint:gocyclo // Resume preserves distinct pause, retry, pending and signal semantics.
func (r *Runtime) activateGoal(ctx context.Context, operation goalWorkOperation) (continuation.Execution, error) {
	c := r.goalControl
	if c.id == "" || c.ledger.Cleared {
		return continuation.Execution{}, ErrGoalMissing
	}
	execution, err := c.engine.Get(ctx, c.id)
	if err != nil {
		return execution, err
	}
	if execution.Status.Terminal() {
		return execution, continuation.ErrTerminal
	}
	if operation.kind != "" && execution.Status == continuation.StatusPaused {
		return execution, ErrRuntimePending
	}
	if operation.notification != nil && execution.Status != continuation.StatusWaiting {
		return execution, ErrRuntimeBusy
	}
	if execution.Status == continuation.StatusPaused {
		execution, err = c.engine.Resume(ctx, c.id, execution.Revision, "Goal explicitly resumed")
		if errors.Is(err, continuation.ErrRetryRequired) {
			execution, err = c.engine.RetryWork(ctx, c.id, execution.Revision, "User explicitly retries interrupted Goal work")
		}
		if err != nil {
			return execution, err
		}
	}
	switch execution.Status {
	case continuation.StatusInterrupted:
		if execution.Phase == continuation.PhaseDecision {
			return c.engine.RetryDecision(ctx, c.id, execution.Revision, "Retry independent assessment only")
		}
		return c.engine.RetryWork(ctx, c.id, execution.Revision, "User explicitly retries interrupted Goal work")
	case continuation.StatusBlocked:
		r.mu.Lock()
		parked := r.state.Phase == PhasePaused && r.recovery.PendingID == ""
		r.mu.Unlock()
		if parked && operation.kind == "" {
			return execution, nil
		}
		return c.engine.ResolveBlock(ctx, c.id, execution.Revision, ai.JSON(`{"explicit":true}`))
	case continuation.StatusWaiting:
		if operation.notification == nil {
			return execution, nil
		}
		return c.engine.Signal(ctx, c.id, execution.Revision, continuation.Signal{
			ID: strings.Join(operation.notification.notificationIDs, "-"), Key: "coding.goal.background",
		})
	default:
		return execution, nil
	}
}

func (r *Runtime) invalidateGoalEvidence(ctx context.Context) error {
	if !r.GoalSnapshot().Active() {
		return nil
	}
	if err := r.PauseGoal(ctx); err != nil {
		return err
	}
	c := r.goalControl
	c.mu.Lock()
	c.ledger.Invalidated = true
	err := c.saveLedger()
	c.mu.Unlock()
	return err
}

//nolint:exhaustive // Only interaction operations may enter the Goal driver.
func (r *Runtime) goalOperation(ctx context.Context, operation goalWorkOperation, yield func(Event, error) bool) bool {
	if ctx.Value(goalContextKey{}) != nil {
		return false
	}
	snapshot := r.GoalSnapshot()
	if !snapshot.Active() {
		return false
	}
	switch operation.kind {
	case operationResolve, operationResolveQuestion, operationResolvePlanReview, operationRejectQuestion, operationContinue:
		if snapshot.Status == GoalBlocked {
			r.driveGoal(ctx, nil, operation, yield)
			return true
		}
		yield(Event{}, fmt.Errorf("%w: explicitly resume the Goal before continuing pending work", ErrRuntimePending))
		return true
	case operationAgentNotification:
		if snapshot.Status == GoalWaiting {
			r.driveGoal(ctx, nil, operation, yield)
			return true
		}
		yield(Event{}, ErrRuntimeBusy)
		return true
	case operationPrompt:
		yield(Event{}, fmt.Errorf("%w: use Steer/FollowUp while Goal work is active, or clear the Goal", ErrRuntimePending))
		return true
	}
	return false
}
