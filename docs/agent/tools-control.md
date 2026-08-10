# 工具与控制

工具把模型决策连接到应用能力。`agent` 负责声明、调用协议、结果回填和运行内控制；宿主应用负责授权、凭据、幂等性、业务事务与审计。

## 定义类型化工具

多数工具可用 `agent.NewTool` 定义：

```go
lookup := agent.NewTool(
	"lookup_order",
	"按订单号查询只读状态。",
	func(ctx context.Context, args struct {
		OrderID string `json:"order_id" description:"订单号"`
	}) (string, error) {
		return orders.Lookup(ctx, args.OrderID)
	},
)
```

参数结构体通过 `ai.SchemaFor` 转为 JSON Schema。使用 `json` tag 指定字段名，使用 `description` tag 给模型说明字段。无参数工具使用 `struct{}`。

返回图片等多模态内容时，使用 `agent.NewToolParts` 返回 `[]ai.Part`。需要动态 Schema 时，直接实现：

```go
type Tool interface {
	Decl() ai.Tool
	Exec(context.Context, agent.ToolCall) ([]ai.Part, error)
}
```

`Decl` 的名称必须非空且在一个 Agent 中唯一。工具实现应把 `ToolCall.Args` 当作不可信输入，即使上层已经生成 Schema 也要执行领域校验。

## 工具错误不是 Run 错误

以下失败都会形成 `IsError` 工具结果并返回给模型：

- `Exec` 返回错误；
- 工具 panic；
- 参数 JSON 无法解码；
- 模型请求了未知工具；
- 达到 `WithToolTimeout`；
- before-tool gate 拒绝调用。

运行时会为批次中每个调用生成结果，保持 Session 可恢复。模型可以根据错误修正参数或换一种做法。模型调用、guardrail 或基础设施错误才会作为 Run 错误返回。

不要把敏感内部错误原样暴露给模型。工具应返回稳定、可操作但经过脱敏的错误；详细错误写入应用日志，并用 `RunID`、工具调用 ID 关联。

## 串行与并行

工具默认串行执行。只有确认实现可并发安全且调用彼此独立时，才使用 `agent.Parallel`：

```go
agent.WithTools(
	agent.Parallel(readProfile),
	agent.Parallel(readInventory),
	chargeCard, // 串行工具形成批次屏障
)
```

同一轮中标记为并发安全的相邻调用可以并行，默认并发上限为 4；串行工具是屏障。不要仅凭 MCP annotation 或“只读”名称自动标记并行，必须独立核验实现、限流和下游幂等性。

## 进度更新

长工具可以通过执行上下文发布尽力而为的更新：

```go
agent.ReportProgress(ctx, ai.TextPart{Text: "已处理 50%"})
```

观察者收到 `ToolUpdated` payload。更新可能因消费者过慢而丢弃，在活动运行之外调用是 no-op，因此它不能承担持久 checkpoint 或正确性协议。

## 调用前 gate

`WithBeforeTool` 为每个调用提供串行决策点。零值表示允许：

```go
a, err := agent.New(model,
	agent.WithTools(deploy),
	agent.WithBeforeTool(func(
		_ context.Context,
		info agent.ToolCallInfo,
	) agent.ToolDecision {
		if info.Name != "deploy" {
			return agent.ToolDecision{}
		}
		return agent.ToolDecision{Action: agent.ToolDecisionPause}
	}),
)
```

可用动作：

- `ToolDecisionAllow`：执行调用。
- `ToolDecisionDeny`：不执行，把 `Reason` 作为错误工具结果送回模型。可用 `agent.DenyTool(reason)` 简写。
- `ToolDecisionPause`：当前调用及同批后续调用进入 pending，Run 以 `StopPaused` 返回。

`ToolCallInfo` 还包含从 1 开始的 `Turn`、从 0 开始的 `BatchIndex` 和 `BatchSize`，便于识别一个模型响应中的组合调用。`UpdatedInput` 可以替换 JSON 参数；每个后续 gate 和执行器看到替换后的值，应用必须重新验证。

## 持久审批与恢复

暂停后，通过调用 ID 解析待处理项：

```go
result, err := a.Run(ctx, sess, ai.UserText("部署生产环境"))
if err != nil {
	return err
}
if result.Stop != agent.StopPaused {
	return nil
}

pending := sess.Pending()
resolution := agent.ToolResolution{
	ToolCallID: pending[0].ID,
	Content:    agent.TextResult("操作员已拒绝"),
	IsError:    true,
}
if err := sess.ResolveToolCalls(resolution); err != nil {
	return err
}

result, err = a.Run(ctx, sess)
```

