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
	// ErrInvalidCompactionSummary means generated text did not pass the
	// requested cleaned-body quality threshold.
	ErrInvalidCompactionSummary = errors.New("harness: invalid compaction summary")
	// ErrEntryNotFound means the referenced entry ID is not in the session.
	ErrEntryNotFound = errors.New("harness: entry not found")
	// ErrSessionCorrupt means persisted entries do not form a valid append-only
	// session graph. The store must not be used for writes until repaired.
	ErrSessionCorrupt = errors.New("harness: corrupt session")
	// ErrInvalidEntry means a caller attempted to append an invalid entry.
	ErrInvalidEntry = errors.New("harness: invalid entry")
	// ErrStoreAppendNotAttempted marks a Store error known to occur before any
	// bytes were written. A Store must not use it for write, flush or sync errors.
	// It allows checkpoint owners to distinguish preflight rejection from an
	// uncertain durable append without weakening the latter's write fence.
	ErrStoreAppendNotAttempted = errors.New("harness: store append not attempted")
	// ErrStaleContextCheckpoint means the source leaf changed before checkpoint
	// publication. The checkpoint was not appended.
	ErrStaleContextCheckpoint = errors.New("harness: stale context checkpoint")
	// ErrForkArchivesRequired means a fork needs an application archive
	// publisher; generic Harness cannot copy externally owned archive objects.
	ErrForkArchivesRequired = errors.New("harness: fork archive publisher required")
)
