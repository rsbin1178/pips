package git

import (
	"context"
	"errors"
)

// ListWorkspaceFiles returns tracked and non-ignored untracked repository
// paths in stable order. repository is false only outside a Git worktree.
func (i *Inspector) ListWorkspaceFiles(ctx context.Context) ([]string, bool, error) {
	if i == nil {
		return nil, false, ErrClosed
	}

	i.mutex.Lock()
	defer i.mutex.Unlock()

	if i.closed {
		return nil, false, ErrClosed
	}

	if err := ctx.Err(); err != nil {
		return nil, false, err
	}

	ctx, cancel := context.WithTimeout(ctx, i.limits.InspectTime)
	defer cancel()

	if err := i.checkRepository(ctx); err != nil {
		if errors.Is(err, ErrNotRepository) {
			return []string{}, false, nil
		}

		return nil, false, err
	}

	paths, err := i.listPaths(ctx)
	if err != nil {
		return nil, true, err
	}

	return sortedPathKeys(paths), true, nil
}
