package teamworktree

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/rsbin1178/pips/internal/coding/execution/gitcontrol"
)

// subagentWorktreesDirectory is the fixed child-Worktree parent below the
// shared Worktrees root.
const subagentWorktreesDirectory = "subagents"

// SubagentRequest identifies one preserved child Worktree.
type SubagentRequest struct {
	// ID is the child identity. It names the Worktree directory.
	ID string
	// Workspace is the canonical parent workspace path. It must be the Git top
	// level and must already have one commit.
	Workspace string
}

// SubagentWorktree is the exact external identity of one preserved child
// Worktree. It carries no result ref and no capture state: the child's work is
// preserved in place on its own branch rather than committed into a durable
// integration result.
type SubagentWorktree struct {
	ID         string
	Workspace  FileIdentity
	Directory  FileIdentity
	GitDir     FileIdentity
	CommonDir  FileIdentity
	BranchRef  string
	BaseOID    string
	LockReason string
}

// base projects one child Worktree into its materialization identity.
func (w SubagentWorktree) base() worktreeBase {
	return worktreeBase{
		workspace: w.Workspace.Path, gitDir: w.GitDir.Path,
		directory: w.Directory.Path, baseOID: w.BaseOID,
	}
}

// CreateSubagent provisions one preserved, linked Worktree for a child
// execution. The Worktree starts at the workspace's current HEAD and is locked
// under this package's control reason.
//
// The Worktree is never removed by this package. The caller reports its
// directory and keeps it for inspection after the child finishes, so the child
// can work — and commit — without touching the parent working tree. A partial
// resource is returned with an error when Git created the directory but a
// later identity check failed, which is the same recovery evidence an Attempt
// Worktree reports.
//
//nolint:gocyclo // Provisioning keeps every identity, branch, rollback, and retained-state edge explicit.
func (m *Manager) CreateSubagent(
	ctx context.Context,
	request SubagentRequest,
) (SubagentWorktree, error) {
	if err := ctx.Err(); err != nil {
		return SubagentWorktree{}, err
	}

	if m == nil || m.git == nil {
		return SubagentWorktree{}, ErrInvalid
	}

	if !safeIDPattern.MatchString(request.ID) {
		return SubagentWorktree{}, fmt.Errorf("%w: invalid subagent id", ErrInvalid)
	}

	if err := validateAbsolutePath(request.Workspace); err != nil {
		return SubagentWorktree{}, err
	}

	if err := m.validateRoots(); err != nil {
		return SubagentWorktree{}, err
	}

	workspace, err := canonicalDirectory(request.Workspace)
	if err != nil {
		return SubagentWorktree{}, err
	}

	repository, err := m.git.InspectRepository(ctx, workspace.Path)
	if err != nil {
		return SubagentWorktree{}, mapGitError(err)
	}

	if repository.TopLevel != workspace.Path {
		return SubagentWorktree{}, fmt.Errorf("%w: Workspace is not repository root", ErrIdentity)
	}

	if err := validateOID(repository.HeadOID); err != nil {
		return SubagentWorktree{}, fmt.Errorf("%w: Workspace has no commit to isolate from", ErrInvalid)
	}

	commonDir, err := canonicalDirectory(repository.CommonDir)
	if err != nil {
		return SubagentWorktree{}, err
	}

	if err := m.git.ValidateSafeConfig(ctx, workspace.Path); err != nil {
		return SubagentWorktree{}, mapGitError(err)
	}

	token := stableToken(request.ID)

	directory := filepath.Join(
		m.worktreesRoot.Path, subagentWorktreesDirectory, stableToken(workspace.Path), "subagent-"+request.ID,
	)
	if pathWithin(directory, workspace.Path) || pathWithin(directory, commonDir.Path) ||
		pathWithin(directory, m.productRoot) {
		return SubagentWorktree{}, fmt.Errorf("%w: derived Worktree overlaps protected root", ErrInvalid)
	}

	if err := m.ensureTargetAbsent(directory); err != nil {
		return SubagentWorktree{}, err
	}

	branchRef := "refs/heads/pips/subagent/" + token
	if _, err := m.git.ResolveRef(ctx, workspace.Path, branchRef); !errors.Is(err, gitcontrol.ErrNotFound) {
		if err == nil {
			return SubagentWorktree{}, fmt.Errorf("%w: owned branch already exists", ErrConflict)
		}

		return SubagentWorktree{}, mapGitError(err)
	}

	if err := m.ensureTargetParent(directory); err != nil {
		return SubagentWorktree{}, err
	}

	partial := SubagentWorktree{
		ID: "sw-" + token[:24], Workspace: workspace, Directory: FileIdentity{Path: directory},
		CommonDir: commonDir, BranchRef: branchRef, BaseOID: repository.HeadOID,
		LockReason: "pips/subagent-worktree/v1 id=" + token,
	}
	if err := m.git.CreateRef(
		ctx, workspace.Path, repository.ObjectFormat, branchRef, repository.HeadOID, refReason,
	); err != nil {
		return SubagentWorktree{}, fmt.Errorf("coding subagent worktree: create owned branch: %w", mapGitError(err))
	}

	if err := m.git.AddWorktree(
		ctx, workspace.Path, directory, branchRef, partial.LockReason,
	); err != nil {
		deleteErr := m.git.DeleteRef(ctx, workspace.Path, branchRef, repository.HeadOID, refReason)

		cause := fmt.Errorf("coding subagent worktree: add linked Worktree: %w", mapGitError(err))
		if deleteErr != nil {
			return partial, errors.Join(cause, mapGitError(deleteErr))
		}

		return SubagentWorktree{}, cause
	}

	bound, err := m.bindSubagentWorktree(ctx, partial)
	if err != nil {
		return partial, err
	}

	if err := m.materializeBase(ctx, bound.base()); err != nil {
		return bound, err
	}

	return bound, nil
}

// bindSubagentWorktree proves the created directory is the exact linked
// Worktree this package asked Git for, at the expected base commit.
func (m *Manager) bindSubagentWorktree(
	ctx context.Context,
	worktree SubagentWorktree,
) (SubagentWorktree, error) {
	directory, err := canonicalDirectory(worktree.Directory.Path)
	if err != nil {
		return worktree, err
	}

	worktree.Directory = directory

	repository, err := m.git.InspectRepository(ctx, directory.Path)
	if err != nil {
		return worktree, mapGitError(err)
	}

	gitDir, err := canonicalDirectory(repository.GitDir)
	if err != nil {
		return worktree, err
	}

	worktree.GitDir = gitDir

	actualCommon, err := canonicalDirectory(repository.CommonDir)
	if err != nil {
		return worktree, err
	}

	if actualCommon != worktree.CommonDir || repository.TopLevel != directory.Path ||
		repository.HeadOID != worktree.BaseOID || repository.BranchRef != worktree.BranchRef {
		return worktree, ErrIdentity
	}

	return worktree, nil
}
