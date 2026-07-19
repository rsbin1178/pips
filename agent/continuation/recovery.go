package continuation

import (
	"context"
	"errors"
	"fmt"
	"time"
)

func (engine *Engine) recover(ctx context.Context, current Record) (Record, error) {
	status := current.Execution.Status
	if status != StatusRunning && status != StatusPauseRequested && status != StatusCancelRequested {
		return current, nil
	}

	if _, active := engine.activeStage(current.Execution.ID); active {
		return current, nil
	}

	execution := cloneExecution(current.Execution)
	now := utc(engine.clock.Now())
	chargeOrphanDuration(&execution, now)

	switch status {
	case StatusRunning:
		execution.Status = StatusInterrupted
		markAttemptInterrupted(&execution, "recovered", "active stage owner is no longer present")
	case StatusPauseRequested:
		execution.Status = StatusPaused
		execution.Suspension = recoverySuspension(execution)
		markAttemptInterrupted(&execution, "paused", "active stage stopped during pause")
	case StatusCancelRequested:
		execution.Status = StatusCancelled
		execution.Wait = nil
		execution.Block = nil
		execution.Activation = nil
		execution.Suspension = nil
	default:
		return current, nil
	}

	next := engine.nextRecord(current, execution, CauseRecovery, "reconciled orphaned active stage")
	if err := engine.store.CompareAndSwap(ctx, execution.ID, current.Execution.Revision, next); err != nil {
		conflictError := &ConflictError{}
		if errors.As(err, &conflictError) {
			latest, loadErr := engine.store.Load(ctx, execution.ID)
			if loadErr != nil {
				return Record{}, loadErr
			}

			return latest, nil
		}

		return Record{}, err
	}

	return next, nil
}

func recoverySuspension(execution Execution) *Suspension {
	retryRequired := execution.Phase == PhaseWork
	if execution.CurrentAttempt != nil && execution.CurrentAttempt.Work != nil {
		retryRequired = false
	}

	return &Suspension{
		Status: StatusReady, Phase: execution.Phase,
		Wait: cloneWait(execution.Wait), Block: cloneBlock(execution.Block),
		RetryRequired: retryRequired, Reason: execution.Reason,
	}
}

func chargeOrphanDuration(execution *Execution, now time.Time) {
	if execution.CurrentAttempt == nil {
		return
	}

	started := execution.CurrentAttempt.WorkStartedAt
	if execution.Phase == PhaseDecision && !execution.CurrentAttempt.DecisionStartedAt.IsZero() {
		started = execution.CurrentAttempt.DecisionStartedAt
	}

	if !started.IsZero() && now.After(started) {
		execution.Accounting.ActiveDuration += now.Sub(started)
	}
}

func markAttemptInterrupted(execution *Execution, code, message string) {
	if execution.CurrentAttempt == nil {
		return
	}

	execution.CurrentAttempt.Interrupted = true
	execution.CurrentAttempt.Failure = &StageFailure{Code: code, Message: message}
}

func stateError(operation string, execution Execution, err error) error {
	if err == nil {
		err = ErrNotRunnable
	}

	return &StateError{Operation: operation, Status: execution.Status, Phase: execution.Phase, Err: err}
}

func validateExpectedRevision(expected Revision) error {
	if expected == 0 {
		return fmt.Errorf("%w: expected revision must be non-zero", ErrInvalid)
	}

	return nil
}
