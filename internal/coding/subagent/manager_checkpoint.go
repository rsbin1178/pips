//nolint:wsl_v5 // Child request, authorization, and checkpoint state stay grouped.
package subagent

import (
	"context"
	"fmt"
	"slices"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/compaction"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/rsbin1178/pips/internal/coding/tools"
)

// The Manager-level history Tool is a declaration only. Its executable binding
// is always constructed for the newly admitted child, never its parent/sibling.
func (m *Manager) childTools(child *session.Handle, tracker *runTracker) []agent.Tool {
	bound := slices.Clone(m.tools)
	for index, tool := range bound {
		if tool.Decl().Name != tools.HistoryName {
			continue
		}
		bound[index] = tools.NewHistoryTool(func(ctx context.Context) (*session.ArchiveReader, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			tracker.mu.Lock()
			allowed := tracker.historyAllowed && !tracker.finalizationInjected
			tracker.mu.Unlock()
			if !allowed {
				return nil, session.ErrArchiveDenied
			}
			archives, err := m.config.Repository.Archives(child)
			if err != nil {
				return nil, err
			}
			return archives.Bind(compaction.ArchiveIDs(child.Session().Path()))
		})
	}

	return bound
}

func (t *runTracker) captureCheckpointRequest(request *ai.Request) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// A request policy may restrict tools, but must not restore evidence-gathering
	// capabilities once the Manager has entered its terminal finalization phase.
	if t.finalizationInjected {
		request.Tools = nil
		request.ToolChoice = ai.ToolChoice{}
	}
	t.historyAllowed = false
	for _, tool := range request.Tools {
		if tool.Name == tools.HistoryName && tool.IsEnabled() {
			t.historyAllowed = true
			break
		}
	}
	t.fixedTokens, t.checkpointErr = compaction.RequestOverheadTokens(*request)
}

// This hook runs after TurnCompleted. Harness flushes the complete call/result
// batch before forwarding that event and before Agent prepares the next turn.
// Capture additionally rejects any durable pending calls under its snapshot.
func (m *Manager) checkpointChildAfterToolTurn(
	ctx context.Context,
	child *session.Handle,
	tracker *runTracker,
) ([]ai.Message, error) {
	tracker.mu.Lock()
	fixed, allowed, finalized := tracker.fixedTokens, tracker.historyAllowed, tracker.finalizationInjected
	task, requestErr := tracker.task, tracker.checkpointErr
	tracker.mu.Unlock()
	if finalized {
		return nil, nil
	}
	if requestErr != nil {
		return nil, fmt.Errorf("coding subagent: estimate child checkpoint context: %w", requestErr)
	}
	plan, err := compaction.Capture(child.Session(), &fixed, func(message ai.Message) bool {
		// Builtin children have one immutable assignment. Stop-hook follow-ups
		// and finalization reminders are not newer user instructions.
		return messageText(message) != task
	})
	if err != nil {
		return nil, fmt.Errorf("coding subagent: capture child checkpoint: %w", err)
	}
	if !m.config.FullCompaction.ShouldCompact(plan.TokensBefore) {
		return nil, nil
	}
	if !allowed {
		return nil, fmt.Errorf("%w: full child compaction requires authorized %s access", ErrInvalid, tools.HistoryName)
	}
	_, err = compaction.Execute(ctx, m.config.Repository, child, m.config.Model,
		m.config.RequestPolicy, plan, *m.config.FullCompaction, "", "")
	if err != nil {
		// No retry or archive rollback here: a reported append/cleanup failure
		// may have committed. Stop this owner and preserve durable evidence.
		return nil, fmt.Errorf("coding subagent: checkpoint child context: %w", err)
	}
	visible, err := child.Session().Context()
	if err != nil {
		return nil, fmt.Errorf("coding subagent: reload child checkpoint context: %w", err)
	}

	return visible.Messages, nil
}
