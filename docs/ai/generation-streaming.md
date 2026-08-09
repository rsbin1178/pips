# 生成与流式响应

`Generate` 和 `Stream` 接受相同的 `ai.Request`，但错误、资源释放和结果组装方式不同。本页给出可直接放进服务层的处理模式。

## 同步生成

同步调用适合短响应、后台任务和必须拿到完整结构后才能继续的流程：

```go
func answer(ctx context.Context, model ai.LanguageModel, question string) (*ai.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	resp, err := model.Generate(ctx, ai.Request{
		System:      "回答必须简洁、准确。",
		Messages:    []ai.Message{ai.UserText(question)},
		Temperature: ai.Ptr(0.2),
		MaxTokens:  ai.Ptr(500),
	})
	if err != nil {
		return resp, fmt.Errorf("generate: %w", err)
	}

	return resp, nil
}
```

错误时 `resp` 可能为 `nil`，也可能包含 Provider 已返回的部分信息；调用方不要假定错误与响应互斥。使用 `%w` 保留错误链，后续才能用 `errors.Is` 和 `errors.As` 分类。

## 流是惰性的

`model.Stream(ctx, req)` 只创建一个 `iter.Seq2`。连接、配置或认证错误会在第一次迭代时出现：

```go
stream := model.Stream(ctx, req)

for ev, err := range stream {
	if err != nil {
		return fmt.Errorf("stream: %w", err)
	}

	// 处理 ev
}
```

因此以下写法是错误的，它既没有发送请求，也无法发现失败：

```go
_ = model.Stream(ctx, req)
```

## 事件协议

正常流按以下生命周期产生事件：

1. 一个 `message_start`，包含 Provider、响应 ID 和实际模型。
2. 任意交错的文本、推理和 Tool Call 事件。
3. 一个 `message_end`，包含规范化完成原因和可选 Usage。

| `StreamEvent.Type` | 有效字段 |
| --- | --- |
| `StreamMessageStart` | `Provider`、`ID`、`Model` |
| `StreamTextDelta` | `Text` |
| `StreamReasoningDelta` | `Text`、可选 `Signature` |
| `StreamToolCallStart` | `ToolCallIndex`、`ToolCallID`、`ToolCallName` |
| `StreamToolCallDelta` | `ToolCallIndex`、`ArgsDelta` |
| `StreamToolCallEnd` | `ToolCallIndex` |
| `StreamMessageEnd` | `FinishReason`、可选 `Usage` |

只读取当前事件类型定义的字段。多个 Tool Call 可交错传输，必须按 `ToolCallIndex` 分组，不能只维护一个全局参数缓冲区。

## 边输出边收集元数据

下面的完整函数打印文本，同时返回最终 Usage 与完成原因：

```go
func printStream(ctx context.Context, model ai.LanguageModel, req ai.Request) (ai.Usage, ai.FinishReason, error) {
	var usage ai.Usage
	var finish ai.FinishReason

	for ev, err := range model.Stream(ctx, req) {
		if err != nil {
			return usage, finish, err
		}

		switch ev.Type {
		case ai.StreamTextDelta:
			fmt.Print(ev.Text)
		case ai.StreamMessageEnd:
			finish = ev.FinishReason
			if ev.Usage != nil {
				usage = *ev.Usage
			}
		}
	}

	return usage, finish, nil
}
```

不同 Provider 可能不报告 Usage，此时 `ev.Usage` 为 `nil`，而不是全零值指针。调用代码必须处理两种情况。

## 用 `Collect` 组装完整响应

不需要实时渲染时，`ai.Collect` 会正确组合文本、推理、Tool Call 参数、Usage 和完成原因：

```go
resp, err := ai.Collect(model.Stream(ctx, req))
if err != nil {
	// resp 是故障发生前已收到的部分响应。
	return resp, fmt.Errorf("collect: %w", err)
}
```

中途失败时，`Collect` 返回部分 `Response` 和错误。这可用于诊断或向用户标记不完整输出，但不能把部分内容当作成功结果，也不能把它静默加入后续对话。

