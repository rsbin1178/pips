package gitcontrol

import "errors"

var (
	// ErrInvalid means an operation value cannot name a safe fixed Git command.
	ErrInvalid = errors.New("coding git control: invalid operation")
	// ErrUnsupported means the current platform cannot provide the required
	// process and filesystem identity guarantees.
	ErrUnsupported = errors.New("coding git control: unsupported platform")
	// ErrExecutableChanged means the injected Git executable was replaced.
	ErrExecutableChanged = errors.New("coding git control: executable identity changed")
	// ErrLimit means a command input, output, or time budget was exceeded.
	ErrLimit = errors.New("coding git control: resource limit exceeded")
	// ErrGit means a fixed Git command did not complete successfully.
	ErrGit = errors.New("coding git control: git command failed")
	// ErrNotFound means an exact ref, object, or Worktree record is absent.
	ErrNotFound = errors.New("coding git control: resource not found")
	// ErrConflict means an expected-old-value or exact identity did not match.
	ErrConflict = errors.New("coding git control: identity conflict")
	// ErrUnsafeConfig means repository configuration could execute external code.
	ErrUnsafeConfig = errors.New("coding git control: unsafe repository config")
)
