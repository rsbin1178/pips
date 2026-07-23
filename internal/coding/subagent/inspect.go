package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/session"
)

// List returns newest-first durable children owned by this manager's exact
// parent conversation.
func (m *Manager) List(ctx context.Context) ([]Summary, error) {
	if m == nil {
		return nil, ErrClosed
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	parent := m.config.Parent.Metadata()

	metas, err := m.config.Repository.ListSubagents(ctx, parent.WorkspaceID, parent.ID)
	if err != nil {
		return nil, err
	}

	latest, err := latestByChild(m.config.Parent.Session().Entries())
	if err != nil {
		return nil, err
	}

	values := make([]Summary, 0, len(metas))

	seen := make(map[string]struct{}, len(metas))
	for _, meta := range metas {
		seen[meta.ID] = struct{}{}

		value, ok := latest[meta.ID]
		if !ok {
			return nil, fmt.Errorf("%w: parent index is missing child %q", ErrInvalid, meta.ID)
		}

		if err := validateLineage(parent, meta, value); err != nil {
			return nil, err
		}

		values = append(values, summaryFrom(meta, value))
	}

	for childSessionID := range latest {
		if _, ok := seen[childSessionID]; !ok {
			return nil, fmt.Errorf("%w: parent index references a missing child", ErrInvalid)
		}
	}

	return values, nil
}

// Inspect loads one current-parent child transcript and validated structured
// result. Arbitrary session IDs outside the current parent are rejected.
func (m *Manager) Inspect(
	ctx context.Context,
	childSessionID string,
) (Detail, error) {
	if m == nil {
		return Detail{}, ErrClosed
	}

	if err := session.ValidateID(childSessionID); err != nil {
		return Detail{}, fmt.Errorf("%w: invalid child session id", ErrInvalid)
	}

	m.journalMu.Lock()
	defer m.journalMu.Unlock()

	selected, err := m.findSummary(ctx, childSessionID)
	if err != nil {
		return Detail{}, err
	}

	child, owned, err := m.childHandle(ctx, childSessionID)
	if err != nil {
		return Detail{}, err
	}

	return m.inspectChild(child, owned, selected)
}

func (m *Manager) findSummary(ctx context.Context, childSessionID string) (Summary, error) {
	values, err := m.List(ctx)
	if err != nil {
		return Summary{}, err
	}

	for _, value := range values {
		if value.ChildSessionID == childSessionID {
			return value, nil
		}
	}

	return Summary{}, fmt.Errorf("%w: child does not belong to current parent", ErrInvalid)
}

func (m *Manager) inspectChild(
	child *session.Handle,
	owned bool,
	selected Summary,
) (_ Detail, returnErr error) {
	if owned {
		defer func() { returnErr = errors.Join(returnErr, child.Close()) }()
	}

	parent := m.config.Parent.Metadata()
	childMeta := child.Metadata()

	childLatest, err := latestByChild(child.Session().Entries())
	if err != nil {
		return Detail{}, err
	}

	value, ok := childLatest[selected.ChildSessionID]
	if !ok {
		return Detail{}, fmt.Errorf("%w: child lifecycle is missing", ErrInvalid)
	}

	if err := validateLineage(parent, childMeta, value); err != nil {
		return Detail{}, err
	}

	if summaryFrom(childMeta, value) != selected {
		return Detail{}, fmt.Errorf("%w: parent and child lifecycle differ", ErrInvalid)
	}

	contextValue, err := child.Session().Context()
	if err != nil {
		return Detail{}, fmt.Errorf("coding subagent: load transcript: %w", err)
	}

	detail := Detail{Summary: selected, Transcript: contextValue.Messages}
	if selected.State == StateCreated || selected.State == StateRunning {
		detail = m.overlayLiveDetail(detail)
	}

	if selected.State == StateSucceeded {
		text, ok := finalAssistantText(detail.Transcript)
		if !ok {
			return Detail{}, fmt.Errorf("%w: successful child has no final assistant result", ErrInvalid)
		}

		spec, specErr := specFor(selected.Role)
		if specErr != nil {
			return Detail{}, specErr
		}

		detail.Result, err = spec.decode(text, value.Limits.limits())
		if err != nil {
			return Detail{}, fmt.Errorf("%w: persisted child result: %w", ErrInvalid, err)
		}
	}

	return cloneDetail(detail), nil
}

func (m *Manager) childHandle(
	ctx context.Context,
	childSessionID string,
) (*session.Handle, bool, error) {
	m.mu.Lock()

	active := m.active
	if active != nil && active.child != nil && active.child.Metadata().ID == childSessionID {
		child := active.child
		m.mu.Unlock()

		return child, false, nil
	}

	parent := m.config.Parent.Metadata()
	m.mu.Unlock()

	child, err := m.config.Repository.Open(ctx, session.OpenOptions{
		ID: childSessionID, WorkspaceID: parent.WorkspaceID,
	})
	if err != nil {
		return nil, false, err
	}

	return child, true, nil
}

func validateLineage(parent, child session.Metadata, value record) error {
	if child.Kind != session.KindSubagent || child.WorkspaceID != parent.WorkspaceID ||
		child.ParentSessionID != parent.ID || value.ChildSessionID != child.ID ||
		value.ParentSessionID != parent.ID || value.Role != Role(child.Agent) ||
		value.ParentRunID != child.ParentRunID {
		return fmt.Errorf("%w: child lineage mismatch", ErrInvalid)
	}

	return nil
}

func summaryFrom(meta session.Metadata, value record) Summary {
	return Summary{
		ChildSessionID: meta.ID,
		Role:           value.Role,
		State:          value.State,
		TaskPreview:    value.TaskPreview,
		Model:          value.Model,
		CreatedAt:      meta.CreatedAt,
		Duration:       time.Duration(value.DurationMillis) * time.Millisecond,
		Turns:          value.Turns,
		ToolCalls:      value.ToolCalls,
		Usage:          value.Usage,
		Code:           value.Code,
	}
}

func finalAssistantText(messages []ai.Message) (string, bool) {
	for _, v := range slices.Backward(messages) {
		if v.Role != ai.RoleAssistant {
			continue
		}

		return messageText(v), true
	}

	return "", false
}

func cloneDetail(value Detail) Detail {
	value.Transcript = cloneTranscript(value.Transcript)
	value.Activity = cloneActivity(value.Activity)
	value.Result = cloneResult(Result{Role: value.Summary.Role, Value: value.Result}).Value

	return value
}

func (m *Manager) overlayLiveDetail(detail Detail) Detail {
	active := m.activeExecution(detail.Summary.ChildSessionID)
	if active == nil || active.tracker == nil {
		return detail
	}

	snapshot, activity := active.tracker.snapshot()
	detail.Activity = activity
	detail.Summary.Turns = snapshot.turns
	detail.Summary.ToolCalls = snapshot.toolCalls

	detail.Summary.Usage = snapshot.usage
	if !activity.StartedAt.IsZero() {
		detail.Summary.Duration = max(0, time.Since(activity.StartedAt))
	}

	return detail
}

func cloneTranscript(messages []ai.Message) []ai.Message {
	cloned := make([]ai.Message, len(messages))
	for index, message := range messages {
		cloned[index] = cloneTranscriptMessage(message)
	}

	return cloned
}

func cloneTranscriptMessage(message ai.Message) ai.Message {
	data, err := json.Marshal(message)
	if err != nil {
		return ai.Message{Role: message.Role}
	}

	var cloned ai.Message
	if err := json.Unmarshal(data, &cloned); err != nil {
		return ai.Message{Role: message.Role}
	}

	return cloned
}
