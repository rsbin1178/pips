//go:build darwin || linux

package teamworktree

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func acquireLeaseLock(file *os.File) error {
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return ErrLeaseHeld
		}

		return err
	}

	return nil
}

func validateLeaseLock(file *os.File) error {
	if file == nil {
		return ErrLeaseLost
	}
	// A retained descriptor that still exists is the ownership proof. Re-locking
	// would only validate this process's existing lock, not acquire a new owner.
	_, err := file.Stat()

	return err
}

func releaseLeaseLock(file *os.File) error {
	if file == nil {
		return nil
	}

	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
