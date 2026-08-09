# 核心概念

Pips 把模型调用拆成三个层次：Provider 适配器负责协议，`ai` 负责便携数据模型，应用负责业务循环、持久化和权限。理解这个边界后，切换 Provider 通常只需替换构造代码。

## 模型接口与生命周期

文本/对话模型实现：

```go
type LanguageModel interface {
	Generate(context.Context, Request) (*Response, error)
	Stream(context.Context, Request) Stream
	Provider() Provider
	ModelID() string
	Capabilities() Capabilities
}
```

图片和向量使用独立接口 `ai.ImageModel` 与 `ai.EmbeddingModel`；服务端计数能力是可选的 `ai.TokenCounter`。拆分接口可避免把“所有模型都支持所有能力”编码成错误假设。

内置 Provider 模型具有以下生命周期约定：

- 构造器只保存配置，不验证凭据，也不发起网络请求。
- 对象不可变并可并发复用；一个服务通常为每个 Provider/模型构造一次。
- 每次 `Generate` 是一次尝试；`Stream` 在开始迭代时才执行请求。
- 调用者拥有 `context`、请求数据和自定义 `http.Client` 的生命周期。
- Provider 对象不保存对话。继续对话时由应用把历史 `Message` 再次传入。

## 消息与角色

`ai.Request.Messages` 按时间从旧到新排列。常用构造器如下：

```go
messages := []ai.Message{
	ai.UserText("你好"),
	ai.AssistantText("你好，需要什么帮助？"),
	ai.User(
		ai.Text("描述这张图片"),
		ai.ImageURL("https://example.com/cat.png"),
	),
}
```

四种角色的含义：

| 角色 | 用途 |
| --- | --- |
| `ai.RoleSystem` | 历史中的系统消息；新的主系统提示优先放 `Request.System` |
| `ai.RoleUser` | 用户输入，可含文本、图片或文件 |
| `ai.RoleAssistant` | 之前的模型输出；继续对话时直接追加 `Response.Message` |
| `ai.RoleTool` | 应用执行 Tool 后返回的一个或多个结果 |

不同 Provider 会把这些角色翻译到不同位置。例如 Anthropic 和 Gemini 的系统提示不是普通历史消息。便携代码应优先使用 `Request.System`，不要依赖某家的 wire 结构。

## Part 与多模态内容

`Message.Parts` 是封闭接口，只能使用 `ai` 包提供的六种实现：

| Part | 常见方向 | 说明 |
| --- | --- | --- |
| `TextPart` | 输入/输出 | 普通文本 |
| `ImagePart` | 输入 | URL 或内联图片 |
| `FilePart` | 输入 | Provider 文件 ID、URL 或内联文档 |
| `ReasoningPart` | 输出/续接 | 推理文本及不透明续接签名 |
| `ToolCallPart` | 输出/续接 | 模型请求的 Tool 名、调用 ID、JSON 参数 |
| `ToolResultPart` | 输入 | Tool 成功或失败结果，可为多模态内容 |

媒体来源必须只选择 `ID`、`URL` 或 `Data` 中的一种。内联字节必须带 MIME 类型：

```go
msg := ai.User(
	ai.Text("总结附件"),
	ai.FileData("report.pdf", "application/pdf", pdfBytes),
)
```

便携类型不代表每个协议接受每一种来源。典型差异包括：OpenAI Chat Completions 不接受文件 URL；Gemini 文件资源通常使用 `FileURL` 保存 Files API URI，而不是 `FileID`；Provider 文件 ID 不能跨 Provider 复用。应在[Provider 指南](providers.md)确认目标协议。

## 请求参数与“未设置”

`ai.Request` 的零值适合作为最小请求。所有数值采样参数使用指针区分“未设置”和“有意设置为零”：

```go
req := ai.Request{
	Messages:         []ai.Message{ai.UserText("给出确定答案")},
	Temperature:      ai.Ptr(0.0),
	MaxTokens:        ai.Ptr(512),
	FrequencyPenalty: ai.Ptr(0.0),
}
```

可移植参数包括 `Temperature`、`TopP`、`TopK`、`Seed`、两类 penalty、`LogProbs`、`MaxTokens` 和 `Stop`，但具体协议可能拒绝其中一部分。适配器会用可由 `errors.Is` 判断的 `ai.ErrUnsupported` 或 `ai.ErrInvalidRequest` 返回本地校验错误，不会静默伪造支持。

