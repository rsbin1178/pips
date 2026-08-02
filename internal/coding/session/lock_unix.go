//go:build darwin || linux

package session

import (
	"context"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

type fileLock struct {
	file *os.File
}

func acquirePlatformLock(ctx context.Context, path string) (sessionLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	fd, err := unix.Open(path, unix.O_CLOEXEC|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("coding session: open lock: %w", err)
	}

	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("coding session: open lock: invalid file descriptor")
	}

	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("coding session: inspect lock: %w", err)
	}

	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		_ = file.Close()
		return nil, fmt.Errorf("%w: insecure lock file %q", ErrInvalid, path)
	}

	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			owner := readLockRecord(file)
			_ = file.Close()

			return nil, &LockedError{
				Path: path, OwnerPID: owner.PID, AcquiredAt: owner.AcquiredAt,
			}
		}
		_ = file.Close()

		return nil, fmt.Errorf("coding session: acquire lock: %w", err)
	}
	if err := writeLockRecord(file); err != nil {
		return nil, errors.Join(err, unix.Flock(fd, unix.LOCK_UN), file.Close())
	}

	if err := ctx.Err(); err != nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = file.Close()

		return nil, err
	}

	return &fileLock{file: file}, nil
}

func (l *fileLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}

	fd := int(l.file.Fd())

	return errors.Join(unix.Flock(fd, unix.LOCK_UN), l.file.Close())
}
