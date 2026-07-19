package team

import (
	"context"
	"fmt"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/continuation"
	"github.com/rsbin/pips/ai"
)

const (
	defaultToolTaskPage = 100
	maxToolTaskPage     = 200
)

func (binding *toolBinding) memberTools() []agent.Tool {
	return []agent.Tool{
		binding.getStatusTool(),
		binding.listTasksTool(),
		binding.claimTaskTool(),
		binding.releaseTaskTool(),
		binding.finishTaskAttemptTool(),
		binding.sendMessageTool(),
		binding.listMessagesTool(),
		binding.acknowledgeMessagesTool(),
	}
}

func (binding *toolBinding) getStatusTool() agent.Tool {
	return newBoundTool(
		"team_get_status",
		"Get the current durable Team status and aggregate revision.",
		func(ctx context.Context, _ agent.ToolCall, _ struct{}) (any, error) {
			team, err := binding.engine.Get(ctx, binding.teamID)
			if err != nil {
				return nil, err
			}

			return statusView(team), nil
		},
	)
}

type listTasksArguments struct {
	Status TaskStatus `json:"status,omitempty" description:"Optional exact task status filter."`
	Limit  int        `json:"limit,omitempty" description:"Maximum task summaries to return, from 1 to 200."`
}

func (binding *toolBinding) listTasksTool() agent.Tool {
	return newBoundTool(
		"team_list_tasks",
		"List bounded task summaries from the current Team state.",
		func(ctx context.Context, _ agent.ToolCall, arguments listTasksArguments) (any, error) {
			if arguments.Limit == 0 {
				arguments.Limit = defaultToolTaskPage
			}

			if arguments.Limit < 1 || arguments.Limit > maxToolTaskPage {
				return nil, fmt.Errorf("%w: task list limit must be between 1 and %d", ErrInvalid, maxToolTaskPage)
			}

			if arguments.Status != "" && !validTaskStatus(arguments.Status) {
				return nil, fmt.Errorf("%w: invalid task status %q", ErrInvalid, arguments.Status)
			}

			team, err := binding.engine.Get(ctx, binding.teamID)
			if err != nil {
				return nil, err
			}

			views := make([]taskView, 0, min(arguments.Limit, len(team.Tasks)))
			matched := 0

			for _, task := range team.Tasks {
				if arguments.Status != "" && task.Status != arguments.Status {
					continue
				}

				matched++

				if len(views) < arguments.Limit {
					views = append(views, newTaskView(task))
				}
			}

			return struct {
				TeamRevision Revision   `json:"team_revision"`
				Tasks        []taskView `json:"tasks"`
				Truncated    bool       `json:"truncated"`
			}{team.Revision, views, matched > len(views)}, nil
		},
	)
}

type taskIDArguments struct {
	TaskID TaskID `json:"task_id" description:"Existing Team task ID."`
}

func (binding *toolBinding) claimTaskTool() agent.Tool {
	return newTaskMutationBoundTool(
		binding,
		"team_claim_task",
		"Atomically claim one ready and eligible Team task for this member.",
		func(ctx context.Context, command CommandMetadata, arguments taskIDArguments) (Team, TaskID, error) {
			team, err := binding.engine.ClaimTask(ctx, binding.teamID, ClaimTaskRequest{
				Command: command, TaskID: arguments.TaskID,
			})

			return team, arguments.TaskID, err
		},
	)
}

type releaseTaskArguments struct {
	TaskID TaskID `json:"task_id" description:"Currently claimed Team task ID."`
	Reason string `json:"reason,omitempty" description:"Optional audit reason for releasing the claim."`
}

func (binding *toolBinding) releaseTaskTool() agent.Tool {
	return newTaskMutationBoundTool(
		binding,
		"team_release_task",
		"Release this member's unstarted task claim while preserving assignment.",
		func(ctx context.Context, command CommandMetadata, arguments releaseTaskArguments) (Team, TaskID, error) {
			team, err := binding.engine.ReleaseTask(ctx, binding.teamID, ReleaseTaskRequest{
				Command: command, TaskID: arguments.TaskID, Reason: arguments.Reason,
			})

			return team, arguments.TaskID, err
		},
	)
}

type finishTaskAttemptArguments struct {
	TaskID         TaskID          `json:"task_id" description:"Running Team task ID."`
	AttemptID      AttemptID       `json:"attempt_id" description:"Current Team attempt ID."`
	ContinuationID continuation.ID `json:"continuation_id" description:"Bound continuation execution ID."`
	Outcome        AttemptOutcome  `json:"outcome" description:"Terminal outcome: completed or failed."`
	Result         ai.JSON         `json:"result,omitempty" description:"Bounded JSON result."`
	Artifacts      []Artifact      `json:"artifacts,omitempty" description:"Opaque host-owned artifact references."`
	Reason         string          `json:"reason,omitempty" description:"Required failure reason or optional completion note."`
}

