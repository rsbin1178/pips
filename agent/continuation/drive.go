package continuation

import (
	"context"
	"errors"
	"fmt"
)

// Drive synchronously repeats Advance within a mandatory finite quantum.
func (engine *Engine) Drive(
	ctx context.Context,
	id ID,
	expected Revision,
	handlers Handlers,
	options DriveOptions,
) (DriveResult, error) {
	if options.MaxAdvances <= 0 {
		return DriveResult{}, fmt.Errorf("%w: MaxAdvances must be positive", ErrInvalid)
	}

	if err := validateExpectedRevision(expected); err != nil {
		return DriveResult{}, err
	}

	if err := validateHandlers(handlers); err != nil {
		return DriveResult{}, err
	}

	execution, err := engine.Get(ctx, id)
	if err != nil {
		return DriveResult{}, err
	}

	if execution.Revision != expected {
		return DriveResult{Execution: execution}, &ConflictError{Expected: expected, Actual: execution.Revision}
	}

	result := DriveResult{Execution: execution}
	for range options.MaxAdvances {
		stop, stepErr := engine.driveStep(ctx, id, handlers, options.Gate, &result)
		if stop || stepErr != nil {
			return result, stepErr
		}
	}

	result.Yield = YieldQuantum

	return result, nil
}

func (engine *Engine) driveStep(
	ctx context.Context,
	id ID,
	handlers Handlers,
	gate Gate,
	result *DriveResult,
) (bool, error) {
	if yield := statusYield(result.Execution); yield != "" {
		result.Yield = yield

		return true, nil
	}

	if err := ctx.Err(); err != nil {
		result.Yield = YieldContext

		return true, err
	}

	if allowed, err := driveGate(ctx, gate, result.Execution); err != nil || !allowed {
		if err != nil {
			result.Yield = YieldError

			return true, err
		}

		result.Yield = YieldGate

		return true, nil
	}

	next, err := engine.Advance(ctx, id, result.Execution.Revision, handlers)
	result.Execution = next

	result.Advances++
	if err != nil {
		result.Yield = advanceErrorYield(next, err)

		return true, err
	}

	if yield := statusYield(next); yield != "" {
		result.Yield = yield

		return true, nil
	}

	if noProgress(next) {
		result.Yield = YieldNoProgress

		return true, nil
	}

	return false, nil
}

func driveGate(ctx context.Context, gate Gate, execution Execution) (bool, error) {
	if gate == nil {
		return true, nil
	}

	return gate(ctx, cloneExecution(execution))
}

func advanceErrorYield(execution Execution, err error) YieldReason {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return YieldContext
	}

	if execution.Status == StatusInterrupted {
		return YieldInterrupted
	}

	return YieldError
}

func statusYield(execution Execution) YieldReason {
	switch execution.Status {
	case StatusCompleted, StatusFailed, StatusCancelled, StatusLimited:
		return YieldTerminal
	case StatusWaiting:
		return YieldWaiting
	case StatusPaused, StatusPauseRequested:
		return YieldPaused
	case StatusBlocked:
		return YieldBlocked
	case StatusInterrupted:
		return YieldInterrupted
	case StatusReady:
		return ""
	case StatusRunning, StatusCancelRequested:
		return YieldError
	default:
		return YieldError
	}
}

func noProgress(execution Execution) bool {
	if execution.Status != StatusReady || execution.LastAttempt == nil || execution.LastAttempt.Decision == nil {
		return false
	}

	decision := execution.LastAttempt.Decision
	if decision.Action != ActionContinue {
		return false
	}

	if decision.Progress == ProgressUnchanged {
		return true
	}

	return decision.Progress == "" && execution.LastAttempt.Work != nil &&
		execution.LastAttempt.Work.Progress == ProgressUnchanged
}
