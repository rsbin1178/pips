//nolint:wsl_v5 // Application adapter code keeps each lifecycle result contiguous.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/continuation"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/ai"
)

const maxModelResultBytes = 24 << 10

// HandlerRef 会被写入 Continuation，用于进程重启后重新绑定兼容的实现。
// 它是持久化标识，不是 Go 函数指针。
var memberWorkerRef = continuation.HandlerRef{
	Kind:    "team_member_harness",
	Version: "v1",
}

// teamCoordinator 是本示例的应用层协调器，不是使用 Team SDK 必须实现的接口。
// 它显式展示了任务调度、Continuation 创建和结果回写等协调职责。
type teamCoordinator struct {
	model            ai.LanguageModel
	teams            *team.Engine
	attempts         *team.AttemptRuntime
	output           io.Writer
	stream           io.Writer
	coordinatorActor team.Actor
	sessions         map[team.MemberID]*harness.Session
}

type memberWorkInput struct {
	TeamObjective string         `json:"team_objective"`
	Member        memberIdentity `json:"member"`
	Task          taskInput      `json:"task"`
	Mailbox       []team.Message `json:"mailbox,omitempty"`
}

type memberIdentity struct {
	ID   team.MemberID `json:"id"`
	Name string        `json:"name"`
	Role string        `json:"role"`
}

type taskInput struct {
	ID           team.TaskID     `json:"id"`
	AttemptID    team.AttemptID  `json:"attempt_id"`
	Continuation continuation.ID `json:"continuation_id"`
	Title        string          `json:"title"`
	Description  string          `json:"description"`
	Payload      ai.JSON         `json:"payload,omitempty"`
	Dependencies []team.TaskID   `json:"dependencies,omitempty"`
}

type taskResultPayload struct {
	TaskID team.TaskID `json:"task_id"`
	Text   string      `json:"text"`
}

type teamOutput struct {
	Briefing string `json:"briefing"`
	Research string `json:"research"`
	Draft    string `json:"draft"`
}

type teamTurnResult struct {
	Answer   string
	Research string
	Draft    string
}

type turnTaskIDs struct {
	Research team.TaskID
	Draft    team.TaskID
}

func (coordinator *teamCoordinator) Run(ctx context.Context) error {
	// Run 保留单轮综合演示，终端入口则通过 Start/RunTurn/Complete 运行多轮。
	group, err := coordinator.Start(ctx)
	if err != nil {
		return err
	}

	result, group, err := coordinator.RunTurn(
		ctx,
		group.ID,
		1,
		"Explain the release value of the supplied pips Agent runtime capabilities.",
	)
	if err != nil {
		return err
	}

	output, err := jsonValue(teamOutput{
		Briefing: result.Answer,
		Research: result.Research,
		Draft:    result.Draft,
	})
	if err != nil {
		return err
	}

	group, err = coordinator.Complete(ctx, group.ID, output, "lead accepted the release briefing")
	if err != nil {
		return err
	}

	return coordinator.printSummary(ctx, group, result.Answer)
}

// Start 创建一个可承载多轮终端会话的 Team，成员 Session 会在后续轮次复用。
func (coordinator *teamCoordinator) Start(ctx context.Context) (team.Team, error) {
	group, err := coordinator.createTeam(ctx)
	if err != nil {
		return team.Team{}, err
	}

	if coordinator.sessions == nil {
		coordinator.sessions = make(map[team.MemberID]*harness.Session)
	}

	if err := coordinator.printf(
		"Team %s 已创建，成员数：%d。\n",
		group.ID,
		len(group.Members),
	); err != nil {
		return team.Team{}, err
	}

	return group, nil
}

// RunTurn 把一条终端输入转换为 research -> draft -> lead 三段执行。
func (coordinator *teamCoordinator) RunTurn(
	ctx context.Context,
	teamID team.ID,
	turn int,
	request string,
) (teamTurnResult, team.Team, error) {
	group, err := coordinator.teams.Get(ctx, teamID)
	if err != nil {
		return teamTurnResult{}, team.Team{}, err
	}

	group, tasks, err := coordinator.defineTurnTasks(ctx, group, turn, request)
	if err != nil {
		return teamTurnResult{}, team.Team{}, err
	}

	research, group, err := coordinator.executeMemberTask(ctx, group.ID, tasks.Research)
	if err != nil {
		return teamTurnResult{}, team.Team{}, fmt.Errorf("execute research task: %w", err)
	}

	draft, group, err := coordinator.executeMemberTask(ctx, group.ID, tasks.Draft)
	if err != nil {
		return teamTurnResult{}, team.Team{}, fmt.Errorf("execute writing task: %w", err)
	}

	answer, group, err := coordinator.synthesizeAsLead(ctx, group.ID, request, tasks)
	if err != nil {
		return teamTurnResult{}, team.Team{}, fmt.Errorf("synthesize Team output: %w", err)
	}

	return teamTurnResult{Answer: answer, Research: research, Draft: draft}, group, nil
}

