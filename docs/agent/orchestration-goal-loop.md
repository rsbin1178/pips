# Goal 与 Loop 策略

`agent/goal` 和 `agent/loop` 都是 Continuation 的可选 Controller。它们复用 Continuation 的 Store、阶段、limits、控制与恢复语义，自身不保存 Execution、不运行 Worker，也不调度下一步。

## 何时选择 Goal

Goal 适合完成条件可以用 Work 证据评估的任务，例如“所有测试通过且无高危告警”。每轮 Worker 产生证据，Evaluator 返回：

- `OutcomeContinue`：带反馈进入下一次 Work；
- `OutcomeComplete`：提交最终 output；
- `OutcomeBlocked`：等待外部输入。

条件最多 4000 个 Unicode 字符。应写成可验证条件，而不是模糊愿望；Evaluator 只看到有界 JSON 证据，不应依赖隐藏会话状态。

## 构造 Goal

`goal.Prepare` 产生创建 Continuation 所需的 ControllerState 和首个 WorkInput：

```go
setup, err := goal.Prepare("测试通过且安全扫描没有高危问题")
if err != nil {
	return err
}

evaluator := goal.EvaluatorFunc(func(
	_ context.Context,
	evaluation goal.Evaluation,
) (goal.EvaluationResult, error) {
	var evidence struct {
		TestsPassed bool `json:"tests_passed"`
		Critical   int  `json:"critical"`
	}
	if err := json.Unmarshal(evaluation.Evidence, &evidence); err != nil {
		return goal.EvaluationResult{}, err
	}
	if evidence.TestsPassed && evidence.Critical == 0 {
		return goal.EvaluationResult{
			Outcome: goal.OutcomeComplete,
			Reason:  "验收条件已经满足",
			Output:  evaluation.Evidence,
		}, nil
	}
	return goal.EvaluationResult{
		Outcome:  goal.OutcomeContinue,
		Reason:   "仍有未通过检查",
		Feedback: ai.JSON(`{"next":"修复失败项后重试"}`),
	}, nil
})

controller, err := goal.NewController(evaluator)
if err != nil {
	return err
}

execution, err := engine.Create(ctx, continuation.CreateRequest{
	ID:              "release-goal",
	Target:          continuation.Target{Kind: "release", ID: "v1.2.0"},
	Worker:          workerRef,
	Controller:      continuation.HandlerRef{Kind: "goal", Version: "v1"},
	ControllerState: setup.ControllerState,
	Input:           setup.WorkInput,
	Limits:          continuation.Limits{MaxAttempts: 8},
})
```

下一次 Worker 可以用 `goal.DecodeWorkInput(req.Input)` 读取稳定 envelope，其中包含条件、评估次数、上次原因和反馈。`goal.DecodeState` 可供运维/UI 严格读取版本化 ControllerState。

Evaluator 返回 blocked 时必须携带 `continuation.Block`；应用收集输入后调用 `ResolveBlock`。普通 evaluator 错误会把 Execution 中断在 Decision 阶段，`RetryDecision` 不重做 Work。

## ModelEvaluator

`goal.NewModelEvaluator(model)` 使用一个严格、无工具、temperature 为 0 的结构化模型调用评估证据。`WithMaxTokens` 限制评估输出。

内置 ModelEvaluator 只允许 continue 或 complete，不会自行返回 blocked；需要业务阻塞语义时实现自定义 Evaluator。模型输出不合法、provider 失败或超限会成为 Decision 错误，按 Continuation 的 `RetryDecision` 恢复。

把 evaluator 模型 Usage 通过 `EvaluationResult.Usage` 纳入 Continuation 累计记账。不要让模型评估器直接执行工具或外部副作用。

## Goal 的状态更新

Controller 每次决定后持久记录评估次数和最后一个 outcome/reason。continue 时，Feedback 被写入下一次 Goal WorkInput；complete 时 Output 成为 Execution output；blocked 时 Block 成为外部解析边界。

Goal 没有自己的 Store、runner、会话或后台进程。通常的组合是 Harness Worker 产生任务证据，Goal Controller 评估证据，应用驱动 Continuation。

## 何时选择 Loop

Loop 适合“每轮工作完成后，计划下一次激活”，例如：