若要同时实时显示和得到完整响应，当前 API 不提供复制 Stream 的辅助器。应用可自己按事件累积，或把显示逻辑放在 `observability.OnStreamEvent` 中并在结束后另行管理结果。不要对同一个 Stream 迭代两次。

## 提前停止与取消

提前退出 `range` 会取消该流的底层请求并释放连接：

```go
for ev, err := range model.Stream(ctx, req) {
	if err != nil {
		return err
	}
	if ev.Type == ai.StreamTextDelta {
		fmt.Print(ev.Text)
		if userStopped() {
			break
		}
	}
}
```

还应从上游传递 `context`。取消或截止时间错误可用 `errors.Is(err, context.Canceled)` 和 `errors.Is(err, context.DeadlineExceeded)` 判断，且不会被重试中间件再次尝试。

不要在消费 Stream 的 goroutine 之外复用其迭代器，也不要把流放进无人消费的通道。拥有迭代循环的代码同时拥有停止与错误处理责任。

## 推理内容与续接

推理模型可能发出 `StreamReasoningDelta`。`Text` 是可见内容，`Signature` 是不透明的 Provider 状态；签名可能出现在空文本事件中。`ai.Collect` 会把它们组合成 `ReasoningPart`。

继续同一个 Provider 的对话时，追加收集后的完整 `resp.Message`：

```go
messages = append(messages, resp.Message)
```

不要只保存 `resp.Reasoning()` 或自己拼接 `ReasoningPart`，否则可能丢失加密/签名状态。不要向终端用户展示或记录签名。

## Reasoning 配置

便携配置将“是否启用”“投入程度”“显式 Token 预算”分开：

```go
req.Reasoning = &ai.ReasoningConfig{
	Mode:           ai.ReasoningModeEnabled,
	Effort:         ai.ReasoningHigh,
	IncludeSummary: true,
}
```

支持的努力级别为 `none`、`minimal`、`low`、`medium`、`high`、`xhigh`、`max`；支持的模式为 `auto`、`enabled`、`adaptive`、`disabled`。这些是便携意图，不是跨 Provider 的逐项等价承诺：

- OpenAI Responses 支持 effort 和可选 summary，但不支持显式 `BudgetTokens` 或 adaptive。
- OpenAI Chat/兼容端点由 compatibility 配置决定 reasoning wire 形态，不接受 budget 或 summary。
- Anthropic 可映射 effort、显式预算与 adaptive，但会拒绝无效组合。
- Gemini 支持部分 effort/预算映射，不支持 adaptive；超出其支持集合且无显式预算时会返回 `ErrUnsupported`。

在通用代码中优先使用 `Effort`；需要 Provider 特定预算时再分支。

## 用量与成本记账

同步响应读取 `resp.Usage`，流式响应在 `message_end` 读取。五个规范化字段为：

- `InputTokens`
- `OutputTokens`
- `ReasoningTokens`
- `CachedInputTokens`
- `CacheWriteTokens`

零可能表示真实为零，也可能表示 Provider 未报告。计费与审计系统应同时保存 Provider、实际模型、调用类型和原始请求 ID，不应仅凭某一个 Token 字段推断账单。

## 并发与背压

模型实例可并发调用，但每个 Stream 应由一个消费者顺序迭代。事件 Hook 与 `yield` 都在调用路径同步执行：消费过慢会形成背压，并可能占用连接更久。将耗时的持久化或遥测导出交给有界队列或后端 SDK，避免在事件循环中执行无界阻塞。

## 常见错误

- 忘记在循环里判断 `err`，把零值事件当成真实内容。
- 只等待文本 Delta，不处理 `message_end`，因此丢失 Usage 和完成原因。
- 流中途失败后自动重放整个请求，导致用户看到重复内容。内置 retry 只允许首个事件前重试。
- 提前退出后继续持有或再次迭代同一个 Stream。
- 把部分响应标记为成功，或用部分 assistant 消息继续 Tool/Reasoning 对话。

可运行示例见 [`text-stream-openai`](../../examples/text-stream-openai/main.go)、[`text-stream-anthropic`](../../examples/text-stream-anthropic/main.go) 和 [`text-stream-gemini`](../../examples/text-stream-gemini/main.go)。
