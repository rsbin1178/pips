package coding

import (
	"context"

	"github.com/rsbin/pips/internal/coding/attachment"
	"github.com/rsbin/pips/internal/coding/workspace"
)

// ListWorkspaceFiles returns a bounded content-free snapshot from the active
// Workspace. File contents are never read during discovery.
func (r *Runtime) ListWorkspaceFiles(ctx context.Context) (attachment.Snapshot, error) {
	tree, err := r.attachmentTree(ctx, "list Workspace files")
	if err != nil {
		return attachment.Snapshot{}, err
	}

	return attachment.Discover(ctx, tree)
}

// ResolveWorkspaceFile reads one stable text reference through the active
// Workspace tree. Image references remain deferred to image normalization.
func (r *Runtime) ResolveWorkspaceFile(
	ctx context.Context,
	reference attachment.Reference,
) (attachment.Text, error) {
	tree, err := r.attachmentTree(ctx, "resolve Workspace file")
	if err != nil {
		return attachment.Text{}, err
	}

	return attachment.ResolveText(ctx, tree, reference)
}

func (r *Runtime) attachmentTree(
	ctx context.Context,
	operation string,
) (*workspace.Tree, error) {
	if r == nil {
		return nil, ErrRuntimeClosed
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed || r.closing {
		return nil, stateError(operation, r.state.Phase, ErrRuntimeClosed)
	}

	if r.tree == nil {
		return nil, ErrRuntimeInvalid
	}

	return r.tree, nil
}
