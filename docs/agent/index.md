# Agent 使用指南

`github.com/rsbin1178/pips/agent` 提供一个可控、可组合的模型与工具循环。它负责把消息发送给模型、执行工具调用、维护一次运行的状态，并在确定的边界停止。持久会话、跨运行控制和多成员协作分别由其他公共包提供。

这套文档面向熟悉 Go，但首次使用 Pips 的开发者。模型接口与 Provider 配置先见 [AI 包使用指南](../ai/index.md)。本文档描述当前公开 API；每个符号的完整字段和签名以 [API 导航](api-navigation.md) 与 Godoc 为准。

## 从哪里开始

- 第一次运行 Agent：阅读[快速开始](quickstart.md)。
- 理解 `Agent`、`Session`、运行轮次与停止原因：阅读[核心运行时](core-runtime.md)。
- 定义工具、审批副作用、暂停和恢复：阅读[工具与控制](tools-control.md)。
- 保存对话、分支、压缩上下文和加载 Skill：阅读[Harness 与持久化](harness-persistence.md)。
- 接入 Extension、Bundle 或 MCP：阅读[扩展、Bundle 与 MCP](extensions-bundles-mcp.md)。
- 采集事件、内存指标和 OpenTelemetry span：阅读[事件与可观测性](events-observability.md)。
- 选择直接运行、子 Agent、Continuation、Goal、Loop 或 Team：阅读[编排方式选择](choosing-orchestration.md)。

## 能力地图

| 能力 | 公共包 | 负责 | 不负责 |
| --- | --- | --- | --- |
| 单次模型/工具循环 | `agent` | 消息、工具、控制钩子、停止原因、运行事件 | 持久化后端、后台调度、工作流图 |
| 工具目录与延迟发现 | `agent/catalog` | 来源、风险、租户策略、显式工具搜索 | 自动发现可执行文件、授权策略本身 |
| 状态化会话 | `agent/harness` | 追加式会话树、JSONL、上下文重建、压缩、Skill | 自主执行、远程控制平面 |
| 跨运行延续 | `agent/continuation` | 持久阶段、重试边界、等待条件、硬限制 | 定时器、轮询器、队列、完成标准 |
| 目标完成策略 | `agent/goal` | 根据证据决定继续、完成或阻塞 | Worker、存储、调度 |
| 重复激活策略 | `agent/loop` | 计算下次时间或信号条件 | 睡眠、cron、补跑、唤醒投递 |
| 多成员协作 | `agent/team` | 成员、依赖任务、尝试、邮箱、协作状态 | Agent 创建、成员资源、调度器、Workflow |
| 编译期扩展 | `agent/extension` | 可信 Go 扩展、不可变运行代、生命周期 | 动态代码加载、进程沙箱 |
| 本地声明式分发 | `agent/bundle` | 清单、Skill、模板、不透明资源 | 执行脚本、安装包、联网 |
| MCP 工具桥接 | `agent/mcp` | 官方 MCP SDK 会话、工具快照、变更信号 | 自动重连、信任远端注解、热改运行中 Agent |
| 运行观测 | `agent/observability`、`agent/observability/otel` | 运行轨迹、指标、OpenTelemetry span | 日志/指标存储后端、业务审计 |

## 最重要的边界

`Agent` 是不可变配置，可以被多个 goroutine 共享；`Session` 保存一次对话的可变状态，同一时刻只允许一个活动运行。需要并行处理多个请求时，应为每个请求建立独立 `Session`。

`agent.AsTool` 是管理者拥有对话的轻量子 Agent：每次工具调用创建新的子 Session，结束后只把结果返回管理者。若子任务必须跨重启保留审批或运行状态，应由应用持有子 Session，或使用 Continuation；不要把 `AsTool` 当作持久调度设施。

Team 与 Coding 产品中的 Subagent/Team 不是同一层 API：

- `agent/team` 是通用、公开、模型无关的持久协作账本。
- Coding Subagent 与 Coding Team 位于应用层，组合工作区、命令、权限、MCP、Hook 和具体 Coding Agent 行为。
- 公共 Team 不创建 Coding Agent，不分配工作区，不运行成员，也没有后台调度队列。

需要验收具体 Coding 产品能力时，使用 [Coding Agent 人工验收手册](../manual-acceptance/index.md)，不要用通用包的单元测试代替真实 Provider、平台或 Beta 观察证据。

## 推荐的学习顺序

1. 使用 `Agent.Run` 完成一个有工具的请求。
2. 为有副作用的工具添加 `WithBeforeTool`，练习暂停与恢复。
3. 需要跨进程对话时再引入 Harness 和 JSONL。
4. 只有当一个任务需要跨多次有界运行恢复时，才引入 Continuation。
5. 在 Continuation 之上按需求选择 Goal 或 Loop；只有独立成员确实需要共享任务与邮箱时才引入 Team。

这种渐进组合能让资源所有权和失败边界保持清晰，也避免把普通模型调用过度实现成调度系统。