// Complete 在终端 EOF 或 /exit 时显式关闭 Team。
func (coordinator *teamCoordinator) Complete(
	ctx context.Context,
	teamID team.ID,
	output ai.JSON,
	reason string,
) (team.Team, error) {
	group, err := coordinator.teams.Get(ctx, teamID)
	if err != nil {
		return team.Team{}, err
	}

	group, err = coordinator.teams.CompleteTeam(ctx, group.ID, team.CompleteTeamRequest{
		Command: memberCommand("complete-team", group.Revision, "lead"),
		Output:  output,
		Reason:  reason,
	})
	if err != nil {
		return team.Team{}, fmt.Errorf("complete Team: %w", err)
	}

	return group, nil
}

func (coordinator *teamCoordinator) createTeam(ctx context.Context) (team.Team, error) {
	// 创建 Team 和注册成员属于 Coordinator 权限；Lead 和普通成员不能自行注册资源。
	group, err := coordinator.teams.Create(ctx, team.CreateRequest{
		Command: team.CommandMetadata{ID: "create-team", Actor: coordinator.coordinatorActor},
		ID:      "terminal-conversation-team",
		Objective: "Answer terminal requests through research, drafting, and Lead review " +
			"without inventing unsupported claims.",
		Lead: team.MemberSpec{
			ID:         "lead",
			Name:       "Conversation Lead",
			Role:       "Coordinate work and approve each final answer.",
			SessionRef: "session-terminal-lead",
		},
	})
	if err != nil {
		return team.Team{}, fmt.Errorf("create Team: %w", err)
	}

	members := []team.MemberSpec{
		{
			ID:         "researcher",
			Name:       "Conversation Researcher",
			Role:       "Analyze requests, collect relevant facts, and identify uncertainty.",
			SessionRef: "session-terminal-researcher",
		},
		{
			ID:         "writer",
			Name:       "Conversation Writer",
			Role:       "Turn research evidence into a direct answer for the user.",
			SessionRef: "session-terminal-writer",
		},
	}

	for _, member := range members {
		group, err = coordinator.teams.RegisterMember(ctx, group.ID, team.RegisterMemberRequest{
			Command: coordinator.command("register-"+string(member.ID), group.Revision),
			Member:  member,
		})
		if err != nil {
			return team.Team{}, fmt.Errorf("register member %s: %w", member.ID, err)
		}
	}

	return group, nil
}

func (coordinator *teamCoordinator) defineTurnTasks(
	ctx context.Context,
	group team.Team,
	turn int,
	request string,
) (team.Team, turnTaskIDs, error) {
	// Lead 为每轮输入创建一组唯一任务。draft 依赖 research，因此初始状态为 pending。
	tasks := turnTaskIDs{
		Research: team.TaskID(fmt.Sprintf("turn-%d-research", turn)),
		Draft:    team.TaskID(fmt.Sprintf("turn-%d-draft", turn)),
	}

	researchPayload, err := jsonValue(struct {
		UserRequest string `json:"user_request"`
		Instruction string `json:"instruction"`
	}{
		UserRequest: request,
		Instruction: "Analyze the request, collect relevant facts from available knowledge, " +
			"and identify uncertainty or missing information for the writer.",
	})
	if err != nil {
		return team.Team{}, turnTaskIDs{}, err
	}

	group, err = coordinator.teams.CreateTask(ctx, group.ID, team.CreateTaskRequest{
		Command:      memberCommand("create-"+string(tasks.Research), group.Revision, "lead"),
		TaskID:       tasks.Research,
		Title:        fmt.Sprintf("Analyze terminal request %d", turn),
		Description:  "Prepare accurate evidence and caveats for the current user request.",
		Payload:      researchPayload,
		AttemptLimit: 1,
	})
	if err != nil {
		return team.Team{}, turnTaskIDs{}, fmt.Errorf("create research task: %w", err)
	}

	group, err = coordinator.teams.AssignTask(ctx, group.ID, team.AssignTaskRequest{
		Command:  memberCommand("assign-"+string(tasks.Research), group.Revision, "lead"),
		TaskID:   tasks.Research,
		MemberID: "researcher",
	})
	if err != nil {
		return team.Team{}, turnTaskIDs{}, fmt.Errorf("assign research task: %w", err)
	}

	draftPayload, err := jsonValue(struct {
		UserRequest string `json:"user_request"`
		Instruction string `json:"instruction"`
	}{
		UserRequest: request,
		Instruction: "Use the current research mailbox message to draft a direct answer in " +
			"the same language as the user request.",
	})
	if err != nil {
		return team.Team{}, turnTaskIDs{}, err
	}

	group, err = coordinator.teams.CreateTask(ctx, group.ID, team.CreateTaskRequest{
		Command:      memberCommand("create-"+string(tasks.Draft), group.Revision, "lead"),
		TaskID:       tasks.Draft,
		Title:        fmt.Sprintf("Draft terminal answer %d", turn),
		Description:  "Turn the current research result into a useful response for the user.",
		Payload:      draftPayload,
		Dependencies: []team.TaskID{tasks.Research},
		AttemptLimit: 1,
	})
	if err != nil {
		return team.Team{}, turnTaskIDs{}, fmt.Errorf("create writing task: %w", err)
	}

	group, err = coordinator.teams.AssignTask(ctx, group.ID, team.AssignTaskRequest{
		Command:  memberCommand("assign-"+string(tasks.Draft), group.Revision, "lead"),
		TaskID:   tasks.Draft,
		MemberID: "writer",
	})
	if err != nil {
		return team.Team{}, turnTaskIDs{}, fmt.Errorf("assign writing task: %w", err)
	}

	return group, tasks, nil
}