- 每 5 分钟检查一次部署；
- 到时间或收到 `deployment_ready` 信号时再检查；
- 由规则或模型决定下次延迟；
- 满足 Planner 的停止条件后输出最终结果。

Loop 不是长时间运行的 for 循环。Controller 把 `NotBefore` 和/或 SignalSpec 写入 Continuation，随后返回；应用负责未来的唤醒。

## 固定间隔 Loop

```go
setup, err := loop.Prepare(ai.JSON(`{"prompt":"检查部署状态"}`))
if err != nil {
	return err
}
controller, err := loop.Every(5 * time.Minute)
if err != nil {
	return err
}

execution, err := engine.Create(ctx, continuation.CreateRequest{
	ID:              "deployment-watch",
	Target:          continuation.Target{Kind: "deployment", ID: "prod"},
	Worker:          workerRef,
	Controller:      continuation.HandlerRef{Kind: "fixed-loop", Version: "v1"},
	ControllerState: setup.ControllerState,
	Input:           setup.WorkInput,
	Limits: continuation.Limits{
		MaxAttempts:       20,
		MaxActiveDuration: 30 * time.Minute,
	},
})
```

一次 Advance 完成 Work/Decision 后状态为 waiting。宿主维护到期索引，到点后用最新 Revision 调用：

```go
execution, err = engine.ResumeDue(ctx, execution.ID, execution.Revision)
```

`ResumeDue` 不到期时返回 `ErrNotDue`。错过多个周期只合并为下一次激活，不逐个 catch-up；下一次 due time 以 Decision 时的 clock 计算。

## 自定义 Planner

Planner 返回 `Plan`：

```go
planner := loop.PlannerFunc(func(
	_ context.Context,
	req loop.PlanRequest,
) (loop.Plan, error) {
	if bytes.Contains(req.Evidence, []byte(`"done":true`)) {
		return loop.Plan{
			Stop:   true,
			Reason: "部署完成",
			Output: req.Evidence,
		}, nil
	}
	return loop.Plan{
		After:     10 * time.Minute,
		SignalKey: "deployment_ready",
		Reason:    "等待下一次检查或部署事件",
	}, nil
})

controller, err := loop.NewController(planner)
```

非停止 Plan 必须至少提供正 `After` 或 `SignalKey`；同时提供时采用 OR。停止 Plan 不能再携带激活条件或 NextWorkInput。

`PlannerState` 和 `NextWorkInput` 为 nil 表示保留旧值，非 nil 表示替换。`Output` 只允许停止 Plan。`Reason`、Signal key 与 JSON 均有严格大小/格式边界。

外部事件通过稳定 Signal ID 投递：

```go
execution, err = engine.Signal(ctx, execution.ID, execution.Revision,
	continuation.Signal{
		ID:      "event-018f",
		Key:     "deployment_ready",
		Payload: ai.JSON(`{"revision":"abc123"}`),
	},
)
```

重复 Signal ID 幂等，错误 key 返回 mismatch。Loop 不订阅消息系统；订阅、鉴权和重放策略由应用事件入口负责。

## ModelPlanner

`loop.NewModelPlanner` 用严格、无工具的结构化模型调用产生计划。默认 delay bounds 是 1 分钟到 1 小时，可用 `WithDelayBounds(min,max)` 修改；结果必须是边界内的正整秒，非法值直接失败而不是被静默 clamp。`WithMaxTokens` 限制 planner 输出。

模型 Planner 的 Usage 会计入 Continuation。由于模型输出会影响唤醒时机，应同时设置 Continuation attempts/deadline/active duration，并让应用对极端时间和信号策略做产品级限制。

## 恢复与运维

Goal/Loop 的失败和恢复都遵循 Continuation：

- Worker 失败：`interrupted/work`，显式 `RetryWork`；
- Evaluator/Planner 失败：`interrupted/decision`，显式 `RetryDecision`；
- 宿主取消 context：Interrupted，不是业务 Cancelled；
- 重启后由 `Get` 恢复孤儿状态，应用决定是否重试或唤醒；
- limits 命中：`limited` 终态，不再激活。

如果任务只需进程内短循环，普通 Go 控制流和 Agent limits 更简单。只有需要持久的“完成策略”或“下次激活策略”时才引入这两个包。
