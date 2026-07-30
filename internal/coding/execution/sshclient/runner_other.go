//go:build !darwin && !linux

package sshclient

import "context"

// Run reports that the SSH terminal bridge is unavailable on this platform.
func Run(context.Context, Options) error {
	return ErrUnavailable
}