func (coordinator *teamCoordinator) executeMemberTask(
	ctx context.Context,
	teamID team.ID,
	taskID team.TaskID,
) (string, team.Team, error) {
	result, err := coordinator.attempts.Run(ctx, team.AttemptRunRequest{
		TeamID: teamID, TaskID: taskID,
		AttemptID:      team.AttemptID(string(taskID) + "-attempt-1"),
		ContinuationID: continuation.ID(string(taskID) + "-execution-1"),
	})
	if err != nil {
		return "", team.Team{}, err
	}
	if !result.Finished || result.Execution.Status != continuation.StatusCompleted {
		return "", team.Team{}, fmt.Errorf("member execution yielded %s in status %s", result.Yield, result.Execution.Status)
	}
	var payload taskResultPayload
	if err := json.Unmarshal(result.Execution.Output, &payload); err != nil {
		return "", team.Team{}, fmt.Errorf("decode member result: %w", err)
	}
	if err := coordinator.printf("%s: child=%s after %d durable advances.\n", taskID, team.AttemptInspectionTerminal, result.Advances); err != nil {
		return "", team.Team{}, err
	}
	return payload.Text, result.Team, nil
}

func (coordinator *teamCoordinator) newMemberWorker(dispatch team.Dispatch) (continuation.Worker, error) {
	// Toolset 绑定 TeamID + MemberID，模型无法伪造其他成员身份。
	toolset, err := team.NewMemberToolset(coordinator.teams, dispatch.TeamID, dispatch.MemberID)
	if err != nil {
		return nil, err
	}

	// 综合示例由 Coordinator 控制生命周期，所以只向模型暴露三个只读工具。
	// 若希望成员自行 claim、发送消息或完成 attempt，可直接传入完整 toolset.Tools()。
	tools, err := selectTeamTools(toolset.Tools(), "team_get_status", "team_list_tasks", "team_list_messages")
	if err != nil {
		return nil, err
	}

	// 每个成员使用自己的 SessionRef，成员之间不共享模型上下文，只通过 Team mailbox 协作。
	// 同一成员的 Session 会跨终端轮次复用，因此后续问题能延续该成员自己的上下文。
	session, err := coordinator.memberSession(dispatch.MemberID, dispatch.SessionRef)
	if err != nil {
		return nil, err
	}

	memberHarness, err := harness.New(
		coordinator.model,
		session,
		harness.WithTools(tools...),
		harness.WithSystem(memberSystemPrompt),
		harness.WithAgentOptions(
			agent.WithName(string(dispatch.MemberID)),
			agent.WithMaxTurns(8),
		),
	)
	if err != nil {
		return nil, err
	}

	return &streamingMemberWorker{
		coordinator: coordinator,
		harness:     memberHarness,
		taskID:      dispatch.TaskID,
		label:       string(dispatch.MemberID),
	}, nil
}

