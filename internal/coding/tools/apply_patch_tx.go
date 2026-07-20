package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"

	patchdoc "github.com/rsbin/pips/internal/coding/tools/patch"
	"github.com/rsbin/pips/internal/coding/workspace"
)

const temporaryNameAttempts = 16

type patchTransaction struct {
	service *service
	changes []*plannedChange
	dirs    map[string]*workspace.MutationDir
	applied bool
}

func (s *service) commitPatch(ctx context.Context, changes []*plannedChange) error {
	return s.tree.Mutate(ctx, func(mutation *workspace.Mutation) error {
		transaction := &patchTransaction{
			service: s, changes: changes, dirs: make(map[string]*workspace.MutationDir),
		}

		return transaction.run(ctx, mutation)
	})
}

//nolint:gocyclo // Transaction phases and recovery routing are intentionally visible in the orchestrator.
func (t *patchTransaction) run(ctx context.Context, mutation *workspace.Mutation) (returnErr error) {
	defer func() {
		var closeErrors []error

		for _, directory := range t.dirs {
			if err := directory.Close(); err != nil {
				closeErrors = append(closeErrors, err)
			}
		}

		closeErr := errors.Join(closeErrors...)
		if closeErr == nil {
			return
		}

		if t.applied {
			returnErr = t.recoveryError(true, joinedError(returnErr, closeErr))
			return
		}

		returnErr = joinedError(returnErr, closeErr)
	}()

	if err := t.openDirectories(mutation); err != nil {
		return err
	}

	if err := t.prepare(ctx); err != nil {
		cleanupErr := t.cleanupPrepared("prepare_cleanup")
		if cleanupErr != nil {
			return t.recoveryError(false, joinedError(err, cleanupErr))
		}

		return err
	}

	for _, change := range t.changes {
		if err := t.commit(ctx, change); err != nil {
			return t.rollback(ctx, err)
		}
	}

	if err := ctx.Err(); err != nil {
		return t.rollback(ctx, err)
	}

	if err := t.syncDirectories("commit"); err != nil {
		return t.rollback(ctx, err)
	}

	if err := t.cleanupPrepared("success_cleanup"); err != nil {
		return t.recoveryError(true, err)
	}

	if err := t.syncDirectories("success_cleanup"); err != nil {
		return t.recoveryError(true, err)
	}

	t.applied = true

	return nil
}

func (t *patchTransaction) openDirectories(mutation *workspace.Mutation) error {
	for _, change := range t.changes {
		if directory := t.dirs[change.parent]; directory != nil {
			change.directory = directory
			continue
		}

		directory, err := mutation.OpenDir(change.parent)
		if err != nil {
			return err
		}

		t.dirs[change.parent] = directory
		change.directory = directory
	}

	return nil
}

func (t *patchTransaction) prepare(ctx context.Context) error {
	for _, change := range t.changes {
		if err := t.revalidate(ctx, change); err != nil {
			return err
		}
	}

	for _, change := range t.changes {
		if change.kind == patchdoc.Delete {
			continue
		}

		if err := t.stage(ctx, change); err != nil {
			return err
		}
	}

	for _, change := range t.changes {
		if change.kind == patchdoc.Add {
			continue
		}

		if err := t.backup(ctx, change); err != nil {
			return err
		}
	}

	return nil
}

func (t *patchTransaction) stage(ctx context.Context, change *plannedChange) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	for range temporaryNameAttempts {
		name, err := temporaryName(".pips-stage-")
		if err != nil {
			return err
		}

		if err := t.service.patchStep("prepare", "stage", displayPath(change.parent, name)); err != nil {
			return err
		}

		file, err := change.directory.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, change.mode.Perm())
		if errors.Is(err, fs.ErrExist) {
			continue
		}

		if err != nil {
			return err
		}

		change.stage = name

		writeErr := writeStagedFile(file, change.after, change.mode)
		if writeErr != nil {
			return writeErr
		}

		return nil
	}

	return errors.New("coding tools: could not allocate a unique stage file")
}

func (t *patchTransaction) backup(ctx context.Context, change *plannedChange) error {
	if err := t.revalidate(ctx, change); err != nil {
		return err
	}

	for range temporaryNameAttempts {
		name, err := temporaryName(".pips-backup-")
		if err != nil {
			return err
		}

		_, err = change.directory.Lstat(name)
		if err == nil {
			continue
		}

		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}

		if err := t.service.patchStep("prepare", "backup", displayPath(change.parent, name)); err != nil {
			return err
		}

		if err := change.directory.Link(change.base, name); errors.Is(err, fs.ErrExist) {
			continue
		} else if err != nil {
			return err
		}

		change.backup = name
		if err := t.verifyFile(ctx, change, name, change.hash, int64(len(change.before))); err != nil {
			return err
		}

		return nil
	}

	return errors.New("coding tools: could not allocate a unique backup file")
}

