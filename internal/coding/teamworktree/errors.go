package teamworktree

import (
	"errors"
	"fmt"
)

var (
	// ErrInvalid means a request, resource, path, ref, or bound is invalid.
	ErrInvalid = errors.New("coding team worktree: invalid request")
	// ErrUnsupported means the platform cannot provide required identity/lease guarantees.
	ErrUnsupported = errors.New("coding team worktree: unsupported platform")
	// ErrLimit means a file, byte, path, Git output, or time bound was exceeded.
	ErrLimit = errors.New("coding team worktree: resource limit exceeded")
	// ErrLeaseHeld means another process owns the Team lease.
	ErrLeaseHeld = errors.New("coding team worktree: Team lease is held")
	// ErrLeaseLost means the supplied lease is closed, foreign, stale, or replaced.
	ErrLeaseLost = errors.New("coding team worktree: Team lease identity lost")
	// ErrIdentity means repository, Worktree, path, ref, or lock identity changed.
	ErrIdentity = errors.New("coding team worktree: resource identity changed")
	// ErrUnsafeRepository means Git configuration or content needs unsupported execution.
	ErrUnsafeRepository = errors.New("coding team worktree: unsafe repository")
	// ErrConflict means concurrent mutation invalidated an expected value.
	ErrConflict = errors.New("coding team worktree: concurrent mutation")
	// ErrDirty means exact raw Worktree content does not match its expected tree.
	ErrDirty = errors.New("coding team worktree: Worktree is dirty")
	// ErrRetained means cleanup intentionally preserved a resource.
	ErrRetained = errors.New("coding team worktree: resource retained")
	// ErrGit means a trusted fixed Git operation failed.
	ErrGit = errors.New("coding team worktree: Git operation failed")
)

// RetainedError reports a partially completed lifecycle whose exact resource
// must be persisted for explicit recovery instead of force-cleaned.
type RetainedError struct {
	Resource Resource
	Cause    error
}

func (e *RetainedError) Error() string {
	if e == nil || e.Cause == nil {
		return ErrRetained.Error()
	}

	return fmt.Sprintf("%s: %v", ErrRetained, e.Cause)
}

// Unwrap makes retained errors match both ErrRetained and their root cause.
func (e *RetainedError) Unwrap() []error {
	if e == nil || e.Cause == nil {
		return []error{ErrRetained}
	}

	return []error{ErrRetained, e.Cause}
}
