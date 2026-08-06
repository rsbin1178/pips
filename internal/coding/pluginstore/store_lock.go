//nolint:wsl_v5 // Lock acquisition deliberately interleaves retry and cancellation branches.
package pluginstore

import (
	"context"
	"errors"
	"os"
	"time"
)

const storeLockName = "store.lock"

// storeFileLock is a cross-process exclusive lock for one store root. The
// platform implementations deliberately use non-blocking primitives so a
// canceled context can always stop waiting.
type storeFileLock struct {
	file *os.File
}

func (s *Store) acquireStoreLock(ctx context.Context) (*storeFileLock, error) {
	lockPath, err := s.internalPath(storeLockName)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // Store root is application-owned.
	if err != nil {
		return nil, errors.Join(ErrConflict, err)
	}
	for {
		if err := tryLockStoreFile(file); err == nil {
			return &storeFileLock{file: file}, nil
		} else if !isStoreLockBusy(err) {
			_ = file.Close()
			return nil, errors.Join(ErrConflict, err)
		}
		if ctx != nil {
			select {
			case <-ctx.Done():
				_ = file.Close()
				return nil, ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
		} else {
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func (l *storeFileLock) release() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := unlockStoreFile(l.file)
	closeErr := l.file.Close()
	l.file = nil
	return errors.Join(unlockErr, closeErr)
}
