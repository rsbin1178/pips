package teamworktree

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/rsbin/pips/internal/coding/execution/gitcontrol"
)

// Cleanup removes only an exact clean Worktree and CAS-deletes its owned
// branch. A captured result ref is deliberately retained.
//
//nolint:gocyclo // Cleanup keeps every non-force removal and retained-evidence edge explicit.
func (m *Manager) Cleanup(
	ctx context.Context,
	lease *Lease,
	resource Resource,
) (Cleanup, error) {
	status, err := m.Inspect(ctx, lease, resource)
	if err != nil {
		return Cleanup{}, err
	}

	if status.State != StateUnchangedClean && status.State != StateCapturedClean {
		return Cleanup{}, errors.Join(ErrRetained, ErrDirty)
	}

	expectedOID := resource.BaseOID
	if resource.ResultCommitOID != "" {
		expectedOID = resource.ResultCommitOID
	}

	if err := m.git.UnlockWorktree(ctx, resource.Workspace.Path, resource.Directory.Path); err != nil {
		return Cleanup{}, &RetainedError{Resource: resource, Cause: mapGitError(err)}
	}

	if err := m.git.RemoveWorktree(ctx, resource.Workspace.Path, resource.Directory.Path); err != nil {
		relockErr := m.git.LockWorktree(
			ctx, resource.Workspace.Path, resource.Directory.Path, resource.LockReason,
		)

		return Cleanup{}, &RetainedError{
			Resource: resource, Cause: errors.Join(mapGitError(err), mapOptionalGitError(relockErr)),
		}
	}

	result := Cleanup{WorktreeRemoved: true, ResultRetained: resource.ResultCommitOID != ""}
	if _, err := os.Lstat(resource.Directory.Path); !errors.Is(err, fs.ErrNotExist) {
		return result, &RetainedError{Resource: resource, Cause: fmt.Errorf("%w: Worktree path remains", ErrIdentity)}
	}

	worktrees, err := m.git.Worktrees(ctx, resource.Workspace.Path)
	if err != nil {
		return result, &RetainedError{Resource: resource, Cause: mapGitError(err)}
	}

	for _, worktree := range worktrees {
		if worktree.Path == resource.Directory.Path {
			return result, &RetainedError{Resource: resource, Cause: fmt.Errorf("%w: Worktree record remains", ErrIdentity)}
		}
	}

	if err := m.git.DeleteRef(
		ctx, resource.Workspace.Path, resource.BranchRef, expectedOID, refReason,
	); err != nil {
		return result, &RetainedError{Resource: resource, Cause: mapGitError(err)}
	}

	result.BranchDeleted = true
	if _, err := m.git.ResolveRef(ctx, resource.Workspace.Path, resource.BranchRef); !errors.Is(err, gitcontrol.ErrNotFound) {
		return result, &RetainedError{Resource: resource, Cause: fmt.Errorf("%w: branch ref remains", ErrIdentity)}
	}

	if resource.ResultCommitOID != "" {
		oid, err := m.git.ResolveRef(ctx, resource.Workspace.Path, resource.ResultRef)
		if err != nil || oid != resource.ResultCommitOID {
			return result, &RetainedError{
				Resource: resource, Cause: errors.Join(ErrIdentity, mapOptionalGitError(err)),
			}
		}
	}

	return result, nil
}
