//nolint:wsl_v5 // Directory removal and its durability checks stay adjacent.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
)

// deleteSessionDirectory removes one whole session directory, including its
// transcript, index record, archives, and derived sidecars.
//
// The directory is first renamed to a quarantined name inside the same group
// and the group directory is synced, so a crash cannot resurrect a partially
// removed session under its real name; the quarantined remainder is then
// removed recursively. The caller holds the session's writer lock.
func (r *Repository) deleteSessionDirectory(ctx context.Context, group Group, id string) error {
	return deleteSessionDirectoryAt(ctx, group.Dir, id, syncRootDirectory)
}

// deleteSessionDirectoryAt is deleteSessionDirectory with an injectable
// durability step, so the failure path can be exercised without making
// repository configuration mutable.
func deleteSessionDirectoryAt(
	ctx context.Context,
	groupDir string,
	id string,
	sync func(*os.Root) error,
) error {
	parent, err := os.OpenRoot(groupDir)
	if err != nil {
		return fmt.Errorf("coding session: open group for deletion: %w", err)
	}
	defer func() { _ = parent.Close() }()

	info, err := parent.Stat(id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("coding session: inspect session directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: session %q is not a directory", ErrInvalid, id)
	}

	quarantine, err := quarantineName()
	if err != nil {
		return err
	}
	if err := parent.Rename(id, quarantine); err != nil {
		return fmt.Errorf("coding session: quarantine session directory: %w", err)
	}
	if err := sync(parent); err != nil {
		// The session's own name may still be durable, so put it back before
		// reporting. A failed restoration is reported alongside the cause.
		restore := parent.Rename(quarantine, id)

		return errors.Join(
			fmt.Errorf("coding session: session deletion durability uncertain: %w", err),
			restore,
		)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := parent.RemoveAll(quarantine); err != nil {
		return fmt.Errorf("coding session: remove session directory: %w", err)
	}

	return sync(parent)
}

func quarantineName() (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("coding session: generate quarantine name: %w", err)
	}

	return ".delete-" + hex.EncodeToString(nonce[:]), nil
}

func syncRootDirectory(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}

	return errors.Join(dir.Sync(), dir.Close())
}
