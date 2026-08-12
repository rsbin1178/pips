# Agent API 导航

本页按任务连接公共包、主要入口与详细指南。字段、方法签名和错误集合以当前源码 Godoc 为准；指南重点解释组合方式和所有权，不重复每个字段。

## 在线与本地 Godoc

- 在线入口：[`github.com/rsbin1178/pips/agent`](https://pkg.go.dev/github.com/rsbin1178/pips/agent)
- 本地查看根包：`go doc ./agent`
- 查看完整导出符号：`go doc -all ./agent`
- 查看单个类型：`go doc ./agent.Session`
- 查看子包：`go doc ./agent/continuation`

## 公共包索引

| 导入路径 | 主要入口 | 指南 |
| --- | --- | --- |
| `github.com/rsbin1178/pips/agent` | `New`、`NewSession`、`NewTool`、`AsTool`、Run/Stream、控制 Option、Event | [核心运行时](core-runtime.md)、[工具与控制](tools-control.md) |
| `github.com/rsbin1178/pips/agent/catalog` | `New`、`Merge`、`Policy`、`NewToolSearch` | [工具与控制](tools-control.md) |
| `github.com/rsbin1178/pips/agent/harness` | `New`、`NewSession`、`Repo`、JSONL、Skill、Template、Compaction | [Harness 与持久化](harness-persistence.md) |
| `github.com/rsbin1178/pips/agent/continuation` | `New`、`Create`、`Advance`、`Drive`、控制命令、Memory/JSONL Store | [Continuation](orchestration-continuation.md) |
| `github.com/rsbin1178/pips/agent/goal` | `Prepare`、`NewController`、`NewModelEvaluator` | [Goal 与 Loop](orchestration-goal-loop.md) |
| `github.com/rsbin1178/pips/agent/loop` | `Prepare`、`Every`、`NewController`、`NewModelPlanner` | [Goal 与 Loop](orchestration-goal-loop.md) |
| `github.com/rsbin1178/pips/agent/team` | `New`、Team 命令、Toolset、`NewAttemptRuntime`、Memory/JSONL Store | [Team](orchestration-team.md) |
| `github.com/rsbin1178/pips/agent/extension` | `New`、`NewDefinition`、`Runtime.Activate/Acquire/Shutdown`、Snapshot | [扩展、Bundle 与 MCP](extensions-bundles-mcp.md) |
| `github.com/rsbin1178/pips/agent/bundle` | `Open`/`New`、`Loader.Load`、`Activate` | [扩展、Bundle 与 MCP](extensions-bundles-mcp.md) |
| `github.com/rsbin1178/pips/agent/mcp` | `Connect`、`Client.Tools/Session/Close`、`NewRegistry` | [扩展、Bundle 与 MCP](extensions-bundles-mcp.md) |
| `github.com/rsbin1178/pips/agent/observability` | `NewRecorder`、`Recorder.Observe/Trace/Metrics` | [事件与可观测性](events-observability.md) |
| `github.com/rsbin1178/pips/agent/observability/otel` | `New`、`Observer.Observe` | [事件与可观测性](events-observability.md) |

以上是 `go list ./agent/...` 的全部公共包。`internal/coding` 不属于公共 Agent API，外部 module 不应导入；Coding Subagent/Team 的产品行为也不在这些包的兼容承诺内。

## 根包按任务查找

### 构造与运行

- `agent.New(model, options...)`
- `Agent.Run(ctx, session, messages...)`
- `Agent.Stream(ctx, session, messages...)`
- `agent.NewSession(messages...)`
- `Session.Messages`、`Append`、`Replace`、`Usage`
- `RunResult`、`RunInfo`、`StopReason`

### 工具

- `Tool`、`ToolCall`
- `NewTool`、`NewToolParts`、`TextResult`
- `Parallel`、`ConcurrencySafe`、`WithParallelTools`
- `ReportProgress`
- `AsTool`

### 运行控制

- `WithBeforeTool`、`ToolDecision`、`DenyTool`
- `Session.Pending`、`ResolveToolCalls`、`ResolvePending`
- `WithAfterTool`、`ToolResultOverride`
- `Session.Steer`、`FollowUp` 与 QueueMode
- `WithPrepareTurn`、`TurnUpdate`
- `WithTransformContext`
- `WithCandidateAnswer`、`CandidateAnswerDecision`
- `WithInputGuardrail`、`WithOutputGuardrail`、`GuardrailError`

### 限制与观测

- `WithMaxTurns`、`WithMaxTokens`、`WithToolTimeout`
- `WithStopWhen`
- `WithName`、`RunMetadataFromContext`
- `WithOnEvent`、`Event`、`EventType`、`EventPayload`、`NewEvent`
- `RunStarted`、`TurnStarted`、`ModelStreamEvent`、`MessageCommitted`
- `CandidateDiscarded`、`ToolStarted/ToolUpdated/ToolCompleted`
- `TurnCompleted`、`RunCompleted`、`ErrInvalidEvent`、`ErrEventWireFormat`

## Harness 按任务查找

- 构造：`New(model, session, options...)`、`NewSession(store)`
- Prompt：`Prompt`、`PromptMessages`、`PromptTemplate` 及 Stream 变体
- 活动控制：`Phase`、`Cancel`、`Steer`、`FollowUp`、`SetModel`
- 待处理调用：`ResolvePending`、`ResolveToolCalls`
- 内存存储：`NewMemoryStore`、`NewMemoryStoreWithMetadata`
- JSONL：`CreateJSONL`、`OpenJSONL`、`ReadJSONLMetadata`、`ReadJSONLPrefix`
- 仓库：`Repo.Create/Open/List/Fork/ForkSession/Delete`
- 树：`Session.Path/MoveTo/Context/Tree/CommonAncestor`
- 压缩：`ShouldCompact`、`PlanCompaction`、`SummarizeCompaction`、`Harness.Compact`
- Skill：`LoadSkills`、`NewSkillCatalog`、`NewSkillTool`、`SkillCatalog.Activate/Resource`
- 模板：`LoadTemplates`、`ValidateTemplates`、`PromptTemplate.Format`
- Continuation adapter：`NewContinuationWorker`

## 编排按任务查找

### Continuation

- 状态：`Execution`、`Status`、`Phase`、`Attempt`、`Transition`
- 执行：`Worker`、`Controller`、`Handlers`、`HandlerRef`
- 推进：`Engine.Advance`、`Engine.Drive`、`DriveOptions/DriveResult`
- 控制：`Pause`、`Resume`、`RetryWork`、`RetryDecision`、`ResolveBlock`、`Signal`、`ResumeDue`、`Cancel`、`Fail`
- 查询：`Get`、`List`、`History`
- 持久化：`NewMemoryStore`、`NewJSONLStore`、`Store`、`StoreLimits`

### Goal 与 Loop

- Goal：`goal.Prepare`、`Evaluator/EvaluatorFunc`、`NewController`、`NewModelEvaluator`、`DecodeState/DecodeWorkInput`
- Loop：`loop.Prepare`、`Planner/PlannerFunc`、`Every`、`NewController`、`NewModelPlanner`、`DecodeState`

### Team

- 聚合：`Team`、`Member`、`Task`、`Attempt`、`Message`、`Transition`
- 成员/任务/生命周期：`Engine` 上的对应命令方法及 request 类型
- Attempt：`StartTaskAttempt`、`FinishTaskAttempt`、`Dispatch`
- 邮箱：`SendMessage`、`Mailbox`、`AcknowledgeMessages`
- 变化与恢复：`History`、`Changes`、`ActiveDispatches`、`CancellationDispatches`、`InspectActiveAttempts`
- Agent 工具：`NewMemberToolset`、`NewLeadToolset`
- 同步组合：`NewAttemptRuntime`、`AttemptWorkerFactory`、`AttemptResultProjector`

## 错误处理惯例

使用 `errors.Is` 匹配包级 sentinel，例如 conflict、not found、invalid、busy、idle、terminal 或 unsupported；需要 expected/actual、阶段或损坏细节时再用 `errors.As` 读取 typed error。

错误通常会同时返回最后已知快照或部分 RunResult。不要丢弃它：先记录 ID、Revision、状态和已完成用量，再决定是否重读、显式重试或交给人工处理。

## 示例入口

- `examples/agent-basic`：类型化工具与阻塞 Run
- `examples/agent-stream`：流式事件
- `examples/agent-approval`：暂停、人工解析和恢复
- `examples/agent-harness`：JSONL 会话与压缩
- `examples/agent-subagent`：`AsTool` 管理者/专家组合
- `examples/team-agent`：应用协调器、Team、Continuation 与 Harness 的完整组合

包内 `example_test.go` 还提供可由 `go test` 编译执行的最小示例。测试策略见[测试 Agent 应用](testing.md)。
