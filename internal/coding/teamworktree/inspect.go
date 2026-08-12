package teamworktree

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/rsbin1178/pips/internal/coding/execution/gitcontrol"
)

type verifiedResource struct {
	expectedOID string
	treeOID     string
}

// Inspect returns bounded exact Worktree state without changing refs or index.
func (m *Manager) Inspect(
	ctx context.Context,
	lease *Lease,
	resource Resource,
) (Status, error) {
	verified, err := m.verifyResource(ctx, lease, resource)
	if err != nil {
		return Status{}, err
	}

	return m.inspectVerified(ctx, resource, verified)
}

// InspectRetained verifies bounded exact Worktree state without acquiring or
// requiring the Team mutation lease. It is intended only for startup recovery
// discovery; callers must acquire a newer lease and call Takeover before any
// mutation or Worker Runtime is opened.
func (m *Manager) InspectRetained(
	ctx context.Context,
	resource Resource,
) (Status, error) {
	verified, err := m.verifyResourceIdentity(ctx, resource)
	if err != nil {
		return Status{}, err
	}

	return m.inspectVerified(ctx, resource, verified)
}

func (m *Manager) inspectVerified(
	ctx context.Context,
	resource Resource,
	verified verifiedResource,
) (Status, error) {

	expected, err := m.treeManifest(ctx, resource.Workspace.Path, verified.expectedOID)
	if err != nil {
		return Status{}, err
	}

	actual, err := m.scanWorktree(ctx, resource, expected, false)
	if err != nil {
		return Status{}, err
	}

	equal := manifestsEqual(expected, actual)
	state := StateChanged

	switch {
	case resource.ResultCommitOID == "" && equal:
		state = StateUnchangedClean
	case resource.ResultCommitOID != "" && equal:
		state = StateCapturedClean
	case resource.ResultCommitOID != "":
		state = StateDirtyAfterCapture
	}

	return Status{
		State: state, HeadOID: verified.expectedOID, TreeOID: verified.treeOID,
		ManifestDigest: manifestDigest(actual), Files: len(actual), Bytes: manifestBytes(actual),
	}, nil
}

// Takeover binds an exactly retained resource to a newly acquired higher Team
// lease generation. It performs no Git unlock/relock mutation.
func (m *Manager) Takeover(
	ctx context.Context,
	lease *Lease,
	resource Resource,
) (Resource, error) {
	if err := validateResource(resource); err != nil {
		return Resource{}, err
	}

	if lease == nil {
		return Resource{}, ErrLeaseLost
	}

	lease.mu.Lock()
	valid := !lease.closed && lease.manager == m && lease.teamID == resource.Owner.TeamID &&
		lease.generation > resource.Owner.LeaseGeneration
	generation := lease.generation
	lease.mu.Unlock()

	if !valid {
		return Resource{}, ErrLeaseLost
	}

	updated := resource

	updated.Owner.LeaseGeneration = generation
	if _, err := m.verifyResource(ctx, lease, updated); err != nil {
		return Resource{}, err
	}

	return updated, nil
}

//nolint:gocyclo,funlen // Every identity edge is checked independently and fail-closed.
func (m *Manager) verifyResource(
	ctx context.Context,
	lease *Lease,
	resource Resource,
) (verifiedResource, error) {
	if err := validateResource(resource); err != nil {
		return verifiedResource{}, err
	}

	if err := m.validateLease(lease, resource.Owner); err != nil {
		return verifiedResource{}, err
	}

	return m.verifyResourceIdentity(ctx, resource)
}

