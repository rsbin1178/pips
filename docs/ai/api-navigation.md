# API 导航

本页把任务映射到公开包和符号。字段、签名和最新注释以 `go doc`、pkg.go.dev 和源码为准；这里不逐字段复制 Godoc。

## 本地查阅

```sh
go doc -short github.com/rsbin1178/pips/ai
go doc github.com/rsbin1178/pips/ai.Request
go doc github.com/rsbin1178/pips/ai/openai.New
go doc -all github.com/rsbin1178/pips/ai/observability
```

查看当前模块实际使用的源码版本比在线页面更可靠，尤其在固定提交或使用 replace directive 时。

## `ai`：便携模型协议

- Godoc：[`github.com/rsbin1178/pips/ai`](https://pkg.go.dev/github.com/rsbin1178/pips/ai)
- 源码：[`ai/`](../../ai)
- 包示例：[`ai/example_test.go`](../../ai/example_test.go)

| 任务 | 主要符号 | 源码 |
| --- | --- | --- |
| 文本模型抽象 | `LanguageModel`、`Provider`、`Capabilities` | [`model.go`](../../ai/model.go)、[`provider.go`](../../ai/provider.go) |
| 图片生成 | `ImageModel`、`ImageRequest`、`ImageResponse`、`GeneratedImage` | [`image.go`](../../ai/image.go) |
| Embedding | `EmbeddingModel`、`EmbeddingRequest`、`EmbeddingResponse` | [`embedding.go`](../../ai/embedding.go) |
| 服务端 Token 计数 | `TokenCounter` | [`model.go`](../../ai/model.go) |
| 消息与 Part | `Message`、`Messages`、`SystemMessage`、`UserMessage`、`AssistantMessage`、`ToolMessage`、`Part` 及六种具体 Part | [`message.go`](../../ai/message.go) |
| 消息构造与边界 | `SystemText`、`UserText`、`AssistantText`、`ToolResultText`、`Messages.SplitSystem`、`UnmarshalMessage`、`CloneMessage`、`MessageParts` | [`content.go`](../../ai/content.go)、[`message_validate.go`](../../ai/message_validate.go)、[`message_json.go`](../../ai/message_json.go) |
| 生成请求 | `Request`、`LogProbsConfig`、`Ptr` | [`request.go`](../../ai/request.go) |
| 完整响应 | `Response`、`FinishReason`、`Usage`，以及 `Text`、`Reasoning`、`ToolCalls` 方法 | [`response.go`](../../ai/response.go) |
| 流式响应 | `Stream`、`StreamEvent`、`StreamEventType`、`Collect` | [`stream.go`](../../ai/stream.go) |
| Tool | `Tool`、`ToolChoice`、`ToolChoiceMode` | [`tool.go`](../../ai/tool.go) |
| JSON Schema | `Schema`、`ParseSchema`、`SchemaFor` | [`tool.go`](../../ai/tool.go)、[`structured.go`](../../ai/structured.go) |
| 类型化输出 | `ResponseFormat`、`GenerateTyped` | [`structured.go`](../../ai/structured.go) |
| 推理配置 | `ReasoningConfig`、`ReasoningMode`、`ReasoningEffort` | [`reasoning.go`](../../ai/reasoning.go) |
| 错误分类 | 五个 `Err*` sentinel、`Error`、`NewError`、`ClassifyStatus`、`IsRetryable` | [`errors.go`](../../ai/errors.go) |
| 中间件组合 | `Middleware`、`Chain` | [`middleware.go`](../../ai/middleware.go) |
| 请求扩展预检 | `ValidateRequestBodyExtension` | [`request_extension.go`](../../ai/request_extension.go) |

## `ai/openai`：OpenAI 原生与兼容 Wire

- Godoc：[`github.com/rsbin1178/pips/ai/openai`](https://pkg.go.dev/github.com/rsbin1178/pips/ai/openai)
- 源码：[`ai/openai/`](../../ai/openai)
- 使用说明：[Provider 指南](providers.md#openai)

| 任务 | 主要符号 |
| --- | --- |
| 构造文本模型 | `New`、`Model`、`Option` |
| 选择 API surface | `API`、`APIAuto`、`APIChatCompletions`、`APIResponses`、`WithAPI`、`ResolveAPI` |
| 认证与传输 | `WithAPIKey`、`WithBaseURL`、`WithHTTPClient`、`WithHeader`、`WithAllowHTTP`、`WithAllowPrivateIPs`、`WithMaxStreamLineSize`、`DefaultBaseURL` |
| 服务身份/能力 | `WithProvider`、`WithCapabilities` |
| 协议兼容 | `Compatibility`、`WithCompatibility`、`WithCompatMode`，以及 `MaxTokensField`、`StreamUsageMode`、`StructuredOutputMode`、`ChatReasoningFormat`、`ReasoningHistoryField` 常量族 |
| 请求扩展 | `RequestOptions`（MinP、RepetitionPenalty、ExtraFields） |
| 图片 | `NewImageModel`、`ImageModel`、`ImageOptions` |
| Embedding | `NewEmbeddingModel`、`EmbeddingModel` |

实现入口：[`openai.go`](../../ai/openai/openai.go)、[`options.go`](../../ai/openai/options.go)、[`compatibility.go`](../../ai/openai/compatibility.go)、[`chat.go`](../../ai/openai/chat.go)、[`responses.go`](../../ai/openai/responses.go)、[`image.go`](../../ai/openai/image.go)、[`embedding.go`](../../ai/openai/embedding.go)。

## `ai/openai/compat`：审阅后的兼容 Profile

- Godoc：[`github.com/rsbin1178/pips/ai/openai/compat`](https://pkg.go.dev/github.com/rsbin1178/pips/ai/openai/compat)
- 源码：[`compat.go`](../../ai/openai/compat/compat.go)
- 使用说明：[OpenAI 兼容服务](providers.md#openai-兼容服务)

| 任务 | 主要符号 |
| --- | --- |
| 命名 Profile | `DeepSeek`、`Groq`、`XAI`、`OpenRouter`、`Cerebras`、`Together`、`Mistral` |
| 构造自定义 Profile | `Profile`、`New` |
| 构建 Provider 注册表 | `Lookup` |

Profile 复用 `openai.Model`，但保持真实 `Provider`、凭据环境变量、协议、能力和 wire 差异。Native Bedrock Converse、Vertex AI 等不是 OpenAI compatibility profile。

## `ai/anthropic`：Messages API

- Godoc：[`github.com/rsbin1178/pips/ai/anthropic`](https://pkg.go.dev/github.com/rsbin1178/pips/ai/anthropic)
- 源码：[`ai/anthropic/`](../../ai/anthropic)
- 使用说明：[Anthropic](providers.md#anthropic)

| 任务 | 主要符号 |
| --- | --- |
| 构造模型 | `New`、`Model`、`Option` |
| 认证与传输 | `WithAPIKey`、`WithBaseURL`、`WithHTTPClient`、`WithHeader`、`WithAllowHTTP`、`WithAllowPrivateIPs`、`WithMaxStreamLineSize`、`DefaultBaseURL` |
| Anthropic Header | `WithAPIVersion`、`WithBeta` |
| 默认输出限制 | `WithMaxTokens` |
| 服务身份 | `WithProvider` |
| Prompt Cache / 扩展 | `RequestOptions`、`CacheTTL`、`CacheTTL5Minutes`、`CacheTTL1Hour` |
| Token Counting | `Model.CountTokens`（实现 `ai.TokenCounter`） |

主要实现：[`anthropic.go`](../../ai/anthropic/anthropic.go)、[`convert.go`](../../ai/anthropic/convert.go)、[`messages.go`](../../ai/anthropic/messages.go)、[`stream.go`](../../ai/anthropic/stream.go)、[`count_tokens.go`](../../ai/anthropic/count_tokens.go)。

## `ai/gemini`：Gemini API

- Godoc：[`github.com/rsbin1178/pips/ai/gemini`](https://pkg.go.dev/github.com/rsbin1178/pips/ai/gemini)
- 源码：[`ai/gemini/`](../../ai/gemini)
- 使用说明：[Gemini](providers.md#gemini)

| 任务 | 主要符号 |
| --- | --- |
| 构造文本模型 | `New`、`Model`、`Option` |
| 认证与传输 | `WithAPIKey`、`WithBaseURL`、`WithHTTPClient`、`WithHeader`、`WithAllowHTTP`、`WithAllowPrivateIPs`、`WithMaxStreamLineSize`、`DefaultBaseURL` |
| 服务身份 | `WithProvider` |
| 显式缓存/请求扩展 | `RequestOptions` |
| 图片 | `NewImageModel`、`ImageModel`、`ImageOptions` |
| Embedding | `NewEmbeddingModel`、`EmbeddingModel` |
| Token Counting | `Model.CountTokens`（实现 `ai.TokenCounter`） |

主要实现：[`gemini.go`](../../ai/gemini/gemini.go)、[`convert.go`](../../ai/gemini/convert.go)、[`generate.go`](../../ai/gemini/generate.go)、[`stream.go`](../../ai/gemini/stream.go)、[`count_tokens.go`](../../ai/gemini/count_tokens.go)、[`image.go`](../../ai/gemini/image.go)、[`embedding.go`](../../ai/gemini/embedding.go)。

## `ai/middleware/retry`

- Godoc：[`github.com/rsbin1178/pips/ai/middleware/retry`](https://pkg.go.dev/github.com/rsbin1178/pips/ai/middleware/retry)
- 源码：[`retry.go`](../../ai/middleware/retry/retry.go)
- 使用说明：[Retry 中间件](errors-middleware-observability.md#retry-中间件)

公开入口：`New`、`Option`、`WithMaxAttempts`、`WithBaseDelay`、`WithMaxDelay`、`WithSleep`、`WithJitter`。`WithSleep` 与 `WithJitter` 主要用于确定性测试。

## `ai/middleware/ratelimit`

- Godoc：[`github.com/rsbin1178/pips/ai/middleware/ratelimit`](https://pkg.go.dev/github.com/rsbin1178/pips/ai/middleware/ratelimit)
- 源码：[`ratelimit.go`](../../ai/middleware/ratelimit/ratelimit.go)
- 使用说明：[Rate Limit 中间件](errors-middleware-observability.md#rate-limit-中间件)

公开入口：`New`、`Option`、`WithRPM`、`WithTPM`。这是进程内客户端限流，不是分布式配额服务。

## `ai/observability`

- Godoc：[`github.com/rsbin1178/pips/ai/observability`](https://pkg.go.dev/github.com/rsbin1178/pips/ai/observability)
- 源码：[`hooks.go`](../../ai/observability/hooks.go)
- 使用说明：[Observability Hooks](errors-middleware-observability.md#observability-hooks)

公开入口：`Middleware`、`Hooks`、`CallInfo`、`Result`。包只发出同步 callbacks，不创建或管理任何 telemetry backend。

## 示例索引

| 场景 | 示例 |
| --- | --- |
| 切换 Provider | [`examples/provider-switch`](../../examples/provider-switch/main.go) |
| OpenAI 流式文本 | [`examples/text-stream-openai`](../../examples/text-stream-openai/main.go) |
| Anthropic 流式文本 | [`examples/text-stream-anthropic`](../../examples/text-stream-anthropic/main.go) |
| Gemini 流式文本 | [`examples/text-stream-gemini`](../../examples/text-stream-gemini/main.go) |
| 多模态视觉输入 | [`examples/vision`](../../examples/vision/main.go) |
| Tool 循环 | [`examples/tools`](../../examples/tools/main.go) |
| 类型化结构输出 | [`examples/structured`](../../examples/structured/main.go) |
| Retry、限流和 Hooks | [`examples/middleware`](../../examples/middleware/main.go) |
| 图片生成 | [`examples/image-gen`](../../examples/image-gen/main.go) |

## 非公开边界

以下包虽然会出现在 `go list ./ai/...`，但不是公共使用面：`ai/internal/httpx`、`ai/internal/jsonx`、`ai/internal/sse`。其他 `ai/internal` 目录也遵守相同边界。外部应用不得导入它们；需要新增通用能力时应先通过公开 API 设计，而不是复制 internal 类型。
