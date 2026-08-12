package coding

import (
	"context"

	"github.com/rsbin1178/pips/internal/coding/attachment"
	"github.com/rsbin1178/pips/internal/coding/changes/git"
	"github.com/rsbin1178/pips/internal/coding/workspace"
)

// ListWorkspaceFiles returns a bounded content-free snapshot from the active
// Workspace. File contents are never read during discovery.
func (r *Runtime) ListWorkspaceFiles(ctx context.Context) (attachment.Snapshot, error) {
	tree, inspector, err := r.attachmentDependencies(ctx, "list Workspace files")
	if err != nil {
		return attachment.Snapshot{}, err
	}

	if inspector == nil {
		return attachment.Snapshot{}, ErrRuntimeInvalid
	}

	paths, repository, err := inspector.ListWorkspaceFiles(ctx)
	if err != nil {
		return attachment.Snapshot{}, err
	}

	if repository {
		return attachment.DiscoverCandidates(ctx, tree, paths)
	}

	return attachment.Discover(ctx, tree)
}

// ResolveWorkspaceFile reads one stable text or normalized image reference
// through the active Workspace tree.
func (r *Runtime) ResolveWorkspaceFile(
	ctx context.Context,
	reference attachment.Reference,
) (attachment.Resolved, error) {
	tree, err := r.attachmentTree(ctx, "resolve Workspace file")
	if err != nil {
		return attachment.Resolved{}, err
	}

	return attachment.Resolve(ctx, tree, reference)
}

func (r *Runtime) attachmentTree(
	ctx context.Context,
	operation string,
) (*workspace.Tree, error) {
	tree, _, err := r.attachmentDependencies(ctx, operation)

	return tree, err
}

func (r *Runtime) attachmentDependencies(
	ctx context.Context,
	operation string,
) (*workspace.Tree, *git.Inspector, error) {
	if r == nil {
		return nil, nil, ErrRuntimeClosed
	}

	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed || r.closing {
		return nil, nil, stateError(operation, r.state.Phase, ErrRuntimeClosed)
	}

	if r.tree == nil {
		return nil, nil, ErrRuntimeInvalid
	}

	return r.tree, r.inspector, nil
}
