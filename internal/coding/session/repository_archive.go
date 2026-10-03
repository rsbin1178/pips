//nolint:wsl_v5 // Deletion durability and identity checks stay in commit order.
package session

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// deleteConversationFiles runs while the conversation's writer lock is held.
// Missing transcripts never authorize archive cleanup: they may be provisional
// or the result of an uncertain earlier publication.
func (r *Repository) deleteConversationFiles(ctx context.Context, id string) (resultErr error) {
	expected, err := os.Lstat(r.dir)
	if err != nil {
		return err
	}
	if !expected.IsDir() || expected.Mode().Perm()&0o027 != 0 {
		return fmt.Errorf("%w: unsafe session repository directory", ErrInvalid)
	}
	root, err := os.OpenRoot(r.dir)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	opened, err := root.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(expected, opened) {
		return fmt.Errorf("%w: session repository changed while opening", ErrInvalid)
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()

	return deleteConversationAt(ctx, root, id, directory.Sync)
}

// The explicit sync callback permits deterministic fault injection at the
// durability boundary without making repository configuration mutable.
func deleteConversationAt(ctx context.Context, root *os.Root, id string, syncDirectory func() error) error {
	historyName := id + ".history"
	history, err := sessionHistoryForDeletion(root, historyName)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := root.Remove(id + ".jsonl"); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	// A successful unlink is not yet proof that the transcript cannot return
	// after a crash. Keep all archive objects if this synchronization fails.
	if err := syncDirectory(); err != nil {
		return fmt.Errorf("coding session: transcript deletion durability uncertain; archives retained: %w", err)
	}
	if history == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := removeSessionHistory(root, historyName, history); err != nil {
		return fmt.Errorf("coding session: transcript deleted but history cleanup failed: %w", err)
	}
	if err := syncDirectory(); err != nil {
		return fmt.Errorf("coding session: transcript deleted but history cleanup durability uncertain: %w", err)
	}

	return nil
}

func sessionHistoryForDeletion(root *os.Root, name string) (os.FileInfo, error) {
	history, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !history.IsDir() || history.Mode().Perm() != 0o700 {
		return nil, fmt.Errorf("%w: unsafe session history directory", ErrArchiveInvalid)
	}

	return history, nil
}

func removeSessionHistory(root *os.Root, name string, expected os.FileInfo) error {
	current, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !current.IsDir() || !os.SameFile(expected, current) {
		return fmt.Errorf("%w: session history changed before cleanup", ErrArchiveInvalid)
	}
	nonce, err := newSessionID()
	if err != nil {
		return err
	}
	quarantine := ".delete-" + nonce + ".history"
	if _, err := root.Lstat(quarantine); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(ErrArchiveInvalid, err)
	}
	if err := root.Rename(name, quarantine); err != nil {
		return err
	}
	moved, err := root.Lstat(quarantine)
	if err != nil {
		return err
	}
	if !moved.IsDir() || !os.SameFile(expected, moved) {
		return fmt.Errorf("%w: session history changed during cleanup", ErrArchiveInvalid)
	}

	// RemoveAll does not follow symlinks, and Root confines every lookup to the
	// opened repository. The unguessable, identity-checked name belongs only to
	// the deleted session; archive provenance is never used as a deletion path.
	return root.RemoveAll(quarantine)
}
