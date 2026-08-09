# Team：持久的多成员协作

`agent/team` 为一组独立 Agent 会话提供有限、扁平的协作账本。它持久保存成员、依赖任务、排他分配与 claim、attempt、直接邮箱和 Team 生命周期。

Team 不创建或运行 Agent，不拥有模型、Harness、凭据、工作区或工具集，也不是 Workflow engine、scheduler、远程 Worker 控制平面或成员 provisioner。宿主 Go 应用拥有这些资源和执行时机。

## 权限角色

Team 有三种操作视角：

- Coordinator：可信宿主进程的审计身份。创建 Team、注册/启停成员、开始 attempt，并可执行恢复和终态提交。
- Lead：创建时固定的一个成员，拥有普通成员能力和任务/Team 治理命令。
- Member：查看状态、claim/release 自己的任务、完成自己的 attempt、发送和读取消息。

`Actor` 是持久审计身份，不是用户认证或授权系统。应用必须先认证调用者，再构造正确 Actor；模型不能自行选择 actor/team/member 身份。

## 创建 Team 与成员

```go
store, err := team.NewMemoryStore()
if err != nil {
	return err
}
engine, err := team.New(store)
if err != nil {
	return err
}

coordinator := team.Actor{
	Kind: team.ActorKindCoordinator,
	ID:   "api-server",
}
group, err := engine.Create(ctx, team.CreateRequest{
	Command: team.CommandMetadata{
		ID:    "create-release-team",
		Actor: coordinator,
	},
	ID:        "release-team",
	Objective: "准备发布并完成独立复核",
	Lead: team.MemberSpec{
		ID: "lead", Name: "发布负责人", Role: "协调和验收",
	},
})
if err != nil {
	return err
}

group, err = engine.RegisterMember(ctx, group.ID, team.RegisterMemberRequest{
	Command: team.CommandMetadata{
		ID:               "register-reviewer",
		ExpectedRevision: group.Revision,
		Actor:            coordinator,
	},
	Member: team.MemberSpec{
		ID: "reviewer", Name: "复核员", Role: "独立检查",
	},
})
```

每个变更都携带稳定 CommandID、精确 ExpectedRevision 和 Actor。首次创建没有旧 revision。相同 CommandID 与完全相同语义可幂等重放；同 ID 但语义改变会冲突。

`MemberSpec.CapabilityProfileRef` 只引用应用资源配置，Team 不读取它，也不保存 Session ID。禁用只允许空闲的非 Lead 成员；禁用不会销毁宿主资源。

## 任务、依赖与分配

Lead 或相应宿主权限创建不可变任务定义：标题、描述、JSON payload、依赖 ID 和 attempt limit。依赖必须已存在于同一 Team，创建后不能改变。

```go
lead := team.Actor{Kind: team.ActorKindMember, ID: "lead"}
group, err = engine.CreateTask(ctx, group.ID, team.CreateTaskRequest{
	Command: team.CommandMetadata{
		ID:               "create-review",
		ExpectedRevision: group.Revision,
		Actor:            lead,
	},
	TaskID:       "review",
	Title:        "独立复核发布",
	AttemptLimit: 2,
})
```

无依赖或所有依赖完成的任务为 `ready`；尚有依赖为 `pending`。最后一个依赖完成时，解锁在同一 revision 内发生。依赖 failed/cancelled 不会自动解锁后继，必须由 Lead 取消、重试或调整上层计划。

Assignment 是 Lead 指定的唯一候选成员，但不占用成员活动槽。Claim 才原子占用任务和成员槽；一个成员最多有一个 claimed/running 任务。未分配 ready 任务可由合格成员 claim，已分配任务只能由指定成员 claim。

任务状态路径通常为：

```text
pending → ready → claimed → running → completed
                            └────────→ failed → retry → ready/pending
```

取消是显式终态。Release 只释放尚未开始的 claim，并保留 assignment。

## Attempt 与外部执行

Coordinator 用 `StartTaskAttempt` 把 claimed 任务先持久绑定到稳定 AttemptID 和 ContinuationID：

```go
started, err := engine.StartTaskAttempt(ctx, group.ID,
	team.StartTaskAttemptRequest{
		Command: team.CommandMetadata{
			ID:               "start-review-1",
			ExpectedRevision: group.Revision,
			Actor:            coordinator,
		},
		TaskID:         "review",
		AttemptID:      "review-1",
		ContinuationID: "review-execution-1",
	},
)
if err != nil {
	return err
}
dispatch := started.Dispatch
```

顺序必须是“Team attempt commit 在先，子执行创建在后”。这样进程在两者之间崩溃时，恢复检查能看到缺失 child 并安全补建。反过来先创建 child 会留下无法归属的执行。

Dispatch 是给应用 runner 的不可变输入，包含 Team/成员/任务/attempt 身份和 payload，但没有模型或 Harness。

`FinishTaskAttempt` 必须携带完全匹配的 TaskID、AttemptID、ContinuationID，并由 Coordinator 或该 attempt 成员提交。第一个终态结果胜出；旧 attempt 或迟到结果返回 stale，不会覆盖新 attempt。

Result 是有界 JSON，Artifact 只保存引用、digest 和 media type，不应包含凭据或原始 secret。外部 artifact 生命周期由应用管理。

