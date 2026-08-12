package coding

import (
	"context"

	"github.com/rsbin1178/pips/internal/coding/changes"
)

// WorkspaceStatus returns bounded current Git worktree truth. It does not use
// or consume the interaction-attribution baseline.
func (r *Runtime) WorkspaceStatus(ctx context.Context) (changes.WorktreeStatus, error) {
	if r == nil {
		return changes.WorktreeStatus{}, ErrRuntimeClosed
	}

	if err := ctx.Err(); err != nil {
		return changes.WorktreeStatus{}, err
	}

	r.mu.Lock()
	if r.closed || r.closing {
		phase := r.state.Phase
		r.mu.Unlock()

		return changes.WorktreeStatus{}, stateError(
			"inspect workspace status", phase, ErrRuntimeClosed,
		)
	}

	if r.active != nil || r.state.Phase != PhaseIdle {
		phase := r.state.Phase
		r.mu.Unlock()

		return changes.WorktreeStatus{}, stateError(
			"inspect workspace status", phase, ErrRuntimeBusy,
		)
	}

	inspector := r.inspector
	r.mu.Unlock()

	if inspector == nil {
		return changes.WorktreeStatus{}, ErrRuntimeInvalid
	}

	return inspector.Status(ctx)
}
