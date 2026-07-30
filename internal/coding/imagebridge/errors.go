// Package imagebridge transports bounded normalized images into one live
// remote Coding TUI without creating files or durable Runtime events.
package imagebridge

import "errors"

var (
	// ErrProtocol means a bridge frame or token violated the fixed wire contract.
	ErrProtocol = errors.New("coding image bridge: invalid protocol")
	// ErrDeadline means a bridge frame expired or requested an excessive lifetime.
	ErrDeadline = errors.New("coding image bridge: invalid deadline")
	// ErrPeer means a Unix socket peer did not match the owning user.
	ErrPeer = errors.New("coding image bridge: invalid peer")
	// ErrUnavailable means the process-owned bridge endpoint is unavailable.
	ErrUnavailable = errors.New("coding image bridge: unavailable")
	// ErrBusy means the live TUI inbox has reached its fixed capacity.
	ErrBusy = errors.New("coding image bridge: busy")
	// ErrClosed means the live bridge has stopped.
	ErrClosed = errors.New("coding image bridge: closed")
)
