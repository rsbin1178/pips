package team

import (
	"errors"
	"fmt"
)

// Domain and Store errors.
var (
	ErrNotFound          = errors.New("team: not found")
	ErrExists            = errors.New("team: already exists")
	ErrConflict          = errors.New("team: revision conflict")
	ErrCommandConflict   = errors.New("team: command id conflict")
	ErrInvalid           = errors.New("team: invalid value")
	ErrTooLarge          = errors.New("team: value too large")
	ErrUnauthorized      = errors.New("team: unauthorized")
	ErrInvalidState      = errors.New("team: invalid state")
	ErrDependencyBlocked = errors.New("team: task dependency blocked")
	ErrMemberBusy        = errors.New("team: member has active work")
	ErrStaleAttempt      = errors.New("team: stale task attempt")
	ErrAttemptLimit      = errors.New("team: task attempt limit reached")
	ErrTerminal          = errors.New("team: terminal")
	ErrCorruptStore      = errors.New("team: corrupt store")
	ErrStoreFull         = errors.New("team: store limit reached")
)

// ConflictError reports an optimistic revision mismatch.
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

// StateError describes an operation rejected by current Team or task state.
type StateError struct {
	Operation string
	Team      Status
	Task      TaskStatus
	Err       error
}

// Error implements error.
func (e *StateError) Error() string {
	if e.Task != "" {
		return fmt.Sprintf("team: %s rejected in %s/%s: %v", e.Operation, e.Team, e.Task, e.Err)
	}

	return fmt.Sprintf("team: %s rejected in %s: %v", e.Operation, e.Team, e.Err)
}

// Unwrap exposes the state sentinel.
func (e *StateError) Unwrap() error { return e.Err }

// CorruptStoreError identifies malformed durable Team data.
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
