package continuation

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const maxControlReconciliations = 16

// Advance performs at most one Worker invocation and its Controller decision.
func (engine *Engine) Advance(
	ctx context.Context,
	id ID,
	expected Revision,
	handlers Handlers,
) (Execution, error) {
	if err := validateAdvanceRequest(expected, handlers); err != nil {
		return Execution{}, err
	}

	current, err := engine.prepareAdvance(ctx, id, expected, handlers)
	if err != nil {
		return controlExecution(current), err
	}

	limited, limitedErr := engine.applyLimit(ctx, current)
	if limitedErr != nil {
		return cloneExecution(current.Execution), limitedErr
	}

	if limited.Execution.Status == StatusLimited {
		return cloneExecution(limited.Execution), nil
	}

	switch limited.Execution.Phase {
	case PhaseWork:
		return engine.advanceWork(ctx, limited, handlers)
	case PhaseDecision:
		return engine.advanceDecision(ctx, limited, handlers)
	default:
		return cloneExecution(limited.Execution), fmt.Errorf("%w: unknown phase", ErrInvalid)
	}
}

func validateAdvanceRequest(expected Revision, handlers Handlers) error {
	if err := validateExpectedRevision(expected); err != nil {
		return err
	}

	return validateHandlers(handlers)
}

func (engine *Engine) prepareAdvance(
	ctx context.Context,
	id ID,
	expected Revision,
	handlers Handlers,
) (Record, error) {
	if _, active := engine.activeStage(id); active {
		current, err := engine.store.Load(ctx, id)
		if err != nil {
			return Record{}, err
		}

		return current, ErrBusy
	}

	current, err := engine.loadExpected(ctx, id, expected)
	if err != nil {
		return Record{}, err
	}

	current, err = engine.recover(ctx, current)
	if err != nil {
		return current, err
	}

	if current.Execution.Revision != expected {
		return current, &ConflictError{Expected: expected, Actual: current.Execution.Revision}
	}

	if err := matchHandlers(current.Execution, handlers); err != nil {
		return current, err
	}

	if current.Execution.Status.Terminal() {
		return current, stateError("advance", current.Execution, ErrTerminal)
	}

	if current.Execution.Status != StatusReady {
		return current, stateError("advance", current.Execution, runnableError(current.Execution))
	}

	return current, nil
}

func (engine *Engine) advanceWork(
	ctx context.Context,
	current Record,
	handlers Handlers,
) (Execution, error) {
	now := utc(engine.clock.Now())

	rawAttemptID, err := engine.ids("attempt", now)
	if err != nil {
		return cloneExecution(current.Execution), fmt.Errorf("continuation: generate attempt ID: %w", err)
	}

	attemptID := AttemptID(rawAttemptID)
	if err := validateAttemptID(attemptID); err != nil {
		return cloneExecution(current.Execution), err
	}

	stageContext, cancel, err := engine.reserve(ctx, current.Execution.ID, PhaseWork, attemptID)
	if err != nil {
		return cloneExecution(current.Execution), err
	}

	releasePending := true
	defer func() {
		if releasePending {
			engine.release(current.Execution.ID, cancel)
		}
	}()

	execution := cloneExecution(current.Execution)
	if execution.CurrentAttempt != nil {
		execution.LastAttempt = cloneAttempt(execution.CurrentAttempt)
	}

	execution.CurrentAttempt = &Attempt{
		ID: attemptID, Number: execution.Accounting.Attempts + 1, Phase: PhaseWork,
		Activation: cloneActivation(execution.Activation), WorkStartedAt: now,
	}
	execution.Activation = nil
	execution.Status = StatusRunning
	execution.Phase = PhaseWork
	execution.Accounting.Attempts++

	running := engine.nextRecord(current, execution, CauseStageStart, "")
	if err := engine.store.CompareAndSwap(ctx, execution.ID, current.Execution.Revision, running); err != nil {
		return cloneExecution(current.Execution), err
	}

	request := WorkRequest{
		ExecutionID: execution.ID, AttemptID: attemptID, Attempt: execution.CurrentAttempt.Number,
		Target: execution.Target, Input: cloneJSON(execution.NextInput),
		Activation: cloneActivation(execution.CurrentAttempt.Activation), Limits: execution.Limits,
		Accounting: execution.Accounting, Remaining: remaining(execution.Limits, execution.Accounting, now),
	}

	result, workerErr := normalizeWorkOutcome(handlers.Worker.Run(stageContext, request))

	finished, persistErr := engine.finishWork(context.WithoutCancel(ctx), running, result, workerErr)
	engine.release(current.Execution.ID, cancel)

	releasePending = false

	if persistErr != nil {
		return cloneExecution(finished.Execution), persistErr
	}

	if workerErr != nil {
		return cloneExecution(finished.Execution), workerErr
	}

	if finished.Execution.Status != StatusReady || finished.Execution.Phase != PhaseDecision {
		return cloneExecution(finished.Execution), nil
	}

	limited, limitErr := engine.applyLimit(context.WithoutCancel(ctx), finished)
	if limitErr != nil {
		return cloneExecution(finished.Execution), limitErr
	}

	if limited.Execution.Status == StatusLimited {
		return cloneExecution(limited.Execution), nil
	}

	return engine.advanceDecision(ctx, limited, handlers)
}

