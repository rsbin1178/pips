package team

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/rsbin/pips/ai"
)

// CreateTask creates one immutable task definition.
//
//nolint:funlen,gocyclo // Task creation validates immutable graph input before one aggregate commit.
func (engine *Engine) CreateTask(
	ctx context.Context,
	id ID,
	request CreateTaskRequest,
) (Team, error) {
	if err := validateSafeID("task id", string(request.TaskID)); err != nil {
		return Team{}, err
	}

	if err := validateText("task title", request.Title, maxTitleBytes, true); err != nil {
		return Team{}, err
	}

	if err := validateText("task description", request.Description, maxDescriptionBytes, false); err != nil {
		return Team{}, err
	}

	if err := validateJSON("task payload", request.Payload, hardMaxJSONBytes); err != nil {
		return Team{}, err
	}

	if len(request.Dependencies) > hardMaxDependenciesPerTask {
		return Team{}, fmt.Errorf(
			"%w: task dependencies exceed %d",
			ErrTooLarge,
			hardMaxDependenciesPerTask,
		)
	}

	if request.AttemptLimit < 1 || request.AttemptLimit > hardMaxAttemptsPerTask {
		return Team{}, fmt.Errorf("%w: invalid task attempt limit", ErrInvalid)
	}

	for index, dependencyID := range request.Dependencies {
		if err := validateSafeID("task dependency", string(dependencyID)); err != nil {
			return Team{}, err
		}

		for prior := range index {
			if request.Dependencies[prior] == dependencyID {
				return Team{}, fmt.Errorf("%w: duplicate task dependency", ErrInvalid)
			}
		}

		if dependencyID == request.TaskID {
			return Team{}, fmt.Errorf("%w: self task dependency", ErrInvalid)
		}
	}

	semantic := struct {
		TaskID       TaskID   `json:"task_id"`
		Title        string   `json:"title"`
		Description  string   `json:"description,omitempty"`
		Payload      ai.JSON  `json:"payload,omitempty"`
		Dependencies []TaskID `json:"dependencies,omitempty"`
		AttemptLimit int      `json:"attempt_limit"`
	}{
		request.TaskID, request.Title, request.Description, request.Payload,
		request.Dependencies, request.AttemptLimit,
	}

	record, err := engine.apply(
		ctx, id, request.Command, "create_task", semantic,
		func(team *Team, now time.Time) (transitionFields, error) {
			if err := requireActive(*team, "create task"); err != nil {
				return transitionFields{}, err
			}

			if err := requireLeadOrHost(*team, request.Command.Actor); err != nil {
				return transitionFields{}, err
			}

			if err := requireTaskCount(*team); err != nil {
				return transitionFields{}, err
			}

			if len(request.Dependencies) > team.Limits.MaxDependenciesPerTask {
				return transitionFields{}, fmt.Errorf(
					"%w: task dependencies exceed %d",
					ErrTooLarge,
					team.Limits.MaxDependenciesPerTask,
				)
			}

			if request.AttemptLimit > team.Limits.MaxAttemptsPerTask {
				return transitionFields{}, fmt.Errorf(
					"%w: task attempt limit exceeds %d",
					ErrInvalid,
					team.Limits.MaxAttemptsPerTask,
				)
			}

			if err := validateJSON("task payload", request.Payload, team.Limits.MaxJSONBytes); err != nil {
				return transitionFields{}, err
			}

			if _, _, found := findTask(*team, request.TaskID); found {
				return transitionFields{}, ErrExists
			}

			dependenciesComplete := true

			for _, dependencyID := range request.Dependencies {
				dependency, _, found := findTask(*team, dependencyID)
				if !found {
					return transitionFields{}, fmt.Errorf(
						"%w: dependency %q does not exist",
						ErrInvalid,
						dependencyID,
					)
				}

				dependenciesComplete = dependenciesComplete && dependency.Status == TaskStatusCompleted
			}

			status := TaskStatusPending
			if dependenciesComplete {
				status = TaskStatusReady
			}

			team.Tasks = append(team.Tasks, Task{
				ID: request.TaskID, Title: request.Title, Description: request.Description,
				Payload: cloneJSON(request.Payload), DependencyIDs: slices.Clone(request.Dependencies),
				Status: status, AttemptLimit: request.AttemptLimit,
				Attempts: make([]Attempt, 0), CreatedAt: now, UpdatedAt: now,
			})

			return transitionFields{cause: CauseTaskCreated, taskID: request.TaskID}, nil
		},
	)
	if err != nil {
		return Team{}, err
	}

	return cloneTeam(record.Team), nil
}

