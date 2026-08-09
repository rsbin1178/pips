# Continuation：可恢复的跨运行控制

`agent/continuation` 把应用工作拆成一系列有界 Work 与 Decision 阶段，并把每次转移作为完整快照持久化。它适合需要跨进程重启、显式重试、等待外部事件或累计硬限制的任务。

Continuation 是调用驱动的状态机，不是 scheduler、任务队列、daemon、Workflow 图或轮询服务。

## 核心模型

每个 Execution 持久保存：

- 稳定 `ID`、乐观并发 `Revision` 与业务 `Target`；
- `Worker`、`Controller` 的 `HandlerRef`；
- 当前 `Status` 与下一 `Phase`；
- ControllerState、下一次 Work 输入、激活证据；
- 当前/上一次 Attempt 和 Work/Decision 结果；
- 累计 Attempts、Turns、Usage、ActiveDuration；
- limits、等待条件、阻塞信息、终态输出和原因。

`Worker.Run` 做一次有界业务工作，返回 JSON 证据与用量。`Controller.Decide` 只根据已持久 Work 证据决定 continue、wait、block、complete、fail 或 cancel。

这个分离产生重要的重试边界：Decision 失败时可以重新评估同一 Work，而不重复副作用。

## 创建最小执行

下面的 Worker 和 Controller 使用函数适配类型：

```go
type workerFunc func(context.Context, continuation.WorkRequest) (continuation.WorkResult, error)

func (fn workerFunc) Run(
	ctx context.Context,
	req continuation.WorkRequest,
) (continuation.WorkResult, error) {
	return fn(ctx, req)
}

type controllerFunc func(context.Context, continuation.DecisionRequest) (continuation.Decision, error)

func (fn controllerFunc) Decide(
	ctx context.Context,
	req continuation.DecisionRequest,
) (continuation.Decision, error) {
	return fn(ctx, req)
}
```

构造 Store、Engine 和稳定 handler 引用：

```go
store, err := continuation.NewMemoryStore()
if err != nil {
	return err
}
engine, err := continuation.New(store)
if err != nil {
	return err
}

workerRef := continuation.HandlerRef{Kind: "report", Version: "v1"}
controllerRef := continuation.HandlerRef{Kind: "complete", Version: "v1"}

execution, err := engine.Create(ctx, continuation.CreateRequest{
	ID:         "report-42",
	Target:     continuation.Target{Kind: "report", ID: "42"},
	Worker:     workerRef,
	Controller: controllerRef,
	Input:      ai.JSON(`{"section":"summary"}`),
	Limits: continuation.Limits{
		MaxAttempts: 5,
		MaxTurns:    30,
		MaxTokens:   50_000,
	},
})
if err != nil {
	return err
}
```

`HandlerRef` 写入持久记录；应用必须在恢复时把同一 kind/version 绑定到语义兼容实现。改变行为时使用新版本，不要让旧执行静默运行新协议。

## Advance 与 Drive

`Advance(ctx, id, expectedRevision, handlers)` 一次最多调用一个 Worker 及其紧随的 Controller，绝不会开始第二个 Worker。传入的 handler ref 必须与持久引用完全匹配。

```go
handlers := continuation.Handlers{
	WorkerRef: workerRef,
	Worker: workerFunc(func(
		_ context.Context,
		req continuation.WorkRequest,
	) (continuation.WorkResult, error) {
		return continuation.WorkResult{
			Value:    ai.JSON(`{"ready":true}`),
			Progress: continuation.ProgressChanged,
		}, nil
	}),
	ControllerRef: controllerRef,
	Controller: controllerFunc(func(
		_ context.Context,
		req continuation.DecisionRequest,
	) (continuation.Decision, error) {
		return continuation.Decision{
			Action: continuation.ActionComplete,
			Output: req.Work.Value,
		}, nil
	}),
}

execution, err = engine.Advance(ctx, execution.ID, execution.Revision, handlers)
```

`Drive` 在同一调用内有限重复 Advance，但 `MaxAdvances` 必须为正。它按 terminal、waiting、paused、blocked、interrupted、gate、no_progress、quantum、context 或 error 返回 YieldReason。Gate 可在下一步前让出控制且不修改状态。

```go
driven, err := engine.Drive(
	ctx,
	execution.ID,
	execution.Revision,
	handlers,
	continuation.DriveOptions{MaxAdvances: 4},
)
```

`Drive` 的有限 quantum 防止一个调用无限占用 runner；应用收到 `YieldQuantum` 后决定何时再调用。

## 状态与动作

非终态包括 `ready`、`running`、`waiting`、`pause_requested`、`paused`、`blocked`、`interrupted` 和 `cancel_requested`。终态包括 `completed`、`failed`、`cancelled`、`limited`，可用 `Status.Terminal()` 判断。

