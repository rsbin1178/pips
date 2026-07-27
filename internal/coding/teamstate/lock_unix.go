//go:build darwin || linux

//nolint:wsl_v5 // Lock errors stay next to the system call that produced them.
package teamstate

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func acquireJournalLock(file *os.File) error {
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return ErrLocked
		}
		return fmt.Errorf("coding team state: acquire journal lock: %w", err)
	}

	return nil
}

func releaseJournalLock(file *os.File) error {
	if file == nil {
		return nil
	}
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
