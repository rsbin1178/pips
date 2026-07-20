package continuation

import (
	"context"
	"errors"
	"fmt"

	"github.com/rsbin/pips/ai"
)

// Pause durably requests suspension and cancels a locally active stage.
func (engine *Engine) Pause(
	ctx context.Context,
	id ID,
	expected Revision,
	reason string,
) (Execution, error) {
	current, err := engine.controlRecord(ctx, id, expected)
	if err != nil {
		return controlExecution(current), err
	}

	if err := validateReason(reason); err != nil {
		return cloneExecution(current.Execution), err
	}

	if current.Execution.Status == StatusPaused || current.Execution.Status == StatusPauseRequested {
		return cloneExecution(current.Execution), nil
	}

	if current.Execution.Status.Terminal() {
		return cloneExecution(current.Execution), stateError("pause", current.Execution, ErrTerminal)
	}

	if current.Execution.Status == StatusCancelRequested {
		return cloneExecution(current.Execution), stateError("pause", current.Execution, ErrNotRunnable)
	}

	execution := cloneExecution(current.Execution)
	cause := CausePause

	if current.Execution.Status == StatusRunning {
		execution.Status = StatusPauseRequested
		cause = CausePauseRequested
	} else {
		execution.Suspension = &Suspension{
			Status: current.Execution.Status, Phase: current.Execution.Phase,
			Wait: cloneWait(current.Execution.Wait), Block: cloneBlock(current.Execution.Block),
			RetryRequired: current.Execution.Status == StatusInterrupted && current.Execution.Phase == PhaseWork,
			Reason:        current.Execution.Reason,
		}
		execution.Status = StatusPaused
		execution.Wait = nil
		execution.Block = nil
	}

	execution.Reason = reason

	next := engine.nextRecord(current, execution, cause, reason)
	if err := engine.store.CompareAndSwap(ctx, id, expected, next); err != nil {
		return cloneExecution(current.Execution), err
	}

	engine.cancelActive(id)

	return cloneExecution(next.Execution), nil
}

// Resume restores a Paused execution without bypassing waits or retries.
func (engine *Engine) Resume(
	ctx context.Context,
	id ID,
	expected Revision,
	reason string,
) (Execution, error) {
	current, err := engine.controlRecord(ctx, id, expected)
	if err != nil {
		return controlExecution(current), err
	}

	if err := validateReason(reason); err != nil {
		return cloneExecution(current.Execution), err
	}

	if current.Execution.Status.Terminal() {
		return cloneExecution(current.Execution), stateError("resume", current.Execution, ErrTerminal)
	}

	if current.Execution.Status != StatusPaused || current.Execution.Suspension == nil {
		return cloneExecution(current.Execution), stateError("resume", current.Execution, ErrNotRunnable)
	}

	if current.Execution.Suspension.RetryRequired {
		return cloneExecution(current.Execution), stateError("resume", current.Execution, ErrRetryRequired)
	}

	suspension := current.Execution.Suspension
	execution := cloneExecution(current.Execution)
	execution.Status = suspension.Status
	execution.Phase = suspension.Phase
	execution.Wait = cloneWait(suspension.Wait)
	execution.Block = cloneBlock(suspension.Block)
	execution.Suspension = nil
	execution.Reason = reason

	next := engine.nextRecord(current, execution, CauseResume, reason)
	if err := engine.store.CompareAndSwap(ctx, id, expected, next); err != nil {
		return cloneExecution(current.Execution), err
	}

	return cloneExecution(next.Execution), nil
}