Controller 动作映射：

- `continue`：保存 state/next input，下一次 Work 变为 ready；
- `wait`：保存 `WaitCondition`；
- `block`：保存需要外部输入的 `Block`；
- `complete`：保存 output 并终止；
- `fail`、`cancel`：记录相应终态。

若时间和 signal 同时存在，WaitCondition 使用 OR：任一满足即可激活下一次 Work。

## 显式唤醒与控制

包不会等待墙钟或订阅事件。宿主负责：

- 到点后调用 `ResumeDue`；
- 收到外部事件后调用 `Signal`；
- 收到人工输入后调用 `ResolveBlock`；
- 运维操作调用 `Pause`、`Resume`、`Cancel` 或 `Fail`。

Signal 必须有稳定 ID 和精确 key；重复投递同一 ID 幂等。`Signal` 与 `ResumeDue` 都需要调用者提供当前 Revision。

活动 Work/Decision 被 Pause 或 Cancel 时，Engine 先持久化 `pause_requested`/`cancel_requested`，再在锁外取消本进程 context。这样恢复逻辑能判断控制意图。

调用者 context 取消表示宿主执行中断，持久状态成为 `interrupted`，不是产品级 `cancelled`。只有显式 `Cancel` 才表示业务取消。

## 重试边界

Work 失败或进程中断：状态为 `interrupted/work`。调用 `RetryWork` 会创建新的 Attempt ID；外部副作用可能已经发生，所以 Worker 必须使用 ExecutionID/AttemptID 或业务键实现幂等与检查。

Controller 失败：状态为 `interrupted/decision`，已完成 Work 证据仍在。`RetryDecision` 复用同一 Work 和 Attempt，不再次调用 Worker。

Pause 后 Resume 会恢复之前的 waiting/blocked/ready 状态，不会绕过等待，也不会自动允许需要 RetryWork 的中断。

## 限制与记账

`Limits` 可限制：

- `MaxAttempts`：零采用默认 25，`-1` 表示不限；
- `MaxTurns`；
- `MaxTokens`：输入加输出；
- `MaxActiveDuration`；
- `Deadline`。

将 MaxAttempts 设为 `-1` 时必须配置另一项有限硬限制。限制是协作式累计边界：第 n 次 Work 完成后仍会执行它的 Decision，若已超预算则状态变为 `limited`，并阻止第 n+1 次 Work。

Worker 的 `WorkRequest.Remaining` 是开始前提示，不是并发配额锁；Worker 仍应在自己的 Agent/Harness 中设置单次边界。

## Revision 与冲突

每个突变都要求非零精确 Revision，Store 通过 CAS 提交。旧 Revision 返回可由 `errors.Is(err, continuation.ErrConflict)` 识别的 `ConflictError`，其中包含 expected/actual。

不要无条件循环覆盖冲突。重新 `Get`，检查最新状态是否仍符合原意，再用新 Revision 发出语义一致的命令。

同一 Engine 在进程内只允许一个执行 ID 有活动阶段，不同 ID 可并发。Memory/JSONL Store 是单进程控制存储；若实现数据库 Store 并让多个进程跑同一 ID，还必须在应用/Store 层提供 lease 或 claim，CAS 本身不等于执行所有权。

## 持久化与恢复

`MemoryStore` 用于测试；`NewJSONLStore(dir)` 为每个 execution 保存追加式完整快照。JSONL 使用严格边界与单进程假设，文件权限为 `0600`。终止时无需关闭该 Store，因为接口不持有长期打开文件。

`Get`/`List` 会协调孤儿活动状态：

- 孤儿 `running` 恢复为 `interrupted`；
- `pause_requested` 恢复为 `paused`；
- `cancel_requested` 恢复为 `cancelled`。

恢复只修正状态，从不自动启动 Worker。JSONL 最后一个未结束的破损片段可被忽略，并在下一次 CAS 前截断；其他损坏会以 `CorruptStoreError` 返回，不应跳过。

## 与 Harness 配合

`harness.ContinuationWorker` 执行恰好一次完整 Harness prompt，并投影 `RunResult`。Continuation Store 与 Harness Store 是两个控制域，没有跨 Store 事务：

1. 用稳定 execution/attempt ID 关联记录；
2. 让外部副作用幂等；
3. 恢复时同时检查 Execution 与会话保存点；
4. 不声称 exactly-once。

完成标准使用 [Goal](orchestration-goal-loop.md)，重复激活使用 [Loop](orchestration-goal-loop.md)，多成员协调使用 [Team](orchestration-team.md)。