func (engine *Engine) finishWork(
	ctx context.Context,
	running Record,
	result WorkResult,
	workerErr error,
) (Record, error) {
	return engine.finishStage(ctx, running, "work", func(execution Execution) (Execution, Cause) {
		return engine.workOutcome(execution, result, workerErr)
	})
}

func (engine *Engine) workOutcome(execution Execution, result WorkResult, workerErr error) (Execution, Cause) {
	now := utc(engine.clock.Now())
	execution = cloneExecution(execution)

	attempt := execution.CurrentAttempt
	if attempt == nil {
		return execution, CauseStageInterrupted
	}

	chargeStage(&execution, attempt.WorkStartedAt, now)
	attempt.Phase = PhaseWork
	attempt.WorkCompletedAt = now
	attempt.Work = cloneWorkResult(result)
	addWorkAccounting(&execution.Accounting, result)

	switch execution.Status {
	case StatusCancelRequested:
		execution.Status = StatusCancelled
		execution.Reason = "cancelled during work"
		execution.Wait = nil
		execution.Block = nil
		execution.Suspension = nil

		return execution, CauseCancel
	case StatusPauseRequested:
		execution.Status = StatusPaused
		if workerErr == nil {
			execution.Phase = PhaseDecision
			attempt.Phase = PhaseDecision
			execution.Suspension = &Suspension{Status: StatusReady, Phase: PhaseDecision}
			execution.Reason = "paused after work completion"
		} else {
			execution.Phase = PhaseWork
			attempt.Interrupted = true
			attempt.Failure = projectFailure("worker", workerErr)
			execution.Suspension = &Suspension{Status: StatusReady, Phase: PhaseWork, RetryRequired: true}
			execution.Reason = "work interrupted by pause"
		}

		return execution, CausePause
	case StatusRunning:
		if workerErr != nil {
			execution.Status = StatusInterrupted
			execution.Phase = PhaseWork
			attempt.Interrupted = true
			attempt.Failure = projectFailure("worker", workerErr)
			execution.Reason = "worker interrupted"

			return execution, CauseStageInterrupted
		}

		execution.Status = StatusReady
		execution.Phase = PhaseDecision
		attempt.Phase = PhaseDecision
		attempt.Failure = nil
		execution.Reason = ""

		return execution, CauseWorkComplete
	default:
		return execution, CauseStageInterrupted
	}
}