`ResolveToolCalls` 会先原子校验整个输入，再按原始调用顺序提交。未提供的调用继续 pending，并可随 Session JSON 或 Harness 保存。所有 pending 解析前，新 Run 会被拒绝。

`ResolvePending` 适合由一个回调处理完整批次。回调在 Session 锁外执行，回调错误转为错误工具结果。审批决定本身若需要独立审计，应写入业务审计存储；Harness 的 `AppendCustom` 可保存不进入模型上下文的会话内记录。

## 调用后 hook

`WithAfterTool` 只处理真正执行过的调用，不处理未知、拒绝或暂停的调用。它可以返回 `ToolResultOverride` 完整替换内容、错误标志或停止提示；nil 表示保留结果。

适合的用途包括结果脱敏、统一错误映射和审计标签。不适合在这里偷偷重复副作用。hook panic 会被恢复并转换成错误工具结果。

## 输入、候选答案与输出 guardrail

三类控制点解决不同问题：

| 控制点 | 时机 | 适用场景 |
| --- | --- | --- |
| `WithInputGuardrail` | `RunStarted` 后、输入提交和模型 I/O 前 | 租户策略、输入大小、禁止内容 |
| `WithOutputGuardrail` | 无工具候选完整形成后、提交前 | 声明、格式、合规验证 |
| `WithCandidateAnswer` | 输出 guardrail 通过后、提交前 | 接受候选，或让模型基于反馈重试 |

Guardrail 返回错误会中止 Run。输出 guardrail 先于候选 hook 执行；候选 hook 返回 Retry 会丢弃候选，可携带一次性的模型请求更新，已消费 token 不回退。流式 `ModelStreamEvent` 在验证前已经产生，敏感场景应缓冲展示。

副作用授权必须放在 before-tool gate 或工具自身的业务边界，而不是输出 guardrail；最终文本验证无法撤销已经发生的外部操作。

## Catalog 与最小权限工具快照

`agent/catalog` 在应用组合根为工具添加来源、风险和租户策略。它不会发现磁盘脚本，也不会替应用制定权限。

```go
entries := catalog.Local("orders", catalog.RiskRead, lookup)
cat, err := catalog.New(entries...)
if err != nil {
	return err
}

policy := catalog.Policy{
	TenantID:  "tenant-a",
	Allowlist: []string{"lookup_order"},
	MaxRisk:   catalog.RiskRead,
}
tools, err := cat.Snapshot(ctx, policy)
if err != nil {
	return err
}

a, err := agent.New(model, agent.WithTools(tools...))
```

Policy 默认拒绝：空 allowlist 不暴露任何工具，`MaxRisk` 零值只允许 `RiskRead`。可信单租户应用可以显式使用 `catalog.AllowAll`。`Authorizer` 在静态 allowlist 和风险检查通过后执行，适合追加租户级决定。

来源辅助函数包括 `Local`、`MCP`、`Extension` 和 `Team`。`Merge` 保留顺序和元数据，任何重名都会失败。

## 延迟工具发现

当工具很多时，`catalog.ToolSearch` 可以先只暴露直接工具和 `tool_search`，在下一轮加载已搜索并重新授权的精确工具：

```go
search, err := catalog.NewToolSearch(cat, policy, catalog.ToolSearchOptions{
	Enabled: true,
	Limit:   8,
})
if err != nil {
	return err
}
opts, err := search.AgentOptions(ctx)
if err != nil {
	return err
}
a, err := agent.New(model, opts...)
```

默认延迟 MCP 与 Extension 来源，本地和 Team 工具保持直接可见；`DeferredSources` 可覆盖。关闭 `Enabled` 时所有已授权工具直接可见。宿主在 `RunCompleted` 后可调用 `Forget(runID)` 提前清除运行选择；内部还有有界的运行记录清理。

工具搜索只是模型上下文优化，不是授权绕过：搜索结果在展示和应用到下一轮时都会重新检查 Policy。

## 子 Agent 工具

`agent.AsTool(child, name, description)` 把一个 Agent 暴露给管理者。每次调用使用新子 Session，取消和父运行身份会传播。它适合短期、无状态、由管理者综合结果的委托。

子 Agent 暂停会成为 `ErrSubagentPaused`，因为简单工具返回值无法保存审批句柄。需要持久审批、共享草稿、工作区或跨重启恢复时，应由应用显式管理子 Session 和外部资源；这些不是 `AsTool` 自动提供的能力。
