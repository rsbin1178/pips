package teamcontrol

import "errors"

var (
	// ErrInvalid means an API value or durable record is malformed.
	ErrInvalid = errors.New("coding team control: invalid value")
	// ErrNotFound means a command or Team journal does not exist.
	ErrNotFound = errors.New("coding team control: not found")
	// ErrConflict means the expected revision is stale.
	ErrConflict = errors.New("coding team control: revision conflict")
	// ErrIdempotency means a mutation ID was reused for different semantics.
	ErrIdempotency = errors.New("coding team control: idempotency conflict")
	// ErrCorrupt means a durable journal cannot be replayed safely.
	ErrCorrupt = errors.New("coding team control: corrupt journal")
	// ErrUnsafeFile means a journal violates the private-file contract.
	ErrUnsafeFile = errors.New("coding team control: unsafe filesystem object")
	// ErrLimit means an input or durable journal exceeds configured bounds.
	ErrLimit = errors.New("coding team control: limit exceeded")
	// ErrLocked means another process owns the journal writer lock.
	ErrLocked = errors.New("coding team control: journal is locked")
	// ErrUnsupported means the platform cannot enforce the journal contract.
	ErrUnsupported = errors.New("coding team control: unsupported platform")
	// ErrAmbiguous means stable identity did not resolve to exactly one live execution.
	ErrAmbiguous = errors.New("coding team control: ambiguous target")
)