只有没有便携表达的功能才放进 `ProviderOptions`：

```go
req.ProviderOptions = map[ai.Provider]any{
	ai.ProviderGemini: gemini.RequestOptions{
		CachedContent: "cachedContents/abc123",
	},
}
```

值必须是对应适配包公开的 options 类型。未知键或类型不匹配会被忽略。`ExtraFields` 使用有边界、只添加的递归合并；保留字段冲突会返回错误。不要用它覆盖已有便携参数，也不要放凭据。

## 响应、用量与继续对话

`ai.Response` 同时提供规范化字段和原始响应：

- `Text()` 拼接所有文本 Part。
- `Reasoning()` 拼接可见推理 Part。
- `ToolCalls()` 按顺序返回 Tool 调用。
- `FinishReason` 规范化为 `stop`、`length`、`tool_calls`、`content_filter` 或 `other`。
- `Usage` 保存输入、输出、推理、缓存命中和缓存写入 Token；Provider 未报告的字段保持零。
- `Raw` 保存 Provider 原始 JSON，供诊断未抽象字段使用。

继续对话时追加完整的 `Response.Message`，不要只把 `resp.Text()` 重建为 assistant 文本：

```go
resp, err := model.Generate(ctx, ai.Request{Messages: messages})
if err != nil {
	return err
}

messages = append(messages, resp.Message)
messages = append(messages, ai.UserText("再简短一些。"))
```

完整消息会保留 Tool 调用以及 `ReasoningPart.Signature`。签名是不透明、Provider 绑定的续接状态；发回同一 Provider 时原样保存，不要解析、记录或跨 Provider 搬运。`Redacted` 推理即使没有文本，也可能仍需签名才能正确续接。

## 能力提示

`model.Capabilities()` 返回静态、尽力而为的 `ai.Capabilities`：文本、视觉、文档、音频/视频输入、Tools、结构化输出、推理、图片生成、Embeddings、Prompt Cache 和 Token Counting。

正确用法：

- 在 UI 中隐藏明显不适用的入口。
- 在多个已知模型之间选择更合适的默认模型。
- 在发起昂贵请求前给出警告。

错误用法：

- 把 `false` 当作服务端永久不支持。
- 把 `true` 当作当前账户、区域、模型版本一定可用。
- 根据能力表在适配器外复刻 Provider 校验。

未知模型会得到保守的基线能力；Pips 不会基于能力表阻止调用。

## 图片、向量与 Token Counting

图片生成与对话模型分开构造：

```go
imageModel := openai.NewImageModel("gpt-image-1")
result, err := imageModel.GenerateImages(ctx, ai.ImageRequest{
	Prompt: "极简蓝色山脉图标",
	N:      1,
	Size:   "1024x1024",
})
```

OpenAI 与 Gemini 提供图片和 Embedding 适配器。Gemini 图片每次调用只生成一张，`N` 会被忽略。`EmbeddingResponse.Embeddings` 与输入顺序一致；`Dimensions` 只对支持降维的模型生效。

Token Counting 通过类型断言发现：

```go
counter, ok := model.(ai.TokenCounter)
if !ok {
	return errors.New("model has no server-side token counter")
}

n, err := counter.CountTokens(ctx, req)
```

Anthropic 与 Gemini 实现了服务端计数接口；OpenAI 适配器没有实现。客户端 TPM 限流在接口不可用或计数失败时会退回到文本字节数除以四的粗略估算。

## Context、并发与所有权

- 为每次外部调用设置超时或截止时间，并把上游取消信号原样传入。
- 不要把已取消的 `context` 保存到模型对象或跨请求复用。
- 多 goroutine 可以共享模型与中间件链；请求值及其切片/Map 在调用期间应视为只读。
- 自定义 Hooks 在调用 goroutine 同步执行，应保持快速；慢 Export 交给遥测 SDK 的批处理器。
- 调用方提供的 `http.Client` 会被原样使用，连接池、Proxy、TLS、超时和 SSRF 防护都由调用方负责。

## 选择 `ai` 还是 `agent`

使用 `ai`：一次生成、自己管理对话、自己编写有限 Tool 循环、只需要协议统一。

使用 `agent`：需要自主循环、Tool 注册与执行、Guardrail、Approval、事件、Session、恢复或多 Agent 协作。`agent` 建立在 `ai.LanguageModel` 之上，不是另一套 Provider 客户端。
