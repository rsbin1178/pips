# 如何选择编排方式

从最小满足需求的层开始。Pips 的各层按状态所有权组合，不存在一个自动接管所有执行的总调度器。

## 快速决策表

| 需求 | 选择 | 持久状态 | 谁触发下一步 |
| --- | --- | --- | --- |
| 一个有界模型/工具请求 | `Agent.Run` / `Stream` | 可选 Session JSON | 当前调用者 |
| 持久对话、分支、压缩 | Harness | 会话树 Store | 当前调用者 |
| 管理者临时调用专家 | `agent.AsTool` | 无子会话持久状态 | 父 Agent 的工具调用 |
| 跨进程继续多个有界 Work | Continuation | Execution Store | 应用调用 `Advance`/`Drive` |
| 根据证据判断是否完成 | Goal + Continuation | Goal ControllerState | 应用驱动 Continuation |
| 按时间或信号重复激活 | Loop + Continuation | Loop ControllerState/Wait | 应用定时索引或信号入口 |
| 独立成员共享任务、依赖与邮箱 | Team | Team Store；子执行另存 | 应用协调器/Worker runner |

## 直接 Agent.Run

适合能在一次调用生命周期中完成的模型/工具循环。Agent 自带轮次、token、context deadline、工具超时和自定义停止边界。

选择它时，调用者拥有 Session、并发、重试与结果交付。若进程崩溃后无需继续，没必要先建立持久编排层。

## Harness

Harness 解决“这段对话如何持久存在和构造上下文”。它不解决“任务何时再次运行”。同一个 Harness 同时只做一个 prompt/导航/压缩操作；多个会话由应用分别拥有和调度。

如果需求只是聊天历史和断点恢复，Harness 已足够。不要仅因为会话很长就引入 Team。

## AsTool

`agent.AsTool` 适合 manager/worker 模式：父 Agent 决定何时委托，子 Agent 在新 Session 中运行到结束，父 Agent 综合文本结果。

它的关键语义：

- 每次调用隔离，无子 Session 复用；
- context、运行身份与取消传播；
- 可在确认子 Agent 配置安全后标记并行；
- 子 Agent 暂停只能返回 `ErrSubagentPaused`，没有可恢复审批句柄。

因此它不是 handoff，也不是持久 Subagent runtime。若专家应继续拥有对话，应用必须显式选择目标 Session、过滤转移上下文并转移策略。

## Continuation

Continuation 解决“一个应用工作要跨多次有界调用、重启后还能从准确阶段继续”。它持久记录 Work 与 Decision 的边界、尝试、累计限制、等待、阻塞、暂停和取消状态。

应用提供 Worker、Controller 及稳定 HandlerRef，并显式调用 `Advance` 或有上限的 `Drive`。包本身不睡眠、不轮询、不创建队列、不注册 cron、不运行 daemon，也不拥有 Harness transcript。

## Goal

Goal 是 Continuation Controller 策略，适合“直到证据满足明确条件”。Worker 产生证据，Evaluator 返回 continue、complete 或 blocked。

Goal 不运行 Worker、不存状态、不决定唤醒时机。它的价值是把完成标准从 Worker 中分离，让 Decision 失败时可以只重试评估而不重做已经完成的 Work。

## Loop

Loop 是 Continuation Controller 策略，适合“完成一轮后，等待某个时间或信号再做下一轮”。固定间隔使用 `loop.Every`，复杂策略实现 Planner 或使用 ModelPlanner。

Loop 只持久化下一次条件。到点后仍由应用调用 `ResumeDue`，外部事件仍由应用调用 `Signal`。它不 catch-up 每个错过间隔，也不是 scheduler。

## Team

Team 用于多个独立成员需要共享：固定身份、依赖任务、排他 claim/attempt、直接邮箱和 Team 终态。每个成员的 Agent/Harness、模型、凭据、工作区和工具仍由宿主资源注册表拥有。

Team 不创建成员 Agent，不运行任务，不做远程 Worker lease，不解析工作流图。`AttemptRuntime` 只是一次同步组合：claim → start → mailbox → child continuation → project → finish；调用它的时机仍由应用决定。

## 常见组合

### 持久单 Agent

```text
应用请求 → Harness Prompt → Agent Run → Harness Store
```

适合长期对话。应用接收请求并调用 Harness。

### 可恢复目标任务

```text
应用驱动 → Continuation Advance
              ├─ Work: Harness Prompt
              └─ Decision: Goal Evaluator
```

Harness 保存对话，Continuation 保存控制阶段，Goal 判断完成。两份 Store 无跨库事务，Worker 与外部副作用要幂等。

### 定期检查

```text
Loop Decision → Waiting(NotBefore/Signal)
应用定时索引/事件入口 → ResumeDue/Signal → Ready
应用 runner → Advance
```

Loop 不等待墙钟；应用拥有定时器和唤醒投递。

### 多成员协作

```text
Team task/attempt ──绑定──> Continuation execution ──调用──> member Harness
       ↑                         ↑                            ↑
  协作事实 Store             控制状态 Store                对话 Store
```

三份状态各有单独 revision/提交点。应用通过稳定 ID 恢复关联，并为跨系统操作定义幂等协议。

## Coding Subagent 与公共 Team 的区别

Coding 产品中的动态 Subagent 是 `internal/coding` 应用能力：它把具体 Coding Agent、工作区、命令权限、MCP、Hook、前后台交互和产品准入组合起来。它不属于公共 `agent` 包，也不是 `agent/team` 的别名。

公共 Team 更底层、更通用：只管理协调事实，不知道代码仓库、终端、草稿、sandbox 或模型会话。当前公共能力也不包含调度队列、百分比 rollout、第二套 stage 配置、共享草稿或持久准入占位事件。

不要从 `internal/coding` 导入类型来构建通用应用；Go 的 `internal` 边界也会阻止外部 module 这样做。

## 不要混淆的概念

- Agent 不是 Workflow engine。
- Controller 决定状态，不负责调度。
- WaitCondition 是持久条件，不是正在运行的 timer。
- Store 是状态持久化，不等于 exactly-once。
- `AsTool` 是 manager-owned delegation，不是会话 handoff。
- Team 的 Coordinator actor 是可信宿主审计身份，不是认证系统。
- MCP Registry 是 pull-based 快照，不是自动重连服务。
