package teamstate

import "errors"

var (
	// ErrInvalid means an API value or durable snapshot is malformed.
	ErrInvalid = errors.New("coding team state: invalid value")
	// ErrNotFound means no resource journal exists for the requested Team.
	ErrNotFound = errors.New("coding team state: not found")
	// ErrConflict means expected revision does not match the durable revision.
	ErrConflict = errors.New("coding team state: revision conflict")
	// ErrIdempotency means a Command ID was reused for different semantics.
	ErrIdempotency = errors.New("coding team state: idempotency conflict")
	// ErrCorrupt means a journal cannot be replayed safely.
	ErrCorrupt = errors.New("coding team state: corrupt journal")
	// ErrUnsafeFile means a filesystem object violates the private-file contract.
	ErrUnsafeFile = errors.New("coding team state: unsafe filesystem object")
	// ErrLimit means an input or durable journal exceeds configured bounds.
	ErrLimit = errors.New("coding team state: limit exceeded")
	// ErrLocked means another writer owns the Team journal.
	ErrLocked = errors.New("coding team state: journal is locked")
	// ErrUnsupported means this platform cannot enforce the journal contract.
	ErrUnsupported = errors.New("coding team state: unsupported platform")
)
