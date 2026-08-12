package continuation

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/rsbin1178/pips/ai"
)

const (
	defaultMaxJSONBytes = 256 << 10
	maxIDLength         = 128
	maxRefLength        = 128
	maxReasonLength     = 4096
)

var safeIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func validateID(id ID) error {
	if !safeIDPattern.MatchString(string(id)) {
		return fmt.Errorf("%w: execution ID must be 1-%d safe ASCII characters", ErrInvalid, maxIDLength)
	}

	return nil
}

func validateAttemptID(id AttemptID) error {
	if !safeIDPattern.MatchString(string(id)) {
		return fmt.Errorf("%w: invalid attempt ID", ErrInvalid)
	}

	return nil
}

func validateRef(name string, ref HandlerRef) error {
	if strings.TrimSpace(ref.Kind) == "" || len(ref.Kind) > maxRefLength ||
		strings.TrimSpace(ref.Version) == "" || len(ref.Version) > maxRefLength {
		return fmt.Errorf("%w: invalid %s handler reference", ErrInvalid, name)
	}

	return nil
}

func validateTarget(target Target) error {
	if strings.TrimSpace(target.Kind) == "" || len(target.Kind) > maxRefLength ||
		strings.TrimSpace(target.ID) == "" || len(target.ID) > maxIDLength {
		return fmt.Errorf("%w: invalid target", ErrInvalid)
	}

	return nil
}

func validateJSON(name string, value ai.JSON, maxBytes int) error {
	if value == nil {
		return nil
	}

	if len(value) > maxBytes {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrTooLarge, name, maxBytes)
	}

	if !json.Valid(value) {
		return fmt.Errorf("%w: %s is not valid JSON", ErrInvalid, name)
	}

	return nil
}

func validateReason(reason string) error {
	if len(reason) > maxReasonLength {
		return fmt.Errorf("%w: reason exceeds %d bytes", ErrTooLarge, maxReasonLength)
	}

	return nil
}

func validateWait(wait *WaitCondition) error {
	if wait == nil || wait.NotBefore == nil && wait.Signal == nil {
		return fmt.Errorf("%w: wait requires a time or signal", ErrInvalid)
	}

	if wait.NotBefore != nil && wait.NotBefore.IsZero() {
		return fmt.Errorf("%w: zero wait time", ErrInvalid)
	}

	if wait.Signal != nil && strings.TrimSpace(wait.Signal.Key) == "" {
		return fmt.Errorf("%w: empty signal key", ErrInvalid)
	}

	return nil
}

func validateBlock(block *Block) error {
	if block == nil || strings.TrimSpace(block.Kind) == "" {
		return fmt.Errorf("%w: block requires a kind", ErrInvalid)
	}

	return validateJSON("block data", block.Data, defaultMaxJSONBytes)
}

func validateWorkResult(result WorkResult) error {
	if result.Turns < 0 || !validUsage(result.Usage) {
		return fmt.Errorf("%w: negative work accounting", ErrInvalid)
	}

	if !validProgress(result.Progress) {
		return fmt.Errorf("%w: invalid work progress %q", ErrInvalid, result.Progress)
	}

	return validateJSON("work result", result.Value, defaultMaxJSONBytes)
}

func validateDecision(decision Decision) error {
	if err := validateDecisionFields(decision); err != nil {
		return err
	}

	return validateDecisionAction(decision)
}

func validateDecisionFields(decision Decision) error {
	if err := validateReason(decision.Reason); err != nil {
		return err
	}

	if !validUsage(decision.Usage) {
		return fmt.Errorf("%w: negative decision accounting", ErrInvalid)
	}

	for name, value := range map[string]ai.JSON{
		"controller state": decision.State,
		"next input":       decision.NextInput,
		"output":           decision.Output,
	} {
		if err := validateJSON(name, value, defaultMaxJSONBytes); err != nil {
			return err
		}
	}

	if decision.Progress != "" && !validProgress(decision.Progress) {
		return fmt.Errorf("%w: invalid decision progress %q", ErrInvalid, decision.Progress)
	}

	return nil
}

