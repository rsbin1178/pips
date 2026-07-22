package harness

import "errors"

// Sentinel errors returned by [Harness] and session operations. Match them
// with [errors.Is].
var (
	// ErrBusy means the harness is already running a prompt, compaction, or
	// navigation; wait for it to become idle.
	ErrBusy = errors.New("harness: harness is busy")
	// ErrIdle means the operation needs an active run (steering an idle
	// harness, for example).
	ErrIdle = errors.New("harness: no active run")
	// ErrNothingToCompact means the session has no compactable history — it
	// is empty or already ends at a compaction point.
	ErrNothingToCompact = errors.New("harness: nothing to compact")
	// ErrEntryNotFound means the referenced entry ID is not in the session.
	ErrEntryNotFound = errors.New("harness: entry not found")
	// ErrSessionCorrupt means persisted entries do not form a valid append-only
	// session graph. The store must not be used for writes until repaired.
	ErrSessionCorrupt = errors.New("harness: corrupt session")
	// ErrInvalidEntry means a caller attempted to append an invalid entry.
	ErrInvalidEntry = errors.New("harness: invalid entry")
)
