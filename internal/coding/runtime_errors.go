package coding

import (
	"errors"
	"fmt"

	"github.com/rsbin/pips/internal/coding/hooks"
)

var (
	// ErrRuntimeBusy means another Runtime operation owns the session.
	ErrRuntimeBusy = errors.New("coding runtime: busy")
	// ErrRuntimeClosed means the Runtime no longer owns its resources.
	ErrRuntimeClosed = errors.New("coding runtime: closed")
	// ErrRuntimePending means durable tool calls must be continued or resolved.
	ErrRuntimePending = errors.New("coding runtime: pending approval")
	// ErrRuntimeNotPaused means approval resolution requires a paused interaction.
	ErrRuntimeNotPaused = errors.New("coding runtime: not paused")
	// ErrRuntimeInvalid means Runtime construction input is incomplete or inconsistent.
	ErrRuntimeInvalid = errors.New("coding runtime: invalid options")
	// ErrInputRequired means a non-interactive client encountered a structured
	// user question that it must not answer implicitly.
	ErrInputRequired = errors.New("coding runtime: structured user input required")
	// ErrPlanReviewRequired means a non-interactive client encountered a Plan
	// review decision that it must not answer implicitly.
	ErrPlanReviewRequired = errors.New("coding runtime: Plan review required")
	// ErrLegacyToolCallUnresolved means a resumed Session contains a pending
	// call for a removed pre-P1 Coding Tool protocol. It is never replayed or
	// guessed under the replacement name.
	ErrLegacyToolCallUnresolved = errors.New("coding runtime: legacy tool call unresolved")
	// ErrCompactionUnavailable means current configuration or history cannot
	// produce a safe compaction plan.
	ErrCompactionUnavailable = errors.New("coding runtime: compaction unavailable")
	// ErrCompactionStale means history changed after preview and confirmation
	// must be requested again.
	ErrCompactionStale = errors.New("coding runtime: compaction preview is stale")
	// ErrCompactionRetrySuppressed prevents automatic compaction thrashing on
	// an unchanged leaf after a prior model or persistence failure.
	ErrCompactionRetrySuppressed = errors.New("coding runtime: automatic compaction retry suppressed")
	// ErrHookDenied means a trusted UserPromptSubmit lifecycle hook explicitly
	// rejected a prompt before an interaction was created.
	ErrHookDenied = errors.New("coding runtime: prompt denied by hook")
	// ErrHookStopped means a lifecycle hook stopped the event-specific
	// continuation without turning it into a Runtime failure.
	ErrHookStopped = errors.New("coding runtime: lifecycle event stopped by hook")
)

// HookDeniedError includes the reviewed hook's bounded reason while preserving
// a stable sentinel for non-interactive callers.
type HookDeniedError struct {
	Reason string
}

func (e *HookDeniedError) Error() string {
	if e == nil || e.Reason == "" {
		return ErrHookDenied.Error()
	}

	return fmt.Sprintf("%s: %s", ErrHookDenied, e.Reason)
}

// Unwrap exposes the stable hook-denial sentinel.
func (e *HookDeniedError) Unwrap() error { return ErrHookDenied }

// HookStoppedError identifies the hook event and bounded reason that stopped a
// continuation such as compaction or a clean agent turn.
type HookStoppedError struct {
	Event  hooks.Event
	Reason string
}

func (e *HookStoppedError) Error() string {
	if e == nil || e.Reason == "" {
		return ErrHookStopped.Error()
	}

	return fmt.Sprintf("%s: %s", ErrHookStopped, e.Reason)
}

// Unwrap exposes the stable stop sentinel.
func (e *HookStoppedError) Unwrap() error { return ErrHookStopped }

// RuntimeStateError reports a stable rejected operation and the phase that
// rejected it. Cause supports errors.Is without exposing mutable internals.
type RuntimeStateError struct {
	Operation string
	Phase     Phase
	Cause     error
}

func (e *RuntimeStateError) Error() string {
	return fmt.Sprintf("coding runtime: %s rejected in %s phase: %v", e.Operation, e.Phase, e.Cause)
}

// Unwrap returns the stable state sentinel.
func (e *RuntimeStateError) Unwrap() error { return e.Cause }

func stateError(operation string, phase Phase, cause error) error {
	return &RuntimeStateError{Operation: operation, Phase: phase, Cause: cause}
}
