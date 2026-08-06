//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package pluginstore

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func tryLockStoreFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func unlockStoreFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}

func isStoreLockBusy(err error) bool {
	return errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN)
}