func (binding *toolBinding) finishTaskAttemptTool() agent.Tool {
	return newBoundTool(
		"team_finish_task_attempt",
		"Finish this member's current running task attempt with matching durable IDs.",
		func(ctx context.Context, call agent.ToolCall, arguments finishTaskAttemptArguments) (any, error) {
			command, err := binding.metadata(ctx, call)
			if err != nil {
				return nil, err
			}

			team, err := binding.engine.FinishTaskAttempt(ctx, binding.teamID, FinishTaskAttemptRequest{
				Command: command, TaskID: arguments.TaskID, AttemptID: arguments.AttemptID,
				ContinuationID: arguments.ContinuationID, Outcome: arguments.Outcome,
				Result: arguments.Result, Artifacts: arguments.Artifacts, Reason: arguments.Reason,
			})
			if err != nil {
				return nil, err
			}

			return taskResult(team, arguments.TaskID)
		},
	)
}

type sendMessageArguments struct {
	RecipientID MemberID  `json:"recipient_id" description:"Existing direct recipient member ID."`
	TaskID      TaskID    `json:"task_id,omitempty" description:"Optional related Team task ID."`
	ReplyToID   MessageID `json:"reply_to_id,omitempty" description:"Optional prior message in the same conversation."`
	Body        ai.JSON   `json:"body" description:"Non-null bounded JSON message body."`
}

func (binding *toolBinding) sendMessageTool() agent.Tool {
	return newBoundTool(
		"team_send_message",
		"Send one durable direct message as this bound member.",
		func(ctx context.Context, call agent.ToolCall, arguments sendMessageArguments) (any, error) {
			command, err := binding.metadata(ctx, call)
			if err != nil {
				return nil, err
			}

			sent, err := binding.engine.SendMessage(ctx, binding.teamID, SendMessageRequest{
				Command: command, MessageID: MessageID(derivedToolID("message", command.ID)),
				RecipientID: arguments.RecipientID, TaskID: arguments.TaskID,
				ReplyToID: arguments.ReplyToID, Body: arguments.Body,
			})
			if err != nil {
				return nil, err
			}

			return struct {
				TeamRevision Revision `json:"team_revision"`
				Message      Message  `json:"message"`
			}{sent.Team.Revision, sent.Message}, nil
		},
	)
}

type listMessagesArguments struct {
	AfterSequence uint64 `json:"after_sequence,omitempty" description:"Exclusive Team-global sequence cursor."`
	Limit         int    `json:"limit,omitempty" description:"Maximum messages to return, from 1 to 1000."`
}

func (binding *toolBinding) listMessagesTool() agent.Tool {
	return newBoundTool(
		"team_list_messages",
		"List this member's durable direct mailbox after an exclusive sequence.",
		func(ctx context.Context, _ agent.ToolCall, arguments listMessagesArguments) (any, error) {
			return binding.engine.Mailbox(
				ctx,
				binding.teamID,
				binding.memberID,
				MailboxOptions(arguments),
			)
		},
	)
}

type acknowledgeMessagesArguments struct {
	ThroughSequence uint64 `json:"through_sequence" description:"Highest delivered Team sequence processed by this member."`
}

func (binding *toolBinding) acknowledgeMessagesTool() agent.Tool {
	return newBoundTool(
		"team_acknowledge_messages",
		"Advance this member's durable mailbox acknowledgement cursor.",
		func(ctx context.Context, call agent.ToolCall, arguments acknowledgeMessagesArguments) (any, error) {
			command, err := binding.metadata(ctx, call)
			if err != nil {
				return nil, err
			}

			team, err := binding.engine.AcknowledgeMessages(ctx, binding.teamID, AcknowledgeMessagesRequest{
				Command: command, ThroughSequence: arguments.ThroughSequence,
			})
			if err != nil {
				return nil, err
			}

			member, _, _ := findMember(team, binding.memberID)

			return struct {
				TeamRevision Revision `json:"team_revision"`
				Acknowledged uint64   `json:"acknowledged"`
			}{team.Revision, member.MailboxAcknowledged}, nil
		},
	)
}

func validTaskStatus(status TaskStatus) bool {
	switch status {
	case TaskStatusPending, TaskStatusReady, TaskStatusClaimed, TaskStatusRunning,
		TaskStatusCompleted, TaskStatusFailed, TaskStatusCancelled:
		return true
	default:
		return false
	}
}