// RetryWork explicitly permits a new Attempt after interrupted Work.
func (engine *Engine) RetryWork(
	ctx context.Context,
	id ID,
	expected Revision,
	reason string,
) (Execution, error) {
	current, err := engine.controlRecord(ctx, id, expected)
	if err != nil {
		return controlExecution(current), err
	}

	if err := validateReason(reason); err != nil {
		return cloneExecution(current.Execution), err
	}

	if current.Execution.Status.Terminal() {
		return cloneExecution(current.Execution), stateError("retry work", current.Execution, ErrTerminal)
	}

	retryable := current.Execution.Status == StatusInterrupted && current.Execution.Phase == PhaseWork
	if current.Execution.Status == StatusPaused && current.Execution.Suspension != nil {
		retryable = current.Execution.Suspension.Phase == PhaseWork && current.Execution.Suspension.RetryRequired
	}

	if !retryable {
		return cloneExecution(current.Execution), stateError("retry work", current.Execution, ErrNotRunnable)
	}

	execution := cloneExecution(current.Execution)
	if execution.CurrentAttempt != nil {
		execution.LastAttempt = cloneAttempt(execution.CurrentAttempt)
		execution.CurrentAttempt = nil
	}

	execution.Status = StatusReady
	execution.Phase = PhaseWork
	execution.Suspension = nil
	execution.Reason = reason

	next := engine.nextRecord(current, execution, CauseRetryWork, reason)
	if err := engine.store.CompareAndSwap(ctx, id, expected, next); err != nil {
		return cloneExecution(current.Execution), err
	}

	return cloneExecution(next.Execution), nil
}

// RetryDecision re-evaluates the same durable Work result.
func (engine *Engine) RetryDecision(
	ctx context.Context,
	id ID,
	expected Revision,
	reason string,
) (Execution, error) {
	current, err := engine.controlRecord(ctx, id, expected)
	if err != nil {
		return controlExecution(current), err
	}

	if err := validateReason(reason); err != nil {
		return cloneExecution(current.Execution), err
	}

	if current.Execution.Status.Terminal() {
		return cloneExecution(current.Execution), stateError("retry decision", current.Execution, ErrTerminal)
	}

	if current.Execution.Status != StatusInterrupted || current.Execution.Phase != PhaseDecision ||
		current.Execution.CurrentAttempt == nil || current.Execution.CurrentAttempt.Work == nil {
		return cloneExecution(current.Execution), stateError("retry decision", current.Execution, ErrNotRunnable)
	}

	execution := cloneExecution(current.Execution)
	execution.Status = StatusReady
	execution.Phase = PhaseDecision
	execution.CurrentAttempt.Interrupted = false
	execution.CurrentAttempt.Failure = nil
	execution.Reason = reason

	next := engine.nextRecord(current, execution, CauseRetryDecision, reason)
	if err := engine.store.CompareAndSwap(ctx, id, expected, next); err != nil {
		return cloneExecution(current.Execution), err
	}

	return cloneExecution(next.Execution), nil
}

// ResolveBlock supplies external input and starts a new Work attempt later.
func (engine *Engine) ResolveBlock(
	ctx context.Context,
	id ID,
	expected Revision,
	payload ai.JSON,
) (Execution, error) {
	current, err := engine.controlRecord(ctx, id, expected)
	if err != nil {
		return controlExecution(current), err
	}

	if err := validateJSON("block resolution", payload, defaultMaxJSONBytes); err != nil {
		return cloneExecution(current.Execution), err
	}

	if current.Execution.Status.Terminal() {
		return cloneExecution(current.Execution), stateError("resolve block", current.Execution, ErrTerminal)
	}

	if current.Execution.Status != StatusBlocked {
		return cloneExecution(current.Execution), stateError("resolve block", current.Execution, ErrNotRunnable)
	}

	execution := cloneExecution(current.Execution)
	execution.Status = StatusReady
	execution.Phase = PhaseWork
	execution.Activation = &Activation{Source: ActivationBlock, At: utc(engine.clock.Now()), Payload: cloneJSON(payload)}
	execution.Block = nil
	execution.Reason = ""

	next := engine.nextRecord(current, execution, CauseBlockResolved, "")
	if err := engine.store.CompareAndSwap(ctx, id, expected, next); err != nil {
		return cloneExecution(current.Execution), err
	}

	return cloneExecution(next.Execution), nil
}

