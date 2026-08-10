# 事件与可观测性

Agent 为阻塞和流式运行提供同一套归一化事件。事件适合 UI、日志关联、指标与 trace；它们不是业务审计日志，也不替代 Harness/Continuation/Team 的持久状态。

## 事件顺序

一次正常运行产生：

```text
run_started
  turn_started
    model_stream...              # 仅 Stream，承载 ai.StreamEvent
    candidate_discarded          # 被拒候选：不提交 assistant message
    # 或者：
    message_committed            # 已提交 assistant 消息
    tool_started...              # 有工具调用时
      tool_updated...            # 尽力而为
    tool_completed...
    message_committed            # 有工具时提交完整结果批次
  turn_completed
run_completed
```

以上 turn 区段可重复。`CandidateDiscarded` 与该候选的 `MessageCommitted` 是替代关系；丢弃候选不会进入 Session。一个模型响应包含多个工具时，每个调用都有各自的 started/completed，最终提交一条包含完整批次结果的 message。

运行级错误不会产生 `RunCompleted`，而是从 `Run`/`Stream` 返回 error。监控“开始但未正常结束”的 Run 时，需要结合调用错误和进程生命周期判断，不能只计算完成事件。

## Envelope 与类型化载荷

每个 `Event` envelope 都带相同的 `RunID`、`ParentRunID`、Agent 名称和 UTC 时间。variant 数据只存在于 sealed `EventPayload`；使用 `event.Payload()` 做 concrete type switch，使用 `event.Type()` 读取 discriminator。包外代码不能实现新的 payload 或构造 Type/payload 错配。

| `Event.Type()` | concrete payload | 主要载荷 |
| --- | --- | --- |
| `run_started` | `agent.RunStarted` | 无 |
| `turn_started` | `agent.TurnStarted` | `Turn` |
| `model_stream` | `agent.ModelStreamEvent` | `Turn`、`Event ai.StreamEvent` |
| `message_committed` | `agent.MessageCommitted` | `Turn`、`Message` |
| `candidate_discarded` | `agent.CandidateDiscarded` | `Turn`，无候选正文 |
| `tool_started` | `agent.ToolStarted` | `Turn`、`Call` |
| `tool_updated` | `agent.ToolUpdated` | `Turn`、`Call`、`Update` |
| `tool_completed` | `agent.ToolCompleted` | `Turn`、`Call`、`Result` |
| `turn_completed` | `agent.TurnCompleted` | `Turn`、累计 `Usage` |
| `run_completed` | `agent.RunCompleted` | `Turns`、`Stop`、累计 `Usage` |

构造测试夹具或 adapter 输入时使用 `agent.NewEvent(metadata, time, payload)`，并用 `errors.Is(err, agent.ErrInvalidEvent)` 处理校验失败。Runtime 发出的每个 Event 都满足 `Validate()`。

Message Parts、媒体 bytes、Tool Args/Result/Update 和 stream Usage 在事件边界做深拷贝；observer、stream consumer 和 Session 互不共享这些可变数据。Event 可以保留，但 `Payload()` 返回的 slice/bytes 仍应视为只读。

Event 是进程内协议，不是 wire schema。直接 `json.Marshal`/`json.Unmarshal` 会返回 `agent.ErrEventWireFormat`；持久化或远程传输必须使用应用拥有的版本化 projection，并在该边界定义披露和脱敏。

## 阻塞运行观察器

`WithOnEvent` 回调与运行在同一 goroutine 同步执行：

```go
a, err := agent.New(model,
	agent.WithOnEvent(func(_ context.Context, event agent.Event) {
		if completed, ok := event.Payload().(agent.RunCompleted); ok {
			log.Printf(
				"run=%s parent=%s stop=%s",
				event.RunID,
				event.ParentRunID,
				completed.Stop,
			)
		}
	}),
)
```

回调应快速、无阻塞且避免调用同一 Session 的控制路径。需要网络导出时，应向应用自有的有界缓冲提交最小记录，并定义溢出策略；不要让遥测后端延迟模型运行。

Harness 应通过 `harness.WithOnEvent` 安装观察器。Extension Snapshot 的 `HarnessOptions` 也遵守这一入口，不会把 `agent.WithOnEvent` 塞进 Agent options。

## 流式消费

`Agent.Stream` 与 `Harness.PromptStream` 直接把事件交给调用者。`ModelStreamEvent` 是暂定展示内容：候选 hook 或输出 guardrail 尚未运行。严格内容策略应缓冲到最终 `MessageCommitted`/成功结果。

消费者提前 `break` 表示取消运行。Harness 会恢复 idle 并保存已经完成的提交点；直接 Agent Session 也会保留已提交内容，且可能有 pending 工具调用。

## 内存 Recorder

`agent/observability` 的 `Recorder` 是并发安全、无 goroutine 的轻量观察器：

```go
recorder := observability.NewRecorder()
a, err := agent.New(model, agent.WithOnEvent(recorder.Observe))
if err != nil {
	return err
}

result, err := a.Run(ctx, agent.NewSession(), input...)
if err != nil {
	return err
}

trace, ok := recorder.Trace(result.RunID)
metrics := recorder.Metrics()
```

RunTrace 只保存身份、时间、停止原因、轮次、Usage 和工具名/时间/失败标志；它不保留 prompt、工具参数或结果。Metrics 汇总开始/完成 Run、turn、工具调用/失败和 Usage。

Recorder 不做持久化或淘汰。长生命周期服务若无限保留每个 RunTrace，会持续占用内存；应用应按自己的保留策略读取、导出并轮换 Recorder，或实现直接聚合观察器。

## OpenTelemetry

`agent/observability/otel` 把事件映射到 `agent.run`、`agent.turn` 和 `agent.tool` span，以及 Run、工具、失败和 token counter。应用拥有 SDK、exporter、采样、resource 和关闭流程：

```go
observer, err := agentotel.New(agentotel.Config{
	TracerProvider: tracerProvider,
	MeterProvider:  meterProvider,
})
if err != nil {
	return err
}

a, err := agent.New(model, agent.WithOnEvent(observer.Observe))
```

Provider 为 nil 时使用 OTel global provider。Observer 并发安全且不导出 prompt、工具参数或输出，只记录运行身份、Agent、轮次、工具名、停止原因和计数。

因为 Agent 同步调用 `Observe`，应用应配置适合生产的非阻塞/batch exporter。进程退出时按 OTel SDK 要求 flush 和 shutdown；Observer 不拥有这些资源。

## 嵌套 Agent 关联

`agent.AsTool` 把运行身份放入工具 context，子 Run 自动得到 `ParentRunID`。同一个 Recorder 或 OTel Observer 可以观察管理者与多个子 Agent，并按这两个 ID 还原调用树。

这只是运行关联，不表示子任务持久化。子 Agent 的业务任务、审批和外部资源仍由应用根据所选编排层保存。

## 隐私与审计

- 默认避免记录 message、Args、Result 和 Skill 正文。
- 若业务必须保留内容，先定义脱敏、访问控制、保留期和删除策略。
- 工具授权决定、人工审批和外部副作用应写入业务审计系统；事件是运行事实，不是不可抵赖审计。
- 将 RunID 传播到应用日志和下游请求，但不要把它当认证凭据。
- `ToolUpdated` 可能丢失，不可作为进度完成证明。
