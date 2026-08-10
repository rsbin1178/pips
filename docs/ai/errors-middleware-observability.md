# 错误、中间件与可观测性

裸 Provider 客户端只执行一次请求。生产所需的重试、限流和观测通过 `ai.Middleware` 显式组合，因此调用次数、成本和监控口径都能在代码审查中看见。

## 错误分类

Provider 非 2xx 响应通常返回 `*ai.Error`，并包装一个跨 Provider sentinel：

| Sentinel | 典型来源 | 默认可重试 |
| --- | --- | --- |
| `ai.ErrUnsupported` | 本地协议/能力不支持 | 否 |
| `ai.ErrAuth` | HTTP 401/403 | 否 |
| `ai.ErrRateLimited` | HTTP 429 | 是 |
| `ai.ErrOverloaded` | HTTP 408、5xx、Anthropic 529 | 是 |
| `ai.ErrInvalidRequest` | 其他 4xx，例如 400/404/422 | 否 |

先用 `errors.Is` 做可移植分支，再用 `errors.As` 读取诊断字段：

```go
resp, err := model.Generate(ctx, req)
if err == nil {
	return resp, nil
}

switch {
case errors.Is(err, context.Canceled):
	return resp, err
case errors.Is(err, context.DeadlineExceeded):
	return resp, fmt.Errorf("model deadline: %w", err)
case errors.Is(err, ai.ErrAuth):
	return resp, fmt.Errorf("model credential rejected: %w", err)
case errors.Is(err, ai.ErrRateLimited):
	// 交给重试/队列策略，不要记录密钥或完整 Prompt。
}

var apiErr *ai.Error
if errors.As(err, &apiErr) {
	log.Printf("provider=%s status=%d type=%s code=%s retry_after=%s",
		apiErr.Provider,
		apiErr.StatusCode,
		apiErr.Type,
		apiErr.Code,
		apiErr.RetryAfter,
	)
}

return resp, err
```

`ai.Error` 还保存 `Message` 和 `Raw`。二者可能包含 Provider 回显、Prompt 片段或其他敏感内容，默认不要写入普通日志或指标标签。

`ai.ClassifyStatus` 可供自定义适配器复用状态映射；`ai.NewError` 构造带默认 sentinel 的结构错误，`WithSentinel` 可在 Provider 有更精确分类时覆盖。

## `IsRetryable` 的边界

`ai.IsRetryable(err)` 对 Rate Limit、Overload、状态码为 0 的结构错误以及裸 transport 错误返回 true；对取消、截止时间、Auth、Invalid Request 和 Unsupported 返回 false。

这意味着自定义 `LanguageModel` 返回的普通错误默认被视为 transport 错误并可重试。若错误实际上是永久业务校验失败，应包装合适的 sentinel：

```go
return nil, fmt.Errorf("invalid tenant model policy: %w", ai.ErrInvalidRequest)
```

“错误可重试”与“现在重放安全”是两件事。尤其在流已经输出内容、调用可能产生外部副作用或预算已耗尽时，不应仅因 `IsRetryable` 为 true 就重放。

## Retry 中间件

```go
model := ai.Chain(base, retry.New(
	retry.WithMaxAttempts(4),
	retry.WithBaseDelay(500*time.Millisecond),
	retry.WithMaxDelay(30*time.Second),
))
```

默认行为：

- 最多 3 次总尝试，包含第一次。
- Base delay 为 500 ms，指数翻倍并使用 full jitter。
- 指数 delay 在 30 s 封顶。
- `*ai.Error.RetryAfter` 若比 jitter delay 更长则优先；该 Provider 指示值可以长于 `WithMaxDelay`。
- Sleep 响应 `context` 取消与截止时间。
- 只重试 `ai.IsRetryable` 判定的错误。

流式调用只有在第一个事件出现前失败才可能重试。一旦任何事件已交给消费者，后续错误原样返回，不重放 Prompt，从而避免重复文本或 Tool Call。

`WithSleep` 和 `WithJitter` 主要用于确定性测试。生产代码通常不应去掉真实等待；否则可能形成重试风暴。

## Rate Limit 中间件

```go
limit := ratelimit.New(
	ratelimit.WithRPM(60),
	ratelimit.WithTPM(90_000),
)
model := ai.Chain(base, limit)
```

它维护两个进程内 Token Bucket：请求每分钟（RPM）和估算输入 Token 每分钟（TPM）。调用会阻塞直到两个 Bucket 都允许，等待过程服从 `context`。零或负数会关闭对应 Bucket。

TPM 计数顺序：

1. 若模型实现 `ai.TokenCounter`，调用其服务端计数方法。
2. 若接口不存在、计数失败或结果非正数，统计消息序列顶层 `TextPart` 的字节数，按约 4 bytes/token 估算，至少为 1；前置 `SystemMessage` 也在同一序列中。
3. 单个请求估算大于 Bucket burst 时会被截到 burst，避免永远无法放行。