func validateDecisionAction(decision Decision) error {
	switch decision.Action {
	case ActionContinue:
		if decision.Wait != nil || decision.Block != nil {
			return fmt.Errorf("%w: continue cannot carry wait or block", ErrInvalid)
		}
	case ActionWait:
		if decision.Block != nil {
			return fmt.Errorf("%w: wait cannot carry block", ErrInvalid)
		}

		return validateWait(decision.Wait)
	case ActionBlock:
		if decision.Wait != nil {
			return fmt.Errorf("%w: block cannot carry wait", ErrInvalid)
		}

		return validateBlock(decision.Block)
	case ActionComplete, ActionFail, ActionCancel:
		if decision.Wait != nil || decision.Block != nil {
			return fmt.Errorf("%w: terminal action cannot carry wait or block", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: invalid action %q", ErrInvalid, decision.Action)
	}

	return nil
}

func validUsage(usage ai.Usage) bool {
	return usage.InputTokens >= 0 && usage.OutputTokens >= 0 && usage.ReasoningTokens >= 0 &&
		usage.CachedInputTokens >= 0 && usage.CacheWriteTokens >= 0
}

func validProgress(progress Progress) bool {
	switch progress {
	case ProgressUnknown, ProgressChanged, ProgressUnchanged:
		return true
	default:
		return false
	}
}

func validStatus(status Status) bool {
	switch status {
	case StatusReady, StatusRunning, StatusWaiting, StatusPauseRequested, StatusPaused,
		StatusBlocked, StatusInterrupted, StatusCancelRequested, StatusCompleted, StatusFailed,
		StatusCancelled, StatusLimited:
		return true
	default:
		return false
	}
}

func validPhase(phase Phase) bool { return phase == PhaseWork || phase == PhaseDecision }

func validCause(cause Cause) bool {
	switch cause {
	case CauseCreate, CauseStageStart, CauseWorkComplete, CauseStageInterrupted,
		CauseControllerAction, CausePauseRequested, CausePause, CauseResume, CauseRetryWork,
		CauseRetryDecision, CauseSignal, CauseTimeWake, CauseBlockResolved, CauseCancelRequested,
		CauseCancel, CauseFail, CauseLimit, CauseRecovery:
		return true
	default:
		return false
	}
}

func validateExecution(execution Execution) error {
	if err := validateExecutionIdentity(execution); err != nil {
		return err
	}

	if err := validateExecutionPayloads(execution); err != nil {
		return err
	}

	return validateExecutionState(execution)
}

func validateExecutionIdentity(execution Execution) error {
	if err := validateID(execution.ID); err != nil {
		return err
	}

	if !validStatus(execution.Status) || !validPhase(execution.Phase) {
		return fmt.Errorf("%w: invalid execution status or phase", ErrInvalid)
	}

	if err := validateTarget(execution.Target); err != nil {
		return err
	}

	if err := validateRef("worker", execution.Worker); err != nil {
		return err
	}

	if err := validateRef("controller", execution.Controller); err != nil {
		return err
	}

	if execution.CreatedAt.IsZero() || execution.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: zero execution timestamp", ErrInvalid)
	}

	if _, err := resolveLimits(execution.Limits); err != nil {
		return err
	}

	if err := validateReason(execution.Reason); err != nil {
		return err
	}

	return nil
}

func validateExecutionPayloads(execution Execution) error {
	for name, value := range map[string]ai.JSON{
		"controller state": execution.ControllerState,
		"next input":       execution.NextInput,
		"output":           execution.Output,
	} {
		if err := validateJSON(name, value, defaultMaxJSONBytes); err != nil {
			return err
		}
	}

	if execution.Wait != nil {
		if err := validateWait(execution.Wait); err != nil {
			return err
		}
	}

	if execution.Block != nil {
		if err := validateBlock(execution.Block); err != nil {
			return err
		}
	}

	if err := validateActivation(execution.Activation); err != nil {
		return err
	}

	if err := validateSuspension(execution.Suspension); err != nil {
		return err
	}

	return nil
}

func validateExecutionState(execution Execution) error {
	if err := validateAttempt(execution.CurrentAttempt, execution.Accounting.Attempts); err != nil {
		return err
	}

	if err := validateAttempt(execution.LastAttempt, execution.Accounting.Attempts); err != nil {
		return err
	}

	if execution.Accounting.Attempts < 0 || execution.Accounting.Turns < 0 ||
		execution.Accounting.ActiveDuration < 0 || !validUsage(execution.Accounting.Usage) {
		return fmt.Errorf("%w: invalid accounting", ErrInvalid)
	}

	if err := validateStatusState(execution); err != nil {
		return err
	}

	return nil
}

func validateActivation(activation *Activation) error {
	if activation == nil {
		return nil
	}

	if activation.At.IsZero() {
		return fmt.Errorf("%w: zero activation time", ErrInvalid)
	}

	switch activation.Source {
	case ActivationInitial, ActivationTime, ActivationBlock:
		if activation.SignalID != "" {
			return fmt.Errorf("%w: non-signal activation carries signal ID", ErrInvalid)
		}
	case ActivationSignal:
		if activation.SignalID == "" || len(activation.SignalID) > maxIDLength {
			return fmt.Errorf("%w: signal activation requires a bounded ID", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: invalid activation source %q", ErrInvalid, activation.Source)
	}

	return validateJSON("activation payload", activation.Payload, defaultMaxJSONBytes)
}

func validateSuspension(suspension *Suspension) error {
	if suspension == nil {
		return nil
	}

	if err := validateSuspensionIdentity(suspension); err != nil {
		return err
	}

	if err := validateSuspensionState(suspension); err != nil {
		return err
	}

	if suspension.RetryRequired && suspension.Phase != PhaseWork {
		return fmt.Errorf("%w: only Work suspension can require retry", ErrInvalid)
	}

	return nil
}

func validateSuspensionIdentity(suspension *Suspension) error {
	if !validPhase(suspension.Phase) {
		return fmt.Errorf("%w: invalid suspension phase", ErrInvalid)
	}

	return validateReason(suspension.Reason)
}

func validateSuspensionState(suspension *Suspension) error {
	switch suspension.Status {
	case StatusReady:
		if suspension.Wait != nil || suspension.Block != nil {
			return fmt.Errorf("%w: ready suspension carries wait or block", ErrInvalid)
		}
	case StatusWaiting:
		return validateWait(suspension.Wait)
	case StatusBlocked:
		return validateBlock(suspension.Block)
	case StatusInterrupted:
		if suspension.Wait != nil || suspension.Block != nil {
			return fmt.Errorf("%w: interrupted suspension carries wait or block", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: invalid suspended status %q", ErrInvalid, suspension.Status)
	}

	return nil
}

func validateAttempt(attempt *Attempt, attempts int) error {
	if attempt == nil {
		return nil
	}

	if err := validateAttemptIdentity(attempt, attempts); err != nil {
		return err
	}

	if err := validateActivation(attempt.Activation); err != nil {
		return err
	}

	if err := validateAttemptWork(attempt); err != nil {
		return err
	}

	if err := validateAttemptDecision(attempt); err != nil {
		return err
	}

	return validateStageFailure(attempt.Failure)
}

func validateAttemptIdentity(attempt *Attempt, attempts int) error {
	if err := validateAttemptID(attempt.ID); err != nil {
		return err
	}

	if attempt.Number <= 0 || attempt.Number > attempts || !validPhase(attempt.Phase) || attempt.WorkStartedAt.IsZero() {
		return fmt.Errorf("%w: invalid attempt identity or timing", ErrInvalid)
	}

	return nil
}

func validateAttemptWork(attempt *Attempt) error {
	if attempt.Work != nil {
		if attempt.WorkCompletedAt.IsZero() {
			return fmt.Errorf("%w: Work result lacks completion time", ErrInvalid)
		}

		if err := validateWorkResult(*attempt.Work); err != nil {
			return err
		}
	}

	return nil
}

func validateAttemptDecision(attempt *Attempt) error {
	if attempt.Decision != nil {
		if attempt.DecisionStartedAt.IsZero() || attempt.DecisionEndedAt.IsZero() || attempt.Work == nil {
			return fmt.Errorf("%w: Decision lacks durable Work or timing", ErrInvalid)
		}

		if err := validateDecision(*attempt.Decision); err != nil {
			return err
		}
	}

	return nil
}

func validateStageFailure(failure *StageFailure) error {
	if failure == nil {
		return nil
	}

	if failure.Code == "" || len(failure.Code) > maxRefLength || len(failure.Message) > maxReasonLength {
		return fmt.Errorf("%w: invalid stage failure", ErrInvalid)
	}

	return nil
}

func validateStatusState(execution Execution) error {
	switch execution.Status {
	case StatusReady:
		return validateReadyState(execution)
	case StatusRunning, StatusPauseRequested, StatusCancelRequested:
		return validateActiveState(execution)
	case StatusWaiting:
		return validateWaitingState(execution)
	case StatusBlocked:
		return validateBlockedState(execution)
	case StatusPaused:
		return validatePausedState(execution)
	case StatusInterrupted:
		return validateInterruptedState(execution)
	case StatusCompleted, StatusFailed, StatusCancelled, StatusLimited:
		return nil
	default:
		return fmt.Errorf("%w: invalid status", ErrInvalid)
	}
}

func validateWaitingState(execution Execution) error {
	if execution.Wait == nil || execution.Phase != PhaseWork {
		return fmt.Errorf("%w: Waiting requires Work-phase condition", ErrInvalid)
	}

	return nil
}

func validateBlockedState(execution Execution) error {
	if execution.Block == nil || execution.Phase != PhaseWork {
		return fmt.Errorf("%w: Blocked requires Work-phase block", ErrInvalid)
	}

	return nil
}

func validatePausedState(execution Execution) error {
	if execution.Suspension == nil {
		return fmt.Errorf("%w: Paused requires suspension", ErrInvalid)
	}

	return nil
}

func validateInterruptedState(execution Execution) error {
	if execution.CurrentAttempt == nil {
		return fmt.Errorf("%w: Interrupted requires current attempt", ErrInvalid)
	}

	return nil
}

func validateReadyState(execution Execution) error {
	if execution.Wait != nil || execution.Block != nil || execution.Suspension != nil {
		return fmt.Errorf("%w: Ready carries suspension state", ErrInvalid)
	}

	if execution.Phase == PhaseDecision &&
		(execution.CurrentAttempt == nil || execution.CurrentAttempt.Work == nil) {
		return fmt.Errorf("%w: Ready/Decision requires durable Work", ErrInvalid)
	}

	return nil
}

func validateActiveState(execution Execution) error {
	if execution.CurrentAttempt == nil || execution.CurrentAttempt.Phase != execution.Phase {
		return fmt.Errorf("%w: active status requires phase-matched attempt", ErrInvalid)
	}

	if execution.Phase == PhaseDecision && execution.CurrentAttempt.Work == nil {
		return fmt.Errorf("%w: active Decision requires durable Work", ErrInvalid)
	}

	return nil
}

func validateRecord(record Record) error {
	if err := validateExecution(record.Execution); err != nil {
		return err
	}

	t := record.Transition
	if t.Revision == 0 || t.Revision != record.Execution.Revision || t.At.IsZero() ||
		t.To != record.Execution.Status || t.Phase != record.Execution.Phase || !validCause(t.Cause) {
		return fmt.Errorf("%w: inconsistent transition", ErrInvalid)
	}

	if t.From != "" && !validStatus(t.From) {
		return fmt.Errorf("%w: invalid transition source", ErrInvalid)
	}

	if err := validateReason(t.Reason); err != nil {
		return err
	}

	return nil
}

func utc(now time.Time) time.Time { return now.UTC() }
