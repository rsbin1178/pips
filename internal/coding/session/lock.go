package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

const maxLockRecordBytes = 1024

type lockRecord struct {
	PID        int       `json:"pid"`
	AcquiredAt time.Time `json:"acquired_at"`
}

// LockedError reports the best-effort owner of a contended kernel lock.
// Owner metadata is diagnostic only; [ErrLocked] remains the authority.
type LockedError struct {
	Path       string
	OwnerPID   int
	AcquiredAt time.Time
}

func (e *LockedError) Error() string {
	if e == nil {
		return ErrLocked.Error()
	}
	if e.OwnerPID > 0 {
		return fmt.Sprintf(
			"%s: %q (owner pid %d; close that Pips process before retrying)",
			ErrLocked,
			e.Path,
			e.OwnerPID,
		)
	}

	return fmt.Sprintf("%s: %q", ErrLocked, e.Path)
}

// Unwrap preserves errors.Is compatibility with ErrLocked.
func (*LockedError) Unwrap() error { return ErrLocked }

type sessionLock interface {
	io.Closer
}

func acquireSessionLock(ctx context.Context, path string) (sessionLock, error) {
	return acquirePlatformLock(ctx, path)
}

func writeLockRecord(file *os.File) error {
	encoded, err := json.Marshal(lockRecord{PID: os.Getpid(), AcquiredAt: time.Now().UTC()})
	if err != nil {
		return fmt.Errorf("coding session: encode lock owner: %w", err)
	}
	if len(encoded) > maxLockRecordBytes {
		return fmt.Errorf("coding session: lock owner exceeds %d bytes", maxLockRecordBytes)
	}
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("coding session: truncate lock owner: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("coding session: seek lock owner: %w", err)
	}
	if _, err := file.Write(encoded); err != nil {
		return fmt.Errorf("coding session: write lock owner: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("coding session: sync lock owner: %w", err)
	}

	return nil
}

func readLockRecord(file *os.File) lockRecord {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return lockRecord{}
	}
	data, err := io.ReadAll(io.LimitReader(file, maxLockRecordBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxLockRecordBytes {
		return lockRecord{}
	}

	var record lockRecord
	if err := json.Unmarshal(data, &record); err != nil || record.PID <= 0 {
		return lockRecord{}
	}
	if record.AcquiredAt.IsZero() || record.AcquiredAt.After(time.Now().UTC().Add(time.Minute)) {
		return lockRecord{}
	}

	return record
}