//nolint:gocyclo,funlen // Every identity edge is checked independently and fail-closed.
func (m *Manager) verifyResourceIdentity(
	ctx context.Context,
	resource Resource,
) (verifiedResource, error) {
	if err := ctx.Err(); err != nil {
		return verifiedResource{}, err
	}

	if err := validateResource(resource); err != nil {
		return verifiedResource{}, err
	}

	if err := m.validateRoots(); err != nil {
		return verifiedResource{}, err
	}

	for _, identity := range []FileIdentity{
		resource.Workspace, resource.Directory, resource.GitDir, resource.CommonDir,
	} {
		if err := sameFileIdentity(identity); err != nil {
			return verifiedResource{}, errors.Join(ErrIdentity, err)
		}
	}

	derived := m.derive(resource.Owner, resource.Workspace)
	if resource.ID != derived.id || resource.BranchRef != derived.branchRef ||
		resource.ResultRef != derived.resultRef || resource.Directory.Path != derived.directory ||
		resource.LockReason != derived.lockReason {
		return verifiedResource{}, fmt.Errorf("%w: derived ownership mismatch", ErrIdentity)
	}

	parent, err := m.git.InspectRepository(ctx, resource.Workspace.Path)
	if err != nil {
		return verifiedResource{}, mapGitError(err)
	}

	if parent.TopLevel != resource.Workspace.Path || parent.CommonDir != resource.CommonDir.Path ||
		parent.ObjectFormat != resource.ObjectFormat {
		return verifiedResource{}, fmt.Errorf("%w: parent repository mismatch", ErrIdentity)
	}

	if err := m.git.ValidateSafeConfig(ctx, resource.Workspace.Path); err != nil {
		return verifiedResource{}, mapGitError(err)
	}

	child, err := m.git.InspectRepository(ctx, resource.Directory.Path)
	if err != nil {
		return verifiedResource{}, mapGitError(err)
	}

	expectedOID := resource.BaseOID
	if resource.ResultCommitOID != "" {
		expectedOID = resource.ResultCommitOID
	}

	if child.TopLevel != resource.Directory.Path || child.GitDir != resource.GitDir.Path ||
		child.CommonDir != resource.CommonDir.Path || child.ObjectFormat != resource.ObjectFormat ||
		child.HeadOID != expectedOID || child.BranchRef != resource.BranchRef {
		return verifiedResource{}, fmt.Errorf("%w: linked repository mismatch", ErrIdentity)
	}

	if err := m.git.ValidateSafeConfig(ctx, resource.Directory.Path); err != nil {
		return verifiedResource{}, mapGitError(err)
	}

	worktrees, err := m.git.Worktrees(ctx, resource.Workspace.Path)
	if err != nil {
		return verifiedResource{}, mapGitError(err)
	}

	var matched *gitcontrol.Worktree

	for index := range worktrees {
		if worktrees[index].Path == resource.Directory.Path {
			if matched != nil {
				return verifiedResource{}, fmt.Errorf("%w: duplicate Worktree identity", ErrIdentity)
			}

			matched = &worktrees[index]
		}
	}

	if matched == nil || matched.HeadOID != expectedOID || matched.BranchRef != resource.BranchRef ||
		!matched.Locked || matched.LockReason != resource.LockReason || matched.Prunable {
		return verifiedResource{}, fmt.Errorf("%w: Git Worktree lock/ref mismatch", ErrIdentity)
	}

	branchOID, err := m.git.ResolveRef(ctx, resource.Workspace.Path, resource.BranchRef)
	if err != nil || branchOID != expectedOID {
		return verifiedResource{}, errors.Join(ErrIdentity, mapOptionalGitError(err))
	}

	resultOID, err := m.git.ResolveRef(ctx, resource.Workspace.Path, resource.ResultRef)
	if resource.ResultCommitOID == "" {
		if !errors.Is(err, gitcontrol.ErrNotFound) {
			return verifiedResource{}, fmt.Errorf("%w: unexpected result ref", ErrIdentity)
		}
	} else if err != nil || resultOID != resource.ResultCommitOID {
		return verifiedResource{}, errors.Join(ErrIdentity, mapOptionalGitError(err))
	}

	if _, err := m.git.ResolveCommit(ctx, resource.Workspace.Path, resource.BaseOID); err != nil {
		return verifiedResource{}, mapGitError(err)
	}

	indexPath := filepath.Join(resource.GitDir.Path, "index")

	unmerged, err := m.git.UnmergedIndex(ctx, resource.Directory.Path, indexPath)
	if err != nil {
		return verifiedResource{}, mapGitError(err)
	}

	if unmerged {
		return verifiedResource{}, fmt.Errorf("%w: unmerged linked index", ErrUnsafeRepository)
	}

	indexTree, err := m.git.WriteTree(ctx, resource.Directory.Path, indexPath)
	if err != nil {
		return verifiedResource{}, mapGitError(err)
	}

	treeOID, err := m.git.ResolveTree(ctx, resource.Workspace.Path, expectedOID)
	if err != nil {
		return verifiedResource{}, mapGitError(err)
	}

	if indexTree != treeOID {
		return verifiedResource{}, fmt.Errorf("%w: linked index tree mismatch", ErrIdentity)
	}

	return verifiedResource{expectedOID: expectedOID, treeOID: treeOID}, nil
}

func mapGitError(err error) error {
	if err == nil {
		return nil
	}

	switch {
	case errors.Is(err, gitcontrol.ErrLimit):
		return errors.Join(ErrLimit, err)
	case errors.Is(err, gitcontrol.ErrUnsafeConfig):
		return errors.Join(ErrUnsafeRepository, err)
	case errors.Is(err, gitcontrol.ErrConflict):
		return errors.Join(ErrConflict, err)
	case errors.Is(err, gitcontrol.ErrExecutableChanged):
		return errors.Join(ErrIdentity, err)
	case errors.Is(err, gitcontrol.ErrInvalid):
		return errors.Join(ErrInvalid, err)
	default:
		return errors.Join(ErrGit, err)
	}
}

func mapOptionalGitError(err error) error {
	if err == nil {
		return nil
	}

	return mapGitError(err)
}