func (engine *Engine) advanceDecision(
	ctx context.Context,
	current Record,
	handlers Handlers,
) (Execution, error) {
	attempt := current.Execution.CurrentAttempt
	if attempt == nil || attempt.Work == nil {
		return cloneExecution(current.Execution), fmt.Errorf("%w: decision has no durable work result", ErrInvalid)
	}

	stageContext, cancel, err := engine.reserve(ctx, current.Execution.ID, PhaseDecision, attempt.ID)
	if err != nil {
		return cloneExecution(current.Execution), err
	}
	defer engine.release(current.Execution.ID, cancel)

	execution := cloneExecution(current.Execution)
	now := utc(engine.clock.Now())
	execution.Status = StatusRunning
	execution.Phase = PhaseDecision
	execution.CurrentAttempt.Phase = PhaseDecision
	execution.CurrentAttempt.DecisionStartedAt = now

	running := engine.nextRecord(current, execution, CauseStageStart, "")
	if err := engine.store.CompareAndSwap(ctx, execution.ID, current.Execution.Revision, running); err != nil {
		return cloneExecution(current.Execution), err
	}

	request := DecisionRequest{
		ExecutionID: execution.ID, AttemptID: attempt.ID, Attempt: attempt.Number,
		Target: execution.Target, Work: *cloneWorkResult(*attempt.Work),
		Activation: cloneActivation(attempt.Activation), ControllerState: cloneJSON(execution.ControllerState),
		Limits: execution.Limits, Accounting: execution.Accounting,
	}

	decision, controllerErr := handlers.Controller.Decide(stageContext, request)
	if validationErr := validateDecision(decision); validationErr != nil && controllerErr == nil {
		controllerErr = validationErr
	}

	finished, persistErr := engine.finishDecision(context.WithoutCancel(ctx), running, decision, controllerErr)
	if persistErr != nil {
		return cloneExecution(finished.Execution), persistErr
	}

	if controllerErr != nil {
		return cloneExecution(finished.Execution), controllerErr
	}

	if finished.Execution.Status != StatusReady {
		return cloneExecution(finished.Execution), nil
	}

	limited, limitErr := engine.applyLimit(context.WithoutCancel(ctx), finished)
	if limitErr != nil {
		return cloneExecution(finished.Execution), limitErr
	}

	return cloneExecution(limited.Execution), nil
}

func (engine *Engine) finishDecision(
	ctx context.Context,
	running Record,
	decision Decision,
	controllerErr error,
) (Record, error) {
	return engine.finishStage(ctx, running, "decision", func(execution Execution) (Execution, Cause) {
		return engine.decisionOutcome(execution, decision, controllerErr)
	})
}

func (engine *Engine) finishStage(
	ctx context.Context,
	running Record,
	stage string,
	outcome func(Execution) (Execution, Cause),
) (Record, error) {
	current := running
	for range maxControlReconciliations {
		latest, err := engine.store.Load(ctx, running.Execution.ID)
		if err != nil {
			return current, err
		}

		current = latest
		if latest.Execution.Status.Terminal() {
			return latest, nil
		}

		execution, cause := outcome(latest.Execution)

		next := engine.nextRecord(latest, execution, cause, execution.Reason)
		if err := engine.store.CompareAndSwap(ctx, execution.ID, latest.Execution.Revision, next); err != nil {
			if errors.Is(err, ErrConflict) {
				continue
			}

			return current, err
		}

		return next, nil
	}

	return current, fmt.Errorf("%w: %s completion control contention", ErrConflict, stage)
}

func (engine *Engine) decisionOutcome(execution Execution, decision Decision, controllerErr error) (Execution, Cause) {
	now := utc(engine.clock.Now())
	execution = cloneExecution(execution)

	attempt := execution.CurrentAttempt
	if attempt == nil {
		return execution, CauseStageInterrupted
	}

	chargeStage(&execution, attempt.DecisionStartedAt, now)
	attempt.DecisionEndedAt = now

	switch execution.Status {
	case StatusCancelRequested:
		execution.Status = StatusCancelled
		execution.Reason = "cancelled during decision"
		execution.Suspension = nil

		return execution, CauseCancel
	case StatusPauseRequested:
		execution.Status = StatusPaused
		execution.Phase = PhaseDecision
		execution.Suspension = &Suspension{Status: StatusReady, Phase: PhaseDecision}
		execution.Reason = "decision interrupted by pause"

		return execution, CausePause
	case StatusRunning:
		if controllerErr != nil {
			execution.Status = StatusInterrupted
			execution.Phase = PhaseDecision
			attempt.Interrupted = true
			attempt.Failure = projectFailure("controller", controllerErr)
			execution.Reason = "controller interrupted"

			return execution, CauseStageInterrupted
		}
	default:
		return execution, CauseStageInterrupted
	}

	attempt.Decision = new(cloneDecision(decision))
	attempt.Failure = nil
	attempt.Interrupted = false
	execution.LastAttempt = cloneAttempt(attempt)

	execution.CurrentAttempt = nil
	if decision.State != nil {
		execution.ControllerState = cloneJSON(decision.State)
	}

	execution.NextInput = cloneJSON(decision.NextInput)
	execution.Reason = decision.Reason
	execution.Wait = nil
	execution.Block = nil
	execution.Suspension = nil

	switch decision.Action {
	case ActionContinue:
		execution.Status = StatusReady
		execution.Phase = PhaseWork
	case ActionWait:
		execution.Status = StatusWaiting
		execution.Phase = PhaseWork
		execution.Wait = cloneWait(decision.Wait)
	case ActionBlock:
		execution.Status = StatusBlocked
		execution.Phase = PhaseWork
		execution.Block = cloneBlock(decision.Block)
	case ActionComplete:
		execution.Status = StatusCompleted
		execution.Phase = PhaseDecision
		execution.Output = cloneJSON(decision.Output)
	case ActionFail:
		execution.Status = StatusFailed
		execution.Phase = PhaseDecision
		execution.Output = cloneJSON(decision.Output)
	case ActionCancel:
		execution.Status = StatusCancelled
		execution.Phase = PhaseDecision
		execution.Output = cloneJSON(decision.Output)
	default:
		execution.Status = StatusInterrupted
		execution.Phase = PhaseDecision
	}

	return execution, CauseControllerAction
}

