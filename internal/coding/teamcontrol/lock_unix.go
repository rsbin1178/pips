//go:build darwin || linux

package teamcontrol

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

		return fmt.Errorf("coding team control: acquire journal lock: %w", err)
	}

	return nil
}

func releaseJournalLock(file *os.File) error {
	if file == nil {
		return nil
	}

	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
