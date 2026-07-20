# Team Agent 综合示例

这个示例演示如何使用 OpenAI Chat Completions 兼容接口，完成一次完整且可恢复的
Team 生命周期。SDK 会在 Base URL 后追加 `/chat/completions`，因此 Base URL 应填写到
API 版本前缀，例如 `https://api.example/v1`。

## 运行

配置模型后直接运行：

```bash
export PIPS_CHAT_BASE_URL="https://opencode.ai/zen/go/v1"
export PIPS_CHAT_API_KEY="your-api-key"
export PIPS_CHAT_MODEL="your-model-id"

go run ./examples/team-agent
```

如果本地模型服务不要求认证，`PIPS_CHAT_API_KEY` 可以为空。示例允许访问 HTTP 和
私有 IP，因此也可以连接 Ollama、vLLM 等本地 Chat Completions 兼容服务。

启动后进入简易交互终端：

```text
Team Agent 终端已启动。输入问题后按回车；/status 查看状态；/help 查看命令；/exit 退出。
你> 请解释这个 SDK 中 Team 和普通 Agent 的区别
[第 1 轮：Team 正在执行]
[researcher] 正在流式输出研究结果...
[writer] 正在流式输出回答草稿...
[lead] 正在流式输出最终回答...

你> 它适合什么场景？
[第 2 轮：Team 正在执行]
[researcher] ...
[writer] ...
[lead] ...

你> /status
Team=terminal-conversation-team status=active revision=... turns=2 tasks=4/4 messages=4
你> /exit
Team=terminal-conversation-team status=completed revision=... turns=2 tasks=4/4 messages=4
终端会话已结束。
```

每一行非命令输入代表一轮用户请求。Team 和三名成员的 Harness Session 在进程内持续
存在，所以第二轮会带上各成员第一轮的会话上下文。每轮会依次调用 researcher、writer、
lead，因此通常产生 3 次模型请求。三个阶段都使用 Chat Completions SSE，收到 TextDelta
后立即写入终端；不需要等待完整响应。阶段标签用于区分当前输出来自哪个成员。
为保持 Team 终态 JSON 有界，退出时 Team Output 只保存轮次数和最后一轮；完整上下文由
成员 Harness Session 管理。

## 文件职责

- `main.go`：读取模型配置，创建模型、Team Engine 和 Continuation Engine，然后启动
  示例 Coordinator。这里属于基础设施装配代码。
- `prompts.go`：集中保存成员和 Lead 的 System Prompt，以及动态任务 Prompt 的组装函数。
- `team_coordinator.go`：实现本示例的应用层 Coordinator，负责成员注册、任务依赖、claim、attempt、
  Continuation 调度、mailbox 对账和 Team 完成。
- `streaming.go`：消费 Harness 事件流，实时输出 researcher、writer、lead 的 TextDelta，
  并在流结束后提取完整结果供 Continuation 和 Team 持久化。
- `terminal.go`：实现基于标准输入输出的多轮 REPL，以及 `/help`、`/status`、`/exit`。
- `main_test.go`：使用本地模拟的 `/v1/chat/completions` 接口验证完整执行链路，不需要
  真实 API Key。

`team_coordinator.go` 中的 `teamCoordinator` 只是案例协调器，并不是 SDK 要求实现的接口，也不是
SDK 内置的特殊 Agent 类型。

## 提示词设计

业务提示词统一放在 `prompts.go`，运行时底座本身不硬编码 Prompt。示例分为三层：

1. `memberSystemPrompt`：约束 researcher 和 writer 只处理当前 Dispatch，说明 Team 工具
   只读、生命周期由 Coordinator 管理，并要求区分事实和不确定性。
2. `leadSystemPrompt`：要求 Lead 使用已完成任务和 mailbox 证据直接回答用户，处理冲突
   时采用保守结论，并使用与用户相同的语言。
3. `memberDispatchPrompt` 和 `leadAnswerPrompt`：把当前轮的结构化 JSON 输入传给模型。
   JSON 中包含用户请求、成员身份、任务、AttemptID、ContinuationID 和本轮 mailbox。

每轮的 research/draft 任务说明在 `team_coordinator.go` 的 `defineTurnTasks` 中生成。实际项目可以
替换 `prompts.go` 的内容，也可以根据成员 Role、模型能力或租户策略动态生成 System Prompt。

## 为什么综合示例代码较多

这个示例刻意展开了生产级 Coordinator 需要处理的全部边界：

1. Coordinator 创建一次 Team，并注册固定的 Lead、researcher 和 writer。
2. 每条终端输入触发一轮执行；Coordinator 创建两个带依赖关系的唯一任务并分配给对应成员。
3. 成员 claim 任务后，Coordinator 先持久化 Team attempt，再创建对应的 Continuation。
4. 每个成员使用独立 Harness Session，并获得绑定自身身份的只读 Team 工具。
5. Coordinator 检查 `missing`、`nonterminal` 和 `terminal` child 状态，展示进程崩溃后的恢复依据。
6. 成员和 Lead 的 TextDelta 实时输出；完整流结束后，Coordinator 将 child 结果回写为 Team task
   result，并通过 mailbox 传给下一个成员。
7. Lead 在自己的 Session 中汇总当前轮结果；收到 `/exit` 或 EOF 后显式完成 Team。

这些代码主要是**应用层编排和恢复策略**，不是每次调用模型都需要重复编写的样板代码。
实际项目通常会把它们封装为可复用的 Coordinator、调度器或业务服务。

## 使用 SDK 是否都需要这么多代码

不需要。复杂度取决于需要的能力层级：

| 需求 | 建议使用 | 需要自行处理的内容 |
| --- | --- | --- |
| 单个模型工具循环 | `agent.Agent` | Prompt、工具和停止条件 |
| 保留独立会话 | `agent/harness` | Session Store 和上下文策略 |
| 一个 Agent 委托另一个 Agent | `agent.AsTool` | Manager 的工具选择和结果汇总 |
| 跨运行恢复一个任务 | `agent/continuation` | Worker、Controller 和唤醒策略 |
| 多成员共享任务与 mailbox | `agent/team` | Coordinator 调度、成员资源和 child 执行 |

如果只需要创建 Team 并让成员通过工具查看或修改 Team 状态，核心调用只有：

```go
store, _ := team.NewMemoryStore()
runtime, _ := team.New(store)

group, _ := runtime.Create(ctx, team.CreateRequest{
    Command: team.CommandMetadata{
        ID:    "create-team",
        Actor: team.Actor{Kind: team.ActorKindCoordinator, ID: "coordinator"},
    },
    ID:        "demo-team",
    Objective: "完成一次协作任务",
    Lead: team.MemberSpec{
        ID: "lead", Name: "Lead", Role: "协调任务", SessionRef: "session-lead",
    },
})

leadTools, _ := team.NewLeadToolset(runtime, group.ID, "lead")
lead, _ := agent.New(model, agent.WithTools(leadTools.Tools()...))
```

综合示例更长，是因为它还演示了 Team 与 Harness、Continuation 之间的完整绑定、结果对账
和恢复窗口。当前 Team 基础层不会偷偷启动 goroutine、创建成员 Agent 或实现调度器；这些
策略保留在应用层，便于后续按产品需求实现 Team Coordinator。

## 状态存储

为了可以直接运行，示例使用内存 Store。需要跨进程恢复时，可以替换为：

- `team.NewJSONLStore`：保存 Team、任务、attempt、mailbox 和审计历史。
- `continuation.NewJSONLStore`：保存 child execution 及其恢复状态。
- Harness JSONL Store：保存每个成员独立的 Session 树。