//nolint:gocyclo // Each supported change kind has distinct atomic filesystem operations.
func (t *patchTransaction) commit(ctx context.Context, change *plannedChange) error {
	if err := t.revalidate(ctx, change); err != nil {
		return err
	}

	switch change.kind {
	case patchdoc.Add:
		if err := t.service.patchStep("commit", "link", change.path); err != nil {
			return err
		}

		if err := change.directory.Link(change.stage, change.base); err != nil {
			return err
		}

		change.committed = true

		if err := t.service.patchStep("commit", "remove_stage", displayPath(change.parent, change.stage)); err != nil {
			return err
		}

		if err := change.directory.Remove(change.stage); err != nil {
			return err
		}

		change.stage = ""

	case patchdoc.Update:
		if err := t.service.patchStep("commit", "rename", change.path); err != nil {
			return err
		}

		if err := change.directory.Rename(change.stage, change.base); err != nil {
			return err
		}

		change.stage = ""
		change.committed = true

		if err := t.verifyFile(ctx, change, change.backup, change.hash, int64(len(change.before))); err != nil {
			return fmt.Errorf("%w: original %q changed during commit: %w", workspace.ErrChanged, change.path, err)
		}

	case patchdoc.Delete:
		if err := t.service.patchStep("commit", "remove", change.path); err != nil {
			return err
		}

		if err := change.directory.Remove(change.base); err != nil {
			return err
		}

		change.committed = true

		if err := t.verifyFile(ctx, change, change.backup, change.hash, int64(len(change.before))); err != nil {
			return fmt.Errorf("%w: original %q changed during commit: %w", workspace.ErrChanged, change.path, err)
		}
	}

	return nil
}

func (t *patchTransaction) revalidate(ctx context.Context, change *plannedChange) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	info, err := change.directory.Lstat(change.base)
	if change.kind == patchdoc.Add {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}

		if err != nil {
			return err
		}

		return fmt.Errorf("%w: add target %q appeared during patch", workspace.ErrChanged, change.path)
	}

	if err != nil {
		return fmt.Errorf("%w: target %q changed during patch: %w", workspace.ErrChanged, change.path, err)
	}

	if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: target %q changed type during patch", workspace.ErrChanged, change.path)
	}

	if !os.SameFile(change.expected, info) || info.Mode().Perm() != change.mode.Perm() || info.Size() != int64(len(change.before)) {
		return fmt.Errorf("%w: target %q metadata changed during patch", workspace.ErrChanged, change.path)
	}

	return t.verifyFile(ctx, change, change.base, change.hash, int64(len(change.before)))
}

func (t *patchTransaction) verifyFile(
	ctx context.Context,
	change *plannedChange,
	name string,
	wantHash [sha256.Size]byte,
	wantSize int64,
) error {
	data, _, err := readMutationFile(ctx, change.directory, name, t.service.limits.FileBytes)
	if err != nil {
		return err
	}

	if int64(len(data)) != wantSize || sha256.Sum256(data) != wantHash {
		return fmt.Errorf("%w: content changed", workspace.ErrChanged)
	}

	return nil
}

func (t *patchTransaction) rollback(ctx context.Context, cause error) error {
	var rollbackErrors []error

	for _, v := range slices.Backward(t.changes) {
		change := v
		if !change.committed {
			continue
		}

		if err := t.rollbackChange(ctx, change); err != nil {
			rollbackErrors = append(rollbackErrors, err)
		}
	}

	if err := t.cleanupPrepared("rollback_cleanup"); err != nil {
		rollbackErrors = append(rollbackErrors, err)
	}

	if err := t.syncDirectories("rollback"); err != nil {
		rollbackErrors = append(rollbackErrors, err)
	}

	if len(rollbackErrors) > 0 {
		return t.recoveryError(false, joinedError(cause, errors.Join(rollbackErrors...)))
	}

	return cause
}

