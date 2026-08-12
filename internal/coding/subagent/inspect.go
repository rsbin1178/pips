//nolint:wsl_v5,gocyclo // Durable/live child inspection keeps lineage and recovery checks adjacent.
package subagent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/session"
)

type ownedSummary struct {
	summary Summary
	parent  session.Metadata
}

// List returns newest-first durable descendants owned by this manager's
// execution tree.
func (m *Manager) List(ctx context.Context) ([]Summary, error) {
	if m == nil {
		return nil, ErrClosed
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	owned, err := m.listOwnedDescendants(ctx)
	if err != nil {
		return nil, err
	}
	values := make([]Summary, len(owned))
	for index := range owned {
		values[index] = owned[index].summary
	}

	return values, nil
}

func (m *Manager) listOwnedDescendants(ctx context.Context) ([]ownedSummary, error) {
	values := make([]ownedSummary, 0)
	visited := make(map[string]struct{})
	startDepth := m.delegationDepth
	if m.config.Parent.Metadata().Kind == session.KindConversation {
		startDepth = 0
	}
	if err := m.walkOwnedDescendants(
		ctx,
		m.config.Parent,
		false,
		startDepth,
		nil,
		visited,
		&values,
	); err != nil {
		return nil, err
	}
	sort.SliceStable(values, func(left, right int) bool {
		return values[left].summary.CreatedAt.After(values[right].summary.CreatedAt)
	})

	return values, nil
}

func (m *Manager) walkOwnedDescendants(
	ctx context.Context,
	parent *session.Handle,
	closeParent bool,
	childDepth int,
	parentRecord *record,
	visited map[string]struct{},
	values *[]ownedSummary,
) (returnErr error) {
	if closeParent {
		defer func() { returnErr = errors.Join(returnErr, parent.Close()) }()
	}
	parentMeta := parent.Metadata()
	if _, duplicate := visited[parentMeta.ID]; duplicate {
		return fmt.Errorf("%w: recursive child lineage contains a cycle", ErrInvalid)
	}
	visited[parentMeta.ID] = struct{}{}
	defer delete(visited, parentMeta.ID)

	metas, err := m.config.Repository.ListSubagents(ctx, parentMeta.WorkspaceID, parentMeta.ID)
	if err != nil {
		return err
	}
	latest, err := latestByChild(parent.Session().Entries())
	if err != nil {
		return err
	}
	if parentMeta.Kind == session.KindSubagent {
		delete(latest, parentMeta.ID)
	}
	if len(metas) > 0 && childDepth > MaxDelegationDepth {
		return fmt.Errorf("%w: recursive child lineage exceeds hard depth", ErrInvalid)
	}
	seen := make(map[string]struct{}, len(metas))
	for _, meta := range metas {
		if _, cycle := visited[meta.ID]; cycle {
			return fmt.Errorf("%w: recursive child lineage contains a cycle", ErrInvalid)
		}
		seen[meta.ID] = struct{}{}
		value, ok := latest[meta.ID]
		if !ok {
			return fmt.Errorf("%w: parent index is missing child %q", ErrInvalid, meta.ID)
		}
		if err := validateLineage(parentMeta, meta, value); err != nil {
			return err
		}
		if err := validateRecursiveLineage(parentRecord, value, childDepth); err != nil {
			return err
		}
		*values = append(*values, ownedSummary{summary: summaryFrom(meta, value), parent: parentMeta})

		child, owned, openErr := m.childHandle(ctx, meta.ID)
		if openErr != nil {
			return openErr
		}
		valueCopy := value
		if err := m.walkOwnedDescendants(
			ctx, child, owned, childDepth+1, &valueCopy, visited, values,
		); err != nil {
			return err
		}
	}
	for childSessionID := range latest {
		if _, ok := seen[childSessionID]; !ok {
			return fmt.Errorf("%w: parent index references a missing child", ErrInvalid)
		}
	}

	return nil
}

func validateRecursiveLineage(parent *record, child record, depth int) error {
	plan := child.Plan
	if len(plan.Ancestry) == 0 {
		if parent != nil {
			return fmt.Errorf("%w: recursive child is missing delegation lineage", ErrInvalid)
		}

		return nil
	}
	if plan.DelegationDepth != depth || depth > plan.MaxDelegationDepth ||
		plan.MaxDelegationDepth > MaxDelegationDepth {
		return fmt.Errorf("%w: recursive child depth mismatch", ErrInvalid)
	}
	if parent == nil {
		if depth != 0 || len(plan.Ancestry) != 1 {
			return fmt.Errorf("%w: root child delegation lineage mismatch", ErrInvalid)
		}

		return nil
	}
	parentPlan := parent.Plan
	if len(parentPlan.Ancestry) == 0 || plan.MaxDelegationDepth != parentPlan.MaxDelegationDepth ||
		!slices.Equal(plan.Ancestry[:len(plan.Ancestry)-1], parentPlan.Ancestry) ||
		!slices.Contains(parentPlan.DelegationTargets, plan.Identity.ID) {
		return fmt.Errorf("%w: recursive child authority lineage mismatch", ErrInvalid)
	}

	return nil
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

	selected, err := m.findOwnedSummary(ctx, childSessionID)
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
	selected, err := m.findOwnedSummary(ctx, childSessionID)
	return selected.summary, err
}

func (m *Manager) findOwnedSummary(ctx context.Context, childSessionID string) (ownedSummary, error) {
	values, err := m.listOwnedDescendants(ctx)
	if err != nil {
		return ownedSummary{}, err
	}

	for _, value := range values {
		if value.summary.ChildSessionID == childSessionID {
			return value, nil
		}
	}

	return ownedSummary{}, fmt.Errorf("%w: child does not belong to current parent", ErrInvalid)
}

//nolint:nestif // Durable custom and builtin result validation stays beside exact lineage validation.
func (m *Manager) inspectChild(
	child *session.Handle,
	owned bool,
	selected ownedSummary,
) (_ Detail, returnErr error) {
	if owned {
		defer func() { returnErr = errors.Join(returnErr, child.Close()) }()
	}

	parent := selected.parent
	childMeta := child.Metadata()

	childLatest, err := latestByChild(child.Session().Entries())
	if err != nil {
		return Detail{}, err
	}

	value, ok := childLatest[selected.summary.ChildSessionID]
	if !ok {
		return Detail{}, fmt.Errorf("%w: child lifecycle is missing", ErrInvalid)
	}

	if err := validateLineage(parent, childMeta, value); err != nil {
		return Detail{}, err
	}

	if summaryFrom(childMeta, value) != selected.summary {
		return Detail{}, fmt.Errorf("%w: parent and child lifecycle differ", ErrInvalid)
	}

	contextValue, err := child.Session().Context()
	if err != nil {
		return Detail{}, fmt.Errorf("coding subagent: load transcript: %w", err)
	}

	detail := Detail{Summary: selected.summary, Plan: value.Plan.Clone(), Transcript: contextValue.Messages}
	if selected.summary.State == StateCreated || selected.summary.State == StateRunning {
		detail = m.overlayLiveDetail(detail)
	}

	if selected.summary.State == StateSucceeded {
		text, ok := finalAssistantText(detail.Transcript)
		if !ok {
			return Detail{}, fmt.Errorf("%w: successful child has no final assistant result", ErrInvalid)
		}

		if role := selected.summary.Identity.LegacyRole(); role != "" {
			spec, specErr := specFor(role)
			if specErr != nil {
				return Detail{}, specErr
			}
			detail.Result, err = spec.decode(text, value.Limits.limits())
		} else {
			validator, validatorErr := NewOutputValidator(value.Plan.Output, value.Plan.Limits)
			if validatorErr != nil {
				return Detail{}, validatorErr
			}
			detail.Result, err = validator.Validate(text)
		}
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
	m.shared.mu.Lock()
	active := m.shared.active[childSessionID]
	if active != nil && active.child != nil && active.child.Metadata().ID == childSessionID {
		child := active.child
		m.shared.mu.Unlock()

		return child, false, nil
	}
	m.shared.mu.Unlock()
	parent := m.config.Parent.Metadata()

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
		value.ParentSessionID != parent.ID || value.Identity.ID != child.Agent ||
		value.ParentRunID != child.ParentRunID ||
		!matchesSessionIdentity(child.SubagentIdentity, value.Plan) {
		return fmt.Errorf("%w: child lineage mismatch", ErrInvalid)
	}

	return nil
}

func summaryFrom(meta session.Metadata, value record) Summary {
	return Summary{
		ChildSessionID: meta.ID,
		Ownership:      executionOwnership(value),
		Delivery:       value.Delivery,
		Identity:       value.Identity,
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
		if _, ok := v.(ai.AssistantMessage); !ok {
			continue
		}

		return messageText(v), true
	}

	return "", false
}

func cloneDetail(value Detail) Detail {
	value.Transcript = cloneTranscript(value.Transcript)
	value.Activity = cloneActivity(value.Activity)
	value.Plan = value.Plan.Clone()
	value.Result = cloneResult(Result{
		Identity: value.Summary.Identity, Role: value.Summary.Role, Value: value.Result,
	}).Value

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
	cloned, err := ai.CloneMessage(message)
	if err != nil {
		return message
	}

	return cloned
}
