package team

import (
	"context"
	"fmt"
	"time"

	"github.com/rsbin/pips/ai"
)

// CompleteTeam completes a Team whose tasks are all completed or cancelled.
func (engine *Engine) CompleteTeam(
	ctx context.Context,
	id ID,
	request CompleteTeamRequest,
) (Team, error) {
	if err := validateReason(request.Reason, false); err != nil {
		return Team{}, err
	}

	if err := validateJSON("Team output", request.Output, hardMaxJSONBytes); err != nil {
		return Team{}, err
	}

	if err := validateArtifacts(request.Artifacts, hardMaxArtifactsPerResult); err != nil {
		return Team{}, err
	}

	semantic := struct {
		Output    ai.JSON    `json:"output,omitempty"`
		Artifacts []Artifact `json:"artifacts,omitempty"`
		Reason    string     `json:"reason,omitempty"`
	}{request.Output, request.Artifacts, request.Reason}

	record, err := engine.apply(
		ctx, id, request.Command, "complete_team", semantic,
		func(team *Team, _ time.Time) (transitionFields, error) {
			if err := requireActive(*team, "complete Team"); err != nil {
				return transitionFields{}, err
			}

			if err := requireLeadOrCoordinator(*team, request.Command.Actor); err != nil {
				return transitionFields{}, err
			}

			if err := validateJSON("Team output", request.Output, team.Limits.MaxJSONBytes); err != nil {
				return transitionFields{}, err
			}

			if err := validateArtifacts(request.Artifacts, team.Limits.MaxArtifactsPerResult); err != nil {
				return transitionFields{}, err
			}

			for _, task := range team.Tasks {
				if task.Status != TaskStatusCompleted && task.Status != TaskStatusCancelled {
					return transitionFields{}, &StateError{
						Operation: "complete Team", Team: team.Status, Task: task.Status,
						Err: ErrInvalidState,
					}
				}
			}

			team.Status = StatusCompleted
			team.Output = cloneJSON(request.Output)
			team.Artifacts = cloneArtifacts(request.Artifacts)
			team.Reason = request.Reason

			return transitionFields{cause: CauseTeamCompleted, reason: request.Reason}, nil
		},
	)
	if err != nil {
		return Team{}, err
	}

	return cloneTeam(record.Team), nil
}

// FailTeam explicitly fails a Team and cancels its active work.
func (engine *Engine) FailTeam(
	ctx context.Context,
	id ID,
	request FailTeamRequest,
) (Team, error) {
	return engine.terminalizeTeam(
		ctx, id, request.Command, "fail_team", StatusFailed,
		CauseTeamFailed, request.Reason,
	)
}

// CancelTeam explicitly cancels a Team and its active work.
func (engine *Engine) CancelTeam(
	ctx context.Context,
	id ID,
	request CancelTeamRequest,
) (Team, error) {
	return engine.terminalizeTeam(
		ctx, id, request.Command, "cancel_team", StatusCancelled,
		CauseTeamCancelled, request.Reason,
	)
}

func (engine *Engine) terminalizeTeam(
	ctx context.Context,
	id ID,
	command CommandMetadata,
	operation string,
	status Status,
	cause Cause,
	reason string,
) (Team, error) {
	if err := validateReason(reason, true); err != nil {
		return Team{}, err
	}

	semantic := struct {
		Status Status `json:"status"`
		Reason string `json:"reason"`
	}{status, reason}

	record, err := engine.apply(
		ctx, id, command, operation, semantic,
		func(team *Team, now time.Time) (transitionFields, error) {
			if err := requireActive(*team, operation); err != nil {
				return transitionFields{}, err
			}

			if err := requireLeadOrCoordinator(*team, command.Actor); err != nil {
				return transitionFields{}, err
			}

			for index := range team.Tasks {
				task := &team.Tasks[index]
				switch task.Status {
				case TaskStatusCompleted, TaskStatusCancelled, TaskStatusFailed:
					continue
				case TaskStatusRunning:
					attempt := &task.Attempts[len(task.Attempts)-1]
					attempt.Status = AttemptStatusCancelled
					attempt.Reason = reason
					attempt.FinishedAt = now
				case TaskStatusPending, TaskStatusReady, TaskStatusClaimed:
				default:
					return transitionFields{}, fmt.Errorf(
						"%w: unknown task status %q",
						ErrInvalid,
						task.Status,
					)
				}

				task.Status = TaskStatusCancelled
				task.ClaimedMemberID = ""
				task.Reason = reason
				task.UpdatedAt = now
			}

			team.Status = status
			team.Reason = reason

			return transitionFields{cause: cause, reason: reason}, nil
		},
	)
	if err != nil {
		return Team{}, err
	}

	return cloneTeam(record.Team), nil
}

func requireTaskCount(team Team) error {
	if len(team.Tasks) >= team.Limits.MaxTasks {
		return fmt.Errorf("%w: task count exceeds %d", ErrTooLarge, team.Limits.MaxTasks)
	}

	return nil
}
