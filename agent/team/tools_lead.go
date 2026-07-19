package team

import (
	"context"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
)

func (binding *toolBinding) leadTools() []agent.Tool {
	return []agent.Tool{
		binding.createTaskTool(),
		binding.assignTaskTool(),
		binding.unassignTaskTool(),
		binding.cancelTaskTool(),
		binding.retryTaskTool(),
		binding.completeTeamTool(),
		binding.failTeamTool(),
		binding.cancelTeamTool(),
	}
}

type createTaskArguments struct {
	Title        string   `json:"title" description:"Concise task title."`
	Description  string   `json:"description,omitempty" description:"Bounded task instructions."`
	Payload      ai.JSON  `json:"payload,omitempty" description:"Optional bounded JSON task input."`
	Dependencies []TaskID `json:"dependencies,omitempty" description:"Existing prerequisite task IDs."`
	AttemptLimit int      `json:"attempt_limit" description:"Maximum task execution attempts."`
}

func (binding *toolBinding) createTaskTool() agent.Tool {
	return newBoundTool(
		"team_create_task",
		"Create one immutable Team task with optional dependency edges.",
		func(ctx context.Context, call agent.ToolCall, arguments createTaskArguments) (any, error) {
			command, err := binding.metadata(ctx, call)
			if err != nil {
				return nil, err
			}

			taskID := TaskID(derivedToolID("task", command.ID))

			team, err := binding.engine.CreateTask(ctx, binding.teamID, CreateTaskRequest{
				Command: command, TaskID: taskID, Title: arguments.Title,
				Description: arguments.Description, Payload: arguments.Payload,
				Dependencies: arguments.Dependencies, AttemptLimit: arguments.AttemptLimit,
			})
			if err != nil {
				return nil, err
			}

			return taskResult(team, taskID)
		},
	)
}

type assignTaskArguments struct {
	TaskID   TaskID   `json:"task_id" description:"Unclaimed Team task ID."`
	MemberID MemberID `json:"member_id" description:"Active assignee member ID."`
}

func (binding *toolBinding) assignTaskTool() agent.Tool {
	return newTaskMutationBoundTool(
		binding,
		"team_assign_task",
		"Exclusively assign or reassign an unclaimed Team task.",
		func(ctx context.Context, command CommandMetadata, arguments assignTaskArguments) (Team, TaskID, error) {
			team, err := binding.engine.AssignTask(ctx, binding.teamID, AssignTaskRequest{
				Command: command, TaskID: arguments.TaskID, MemberID: arguments.MemberID,
			})

			return team, arguments.TaskID, err
		},
	)
}

func (binding *toolBinding) unassignTaskTool() agent.Tool {
	return newTaskMutationBoundTool(
		binding,
		"team_unassign_task",
		"Remove the exclusive assignment from an unclaimed Team task.",
		func(ctx context.Context, command CommandMetadata, arguments taskIDArguments) (Team, TaskID, error) {
			team, err := binding.engine.UnassignTask(ctx, binding.teamID, UnassignTaskRequest{
				Command: command, TaskID: arguments.TaskID,
			})

			return team, arguments.TaskID, err
		},
	)
}

type taskReasonArguments struct {
	TaskID TaskID `json:"task_id" description:"Existing Team task ID."`
	Reason string `json:"reason" description:"Bounded audit reason."`
}

func (binding *toolBinding) cancelTaskTool() agent.Tool {
	return newTaskMutationBoundTool(
		binding,
		"team_cancel_task",
		"Explicitly cancel one non-completed Team task.",
		func(ctx context.Context, command CommandMetadata, arguments taskReasonArguments) (Team, TaskID, error) {
			team, err := binding.engine.CancelTask(ctx, binding.teamID, CancelTaskRequest{
				Command: command, TaskID: arguments.TaskID, Reason: arguments.Reason,
			})

			return team, arguments.TaskID, err
		},
	)
}

func (binding *toolBinding) retryTaskTool() agent.Tool {
	return newTaskMutationBoundTool(
		binding,
		"team_retry_task",
		"Return a failed task below its attempt limit to pending or ready.",
		func(ctx context.Context, command CommandMetadata, arguments taskReasonArguments) (Team, TaskID, error) {
			team, err := binding.engine.RetryTask(ctx, binding.teamID, RetryTaskRequest{
				Command: command, TaskID: arguments.TaskID, Reason: arguments.Reason,
			})

			return team, arguments.TaskID, err
		},
	)
}

type completeTeamArguments struct {
	Output    ai.JSON    `json:"output,omitempty" description:"Optional bounded final JSON output."`
	Artifacts []Artifact `json:"artifacts,omitempty" description:"Opaque host-owned artifact references."`
	Reason    string     `json:"reason,omitempty" description:"Optional completion note."`
}

func (binding *toolBinding) completeTeamTool() agent.Tool {
	return newBoundTool(
		"team_complete",
		"Complete a Team whose tasks are all completed or explicitly cancelled.",
		func(ctx context.Context, call agent.ToolCall, arguments completeTeamArguments) (any, error) {
			command, err := binding.metadata(ctx, call)
			if err != nil {
				return nil, err
			}

			team, err := binding.engine.CompleteTeam(ctx, binding.teamID, CompleteTeamRequest{
				Command: command, Output: arguments.Output,
				Artifacts: arguments.Artifacts, Reason: arguments.Reason,
			})
			if err != nil {
				return nil, err
			}

			return statusView(team), nil
		},
	)
}

type teamReasonArguments struct {
	Reason string `json:"reason" description:"Bounded terminal audit reason."`
}

func (binding *toolBinding) failTeamTool() agent.Tool {
	return newBoundTool(
		"team_fail",
		"Explicitly fail the Team and cancel its active work.",
		func(ctx context.Context, call agent.ToolCall, arguments teamReasonArguments) (any, error) {
			command, err := binding.metadata(ctx, call)
			if err != nil {
				return nil, err
			}

			team, err := binding.engine.FailTeam(ctx, binding.teamID, FailTeamRequest{
				Command: command, Reason: arguments.Reason,
			})
			if err != nil {
				return nil, err
			}

			return statusView(team), nil
		},
	)
}

func (binding *toolBinding) cancelTeamTool() agent.Tool {
	return newBoundTool(
		"team_cancel",
		"Explicitly cancel the Team and cancel its active work.",
		func(ctx context.Context, call agent.ToolCall, arguments teamReasonArguments) (any, error) {
			command, err := binding.metadata(ctx, call)
			if err != nil {
				return nil, err
			}

			team, err := binding.engine.CancelTeam(ctx, binding.teamID, CancelTeamRequest{
				Command: command, Reason: arguments.Reason,
			})
			if err != nil {
				return nil, err
			}

			return statusView(team), nil
		},
	)
}
