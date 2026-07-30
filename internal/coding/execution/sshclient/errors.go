// Package sshclient owns the fixed system OpenSSH process, terminal proxy, and
// push-only local clipboard upload lifecycle for pips ssh.
package sshclient

import (
	"errors"
	"fmt"
)

var (
	// ErrInvalid means public or hidden bridge input violated the fixed contract.
	ErrInvalid = errors.New("coding ssh client: invalid input")
	// ErrUnavailable means a trusted fixed OpenSSH executable or bridge is unavailable.
	ErrUnavailable = errors.New("coding ssh client: unavailable")
	// ErrTerminal means the local PTY or terminal lifecycle failed.
	ErrTerminal = errors.New("coding ssh client: terminal failure")
)

// ExitError preserves the OpenSSH/remote command exit status for the CLI.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string {
	if e == nil {
		return "coding ssh client: remote command exited"
	}

	return fmt.Sprintf("coding ssh client: remote command exited with status %d", e.Code)
}

// ExitCode returns the process status reported by OpenSSH.
func (e *ExitError) ExitCode() int {
	if e == nil {
		return 1
	}

	return e.Code
}