// Signal delivers one exact idempotent external wakeup.
func (engine *Engine) Signal(
	ctx context.Context,
	id ID,
	expected Revision,
	signal Signal,
) (Execution, error) {
	if err := validateSignal(signal); err != nil {
		return Execution{}, err
	}

	duplicate, found, err := engine.duplicateSignal(ctx, id, signal.ID)
	if err != nil {
		return Execution{}, err
	}

	if found {
		return duplicate, nil
	}

	current, err := engine.controlRecord(ctx, id, expected)
	if err != nil {
		return controlExecution(current), err
	}

	if err := validateSignalTarget(current.Execution, signal.Key); err != nil {
		return cloneExecution(current.Execution), err
	}

	execution := cloneExecution(current.Execution)
	execution.Status = StatusReady
	execution.Phase = PhaseWork
	execution.Activation = &Activation{
		Source: ActivationSignal, At: utc(engine.clock.Now()), SignalID: signal.ID,
		Payload: cloneJSON(signal.Payload),
	}
	execution.Wait = nil
	execution.Reason = ""
	next := engine.nextRecord(current, execution, CauseSignal, "")

	next.Transition.SignalID = signal.ID
	if err := engine.store.CompareAndSwap(ctx, id, expected, next); err != nil {
		if errors.Is(err, ErrConflict) {
			return engine.signalConflict(ctx, id, signal.ID, err)
		}

		return cloneExecution(current.Execution), err
	}

	return cloneExecution(next.Execution), nil
}

func validateSignal(signal Signal) error {
	if signal.ID == "" || signal.Key == "" {
		return fmt.Errorf("%w: signal ID and key are required", ErrInvalid)
	}

	if len(signal.ID) > maxIDLength || len(signal.Key) > maxRefLength {
		return fmt.Errorf("%w: signal ID or key too large", ErrTooLarge)
	}

	return validateJSON("signal payload", signal.Payload, defaultMaxJSONBytes)
}

func (engine *Engine) duplicateSignal(ctx context.Context, id ID, signalID string) (Execution, bool, error) {
	history, err := engine.store.History(ctx, id)
	if err != nil {
		return Execution{}, false, err
	}

	for _, record := range history {
		if record.Transition.Cause == CauseSignal && record.Transition.SignalID == signalID {
			latest, loadErr := engine.store.Load(ctx, id)
			if loadErr != nil {
				return Execution{}, false, loadErr
			}

			return cloneExecution(latest.Execution), true, nil
		}
	}

	return Execution{}, false, nil
}

func validateSignalTarget(execution Execution, key string) error {
	if execution.Status.Terminal() {
		return stateError("signal", execution, ErrTerminal)
	}

	if execution.Status != StatusWaiting || execution.Wait == nil {
		return stateError("signal", execution, ErrNotWaiting)
	}

	if execution.Wait.Signal == nil || execution.Wait.Signal.Key != key {
		return ErrSignalMismatch
	}

	return nil
}

// ResumeDue explicitly wakes a wait whose NotBefore time has arrived.
func (engine *Engine) ResumeDue(
	ctx context.Context,
	id ID,
	expected Revision,
) (Execution, error) {
	current, err := engine.controlRecord(ctx, id, expected)
	if err != nil {
		return controlExecution(current), err
	}

	if current.Execution.Status.Terminal() {
		return cloneExecution(current.Execution), stateError("resume due", current.Execution, ErrTerminal)
	}

	if current.Execution.Status != StatusWaiting || current.Execution.Wait == nil {
		return cloneExecution(current.Execution), stateError("resume due", current.Execution, ErrNotWaiting)
	}

	now := utc(engine.clock.Now())
	if current.Execution.Wait.NotBefore == nil || now.Before(*current.Execution.Wait.NotBefore) {
		return cloneExecution(current.Execution), ErrNotDue
	}

	execution := cloneExecution(current.Execution)
	execution.Status = StatusReady
	execution.Phase = PhaseWork
	execution.Activation = &Activation{Source: ActivationTime, At: now}
	execution.Wait = nil
	execution.Reason = ""

	next := engine.nextRecord(current, execution, CauseTimeWake, "")
	if err := engine.store.CompareAndSwap(ctx, id, expected, next); err != nil {
		return cloneExecution(current.Execution), err
	}

	return cloneExecution(next.Execution), nil
}