//nolint:gocyclo // Recovery spells out non-overwriting behavior for every change kind.
func (t *patchTransaction) rollbackChange(ctx context.Context, change *plannedChange) error {
	recoveryContext := context.WithoutCancel(ctx)

	switch change.kind {
	case patchdoc.Add:
		data, _, err := readMutationFile(recoveryContext, change.directory, change.base, t.service.limits.FileBytes)
		if err != nil {
			return err
		}

		if sha256.Sum256(data) != sha256.Sum256(change.after) {
			return fmt.Errorf("coding tools: refuse to overwrite changed rollback target %q", change.path)
		}

		if err := t.service.patchStep("rollback", "remove", change.path); err != nil {
			return err
		}

		if err := change.directory.Remove(change.base); err != nil {
			return err
		}

	case patchdoc.Update:
		data, _, err := readMutationFile(recoveryContext, change.directory, change.base, t.service.limits.FileBytes)
		if err != nil {
			return err
		}

		if sha256.Sum256(data) != sha256.Sum256(change.after) {
			return fmt.Errorf("coding tools: refuse to overwrite changed rollback target %q", change.path)
		}

		if err := t.service.patchStep("rollback", "rename", change.path); err != nil {
			return err
		}

		if err := change.directory.Rename(change.backup, change.base); err != nil {
			return err
		}

		change.backup = ""

	case patchdoc.Delete:
		_, err := change.directory.Lstat(change.base)
		if err == nil {
			return fmt.Errorf("coding tools: refuse to overwrite recreated rollback target %q", change.path)
		}

		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}

		if err := t.service.patchStep("rollback", "rename", change.path); err != nil {
			return err
		}

		if err := change.directory.Rename(change.backup, change.base); err != nil {
			return err
		}

		change.backup = ""
	}

	change.committed = false
	change.restored = true

	return nil
}

func (t *patchTransaction) cleanupPrepared(phase string) error {
	var cleanupErrors []error

	for _, change := range t.changes {
		if change.stage != "" {
			if err := t.removeTemporary(phase, change, &change.stage); err != nil {
				cleanupErrors = append(cleanupErrors, err)
			}
		}

		if change.backup != "" && (!change.committed || phase == "success_cleanup") {
			if err := t.removeTemporary(phase, change, &change.backup); err != nil {
				cleanupErrors = append(cleanupErrors, err)
			}
		}
	}

	return errors.Join(cleanupErrors...)
}

func (t *patchTransaction) removeTemporary(phase string, change *plannedChange, name *string) error {
	if *name == "" {
		return nil
	}

	display := displayPath(change.parent, *name)
	if err := t.service.patchStep(phase, "remove", display); err != nil {
		return err
	}

	if err := change.directory.Remove(*name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	*name = ""

	return nil
}

func (t *patchTransaction) syncDirectories(phase string) error {
	parents := make([]string, 0, len(t.dirs))
	for parent := range t.dirs {
		parents = append(parents, parent)
	}

	slices.Sort(parents)

	var syncErrors []error

	for _, parent := range parents {
		if err := t.service.patchStep(phase, "sync", parent); err != nil {
			syncErrors = append(syncErrors, err)
			continue
		}

		if err := t.dirs[parent].Sync(); err != nil {
			syncErrors = append(syncErrors, err)
		}
	}

	return errors.Join(syncErrors...)
}

func (t *patchTransaction) recoveryError(applied bool, cause error) error {
	var paths []string

	for _, change := range t.changes {
		if change.stage != "" {
			paths = append(paths, displayPath(change.parent, change.stage))
		}

		if change.backup != "" {
			paths = append(paths, displayPath(change.parent, change.backup))
		}

		if change.committed && !change.restored {
			paths = append(paths, change.path)
		}
	}

	slices.Sort(paths)
	paths = slices.Compact(paths)

	return &RecoveryError{Applied: applied, RecoveryPaths: paths, cause: cause}
}

func readMutationFile(
	ctx context.Context,
	directory *workspace.MutationDir,
	name string,
	maxBytes int64,
) ([]byte, fs.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	expected, err := directory.Lstat(name)
	if err != nil {
		return nil, nil, err
	}

	if expected.Mode()&fs.ModeSymlink != 0 || !expected.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%w: mutation target is not regular", workspace.ErrUnsupportedType)
	}

	if expected.Size() > maxBytes {
		return nil, nil, errFileTooLarge
	}

	file, err := directory.Open(name)
	if err != nil {
		return nil, nil, err
	}

	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}

	if !os.SameFile(expected, opened) || !opened.Mode().IsRegular() {
		_ = file.Close()
		return nil, nil, workspace.ErrChanged
	}

	data, readErr := io.ReadAll(io.LimitReader(file, maxBytes+1))

	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, nil, errors.Join(readErr, closeErr)
	}

	if int64(len(data)) > maxBytes {
		return nil, nil, errFileTooLarge
	}

	return data, opened, nil
}

func writeStagedFile(file *os.File, content []byte, mode fs.FileMode) error {
	if err := file.Chmod(mode.Perm()); err != nil {
		_ = file.Close()
		return err
	}

	written, writeErr := io.Copy(file, bytes.NewReader(content))
	if writeErr == nil && written != int64(len(content)) {
		writeErr = io.ErrShortWrite
	}

	syncErr := file.Sync()
	closeErr := file.Close()

	return errors.Join(writeErr, syncErr, closeErr)
}

func temporaryName(prefix string) (string, error) {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("coding tools: generate temporary name: %w", err)
	}

	return prefix + hex.EncodeToString(value), nil
}

func displayPath(parent, base string) string {
	if parent == "." {
		return base
	}

	return parent + "/" + base
}
