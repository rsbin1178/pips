package team

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/rsbin1178/pips/agent/continuation"
	"github.com/rsbin1178/pips/ai"
)

// StartTaskAttempt durably binds one claimed task to a continuation execution.
//
//nolint:gocyclo // Attempt start validates the complete cross-store identity before committing it.
func (engine *Engine) StartTaskAttempt(
	ctx context.Context,
	id ID,
	request StartTaskAttemptRequest,
) (TaskAttemptStart, error) {
	if err := validateSafeID("task id", string(request.TaskID)); err != nil {
		return TaskAttemptStart{}, err
	}

	if err := validateSafeID("attempt id", string(request.AttemptID)); err != nil {
		return TaskAttemptStart{}, err
	}

	if err := validateSafeID("continuation id", string(request.ContinuationID)); err != nil {
		return TaskAttemptStart{}, err
	}

	semantic := struct {
		TaskID         TaskID          `json:"task_id"`
		AttemptID      AttemptID       `json:"attempt_id"`
		ContinuationID continuation.ID `json:"continuation_id"`
	}{request.TaskID, request.AttemptID, request.ContinuationID}

	record, err := engine.apply(
		ctx, id, request.Command, "start_task_attempt", semantic,
		func(team *Team, now time.Time) (transitionFields, error) {
			if err := requireActive(*team, "start task attempt"); err != nil {
				return transitionFields{}, err
			}

			if err := requireCoordinator(request.Command.Actor); err != nil {
				return transitionFields{}, err
			}

			task, index, found := findTask(*team, request.TaskID)
			if !found {
				return transitionFields{}, ErrNotFound
			}

			if task.Status != TaskStatusClaimed {
				return transitionFields{}, taskStateError("start task attempt", *team, task)
			}

			if len(task.Attempts) >= task.AttemptLimit {
				return transitionFields{}, ErrAttemptLimit
			}

			for _, existingTask := range team.Tasks {
				for _, attempt := range existingTask.Attempts {
					if attempt.ID == request.AttemptID || attempt.ContinuationID == request.ContinuationID {
						return transitionFields{}, ErrExists
					}
				}
			}

			task.Status = TaskStatusRunning
			task.Attempts = append(task.Attempts, Attempt{
				ID: request.AttemptID, Number: len(task.Attempts) + 1,
				Status: AttemptStatusRunning, MemberID: task.ClaimedMemberID,
				ContinuationID: request.ContinuationID, StartedAt: now,
			})
			task.UpdatedAt = now
			team.Tasks[index] = task

			return transitionFields{
				cause: CauseTaskAttemptStarted, taskID: task.ID,
				memberID: task.ClaimedMemberID, attemptID: request.AttemptID,
			}, nil
		},
	)
	if err != nil {
		return TaskAttemptStart{}, err
	}

	task, _, found := findTask(record.Team, request.TaskID)
	if !found {
		return TaskAttemptStart{}, fmt.Errorf("%w: committed task missing", ErrCorruptStore)
	}

	attempt, found := findAttempt(task, request.AttemptID)
	if !found {
		return TaskAttemptStart{}, fmt.Errorf("%w: committed attempt missing", ErrCorruptStore)
	}

	return TaskAttemptStart{
		Team: cloneTeam(record.Team), Dispatch: dispatchFor(record.Team, task, attempt),
	}, nil
}