// Cancel records product cancellation and cancels a locally active stage.
func (engine *Engine) Cancel(
	ctx context.Context,
	id ID,
	expected Revision,
	reason string,
) (Execution, error) {
	current, err := engine.controlRecord(ctx, id, expected)
	if err != nil {
		return controlExecution(current), err
	}

	if err := validateReason(reason); err != nil {
		return cloneExecution(current.Execution), err
	}

	if current.Execution.Status == StatusCancelled || current.Execution.Status == StatusCancelRequested {
		return cloneExecution(current.Execution), nil
	}

	if current.Execution.Status.Terminal() {
		return cloneExecution(current.Execution), stateError("cancel", current.Execution, ErrTerminal)
	}

	execution := cloneExecution(current.Execution)
	cause := CauseCancel

	if current.Execution.Status == StatusRunning || current.Execution.Status == StatusPauseRequested {
		execution.Status = StatusCancelRequested
		cause = CauseCancelRequested
	} else {
		execution.Status = StatusCancelled
		execution.Wait = nil
		execution.Block = nil
		execution.Activation = nil
		execution.Suspension = nil
	}

	execution.Reason = reason

	next := engine.nextRecord(current, execution, cause, reason)
	if err := engine.store.CompareAndSwap(ctx, id, expected, next); err != nil {
		return cloneExecution(current.Execution), err
	}

	engine.cancelActive(id)

	return cloneExecution(next.Execution), nil
}

// Fail records an explicit caller-requested terminal failure.
func (engine *Engine) Fail(
	ctx context.Context,
	id ID,
	expected Revision,
	reason string,
) (Execution, error) {
	current, err := engine.controlRecord(ctx, id, expected)
	if err != nil {
		return controlExecution(current), err
	}

	if err := validateReason(reason); err != nil {
		return cloneExecution(current.Execution), err
	}

	if current.Execution.Status.Terminal() {
		return cloneExecution(current.Execution), stateError("fail", current.Execution, ErrTerminal)
	}

	execution := cloneExecution(current.Execution)
	execution.Status = StatusFailed
	execution.Reason = reason
	execution.Wait = nil
	execution.Block = nil
	execution.Activation = nil
	execution.Suspension = nil

	next := engine.nextRecord(current, execution, CauseFail, reason)
	if err := engine.store.CompareAndSwap(ctx, id, expected, next); err != nil {
		return cloneExecution(current.Execution), err
	}

	engine.cancelActive(id)

	return cloneExecution(next.Execution), nil
}

func (engine *Engine) controlRecord(
	ctx context.Context,
	id ID,
	expected Revision,
) (Record, error) {
	if err := validateExpectedRevision(expected); err != nil {
		return Record{}, err
	}

	current, err := engine.loadExpected(ctx, id, expected)
	if err != nil {
		return Record{}, err
	}

	recovered, err := engine.recover(ctx, current)
	if err != nil {
		return current, err
	}

	if recovered.Execution.Revision != expected {
		return recovered, &ConflictError{Expected: expected, Actual: recovered.Execution.Revision}
	}

	return recovered, nil
}

func (engine *Engine) cancelActive(id ID) {
	stage, exists := engine.activeStage(id)
	if exists {
		stage.cancel()
	}
}

func (engine *Engine) signalConflict(
	ctx context.Context,
	id ID,
	signalID string,
	conflict error,
) (Execution, error) {
	history, err := engine.store.History(ctx, id)
	if err != nil {
		return Execution{}, err
	}

	for _, record := range history {
		if record.Transition.Cause == CauseSignal && record.Transition.SignalID == signalID {
			return cloneExecution(history[len(history)-1].Execution), nil
		}
	}

	return cloneExecution(history[len(history)-1].Execution), conflict
}

func controlExecution(record Record) Execution {
	if record.Execution.ID == "" {
		return Execution{}
	}

	return cloneExecution(record.Execution)
}