## Mailbox

成员之间发送不可变直接消息。每条消息有 Team 全局连续 Sequence、发送者、接收者、可选 TaskID/ReplyToID 和有界 JSON Body。

`Mailbox` 按接收者和 `AfterSequence` 分页；`AcknowledgeMessages` 只单调推进 cursor，不删除消息。重复读取是正常恢复手段，消费者应按 MessageID/Sequence 幂等处理。

邮箱不是广播总线、工作队列或秘密存储。消息发送与外部副作用之间没有事务；敏感信息应只传引用。

## 给 Agent 使用的绑定 Toolset

`NewMemberToolset` 把 TeamID 和 MemberID 绑定在 Go 侧，模型参数中不暴露身份选择：

```go
toolset, err := team.NewMemberToolset(engine, group.ID, "reviewer")
if err != nil {
	return err
}
memberAgent, err := agent.New(model,
	agent.WithTools(toolset.Tools()...),
)
```

成员工具包括状态、任务列表、claim/release、完成 attempt、发消息、列消息和确认消息。`NewLeadToolset` 追加创建/分配/取消/重试任务与完成/失败/取消 Team 的治理工具。

Toolset 不含注册成员或开始 attempt 工具，这些保留给可信 Coordinator。每个模型发起的变更要求 provider ToolCall ID，并派生稳定 CommandID；缺失调用 ID 会失败。自定义 `WithToolCommandIDSource` 时仍要保证稳定、唯一和语义绑定。

绑定 Toolset 是领域权限边界，但模型仍是不可信调用者。高影响 Team 工具可再经 `agent.WithBeforeTool` 或 Catalog Policy 审批。

## AttemptRuntime

`NewAttemptRuntime` 为常见的一次任务执行提供同步、有限组合：

1. claim 已分配任务并 start attempt；
2. 读取成员 mailbox 快照；
3. 调用 `AttemptWorkerFactory` 准备应用 Worker；
4. 创建缺失的 child Continuation，或读取已有 child；
5. 使用有限 `DriveOptions` 驱动 child；
6. child 终态后由 `AttemptResultProjector` 生成通用 completion；
7. 按确认 mailbox、发送消息、finish attempt 的顺序提交。

构造时必须提供 Team Engine、Continuation Engine、Worker factory、projector、Coordinator、handler refs 和 Controller。默认 drive quantum 为 4；默认 `CompleteAfterWork` 可用于单次 Work 后完成，但仍需显式配置 handler。

```go
runtime, err := team.NewAttemptRuntime(
	engine,
	continuationEngine,
	workerFactory,
	projector,
	team.WithAttemptCoordinator(coordinator),
	team.WithAttemptHandlers(
		workerRef,
		team.DefaultAttemptControllerRef(),
		team.CompleteAfterWork{},
	),
	team.WithAttemptLimits(continuation.Limits{MaxAttempts: 3}),
	team.WithAttemptDriveOptions(continuation.DriveOptions{MaxAdvances: 4}),
)
```

`AttemptRuntime.Run` 不轮询、不启动 goroutine、不重试 Worker，也不拥有模型/Harness。非终态 Yield 是正常调用边界，宿主决定下一次调用时间。Projector 不应直接修改 Team；投影失败会让 task 保持 running，供恢复后重试投影。

Goal/Loop 可以作为 child Continuation 的 Controller，但 Team 不依赖它们。

## 恢复

应用启动后可以：

- `ActiveDispatches` 列出 running attempt；
- `InspectActiveAttempts` 对每个 dispatch 做一次有限 child lookup，区分 missing/nonterminal/terminal；
- `CancellationDispatches` 找出 Team 已取消、需要外部传播取消的 attempt；
- `History` 和 `Changes` 按 revision 检查审计与增量。

这些 API 只检查，不会运行、取消、重试或调度任何 child。恢复器根据检查结果显式调用 AttemptRuntime、Continuation 控制或外部资源。

## Store 与一致性

`MemoryStore` 用于测试，`NewJSONLStore(dir)` 提供严格、有界、单进程的持久记录，并可读取旧 v1 后以 v2 写入。每次 Record 保存完整 Team snapshot、Transition 和可选 message delta。

Team Store、Continuation Store、Harness Store 与外部工具系统之间没有分布式事务或 exactly-once。可靠组合需要：

- 先持久化 Team attempt 绑定，再创建 child；
- 所有命令使用确定 idempotency key；
- Worker 和外部工具使用业务幂等键；
- 恢复时按稳定 TeamID/TaskID/AttemptID/ContinuationID 核对；
- CAS 冲突后重新读取并验证意图，而不是盲目覆盖。

内置 Store 适合单进程协调。多进程/远程 Worker 场景还需要数据库 lease、claim 超时和故障检测；这些不在当前 Team 能力内。

## Team 与 Coding Team

公共 `agent/team` 是领域无关账本。Coding Team 是 `internal/coding` 的产品组合，会另外拥有 Coding Agent、工作区、权限、命令、MCP、Hook 和 UI 生命周期。

公共 Team 不实现 Coding Subagent 的前后台交互，也没有调度队列、成员进程、共享草稿或 workspace 隔离。应用可以用这些公共原语构建自己的产品层，但不应把内部 Coding 类型当公共 API。