// FinishTaskAttempt commits the current attempt outcome.
//
//nolint:gocyclo // Attempt completion centralizes authorization, stale-result, and outcome transitions.
func (engine *Engine) FinishTaskAttempt(
	ctx context.Context,
	id ID,
	request FinishTaskAttemptRequest,
) (Team, error) {
	if err := validateSafeID("task id", string(request.TaskID)); err != nil {
		return Team{}, err
	}

	if err := validateSafeID("attempt id", string(request.AttemptID)); err != nil {
		return Team{}, err
	}

	if err := validateSafeID("continuation id", string(request.ContinuationID)); err != nil {
		return Team{}, err
	}

	if request.Outcome != AttemptOutcomeCompleted && request.Outcome != AttemptOutcomeFailed {
		return Team{}, fmt.Errorf("%w: invalid attempt outcome %q", ErrInvalid, request.Outcome)
	}

	if err := validateReason(request.Reason, request.Outcome == AttemptOutcomeFailed); err != nil {
		return Team{}, err
	}

	if err := validateJSON("attempt result", request.Result, hardMaxJSONBytes); err != nil {
		return Team{}, err
	}

	if err := validateArtifacts(request.Artifacts, hardMaxArtifactsPerResult); err != nil {
		return Team{}, err
	}

	semantic := struct {
		TaskID         TaskID          `json:"task_id"`
		AttemptID      AttemptID       `json:"attempt_id"`
		ContinuationID continuation.ID `json:"continuation_id"`
		Outcome        AttemptOutcome  `json:"outcome"`
		Result         ai.JSON         `json:"result,omitempty"`
		Artifacts      []Artifact      `json:"artifacts,omitempty"`
		Reason         string          `json:"reason,omitempty"`
	}{
		request.TaskID, request.AttemptID, request.ContinuationID, request.Outcome,
		request.Result, request.Artifacts, request.Reason,
	}

	record, err := engine.apply(
		ctx, id, request.Command, "finish_task_attempt", semantic,
		func(team *Team, now time.Time) (transitionFields, error) {
			if err := requireActive(*team, "finish task attempt"); err != nil {
				return transitionFields{}, err
			}

			if err := validateJSON("attempt result", request.Result, team.Limits.MaxJSONBytes); err != nil {
				return transitionFields{}, err
			}

			if err := validateArtifacts(request.Artifacts, team.Limits.MaxArtifactsPerResult); err != nil {
				return transitionFields{}, err
			}

			task, index, found := findTask(*team, request.TaskID)
			if !found {
				return transitionFields{}, ErrNotFound
			}

			if task.Status != TaskStatusRunning || len(task.Attempts) == 0 {
				return transitionFields{}, ErrStaleAttempt
			}

			attempt := &task.Attempts[len(task.Attempts)-1]
			if attempt.Status != AttemptStatusRunning || attempt.ID != request.AttemptID ||
				attempt.ContinuationID != request.ContinuationID {
				return transitionFields{}, ErrStaleAttempt
			}

			if request.Command.Actor.Kind == ActorKindMember {
				member, actorErr := activeActorMember(*team, request.Command.Actor)
				if actorErr != nil || member.ID != attempt.MemberID || member.ID != task.ClaimedMemberID {
					return transitionFields{}, ErrUnauthorized
				}
			} else if err := requireCoordinator(request.Command.Actor); err != nil {
				return transitionFields{}, err
			}

			attempt.Result = cloneJSON(request.Result)
			attempt.Artifacts = cloneArtifacts(request.Artifacts)
			attempt.Reason = request.Reason
			attempt.FinishedAt = now
			task.ClaimedMemberID = ""
			task.Reason = request.Reason
			task.UpdatedAt = now

			fields := transitionFields{
				taskID: task.ID, memberID: attempt.MemberID,
				attemptID: attempt.ID, reason: request.Reason,
			}
			if request.Outcome == AttemptOutcomeCompleted {
				attempt.Status = AttemptStatusCompleted
				task.Status = TaskStatusCompleted
				fields.cause = CauseTaskAttemptCompleted
			} else {
				attempt.Status = AttemptStatusFailed
				task.Status = TaskStatusFailed
				fields.cause = CauseTaskAttemptFailed
			}

			team.Tasks[index] = task
			if request.Outcome == AttemptOutcomeCompleted {
				promoteReadyTasks(team, now)
			}

			return fields, nil
		},
	)
	if err != nil {
		return Team{}, err
	}

	return cloneTeam(record.Team), nil
}

