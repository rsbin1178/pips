# 核心运行时

`agent` 根包实现一个有界的模型/工具循环。本章解释它的状态所有权、运行协议、停止条件和并发语义。

## Agent 与 Session

`Agent` 保存模型、系统提示词、工具快照、限制和钩子。构造完成后它不可变且可并发共享。

`Session` 保存消息、待处理工具调用、steering 和 follow-up 队列，以及当前是否正在运行。它是一次对话的可变状态；同一时刻只允许一个活动运行。

```go
a, err := agent.New(model,
	agent.WithName("support"),
	agent.WithSystem("回答产品支持问题。"),
	agent.WithMaxTurns(12),
)
if err != nil {
	return err
}

alice := agent.NewSession()
bob := agent.NewSession()

// a 可以被共享；alice 和 bob 彼此隔离。
```

不要复制 Session 来实现分支，也不要从并发 goroutine 调用同一个 Session 的 `Run`。内存 Session 可以序列化为稳定 JSON，但零值无效，应始终通过 `agent.NewSession` 创建。需要追加式持久树和分支导航时使用 Harness。

## 一次 Run 的生命周期

一次阻塞运行大致经历：

1. 获得 Session 的单运行所有权，并生成 `RunID`。
2. 发出 `run_start` 事件，执行输入 guardrail。
3. 提交输入消息并开始一轮模型请求。
4. 若模型返回工具调用，依次经过 before-tool gate、执行器和 after-tool hook，再提交完整工具结果消息。
5. 若模型返回最终答案，执行候选答案与输出 guardrail。
6. 处理已排队的 steering/follow-up，或按停止条件结束。
7. 成功结束时发出 `run_end`，释放 Session。

流式运行遵循相同的提交点，只多出 provisional delta。事件的精确顺序见[事件与可观测性](events-observability.md)。

## 消息提交与失败

Session 只保留已提交消息。输入 guardrail 在新输入进入 transcript 和模型 I/O 之前运行，因此输入被拒绝不会污染对话。

工具批次会为每个调用形成结果，即使发生未知工具、参数解码错误、panic、超时或执行错误。这样 transcript 仍满足“每个工具调用都有结果”的协议，可以继续运行或序列化。

模型、guardrail、prepare hook 等运行级错误会返回 `error` 和部分 `RunResult`。部分结果便于审计已消费的轮次和 token，但它不是正常结束；此时不要依赖 `Stop`。

## 停止原因

| `StopReason` | 含义 | 常见后续动作 |
| --- | --- | --- |
| `end_turn` | 模型给出最终答案且没有待处理控制消息 | 展示或保存结果 |
| `max_turns` | 已达到 Agent 轮次上限 | 缩小任务、提高明确有限的预算或转为 Continuation |
| `budget` | 累计 token 预算耗尽 | 保存部分结果，由产品策略决定是否重试 |
| `paused` | before-tool gate 暂停了工具批次 | 审批并解析 `Session.Pending()` 后再次 `Run` |
| `stop_when` | 自定义停止谓词命中 | 按应用协议解释最后状态 |
| `terminated` | 工具批次一致请求终止 | 读取工具结果；follow-up 仍会先运行 |

默认 `MaxTurns` 为 25。`WithMaxTurns(0)` 或负数取消轮次上限；这类配置必须配合 token、deadline、Continuation limit 或宿主的其他有限硬边界。

`WithMaxTokens` 按一次 Run 的累计输入加输出 token 计数，零表示不限制。它是协作式边界：已经开始的模型调用可能使总量略微越过阈值，运行时会阻止下一轮。

## 动态输入与控制队列

活动运行期间可以向 Session 注入消息：

- `Steer` 在下一次模型调用前加入消息，适合修正当前方向。
- `FollowUp` 在模型原本准备结束后继续一次对话，适合追加用户问题。
- `Append` 直接追加消息，并在下一次模型调用可见。

默认队列策略每个边界消费一组消息；`QueueDrainAll` 可以一次排空。控制队列是进程内运行控制，不是持久任务队列，也没有优先级、百分比 rollout 或后台消费者。

## 每轮准备与上下文变换

`WithPrepareTurn` 在模型调用前得到 `RunInfo`，可以返回 `TurnUpdate`：替换下一轮模型、工具快照，或持久重写消息。运行时在暴露给模型前验证更新。

`WithTransformContext` 只改变某次模型请求看到的消息视图，不修改 Session。适合裁剪或脱敏临时上下文。

若希望重写结果永久影响以后运行，使用 `WithPrepareTurn` 的 `ReplaceMessages`；若只需要临时视图，使用 `WithTransformContext`。不要在钩子中偷偷创建第二份会话事实来源。

## 候选答案策略

无工具调用的完整候选形成后，输出 guardrail 先验证候选；通过后 `WithCandidateAnswer` 再决定接受、终止或重试：

- 返回零值表示接受。
- 返回错误会终止 Run。
- 返回 Retry 会丢弃尚未提交的候选，并可用一次性的 `ModelRequestUpdate` 调整重试请求。

被丢弃候选的 token 仍计入 Usage；事件只说明候选被丢弃，不携带候选正文。流式客户端可能已经看到该候选的 delta，因此严格审核场景应缓冲显示。

## 嵌套运行与身份

每个 `RunResult` 和事件都带有 `RunID`、`ParentRunID` 与 Agent 名称。工具执行上下文可以通过 `agent.RunMetadataFromContext` 读取同一身份。

`agent.AsTool` 会把父运行上下文传入子运行，因此共享观察器可以还原管理者与子 Agent 的调用树：

```go
researcher, err := agent.New(model,
	agent.WithName("researcher"),
	agent.WithSystem("只返回核验后的事实。"),
)
if err != nil {
	return err
}

manager, err := agent.New(model,
	agent.WithTools(agent.AsTool(
		researcher,
		"research",
		"委托一个独立研究任务。",
	)),
)
```

每次 `AsTool` 调用都创建全新子 Session，因此它天然无状态、可以并行调用。它不会保留子会话的审批状态；子 Agent 若暂停，工具返回 `ErrSubagentPaused`。需要可恢复子任务时，由应用持有子 Session 或使用 Continuation。

## 并发与取消

- 多个 goroutine 可以使用同一个 Agent，但必须使用不同 Session。
- 同一个 Session 的第二个运行返回 `ErrRunActive`。
- `context.Context` 的取消和 deadline 会传播到模型、工具和嵌套 Agent。
- 工具级超时由 `WithToolTimeout` 设置；超时转换成错误工具结果，主 Run 可以继续。
- 事件回调在运行 goroutine 同步执行，应保持快速且避免阻塞。

若调用者放弃流式迭代，运行会被取消。结束后先检查错误与待处理调用，再决定是否复用 Session。

## 何时升级到其他层

- 需要追加式持久对话、分支或压缩：使用 Harness。
- 需要一次可恢复的跨运行状态机：使用 Continuation。
- 需要完成条件或重复激活策略：在 Continuation 上使用 Goal 或 Loop。
- 需要多个独立成员共享任务与邮箱：使用 Team。

这些层不会自动启动 goroutine 或调度后台工作；宿主应用始终拥有运行时机和外部资源。