func (engine *Engine) applyLimit(ctx context.Context, current Record) (Record, error) {
	reason := limitReason(
		current.Execution.Limits,
		current.Execution.Accounting,
		current.Execution.Phase,
		utc(engine.clock.Now()),
	)
	if reason == "" || current.Execution.Status.Terminal() {
		return current, nil
	}

	execution := cloneExecution(current.Execution)
	execution.Status = StatusLimited
	execution.Reason = reason
	execution.Wait = nil
	execution.Block = nil
	execution.Suspension = nil

	next := engine.nextRecord(current, execution, CauseLimit, reason)
	if err := engine.store.CompareAndSwap(ctx, execution.ID, current.Execution.Revision, next); err != nil {
		return current, err
	}

	return next, nil
}

func validateHandlers(handlers Handlers) error {
	if handlers.Worker == nil || handlers.Controller == nil {
		return fmt.Errorf("%w: nil Worker or Controller", ErrInvalid)
	}

	if err := validateRef("worker", handlers.WorkerRef); err != nil {
		return err
	}

	return validateRef("controller", handlers.ControllerRef)
}

func matchHandlers(execution Execution, handlers Handlers) error {
	if execution.Worker != handlers.WorkerRef || execution.Controller != handlers.ControllerRef {
		return ErrHandlerMismatch
	}

	return nil
}

func runnableError(execution Execution) error {
	if execution.Status == StatusInterrupted && execution.Phase == PhaseWork {
		return ErrRetryRequired
	}

	if execution.Status.Terminal() {
		return ErrTerminal
	}

	return ErrNotRunnable
}

func chargeStage(execution *Execution, started, ended time.Time) {
	if !started.IsZero() && ended.After(started) {
		execution.Accounting.ActiveDuration += ended.Sub(started)
	}
}

func addWorkAccounting(accounting *Accounting, result WorkResult) {
	accounting.Turns += max(result.Turns, 0)
	if validUsage(result.Usage) {
		accounting.Usage.Add(result.Usage)
	}
}

func sanitizedPartialResult(result WorkResult) WorkResult {
	if result.Turns < 0 {
		result.Turns = 0
	}

	if !validUsage(result.Usage) {
		result.Usage.InputTokens = max(result.Usage.InputTokens, 0)
		result.Usage.OutputTokens = max(result.Usage.OutputTokens, 0)
		result.Usage.ReasoningTokens = max(result.Usage.ReasoningTokens, 0)
		result.Usage.CachedInputTokens = max(result.Usage.CachedInputTokens, 0)
		result.Usage.CacheWriteTokens = max(result.Usage.CacheWriteTokens, 0)
	}

	if !validProgress(result.Progress) {
		result.Progress = ProgressUnknown
	}

	if validateJSON("partial work result", result.Value, defaultMaxJSONBytes) != nil {
		result.Value = nil
	}

	return result
}

func normalizeWorkOutcome(result WorkResult, workerErr error) (WorkResult, error) {
	validationErr := validateWorkResult(result)
	if validationErr == nil {
		return result, workerErr
	}

	if workerErr == nil {
		workerErr = validationErr
	}

	return sanitizedPartialResult(result), workerErr
}

func cloneWorkResult(result WorkResult) *WorkResult {
	out := result
	out.Value = cloneJSON(result.Value)

	return &out
}

func projectFailure(code string, err error) *StageFailure {
	message := ""
	if err != nil {
		message = err.Error()
	}

	if len(message) > maxReasonLength {
		message = message[:maxReasonLength]
	}

	return &StageFailure{Code: code, Message: message}
}

//go:fix inline
