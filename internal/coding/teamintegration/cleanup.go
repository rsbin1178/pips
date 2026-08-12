//nolint:wsl_v5 // Every non-force mutation remains adjacent to its identity check.
package teamintegration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/rsbin1178/pips/internal/coding/execution/gitcontrol"
	"github.com/rsbin1178/pips/internal/coding/workspace"
)

// Cleanup removes one exact clean Integration Worktree and its owned refs.
// It validates a terminal journal before mutation and never uses force,
// recursive deletion, prune, or heuristic identity recovery.
//
//nolint:gocyclo // Each exact mutation and retained-evidence edge stays explicit.
func (m *Manager) Cleanup(ctx context.Context, request CleanupRequest) (Cleanup, error) {
	if err := m.validate(); err != nil {
		return Cleanup{}, err
	}
	resource := request.Resource
	if request.TeamID == "" || !validIntegrationID(resource.ID) ||
		resource.ObjectFormat == "" || resource.BranchRef == "" ||
		resource.IntegrationRef == "" || resource.BaseOID == "" ||
		resource.CommitOID == "" || resource.LockReason == "" {
		return Cleanup{}, fmt.Errorf("%w: cleanup request", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return Cleanup{}, err
	}
	if err := m.validateCleanupResource(ctx, request); err != nil {
		return Cleanup{}, retainedIntegration(resource.ID, err)
	}
	journal, err := m.cleanupJournalState(resource.ID)
	if err != nil {
		return Cleanup{}, retainedIntegration(resource.ID, err)
	}

	if err := m.git.UnlockWorktree(ctx, resource.Workspace.Path(), resource.Directory.Path()); err != nil {
		return Cleanup{}, retainedIntegration(resource.ID, err)
	}
	if err := m.git.RemoveWorktree(ctx, resource.Workspace.Path(), resource.Directory.Path()); err != nil {
		relockErr := m.git.LockWorktree(
			ctx, resource.Workspace.Path(), resource.Directory.Path(), resource.LockReason,
		)

		return Cleanup{}, retainedIntegration(resource.ID, errors.Join(err, relockErr))
	}

	result := Cleanup{WorktreeRemoved: true}
	if _, err := os.Lstat(resource.Directory.Path()); !errors.Is(err, fs.ErrNotExist) {
		return result, retainedIntegration(resource.ID, fmt.Errorf("%w: Worktree path remains", ErrStale))
	}
	worktrees, err := m.git.Worktrees(ctx, resource.Workspace.Path())
	if err != nil {
		return result, retainedIntegration(resource.ID, err)
	}
	for _, worktree := range worktrees {
		if worktree.Path == resource.Directory.Path() {
			return result, retainedIntegration(resource.ID, fmt.Errorf("%w: Worktree record remains", ErrStale))
		}
	}

	if err := m.git.DeleteRef(
		ctx, resource.Workspace.Path(), resource.BranchRef,
		resource.CommitOID, integrationRefReason,
	); err != nil {
		return result, retainedIntegration(resource.ID, err)
	}
	result.BranchDeleted = true
	if err := verifyRefAbsent(ctx, m, resource.Workspace.Path(), resource.BranchRef); err != nil {
		return result, retainedIntegration(resource.ID, err)
	}

	if err := m.git.DeleteRef(
		ctx, resource.Workspace.Path(), resource.IntegrationRef,
		resource.CommitOID, integrationRefReason,
	); err != nil {
		return result, retainedIntegration(resource.ID, err)
	}
	result.IntegrationRefDeleted = true
	if err := verifyRefAbsent(ctx, m, resource.Workspace.Path(), resource.IntegrationRef); err != nil {
		return result, retainedIntegration(resource.ID, err)
	}

	if journal {
		if err := m.removeCleanupJournal(resource.ID); err != nil {
			return result, retainedIntegration(resource.ID, err)
		}
		result.JournalRemoved = true
	}
	m.mu.Lock()
	delete(m.records, resource.ID)
	m.mu.Unlock()

	return result, nil
}

//nolint:gocyclo // Full identity validation deliberately checks every immutable binding.
func (m *Manager) validateCleanupResource(ctx context.Context, request CleanupRequest) error {
	resource := request.Resource
	for _, identity := range []workspace.Identity{
		resource.Workspace, resource.Directory, resource.GitDir, resource.CommonDir,
	} {
		if err := validateCleanupIdentity(identity); err != nil {
			return err
		}
	}
	derived := m.derive(request.TeamID, resource.ID, resource.Workspace.Key())
	if derived.directory != resource.Directory.Path() ||
		derived.branchRef != resource.BranchRef ||
		derived.integrationRef != resource.IntegrationRef ||
		derived.lockReason != resource.LockReason {
		return fmt.Errorf("%w: derived Integration identity changed", ErrStale)
	}

	repository, err := m.git.InspectRepository(ctx, resource.Directory.Path())
	if err != nil {
		return err
	}
	if repository.TopLevel != resource.Directory.Path() ||
		repository.GitDir != resource.GitDir.Path() ||
		repository.CommonDir != resource.CommonDir.Path() ||
		repository.ObjectFormat != resource.ObjectFormat ||
		repository.BranchRef != resource.BranchRef ||
		repository.HeadOID != resource.CommitOID {
		return fmt.Errorf("%w: Integration repository changed", ErrStale)
	}
	status, err := m.git.SnapshotStatus(ctx, resource.Directory.Path())
	if err != nil {
		return err
	}
	if !status.Clean {
		return fmt.Errorf("%w: Integration Worktree is dirty", ErrRetained)
	}
	for _, ref := range []string{resource.BranchRef, resource.IntegrationRef} {
		oid, err := m.git.ResolveRef(ctx, resource.Workspace.Path(), ref)
		if err != nil || oid != resource.CommitOID {
			return errors.Join(fmt.Errorf("%w: Integration ref changed", ErrStale), err)
		}
	}
	worktrees, err := m.git.Worktrees(ctx, resource.Workspace.Path())
	if err != nil {
		return err
	}
	for _, worktree := range worktrees {
		if worktree.Path != resource.Directory.Path() {
			continue
		}
		if worktree.HeadOID != resource.CommitOID || worktree.BranchRef != resource.BranchRef ||
			!worktree.Locked || worktree.LockReason != resource.LockReason {
			return fmt.Errorf("%w: Integration Worktree record changed", ErrStale)
		}

		return nil
	}

	return fmt.Errorf("%w: Integration Worktree record missing", ErrStale)
}

func validateCleanupIdentity(expected workspace.Identity) error {
	if expected.Path() == "" || !filepath.IsAbs(expected.Path()) ||
		expected.Device() == 0 || expected.Inode() == 0 {
		return fmt.Errorf("%w: incomplete filesystem identity", ErrInvalid)
	}
	actual, err := workspace.Open(expected.Path())
	if err != nil {
		return err
	}
	if actual.Identity().Key() != expected.Key() {
		return fmt.Errorf("%w: filesystem identity changed", ErrStale)
	}

	return nil
}

func verifyRefAbsent(ctx context.Context, m *Manager, directory, ref string) error {
	_, err := m.git.ResolveRef(ctx, directory, ref)
	if !errors.Is(err, gitcontrol.ErrNotFound) {
		return errors.Join(fmt.Errorf("%w: deleted ref remains", ErrStale), err)
	}

	return nil
}

func (m *Manager) cleanupJournalState(id string) (bool, error) {
	journal, err := m.readJournal(id)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if journal.State != journalApplied && journal.State != journalRolledBack {
		return false, ErrRecovery
	}

	return true, nil
}

//nolint:gocyclo // Journal removal revalidates every private-directory identity edge before deletion.
func (m *Manager) removeCleanupJournal(id string) error {
	root, err := os.OpenRoot(m.integrationsRoot.Root())
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	info, err := root.Lstat(id)
	if err != nil || !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 ||
		info.Mode().Perm() != privateDirectoryMode {
		return errors.Join(ErrRecovery, err)
	}
	directory, err := root.Open(id)
	if err != nil {
		return err
	}
	openedInfo, err := directory.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		_ = directory.Close()

		return errors.Join(ErrRecovery, err)
	}
	entries, readErr := directory.ReadDir(2)
	closeErr := directory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	if len(entries) != 1 || entries[0].Name() != journalFileName || entries[0].IsDir() {
		return fmt.Errorf("%w: unexpected journal directory contents", ErrRecovery)
	}
	journalInfo, err := root.Lstat(filepath.Join(id, journalFileName))
	if err != nil || !journalInfo.Mode().IsRegular() ||
		journalInfo.Mode()&fs.ModeSymlink != 0 || journalInfo.Mode().Perm() != privateFileMode {
		return errors.Join(ErrRecovery, err)
	}
	current, err := root.Lstat(id)
	if err != nil || !os.SameFile(info, current) {
		return errors.Join(ErrRecovery, err)
	}
	if err := root.Remove(filepath.Join(id, journalFileName)); err != nil {
		return err
	}
	if err := root.Remove(id); err != nil {
		return err
	}

	return syncDirectory(m.integrationsRoot.Root())
}

func retainedIntegration(id string, cause error) error {
	return &RetainedError{IntegrationID: id, Cause: cause}
}
