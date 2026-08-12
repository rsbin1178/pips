package teamcontrol

import (
	"fmt"

	"github.com/rsbin1178/pips/agent/continuation"
	"github.com/rsbin1178/pips/agent/team"
	"github.com/rsbin1178/pips/internal/coding/teamstate"
)

// ResolveCommand validates its domain target and resolves live actions to one execution.
//
//nolint:gocyclo // Action-specific fail-closed state checks are intentionally explicit.
func ResolveCommand(group team.Team, resources teamstate.Snapshot, command Command) (ResolvedTarget, error) {
	if err := validateCommand(command, DefaultLimits()); err != nil {
		return ResolvedTarget{}, err
	}

	if command.Target.TeamID != group.ID || command.Target.TeamID != resources.TeamID {
		return ResolvedTarget{}, fmt.Errorf("%w: Team identity mismatch", ErrInvalid)
	}

	switch command.Action {
	case ActionCancelTeam:
		if group.Status != team.StatusActive {
			return ResolvedTarget{}, fmt.Errorf("%w: Team is not active", ErrNotFound)
		}

		return ResolvedTarget{TeamID: group.ID}, nil
	case ActionCancelTask, ActionRetryTask:
		for _, task := range group.Tasks {
			if task.ID != command.Target.TaskID {
				continue
			}

			if command.Action == ActionCancelTask &&
				task.Status != team.TaskStatusPending && task.Status != team.TaskStatusReady &&
				task.Status != team.TaskStatusClaimed && task.Status != team.TaskStatusRunning {
				return ResolvedTarget{}, fmt.Errorf("%w: task is not cancellable", ErrNotFound)
			}

			if command.Action == ActionRetryTask && task.Status != team.TaskStatusFailed {
				return ResolvedTarget{}, fmt.Errorf("%w: task is not retryable", ErrNotFound)
			}

			return ResolvedTarget{TeamID: group.ID, TaskID: task.ID}, nil
		}

		return ResolvedTarget{}, fmt.Errorf("%w: task", ErrNotFound)
	default:
		return ResolveTarget(group, resources, command.Target)
	}
}

// ResolveTarget deterministically binds stable operator identity to one live execution.
//
//nolint:gocyclo // Exact cross-journal identity matching deliberately avoids implicit fallthrough.
func ResolveTarget(group team.Team, resources teamstate.Snapshot, target Target) (ResolvedTarget, error) {
	if target.TeamID == "" || group.ID != target.TeamID || resources.TeamID != target.TeamID {
		return ResolvedTarget{}, fmt.Errorf("%w: Team identity mismatch", ErrInvalid)
	}

	if group.Status != team.StatusActive {
		return ResolvedTarget{}, fmt.Errorf("%w: Team is not active", ErrNotFound)
	}

	if resources.State != teamstate.StateActive && resources.State != teamstate.StateWorkComplete &&
		resources.State != teamstate.StateIntegrationPending {
		return ResolvedTarget{}, fmt.Errorf("%w: Team resources are not live", ErrNotFound)
	}

	resolved := ResolvedTarget{TeamID: target.TeamID, MemberID: target.MemberID, TaskID: target.TaskID}
	if target.MemberID != "" && !activeMember(group, target.MemberID) {
		return ResolvedTarget{}, fmt.Errorf("%w: active member", ErrNotFound)
	}

	if target.MemberID != "" && !boundMember(resources, target.MemberID) {
		return ResolvedTarget{}, fmt.Errorf("%w: member resource binding", ErrNotFound)
	}

	if target.TaskID == "" && target.ExpectedAttemptID == "" && target.MemberID == "" {
		return resolved, nil
	}

	matches := make([]teamstate.AttemptResource, 0, 1)

	for _, resource := range resources.Attempts {
		if resource.State != teamstate.AttemptRunning ||
			target.MemberID != "" && resource.MemberID != target.MemberID ||
			target.TaskID != "" && resource.TaskID != target.TaskID ||
			target.ExpectedAttemptID != "" && resource.AttemptID != target.ExpectedAttemptID ||
			target.OwnerGeneration != 0 &&
				resource.Worktree.LeaseGeneration != target.OwnerGeneration {
			continue
		}

		if !runningAttempt(
			group, resource.TaskID, resource.AttemptID, resource.MemberID, resource.ContinuationID,
		) {
			continue
		}

		matches = append(matches, resource)
	}

	if len(matches) != 1 {
		return ResolvedTarget{}, fmt.Errorf("%w: expected one running Attempt, found %d", ErrAmbiguous, len(matches))
	}

	match := matches[0]
	if match.ContinuationID == "" || match.Session.SessionID == "" ||
		match.Session.WorkspaceID == "" || match.Worktree.LeaseGeneration == 0 {
		return ResolvedTarget{}, fmt.Errorf("%w: incomplete execution identity", ErrInvalid)
	}

	resolved.MemberID = match.MemberID
	resolved.TaskID = match.TaskID
	resolved.AttemptID = match.AttemptID
	resolved.ContinuationID = match.ContinuationID
	resolved.SessionID = match.Session.SessionID
	resolved.WorkspaceID = match.Session.WorkspaceID
	resolved.OwnerGeneration = match.Worktree.LeaseGeneration

	return resolved, nil
}

func boundMember(resources teamstate.Snapshot, id team.MemberID) bool {
	for _, member := range resources.Members {
		if member.MemberID == id {
			return true
		}
	}

	return false
}

func activeMember(group team.Team, id team.MemberID) bool {
	for _, member := range group.Members {
		if member.ID == id {
			return member.Status == team.MemberStatusActive
		}
	}

	return false
}

func runningAttempt(
	group team.Team,
	taskID team.TaskID,
	attemptID team.AttemptID,
	memberID team.MemberID,
	continuationID continuation.ID,
) bool {
	for _, task := range group.Tasks {
		if task.ID != taskID || task.Status != team.TaskStatusRunning {
			continue
		}

		for _, attempt := range task.Attempts {
			if attempt.ID == attemptID && attempt.MemberID == memberID &&
				attempt.ContinuationID == continuationID &&
				attempt.Status == team.AttemptStatusRunning {
				return true
			}
		}
	}

	return false
}

// ClassifyInterrupted returns the only safe automatic handling for an applying entry.
func ClassifyInterrupted(entry Entry) InterruptedDisposition {
	if entry.State != StateApplying {
		return DispositionNone
	}

	if liveDeliveryAction(entry.Command.Action) {
		return DispositionDeliveryUnknown
	}

	return DispositionReconcile
}
