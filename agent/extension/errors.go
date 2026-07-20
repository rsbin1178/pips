package extension

import "errors"

var (
	// ErrInvalid reports an invalid descriptor, contribution, or runtime
	// configuration.
	ErrInvalid = errors.New("extension: invalid")
	// ErrCapabilityUnavailable reports a required capability not supplied by
	// the Runtime.
	ErrCapabilityUnavailable = errors.New("extension: capability unavailable")
	// ErrNotRegistered reports a requested Extension ID absent from the
	// Runtime's immutable registry.
	ErrNotRegistered = errors.New("extension: not registered")
	// ErrNotActive reports an Acquire call before the first successful
	// activation.
	ErrNotActive = errors.New("extension: not active")
	// ErrClosed reports an operation attempted after Runtime.Shutdown.
	ErrClosed = errors.New("extension: runtime closed")
)
