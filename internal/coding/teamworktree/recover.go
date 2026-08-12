package teamworktree

import (
	"context"
	"errors"
	"fmt"

	"github.com/rsbin1178/pips/internal/coding/execution/gitcontrol"
)

// Recover reconstructs and verifies the deterministic resource created for a
// request. It covers the crash window where Git creation completed before the
// application resource journal did. It never creates or mutates a Worktree.
func (m *Manager) Recover(
	ctx context.Context,
	lease *Lease,
	request CreateRequest,
) (Resource, error) {
	if err := ctx.Err(); err != nil {
		return Resource{}, err
	}
	if err := validateOwner(request.Owner); err != nil {
		return Resource{}, err
	}
	if err := validateOID(request.BaseOID); err != nil {
		return Resource{}, err
	}
	if err := validateAbsolutePath(request.Workspace); err != nil {
		return Resource{}, err
	}
	if err := m.validateLease(lease, request.Owner); err != nil {
		return Resource{}, err
	}
	if err := m.validateRoots(); err != nil {
		return Resource{}, err
	}

	workspace, err := canonicalDirectory(request.Workspace)
	if err != nil {
		return Resource{}, err
	}
	parent, err := m.git.InspectRepository(ctx, workspace.Path)
	if err != nil {
		return Resource{}, mapGitError(err)
	}
	if parent.TopLevel != workspace.Path {
		return Resource{}, fmt.Errorf("%w: Workspace is not repository root", ErrIdentity)
	}
	commonDir, err := canonicalDirectory(parent.CommonDir)
	if err != nil {
		return Resource{}, err
	}

	derived := m.derive(request.Owner, workspace)
	directory, err := canonicalDirectory(derived.directory)
	if err != nil {
		return Resource{}, err
	}
	child, err := m.git.InspectRepository(ctx, directory.Path)
	if err != nil {
		return Resource{}, mapGitError(err)
	}
	gitDir, err := canonicalDirectory(child.GitDir)
	if err != nil {
		return Resource{}, err
	}
	if child.TopLevel != directory.Path || child.CommonDir != commonDir.Path ||
		child.ObjectFormat != parent.ObjectFormat || child.BranchRef != derived.branchRef {
		return Resource{}, fmt.Errorf("%w: recovered Worktree identity mismatch", ErrIdentity)
	}

	resultCommit := ""
	resultOID, resolveErr := m.git.ResolveRef(ctx, workspace.Path, derived.resultRef)
	switch {
	case resolveErr == nil:
		resultCommit = resultOID
	case errors.Is(resolveErr, gitcontrol.ErrNotFound):
	default:
		return Resource{}, mapGitError(resolveErr)
	}

	resource := Resource{
		ID: derived.id, Owner: request.Owner, Workspace: workspace,
		Directory: directory, GitDir: gitDir, CommonDir: commonDir,
		ObjectFormat: parent.ObjectFormat, BranchRef: derived.branchRef,
		ResultRef: derived.resultRef, BaseOID: request.BaseOID,
		ResultCommitOID: resultCommit, LockReason: derived.lockReason,
	}
	if _, err := m.verifyResource(ctx, lease, resource); err != nil {
		return Resource{}, err
	}

	return resource, nil
}