// AssignTask exclusively assigns an unclaimed task.
func (engine *Engine) AssignTask(
	ctx context.Context,
	id ID,
	request AssignTaskRequest,
) (Team, error) {
	if err := validateSafeID("task id", string(request.TaskID)); err != nil {
		return Team{}, err
	}

	if err := validateSafeID("member id", string(request.MemberID)); err != nil {
		return Team{}, err
	}

	semantic := struct {
		TaskID   TaskID   `json:"task_id"`
		MemberID MemberID `json:"member_id"`
	}{request.TaskID, request.MemberID}

	record, err := engine.apply(
		ctx, id, request.Command, "assign_task", semantic,
		func(team *Team, now time.Time) (transitionFields, error) {
			if err := requireActive(*team, "assign task"); err != nil {
				return transitionFields{}, err
			}

			if err := requireLeadOrHost(*team, request.Command.Actor); err != nil {
				return transitionFields{}, err
			}

			member, _, found := findMember(*team, request.MemberID)
			if !found {
				return transitionFields{}, ErrNotFound
			}

			if member.Status != MemberStatusActive {
				return transitionFields{}, ErrUnauthorized
			}

			task, index, found := findTask(*team, request.TaskID)
			if !found {
				return transitionFields{}, ErrNotFound
			}

			if task.Status != TaskStatusPending && task.Status != TaskStatusReady {
				return transitionFields{}, taskStateError("assign task", *team, task)
			}

			if task.AssignedMemberID == request.MemberID {
				return transitionFields{}, ErrExists
			}

			task.AssignedMemberID = request.MemberID
			task.UpdatedAt = now
			team.Tasks[index] = task

			return transitionFields{
				cause: CauseTaskAssigned, taskID: task.ID, memberID: request.MemberID,
			}, nil
		},
	)
	if err != nil {
		return Team{}, err
	}

	return cloneTeam(record.Team), nil
}

// UnassignTask removes an unclaimed task assignment.
func (engine *Engine) UnassignTask(
	ctx context.Context,
	id ID,
	request UnassignTaskRequest,
) (Team, error) {
	if err := validateSafeID("task id", string(request.TaskID)); err != nil {
		return Team{}, err
	}

	semantic := struct {
		TaskID TaskID `json:"task_id"`
	}{request.TaskID}

	record, err := engine.apply(
		ctx, id, request.Command, "unassign_task", semantic,
		func(team *Team, now time.Time) (transitionFields, error) {
			if err := requireActive(*team, "unassign task"); err != nil {
				return transitionFields{}, err
			}

			if err := requireLeadOrHost(*team, request.Command.Actor); err != nil {
				return transitionFields{}, err
			}

			task, index, found := findTask(*team, request.TaskID)
			if !found {
				return transitionFields{}, ErrNotFound
			}

			if task.Status != TaskStatusPending && task.Status != TaskStatusReady ||
				task.AssignedMemberID == "" {
				return transitionFields{}, taskStateError("unassign task", *team, task)
			}

			memberID := task.AssignedMemberID
			task.AssignedMemberID = ""
			task.UpdatedAt = now
			team.Tasks[index] = task

			return transitionFields{
				cause: CauseTaskUnassigned, taskID: task.ID, memberID: memberID,
			}, nil
		},
	)
	if err != nil {
		return Team{}, err
	}

	return cloneTeam(record.Team), nil
}

