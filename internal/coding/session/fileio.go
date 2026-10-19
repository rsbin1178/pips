//nolint:wsl_v5 // Durability steps of one atomic write stay adjacent.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// This file holds the package's filesystem primitives: the private directory
// check, the atomic publish, and the bounded read every layer builds on.

// ensureSessionDirectory creates one private directory and verifies its mode.
func ensureSessionDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("coding session: create directory: %w", err)
	}

	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("coding session: inspect directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %q is not a directory", ErrLayout, path)
	}

	return nil
}

// writeFileAtomic publishes contents at path with one rename: a private temp
// file in the same directory is written, synced, renamed, and the directory is
// synced. A reader sees either the old file or the complete new one.
func writeFileAtomic(path string, contents []byte, mode os.FileMode) (resultErr error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("coding session: create directory: %w", err)
	}

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("coding session: generate temp name: %w", err)
	}

	temp := filepath.Join(dir, ".tmp-"+hex.EncodeToString(nonce[:]))
	file, err := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode) //nolint:gosec // caller-owned store path
	if err != nil {
		return fmt.Errorf("coding session: create temp file: %w", err)
	}

	published := false
	defer func() {
		if !published {
			resultErr = errors.Join(resultErr, os.Remove(temp))
		}
	}()

	if _, err := file.Write(contents); err != nil {
		return errors.Join(fmt.Errorf("coding session: write temp file: %w", err), file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(fmt.Errorf("coding session: sync temp file: %w", err), file.Close())
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("coding session: close temp file: %w", err)
	}
	if err := os.Rename(temp, path); err != nil {
		return fmt.Errorf("coding session: publish file: %w", err)
	}
	published = true

	return syncDirectory(dir)
}

// syncDirectory flushes a directory entry change to stable storage.
func syncDirectory(dir string) error {
	handle, err := os.Open(dir) //nolint:gosec // caller-owned store path
	if err != nil {
		return fmt.Errorf("coding session: open directory for sync: %w", err)
	}

	return errors.Join(handle.Sync(), handle.Close())
}

// readBoundedFile reads at most limit bytes and rejects a file that is larger,
// so a corrupt or hostile sidecar cannot force an unbounded allocation.
func readBoundedFile(file *os.File, limit int64) ([]byte, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("coding session: inspect file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: not a regular file", ErrLayout)
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("%w: file exceeds %d bytes", ErrLayout, limit)
	}

	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, fmt.Errorf("coding session: read file: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: file exceeds %d bytes", ErrLayout, limit)
	}

	return data, nil
}