// prepareMemberAttempt is the example's infrastructure adapter: it builds the
// member-specific Harness Worker and retains the application prompt schema.
func (coordinator *teamCoordinator) prepareMemberAttempt(
	_ context.Context,
	input team.AttemptInput,
) (team.PreparedAttempt, error) {
	worker, err := coordinator.newMemberWorker(input.Dispatch)
	if err != nil {
		return team.PreparedAttempt{}, err
	}
	payload, err := memberInputJSON(input.Dispatch, input.Mailbox)
	if err != nil {
		return team.PreparedAttempt{}, err
	}
	return team.PreparedAttempt{Worker: worker, Input: payload}, nil
}

// projectMemberAttempt keeps business routing and result JSON in the example;
// the Team runtime owns acknowledgement, message, and finish ordering.
func (coordinator *teamCoordinator) projectMemberAttempt(
	_ context.Context,
	input team.AttemptInput,
	execution continuation.Execution,
) (team.AttemptCompletion, error) {
	if execution.Status != continuation.StatusCompleted {
		return team.AttemptCompletion{
			Outcome: team.AttemptOutcomeFailed,
			Reason:  "member continuation ended in " + string(execution.Status),
		}, nil
	}
	var payload taskResultPayload
	if err := json.Unmarshal(execution.Output, &payload); err != nil {
		return team.AttemptCompletion{}, fmt.Errorf("decode member result: %w", err)
	}
	recipient, err := memberResultRecipient(input.Dispatch.TaskID)
	if err != nil {
		return team.AttemptCompletion{}, err
	}
	body, err := jsonValue(payload)
	if err != nil {
		return team.AttemptCompletion{}, err
	}
	return team.AttemptCompletion{
		Outcome: team.AttemptOutcomeCompleted,
		Result:  body, Reason: "Coordinator reconciled the completed child execution",
		AcknowledgeMailbox: true,
		Messages: []team.AttemptMessage{{
			ID: team.MessageID("message-" + string(input.Dispatch.TaskID)), RecipientID: recipient, Body: body,
		}},
	}, nil
}

func memberResultRecipient(taskID team.TaskID) (team.MemberID, error) {
	switch {
	case strings.HasSuffix(string(taskID), "-research"):
		return "writer", nil
	case strings.HasSuffix(string(taskID), "-draft"):
		return "lead", nil
	default:
		return "", fmt.Errorf("no result recipient for task %q", taskID)
	}
}

func (coordinator *teamCoordinator) synthesizeAsLead(
	ctx context.Context,
	teamID team.ID,
	request string,
	tasks turnTaskIDs,
) (string, team.Team, error) {
	// Lead 也是一个拥有独立 Session 的 Agent。这里读取 writer 的 mailbox 消息并生成最终稿。
	group, err := coordinator.teams.Get(ctx, teamID)
	if err != nil {
		return "", team.Team{}, err
	}

	mailbox, err := coordinator.teams.Mailbox(ctx, teamID, "lead", team.MailboxOptions{
		AfterSequence: memberMailboxCursor(group, "lead"),
	})
	if err != nil {
		return "", team.Team{}, err
	}

	toolset, err := team.NewLeadToolset(coordinator.teams, teamID, "lead")
	if err != nil {
		return "", team.Team{}, err
	}

	tools, err := selectTeamTools(toolset.Tools(), "team_get_status", "team_list_tasks", "team_list_messages")
	if err != nil {
		return "", team.Team{}, err
	}

	session, err := coordinator.memberSession("lead", "session-terminal-lead")
	if err != nil {
		return "", team.Team{}, err
	}

	leadHarness, err := harness.New(
		coordinator.model,
		session,
		harness.WithTools(tools...),
		harness.WithSystem(leadSystemPrompt),
		harness.WithAgentOptions(agent.WithName("lead"), agent.WithMaxTurns(8)),
	)
	if err != nil {
		return "", team.Team{}, err
	}

	input, err := jsonValue(struct {
		Objective   string         `json:"objective"`
		UserRequest string         `json:"user_request"`
		Tasks       []team.Task    `json:"tasks"`
		Mailbox     []team.Message `json:"mailbox"`
	}{group.Objective, request, selectTasks(group, tasks), mailbox.Messages})
	if err != nil {
		return "", team.Team{}, err
	}

	streamed, err := coordinator.streamHarness(ctx, "lead", leadHarness, ai.UserText(leadAnswerPrompt(input)))
	if err != nil {
		return "", team.Team{}, err
	}

	briefing, err := boundedStreamText(streamed.Text, streamed.Stop)
	if err != nil {
		return "", team.Team{}, err
	}

	if mailbox.NextAfter > 0 {
		group, err = coordinator.teams.AcknowledgeMessages(ctx, group.ID, team.AcknowledgeMessagesRequest{
			Command: memberCommand(
				"ack-lead-"+string(tasks.Draft),
				group.Revision,
				"lead",
			),
			ThroughSequence: mailbox.NextAfter,
		})
		if err != nil {
			return "", team.Team{}, err
		}
	}

	return briefing, group, nil
}

