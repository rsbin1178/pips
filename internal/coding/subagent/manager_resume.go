package subagent

import (
	"context"
	"errors"
	"fmt"

	"github.com/rsbin1178/pips/internal/coding/session"
)

// createChildSession establishes the durable child Session for one admitted
// execution. A resumed execution forks its completed source instead, so the
// new child inherits the source transcript, its tool calls and results, and its
// archive references, while the source stays readable for a later resume.
func (m *Manager) createChildSession(
	ctx context.Context,
	parentMeta session.Metadata,
	request Request,
	identity session.SubagentIdentity,
	resume *ResumeSource,
	source *session.Handle,
) (*session.Handle, error) {
	if resume == nil {
		return m.config.Repository.Create(ctx, session.CreateOptions{
			WorkspaceID:      parentMeta.WorkspaceID,
			WorkspacePath:    parentMeta.WorkspacePath,
			Kind:             session.KindSubagent,
			ParentSessionID:  parentMeta.ID,
			ParentRunID:      request.Ownership.ParentRunID,
			Agent:            request.AgentID,
			SubagentIdentity: &identity,
		})
	}

	return m.config.Repository.ForkSubagent(ctx, source, session.ForkSubagentOptions{
		ParentSessionID:  parentMeta.ID,
		ParentRunID:      request.Ownership.ParentRunID,
		Agent:            request.AgentID,
		SubagentIdentity: &identity,
	})
}

// resolveResumeSource resolves and validates the completed sibling child a
// resumed request inherits from. It returns the frozen source description and
// the open source Session that the caller must close after forking it. A nil
// source means the request does not resume anything.
//
// Every rejection is final: an active source, an unknown or foreign source, an
// interrupted source, and a source whose stored lineage does not match its
// Session header all fail the spawn rather than silently starting a fresh
// child.
func (m *Manager) resolveResumeSource(
	ctx context.Context,
	request Request,
) (*ResumeSource, *session.Handle, error) {
	if request.ResumeFrom == "" {
		return nil, nil, nil
	}

	if m.activeExecution(request.ResumeFrom) != nil {
		return nil, nil, fmt.Errorf(
			"%w: cannot resume %q: it is still running", ErrInvalid, request.ResumeFrom,
		)
	}

	parentMeta := m.config.Parent.Metadata()

	source, err := m.config.Repository.Open(ctx, session.OpenOptions{
		ID:            request.ResumeFrom,
		WorkspaceID:   parentMeta.WorkspaceID,
		WorkspacePath: parentMeta.WorkspacePath,
	})
	if err != nil {
		return nil, nil, fmt.Errorf(
			"%w: cannot resume %q: no completed child of this session has that id",
			ErrInvalid, request.ResumeFrom,
		)
	}

	meta := source.Metadata()

	values, recordsErr := authoritativeChildRecords(source)
	if recordsErr != nil || meta.Kind != session.KindSubagent || meta.ParentSessionID != parentMeta.ID {
		return nil, nil, errors.Join(
			recordsErr,
			source.Close(),
			fmt.Errorf(
				"%w: cannot resume %q: no completed child of this session has that id",
				ErrInvalid, request.ResumeFrom,
			),
		)
	}

	latest := values[len(values)-1]
	switch {
	case latest.State == StateCreated || latest.State == StateRunning:
		return nil, nil, errors.Join(source.Close(), fmt.Errorf(
			"%w: cannot resume %q: it is still running", ErrInvalid, request.ResumeFrom,
		))
	case !resumableState(latest.State):
		return nil, nil, errors.Join(source.Close(), fmt.Errorf(
			"%w: cannot resume %q: the source child is not resumable", ErrInvalid, request.ResumeFrom,
		))
	}

	// A source with no model-visible transcript has nothing to inherit. The
	// child would silently start empty, so the spawn fails closed instead.
	conversation, contextErr := source.Session().Context()
	if contextErr != nil {
		return nil, nil, errors.Join(source.Close(), fmt.Errorf(
			"%w: cannot resume %q: the source transcript is unreadable",
			ErrInvalid, request.ResumeFrom,
		))
	}

	if len(conversation.Messages) == 0 {
		return nil, nil, errors.Join(source.Close(), fmt.Errorf(
			"%w: cannot resume %q: the source child has no transcript to inherit",
			ErrInvalid, request.ResumeFrom,
		))
	}

	return &ResumeSource{
		ChildSessionID: latest.ChildSessionID,
		AgentID:        latest.Identity.ID,
		Model:          latest.Model,
		Cwd:            latest.Cwd,
		WorktreePath:   latest.WorktreePath,
		Usage:          latest.Usage,
	}, source, nil
}

// applyResumeSource pins one request to its resolved source. The source type
// and model replace the caller's selection, and a caller working directory is
// dropped because the resumed child inherits the source's directory instead.
// The resumed child also inherits the source's environment, so neither the
// caller's nor a preset isolation applies: only the source's preserved
// Worktree keeps the child isolated.
func applyResumeSource(request Request, source *ResumeSource) Request {
	request.AgentID = source.AgentID
	request.Model = source.Model
	request.Cwd = ""
	request.Isolation = ""

	if source.WorktreePath != "" {
		request.Isolation = IsolationWorktree
	}

	return request
}

// validateResumePlan proves the compiled plan still names the source's agent
// type and is bound to the source child. The inherited transcript was produced
// under the source definition, so a plan for any other type must not run on it.
func validateResumePlan(source *ResumeSource, plan ExecutionPlan) error {
	if source == nil {
		return nil
	}

	if plan.Identity.ID != source.AgentID {
		return fmt.Errorf(
			"%w: cannot resume: compiled type %q differs from the source type %q",
			ErrInvalid, plan.Identity.ID, source.AgentID,
		)
	}

	if plan.ResumeFrom != source.ChildSessionID {
		return fmt.Errorf("%w: cannot resume: the plan is not bound to its source", ErrInvalid)
	}

	return nil
}
