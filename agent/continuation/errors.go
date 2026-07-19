package continuation

import (
	"errors"
	"fmt"
)

// Lifecycle and store errors.
var (
	ErrNotFound        = errors.New("continuation: not found")
	ErrExists          = errors.New("continuation: already exists")
	ErrConflict        = errors.New("continuation: revision conflict")
	ErrBusy            = errors.New("continuation: execution is active")
	ErrInvalid         = errors.New("continuation: invalid value")
	ErrTooLarge        = errors.New("continuation: value too large")
	ErrHandlerMismatch = errors.New("continuation: handler mismatch")
	ErrNotRunnable     = errors.New("continuation: execution is not runnable")
	ErrTerminal        = errors.New("continuation: execution is terminal")
	ErrRetryRequired   = errors.New("continuation: explicit retry required")
	ErrNotWaiting      = errors.New("continuation: execution is not waiting")
	ErrNotDue          = errors.New("continuation: wait is not due")
	ErrSignalMismatch  = errors.New("continuation: signal does not match")
	ErrSignalExpired   = errors.New("continuation: signal wait expired")
	ErrCorruptStore    = errors.New("continuation: corrupt store")
	ErrStoreFull       = errors.New("continuation: store limit reached")
)

// ConflictError reports the optimistic revision mismatch.
type ConflictError struct {
	Expected Revision
	Actual   Revision
}

// Error implements error.
func (e *ConflictError) Error() string {
	return fmt.Sprintf("%v: expected %d, actual %d", ErrConflict, e.Expected, e.Actual)
}

// Unwrap exposes ErrConflict.
func (e *ConflictError) Unwrap() error { return ErrConflict }

// StateError describes an operation rejected by the current lifecycle state.
type StateError struct {
	Operation string
	Status    Status
	Phase     Phase
	Err       error
}

// Error implements error.
func (e *StateError) Error() string {
	return fmt.Sprintf("continuation: %s rejected in %s/%s: %v", e.Operation, e.Status, e.Phase, e.Err)
}

// Unwrap exposes the state sentinel.
func (e *StateError) Unwrap() error { return e.Err }

// CorruptStoreError identifies a malformed durable execution record.
type CorruptStoreError struct {
	Path   string
	Line   int
	Reason string
	Err    error
}

// Error implements error.
func (e *CorruptStoreError) Error() string {
	where := e.Path
	if e.Line > 0 {
		where = fmt.Sprintf("%s:%d", e.Path, e.Line)
	}

	if e.Err != nil {
		return fmt.Sprintf("%v: %s: %s: %v", ErrCorruptStore, where, e.Reason, e.Err)
	}

	return fmt.Sprintf("%v: %s: %s", ErrCorruptStore, where, e.Reason)
}

// Unwrap exposes ErrCorruptStore.
func (e *CorruptStoreError) Unwrap() error { return ErrCorruptStore }