func (coordinator *teamCoordinator) printSummary(ctx context.Context, group team.Team, briefing string) error {
	history, err := coordinator.teams.History(ctx, group.ID)
	if err != nil {
		return fmt.Errorf("load Team history: %w", err)
	}

	if err := coordinator.printf("\nTeam status: %s (revision %d)\n", group.Status, group.Revision); err != nil {
		return err
	}

	for _, task := range group.Tasks {
		if err := coordinator.printf(
			"- %s: %s (%d attempt)\n",
			task.ID,
			task.Status,
			len(task.Attempts),
		); err != nil {
			return err
		}
	}

	if err := coordinator.printf(
		"Messages: %d; durable transitions: %d\n",
		len(group.Messages),
		len(history),
	); err != nil {
		return err
	}

	if err := coordinator.printf("\nFinal briefing:\n%s\n", briefing); err != nil {
		return err
	}

	return nil
}

func (coordinator *teamCoordinator) printf(format string, arguments ...any) error {
	if _, err := fmt.Fprintf(coordinator.output, format, arguments...); err != nil {
		return fmt.Errorf("write example output: %w", err)
	}

	return nil
}

func (coordinator *teamCoordinator) command(id string, revision team.Revision) team.CommandMetadata {
	return team.CommandMetadata{ID: team.CommandID(id), ExpectedRevision: revision, Actor: coordinator.coordinatorActor}
}

func memberCommand(id string, revision team.Revision, memberID team.MemberID) team.CommandMetadata {
	return team.CommandMetadata{
		ID:               team.CommandID(id),
		ExpectedRevision: revision,
		Actor:            team.Actor{Kind: team.ActorKindMember, ID: string(memberID)},
	}
}

func (coordinator *teamCoordinator) memberSession(
	memberID team.MemberID,
	sessionRef string,
) (*harness.Session, error) {
	if session, ok := coordinator.sessions[memberID]; ok {
		return session, nil
	}

	session, err := harness.NewSession(harness.NewMemoryStore(sessionRef))
	if err != nil {
		return nil, err
	}

	coordinator.sessions[memberID] = session

	return session, nil
}

func memberMailboxCursor(group team.Team, memberID team.MemberID) uint64 {
	for _, member := range group.Members {
		if member.ID == memberID {
			return member.MailboxAcknowledged
		}
	}

	return 0
}

func selectTasks(group team.Team, ids turnTaskIDs) []team.Task {
	selected := make([]team.Task, 0, 2)

	for _, task := range group.Tasks {
		if task.ID == ids.Research || task.ID == ids.Draft {
			selected = append(selected, task)
		}
	}

	return selected
}

func memberInputJSON(dispatch team.Dispatch, mailbox team.MessagePage) (ai.JSON, error) {
	// Dispatch 是 StartTaskAttempt 产生的不可变执行输入；mailbox 是成员本次可见的协作消息。
	return jsonValue(memberWorkInput{
		TeamObjective: dispatch.Objective,
		Member: memberIdentity{
			ID:   dispatch.MemberID,
			Name: dispatch.MemberName,
			Role: dispatch.MemberRole,
		},
		Task: taskInput{
			ID:           dispatch.TaskID,
			AttemptID:    dispatch.AttemptID,
			Continuation: dispatch.ContinuationID,
			Title:        dispatch.Title,
			Description:  dispatch.Description,
			Payload:      dispatch.Payload,
			Dependencies: dispatch.DependencyIDs,
		},
		Mailbox: mailbox.Messages,
	})
}

func selectTeamTools(tools []agent.Tool, names ...string) ([]agent.Tool, error) {
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		wanted[name] = struct{}{}
	}

	selected := make([]agent.Tool, 0, len(names))

	for _, tool := range tools {
		name := tool.Decl().Name
		if _, ok := wanted[name]; ok {
			selected = append(selected, tool)

			delete(wanted, name)
		}
	}

	if len(wanted) > 0 {
		return nil, fmt.Errorf("team toolset is missing requested tools: %v", wanted)
	}

	return selected, nil
}

func jsonValue(value any) (ai.JSON, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode JSON: %w", err)
	}

	return ai.JSON(data), nil
}