// ClaimTask atomically claims one ready task for its member actor.
func (engine *Engine) ClaimTask(
	ctx context.Context,
	id ID,
	request ClaimTaskRequest,
) (Team, error) {
	if err := validateSafeID("task id", string(request.TaskID)); err != nil {
		return Team{}, err
	}

	semantic := struct {
		TaskID TaskID `json:"task_id"`
	}{request.TaskID}

	record, err := engine.apply(
		ctx, id, request.Command, "claim_task", semantic,
		func(team *Team, now time.Time) (transitionFields, error) {
			if err := requireActive(*team, "claim task"); err != nil {
				return transitionFields{}, err
			}

			member, err := activeActorMember(*team, request.Command.Actor)
			if err != nil {
				return transitionFields{}, err
			}

			task, index, found := findTask(*team, request.TaskID)
			if !found {
				return transitionFields{}, ErrNotFound
			}

			if task.Status != TaskStatusReady {
				if task.Status == TaskStatusPending {
					return transitionFields{}, ErrDependencyBlocked
				}

				return transitionFields{}, taskStateError("claim task", *team, task)
			}

			if task.AssignedMemberID != "" && task.AssignedMemberID != member.ID {
				return transitionFields{}, ErrUnauthorized
			}

			if memberHasActiveTask(*team, member.ID) {
				return transitionFields{}, ErrMemberBusy
			}

			if activeTaskCount(*team) >= team.Limits.MaxActiveTasks {
				return transitionFields{}, ErrMemberBusy
			}

			task.Status = TaskStatusClaimed
			task.ClaimedMemberID = member.ID
			task.Reason = ""
			task.UpdatedAt = now
			team.Tasks[index] = task

			return transitionFields{
				cause: CauseTaskClaimed, taskID: task.ID, memberID: member.ID,
			}, nil
		},
	)
	if err != nil {
		return Team{}, err
	}

	return cloneTeam(record.Team), nil
}

// ReleaseTask releases one unstarted claim.
func (engine *Engine) ReleaseTask(
	ctx context.Context,
	id ID,
	request ReleaseTaskRequest,
) (Team, error) {
	if err := validateSafeID("task id", string(request.TaskID)); err != nil {
		return Team{}, err
	}

	if err := validateReason(request.Reason, false); err != nil {
		return Team{}, err
	}

	semantic := struct {
		TaskID TaskID `json:"task_id"`
		Reason string `json:"reason,omitempty"`
	}{request.TaskID, request.Reason}

	record, err := engine.apply(
		ctx, id, request.Command, "release_task", semantic,
		func(team *Team, now time.Time) (transitionFields, error) {
			if err := requireActive(*team, "release task"); err != nil {
				return transitionFields{}, err
			}

			task, index, found := findTask(*team, request.TaskID)
			if !found {
				return transitionFields{}, ErrNotFound
			}

			if task.Status != TaskStatusClaimed {
				return transitionFields{}, taskStateError("release task", *team, task)
			}

			if err := requireTaskOperator(*team, request.Command.Actor, task.ClaimedMemberID); err != nil {
				return transitionFields{}, err
			}

			memberID := task.ClaimedMemberID
			task.Status = TaskStatusReady
			task.ClaimedMemberID = ""
			task.UpdatedAt = now
			team.Tasks[index] = task

			return transitionFields{
				cause: CauseTaskReleased, taskID: task.ID, memberID: memberID,
				reason: request.Reason,
			}, nil
		},
	)
	if err != nil {
		return Team{}, err
	}

	return cloneTeam(record.Team), nil
}

// RetryTask returns one failed task to pending or ready.
func (engine *Engine) RetryTask(
	ctx context.Context,
	id ID,
	request RetryTaskRequest,
) (Team, error) {
	if err := validateSafeID("task id", string(request.TaskID)); err != nil {
		return Team{}, err
	}

	if err := validateReason(request.Reason, false); err != nil {
		return Team{}, err
	}

	semantic := struct {
		TaskID TaskID `json:"task_id"`
		Reason string `json:"reason,omitempty"`
	}{request.TaskID, request.Reason}

	record, err := engine.apply(
		ctx, id, request.Command, "retry_task", semantic,
		func(team *Team, now time.Time) (transitionFields, error) {
			if err := requireActive(*team, "retry task"); err != nil {
				return transitionFields{}, err
			}

			if err := requireLeadOrHost(*team, request.Command.Actor); err != nil {
				return transitionFields{}, err
			}

			task, index, found := findTask(*team, request.TaskID)
			if !found {
				return transitionFields{}, ErrNotFound
			}

			if task.Status != TaskStatusFailed {
				return transitionFields{}, taskStateError("retry task", *team, task)
			}

			if len(task.Attempts) >= task.AttemptLimit {
				return transitionFields{}, ErrAttemptLimit
			}

			task.Status = dependencyStatus(task, *team)
			task.Reason = ""
			task.UpdatedAt = now
			team.Tasks[index] = task

			return transitionFields{
				cause: CauseTaskRetried, taskID: task.ID, reason: request.Reason,
			}, nil
		},
	)
	if err != nil {
		return Team{}, err
	}

	return cloneTeam(record.Team), nil
}