// ActiveDispatches returns currently running task attempts.
func (engine *Engine) ActiveDispatches(ctx context.Context, id ID) ([]Dispatch, error) {
	team, err := engine.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	dispatches := make([]Dispatch, 0)

	for _, task := range team.Tasks {
		if task.Status != TaskStatusRunning || len(task.Attempts) == 0 {
			continue
		}

		attempt := task.Attempts[len(task.Attempts)-1]
		dispatches = append(dispatches, dispatchFor(team, task, attempt))
	}

	return dispatches, nil
}

// CancellationDispatches returns attempts cancelled while externally running.
func (engine *Engine) CancellationDispatches(ctx context.Context, id ID) ([]Dispatch, error) {
	team, err := engine.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	dispatches := make([]Dispatch, 0)

	for _, task := range team.Tasks {
		if len(task.Attempts) == 0 {
			continue
		}

		attempt := task.Attempts[len(task.Attempts)-1]
		if attempt.Status == AttemptStatusCancelled {
			dispatches = append(dispatches, dispatchFor(team, task, attempt))
		}
	}

	return dispatches, nil
}

// InspectActiveAttempts performs one finite child lookup per active dispatch.
func (engine *Engine) InspectActiveAttempts(
	ctx context.Context,
	id ID,
	reader ExecutionReader,
) ([]AttemptInspection, error) {
	if reader == nil {
		return nil, fmt.Errorf("%w: nil execution reader", ErrInvalid)
	}

	dispatches, err := engine.ActiveDispatches(ctx, id)
	if err != nil {
		return nil, err
	}

	inspections := make([]AttemptInspection, 0, len(dispatches))
	for _, dispatch := range dispatches {
		execution, readErr := reader.Get(ctx, dispatch.ContinuationID)
		if readErr != nil {
			if errors.Is(readErr, continuation.ErrNotFound) {
				inspections = append(inspections, AttemptInspection{
					Dispatch: cloneDispatch(dispatch), State: AttemptInspectionMissing,
				})

				continue
			}

			return nil, readErr
		}

		state := AttemptInspectionNonterminal
		if terminalExecutionStatus(execution.Status) {
			state = AttemptInspectionTerminal
		}

		executionCopy := execution
		inspections = append(inspections, AttemptInspection{
			Dispatch: cloneDispatch(dispatch), State: state, Execution: &executionCopy,
		})
	}

	return inspections, nil
}

func promoteReadyTasks(team *Team, now time.Time) {
	for index := range team.Tasks {
		task := &team.Tasks[index]
		if task.Status == TaskStatusPending && dependencyStatus(*task, *team) == TaskStatusReady {
			task.Status = TaskStatusReady
			task.UpdatedAt = now
		}
	}
}

func findAttempt(task Task, id AttemptID) (Attempt, bool) {
	for _, attempt := range task.Attempts {
		if attempt.ID == id {
			return attempt, true
		}
	}

	return Attempt{}, false
}

func dispatchFor(team Team, task Task, attempt Attempt) Dispatch {
	member, _, _ := findMember(team, attempt.MemberID)

	return Dispatch{
		TeamID: team.ID, TeamRevision: team.Revision, Objective: team.Objective,
		MemberID: member.ID, MemberName: member.Name, MemberRole: member.Role,
		CapabilityProfileRef: member.CapabilityProfileRef,
		TaskID:               task.ID, AttemptID: attempt.ID, ContinuationID: attempt.ContinuationID,
		Title: task.Title, Description: task.Description, Payload: cloneJSON(task.Payload),
		DependencyIDs: slices.Clone(task.DependencyIDs),
	}
}

func terminalExecutionStatus(status continuation.Status) bool {
	switch status {
	case continuation.StatusCompleted, continuation.StatusFailed,
		continuation.StatusCancelled, continuation.StatusLimited:
		return true
	default:
		return false
	}
}