该估算不包含图片、文件、Tool Schema 等复杂开销，不应当作账单或精确配额。Anthropic/Gemini 的服务端计数本身也是一次外部调用；为它设置的 context 与调用请求相同。

限流器只在当前进程内生效。多实例服务需要额外的集中式配额协调；不要把一个本地 Bucket 描述成全局 Provider 配额。

## 中间件顺序

`ai.Chain(base, a, b, c)` 让 `a` 成为最外层，请求方向为 `a → b → c → base`，响应反向返回。

推荐的逻辑调用口径：

```go
model := ai.Chain(base,
	observability.Middleware(hooks),
	ratelimit.New(ratelimit.WithRPM(60), ratelimit.WithTPM(90_000)),
	retry.New(retry.WithMaxAttempts(3)),
)
```

效果：

- observability 记录一次用户可见逻辑调用。
- ratelimit 为逻辑调用放行一次。
- retry 在内部最多执行三次 Provider 尝试。

如果 Provider 将每次尝试都计入 RPM，应把 rate limit 放在 retry 内侧：

```go
model := ai.Chain(base,
	observability.Middleware(logicalHooks),
	retry.New(),
	ratelimit.New(ratelimit.WithRPM(60)),
)
```

如果还要观测每次尝试，可在 retry 内侧再放一个 observability middleware。不要盲目复制推荐顺序；先定义你希望限制和统计的是“逻辑调用”还是“Provider 尝试”。

## Observability Hooks

`ai/observability` 不绑定日志、Metrics 或 Trace SDK。它提供四个可选回调：

| Hook | 时机 | 主要数据 |
| --- | --- | --- |
| `OnStart` | 底层调用前 | Provider、ModelID、Request、是否流式；可返回派生 context |
| `OnStreamEvent` | 每个成功流事件 | `CallInfo` 与 `StreamEvent` |
| `OnFinish` | 调用无错误结束 | Response/Usage、FinishReason、Duration |
| `OnError` | 同步调用或 Stream 失败 | `CallInfo` 与错误 |

示例：

```go
hooks := observability.Hooks{
	OnStart: func(ctx context.Context, info observability.CallInfo) context.Context {
		log.Printf("model.start provider=%s model=%s stream=%t",
			info.Provider, info.ModelID, info.Streaming)
		return ctx
	},
	OnFinish: func(_ context.Context, result observability.Result) {
		log.Printf("model.finish provider=%s model=%s duration=%s input=%d output=%d finish=%s",
			result.Provider,
			result.ModelID,
			result.Duration,
			result.Usage.InputTokens,
			result.Usage.OutputTokens,
			result.FinishReason,
		)
	},
	OnError: func(_ context.Context, info observability.CallInfo, err error) {
		log.Printf("model.error provider=%s model=%s err=%v",
			info.Provider, info.ModelID, err)
	},
}

model := ai.Chain(base, observability.Middleware(hooks))
```

同步 `OnFinish.Result.Response` 为完整响应；Stream 时它为 `nil`，Usage 和 FinishReason 从 `message_end` 累积。`OnStreamEvent` 在事件交给调用方前同步执行。

## Hook 生命周期注意事项

- Hooks 在调用 goroutine 同步执行。保持快速、非阻塞，并让遥测 SDK 负责有界批处理。
- `OnStart` 返回的非 nil context 会传给底层调用；返回 nil 会继续使用原 context。
- Duration 从 `OnStart` 结束后开始计算。流式 Duration 包含整个消费过程直到正常结束。
- 消费者提前 `break` 时不会产生 `OnFinish` 或 `OnError`；将其视为调用方主动停止，并从上层取消路径记录。
- Hook panic 不会被中间件恢复。应用自己的 Hook 必须自我保护，不能让遥测故障击穿业务调用。
- `CallInfo.Request` 包含完整 Prompt、媒体引用和 ProviderOptions。默认只记录元数据，敏感内容应脱敏或完全不采集。
- 不要把 Provider、模型 ID、错误 message 等无界值直接用作高基数 Metric label。

## 生产观测建议

至少记录：逻辑调用/尝试口径、Provider、配置模型 ID、实际响应模型、同步/流式、耗时、完成原因、规范化错误类、HTTP 状态以及五类 Usage。Trace 中可用 `OnStart` 返回带 Span 的 context，在 `OnFinish`/`OnError` 结束 Span。

以下内容默认不记录：API Key、Authorization Header、完整 Request/Response Raw、Reasoning Signature、用户 Prompt、文件内容和 Tool Result。若业务确需内容审计，应采用独立授权、加密、留存与删除策略。

完整组合示例见 [`examples/middleware`](../../examples/middleware/main.go)。