// CancelTask explicitly cancels one non-completed task.
func (engine *Engine) CancelTask(
	ctx context.Context,
	id ID,
	request CancelTaskRequest,
) (Team, error) {
	if err := validateSafeID("task id", string(request.TaskID)); err != nil {
		return Team{}, err
	}

	if err := validateReason(request.Reason, true); err != nil {
		return Team{}, err
	}

	semantic := struct {
		TaskID TaskID `json:"task_id"`
		Reason string `json:"reason"`
	}{request.TaskID, request.Reason}

	record, err := engine.apply(
		ctx, id, request.Command, "cancel_task", semantic,
		func(team *Team, now time.Time) (transitionFields, error) {
			if err := requireActive(*team, "cancel task"); err != nil {
				return transitionFields{}, err
			}

			if err := requireLeadOrHost(*team, request.Command.Actor); err != nil {
				return transitionFields{}, err
			}

			task, index, found := findTask(*team, request.TaskID)
			if !found {
				return transitionFields{}, ErrNotFound
			}

			if task.Status == TaskStatusCompleted || task.Status == TaskStatusCancelled {
				return transitionFields{}, taskStateError("cancel task", *team, task)
			}

			attemptID := AttemptID("")
			memberID := task.ClaimedMemberID

			if task.Status == TaskStatusRunning {
				attempt := &task.Attempts[len(task.Attempts)-1]
				attempt.Status = AttemptStatusCancelled
				attempt.Reason = request.Reason
				attempt.FinishedAt = now
				attemptID = attempt.ID
			}

			task.Status = TaskStatusCancelled
			task.ClaimedMemberID = ""
			task.Reason = request.Reason
			task.UpdatedAt = now
			team.Tasks[index] = task

			return transitionFields{
				cause: CauseTaskCancelled, taskID: task.ID, memberID: memberID,
				attemptID: attemptID, reason: request.Reason,
			}, nil
		},
	)
	if err != nil {
		return Team{}, err
	}

	return cloneTeam(record.Team), nil
}

func requireTaskOperator(team Team, actor Actor, claimant MemberID) error {
	if actor.Kind == ActorKindHostRuntime {
		return nil
	}

	member, err := activeActorMember(team, actor)
	if err != nil {
		return err
	}

	if member.ID != claimant && member.ID != team.LeadMemberID {
		return ErrUnauthorized
	}

	return nil
}

func memberHasActiveTask(team Team, memberID MemberID) bool {
	for _, task := range team.Tasks {
		if task.ClaimedMemberID == memberID &&
			(task.Status == TaskStatusClaimed || task.Status == TaskStatusRunning) {
			return true
		}
	}

	return false
}

func activeTaskCount(team Team) int {
	count := 0

	for _, task := range team.Tasks {
		if task.Status == TaskStatusClaimed || task.Status == TaskStatusRunning {
			count++
		}
	}

	return count
}

func dependencyStatus(task Task, team Team) TaskStatus {
	for _, dependencyID := range task.DependencyIDs {
		dependency, _, _ := findTask(team, dependencyID)
		if dependency.Status != TaskStatusCompleted {
			return TaskStatusPending
		}
	}

	return TaskStatusReady
}

func taskStateError(operation string, team Team, task Task) error {
	return &StateError{
		Operation: operation, Team: team.Status, Task: task.Status, Err: ErrInvalidState,
	}
}
