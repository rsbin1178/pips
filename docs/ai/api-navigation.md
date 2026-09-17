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
| 能力声明覆盖 | `CapabilityOverride`、`CapabilityDeclaration`，方法 `Apply`、`Overlay`、`Clone`、`IsZero`、`Declarations` | [`capability.go`](../../ai/capability.go) |
| 图片生成 | `ImageModel`、`ImageRequest`、`ImageUsage`、`ImageResponse`、`GeneratedImage` | [`image.go`](../../ai/image.go) |
| 图片编辑/变体/流式 | `ImageEditRequest`、`ImageVariationRequest`、`ImageEditor`、`ImageVariator`、`ImageStreamer`、`ImageStream`、`ImageStreamEvent`、`ImageStreamEventType` | [`image.go`](../../ai/image.go)、[`model.go`](../../ai/model.go) |
| Embedding | `EmbeddingModel`、`EmbeddingRequest`、`EmbeddingResponse`、`EmbeddingTaskType`、`EmbeddingEncodingFormat` | [`embedding.go`](../../ai/embedding.go) |
| Rerank | `RerankModel`、`RerankRequest`、`RerankResponse`、`RerankResult` | [`rerank.go`](../../ai/rerank.go)、[`model.go`](../../ai/model.go) |
| 服务端 Token 计数 | `TokenCounter` | [`model.go`](../../ai/model.go) |
| 消息与 Part | `Message`、`Messages`、`SystemMessage`、`UserMessage`、`AssistantMessage`、`ToolMessage`、`Part` 及六种具体 Part | [`message.go`](../../ai/message.go) |
| 消息构造与边界 | `SystemText`、`UserText`、`AssistantText`、`ToolResultText`、`Messages.SplitSystem`、`UnmarshalMessage`、`CloneMessage`、`MessageParts` | [`content.go`](../../ai/content.go)、[`message_validate.go`](../../ai/message_validate.go)、[`message_json.go`](../../ai/message_json.go) |
| 生成请求 | `Request`、`LogProbsConfig`、`Ptr` | [`request.go`](../../ai/request.go) |
| 完整响应 | `Response`（含 `Warnings`）、`FinishReason`、`Usage`，以及 `Text`、`Reasoning`、`ToolCalls` 方法 | [`response.go`](../../ai/response.go) |
| 兼容性告警 | `Warning`、`WarningType`（`unsupported`/`compatibility`/`deprecated`） | [`warning.go`](../../ai/warning.go) |
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
| 图片 | `NewImageModel`、`ImageModel`、`ImageOptions`、`EditEncoding`（`EditEncodingAuto`/`EditEncodingMultipart`/`EditEncodingJSON`）；实现 `ai.ImageEditor`、`ai.ImageVariator`、`ai.ImageStreamer` |
| Embedding | `NewEmbeddingModel`、`EmbeddingModel` |

实现入口：[`openai.go`](../../ai/openai/openai.go)、[`options.go`](../../ai/openai/options.go)、[`compatibility.go`](../../ai/openai/compatibility.go)、[`chat.go`](../../ai/openai/chat.go)、[`responses.go`](../../ai/openai/responses.go)、[`image.go`](../../ai/openai/image.go)、[`image_edit.go`](../../ai/openai/image_edit.go)、[`image_variations.go`](../../ai/openai/image_variations.go)、[`image_stream.go`](../../ai/openai/image_stream.go)、[`embedding.go`](../../ai/openai/embedding.go)。

## `ai/openai/compat`：审阅后的兼容 Profile（通用注册表 + 向后兼容）

- Godoc：[`github.com/rsbin1178/pips/ai/openai/compat`](https://pkg.go.dev/github.com/rsbin1178/pips/ai/openai/compat)
- 源码：[`compat.go`](../../ai/openai/compat/compat.go)
- 使用说明：[OpenAI 兼容服务](providers.md#openai-兼容服务)

| 任务 | 主要符号 |
| --- | --- |
| 构建 Provider 注册表 | `Providers`（全部已审阅 provider，稳定排序）、`Lookup` |
| 构造自定义 Profile | `Profile`、`New`、`Embedding` |
| 向后兼容的命名构造函数 | `DeepSeek`、`Groq`、`XAI`、`OpenRouter`、`Cerebras`、`Together`、`Mistral`、`Zhipu`、`SiliconFlow`、`Kimi`、`Qwen`、`MiniMax`（已标记 Deprecated） |

Profile 复用 `openai.Model`，但保持真实 `Provider`、凭据环境变量、协议、能力和 wire 差异。Native Bedrock Converse、Vertex AI 等不是 OpenAI compatibility profile。

`Providers` 与 `Lookup` 读取同一份审阅注册表，因此应用（例如 Coding Agent 的内置 provider 目录）应通过 `Providers` 派生内置列表，而不是复制一份 provider 名单。命名构造函数保留以兼容旧代码，新代码请优先使用各厂商独立门面包：`ai/deepseek`、`ai/groq`、`ai/xai`、`ai/openrouter`、`ai/cerebras`、`ai/together`、`ai/mistral`、`ai/zhipu`、`ai/siliconflow`、`ai/kimi`、`ai/qwen`、`ai/minimax`。

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

## `ai/agnes`：Agnes 图片 API

- Godoc：[`github.com/rsbin1178/pips/ai/agnes`](https://pkg.go.dev/github.com/rsbin1178/pips/ai/agnes)
- 源码：[`ai/agnes/`](../../ai/agnes)
- 使用说明：[Agnes](providers.md#agnes)

| 任务 | 主要符号 |
| --- | --- |
| 构造图片模型 | `NewImageModel`、`ImageModel`、`Option` |
| 站点与传输 | `BaseURLCN`、`BaseURLGlobal`、`DefaultBaseURL`、`WithAPIKey`、`WithBaseURL`、`WithHTTPClient`、`WithHeader`、`WithAllowHTTP`、`WithAllowPrivateIPs`、`WithMaxStreamLineSize`、`WithCapabilities` |
| 请求扩展 | `ImageOptions`（Ratio、ResponseFormat、ReturnBase64、ExtraFields） |
| 能力 | 实现 `ai.ImageModel` 与 `ai.ImageEditor`（无流式与变体） |

主要实现：[`agnes.go`](../../ai/agnes/agnes.go)、[`wire.go`](../../ai/agnes/wire.go)、[`options.go`](../../ai/agnes/options.go)、[`image.go`](../../ai/agnes/image.go)。

## `ai/cohere`：Cohere 与兼容重排 Wire

- Godoc：[`github.com/rsbin1178/pips/ai/cohere`](https://pkg.go.dev/github.com/rsbin1178/pips/ai/cohere)
- 源码：[`ai/cohere/`](../../ai/cohere)
- 使用说明：[重排模型指南](rerank.md)

| 任务 | 主要符号 |
| --- | --- |
| 构造重排模型 | `NewRerankModel`、`RerankModel`、`Option` |
| 认证与传输 | `WithAPIKey`、`WithBaseURL`、`WithHTTPClient`、`WithHeader`、`WithAllowHTTP`、`WithAllowPrivateIPs`、`DefaultBaseURL` |
| 厂商身份/能力 | `WithProvider`、`WithCapabilities` |
| 请求扩展 | `RerankOptions`（MaxTokensPerDoc、RankFields、Priority、ExtraFields） |
| 预设兼容构造器 | `SiliconFlowRerank`、`JinaRerank`、`TogetherRerank`、`BaseURLSiliconFlow`、`BaseURLJina`、`BaseURLTogether` |

主要实现：[`cohere.go`](../../ai/cohere/cohere.go)、[`options.go`](../../ai/cohere/options.go)、[`rerank.go`](../../ai/cohere/rerank.go)、[`error.go`](../../ai/cohere/error.go)、[`compat.go`](../../ai/cohere/compat.go)。

## `ai/deepseek`

- Godoc：[`github.com/rsbin1178/pips/ai/deepseek`](https://pkg.go.dev/github.com/rsbin1178/pips/ai/deepseek)
- 源码：[`ai/deepseek/`](../../ai/deepseek)

DeepSeek 官方模型门面，原生支持 OpenAI 协议与 Anthropic Messages 协议双端点：
| 任务 | 主要符号 |
| --- | --- |
| OpenAI 协议对话/推理 | `New(model, opts...)`（如 `deepseek-flash`, `deepseek-v4-pro`） |
| Anthropic 协议对话/推理 | `NewAnthropic(model, opts...)`（连接 `https://api.deepseek.com/anthropic`） |
| 选项配置 | `WithAPIKey`、`WithBaseURL`、`WithHTTPClient`、`WithHeader`、`WithAllowHTTP`、`WithAllowPrivateIPs`、`DefaultBaseURL`、`DefaultAnthropicBaseURL` |

## `ai/groq`

- Godoc：[`github.com/rsbin1178/pips/ai/groq`](https://pkg.go.dev/github.com/rsbin1178/pips/ai/groq)
- 源码：[`ai/groq/`](../../ai/groq)

Groq 超高速推理门面，原生支持自动能力识别与 Reasoning Format 扩展：
| 任务 | 主要符号 |
| --- | --- |
| 对话模型 | `New(model, opts...)` |
| 推理格式选项 | `RequestOptions(reasoningFormat)`、`ReasoningFormatParsed`、`ReasoningFormatRaw`、`ReasoningFormatHidden` |
| 选项配置 | `WithAPIKey`、`WithBaseURL`、`WithHTTPClient`、`WithHeader`、`WithAllowHTTP`、`WithAllowPrivateIPs`、`DefaultBaseURL` |

## `ai/mistral`

- Godoc：[`github.com/rsbin1178/pips/ai/mistral`](https://pkg.go.dev/github.com/rsbin1178/pips/ai/mistral)
- 源码：[`ai/mistral/`](../../ai/mistral)

Mistral AI 专属门面：
| 任务 | 主要符号 |
| --- | --- |
| 对话模型 | `New(model, opts...)` |
| 向量嵌入 | `NewEmbeddingModel(model, opts...)` |
| 选项配置 | `WithAPIKey`、`WithBaseURL`、`WithHTTPClient`、`WithHeader`、`WithAllowHTTP`、`WithAllowPrivateIPs`、`DefaultBaseURL` |

## `ai/xai`

- Godoc：[`github.com/rsbin1178/pips/ai/xai`](https://pkg.go.dev/github.com/rsbin1178/pips/ai/xai)
- 源码：[`ai/xai/`](../../ai/xai)

xAI (Grok) 专属门面，支持 Responses API 与 Chat Completions 自由切换：
| 任务 | 主要符号 |
| --- | --- |
| 对话/推理模型 | `New(model, opts...)`（默认 Responses API） |
| 选项配置 | `WithAPI`（`openai.APIResponses` / `openai.APIChatCompletions`）、`WithAPIKey`、`WithBaseURL`、`WithHTTPClient`、`WithHeader`、`WithAllowHTTP`、`WithAllowPrivateIPs`、`DefaultBaseURL` |

## `ai/cerebras`

- Godoc：[`github.com/rsbin1178/pips/ai/cerebras`](https://pkg.go.dev/github.com/rsbin1178/pips/ai/cerebras)
- 源码：[`ai/cerebras/`](../../ai/cerebras)

Cerebras 专属门面：
| 任务 | 主要符号 |
| --- | --- |
| 对话模型 | `New(model, opts...)` |
| 选项配置 | `WithAPIKey`、`WithBaseURL`、`WithHTTPClient`、`WithHeader`、`WithAllowHTTP`、`WithAllowPrivateIPs`、`DefaultBaseURL` |

## `ai/zhipu`

- Godoc：[`github.com/rsbin1178/pips/ai/zhipu`](https://pkg.go.dev/github.com/rsbin1178/pips/ai/zhipu)
- 源码：[`ai/zhipu/`](../../ai/zhipu)
- 使用说明：[Provider 指南](providers.md)、[重排模型指南](rerank.md)

智谱 AI 专属厂商门面，统一提供 GLM 对话、CogView 文生图、文本嵌入与文本重排：
| 任务 | 主要符号 |
| --- | --- |
| 对话模型 | `New(model, opts...)` |
| 图片生成 | `NewImageModel(model, opts...)`（CogView-4 / CogView-3-Plus） |
| 向量嵌入 | `NewEmbeddingModel(model, opts...)` |
| 文本重排 | `NewRerankModel(model, opts...)`、`DefaultRerankModel` |
| 选项配置 | `WithAPIKey`、`WithBaseURL`、`WithHTTPClient`、`WithHeader`、`WithAllowHTTP`、`WithAllowPrivateIPs`、`DefaultBaseURL` |

## `ai/together`

- Godoc：[`github.com/rsbin1178/pips/ai/together`](https://pkg.go.dev/github.com/rsbin1178/pips/ai/together)
- 源码：[`ai/together/`](../../ai/together)
- 使用说明：[Provider 指南](providers.md)、[重排模型指南](rerank.md)

Together AI 专属厂商门面，统一提供对话、FLUX/SD 文生图、文本嵌入与文本重排：
| 任务 | 主要符号 |
| --- | --- |
| 对话模型 | `New(model, opts...)` |
| 图片生成 | `NewImageModel(model, opts...)`（FLUX.1、Stable Diffusion 等） |
| 向量嵌入 | `NewEmbeddingModel(model, opts...)` |
| 文本重排 | `NewRerankModel(model, opts...)` |
| 选项配置 | `WithAPIKey`、`WithBaseURL`、`WithHTTPClient`、`WithHeader`、`WithAllowHTTP`、`WithAllowPrivateIPs`、`DefaultBaseURL` |

## `ai/siliconflow`

- Godoc：[`github.com/rsbin1178/pips/ai/siliconflow`](https://pkg.go.dev/github.com/rsbin1178/pips/ai/siliconflow)
- 源码：[`ai/siliconflow/`](../../ai/siliconflow)
- 使用说明：[Provider 指南](providers.md)、[重排模型指南](rerank.md)

硅基流动 SiliconFlow 专属厂商门面，统一提供对话、FLUX/SD 文生图、文本嵌入与文本重排：
| 任务 | 主要符号 |
| --- | --- |
| 对话模型 | `New(model, opts...)` |
| 图片生成 | `NewImageModel(model, opts...)`（FLUX.1、SD3 等） |
| 向量嵌入 | `NewEmbeddingModel(model, opts...)` |
| 文本重排 | `NewRerankModel(model, opts...)`、`DefaultRerankModel` |
| 选项配置 | `WithAPIKey`、`WithBaseURL`、`WithHTTPClient`、`WithHeader`、`WithAllowHTTP`、`WithAllowPrivateIPs`、`DefaultBaseURL` |

## `ai/openrouter`

- Godoc：[`github.com/rsbin1178/pips/ai/openrouter`](https://pkg.go.dev/github.com/rsbin1178/pips/ai/openrouter)
- 源码：[`ai/openrouter/`](../../ai/openrouter)

OpenRouter 聚合网关门面，一个 OpenAI 兼容端点路由到多家厂商模型：
| 任务 | 主要符号 |
| --- | --- |
| 对话模型 | `New(model, opts...)`（模型为组织前缀 slug，如 `openai/gpt-5.2`） |
| 归因头 | `WithReferer`（`HTTP-Referer`）、`WithAppTitle`（`X-OpenRouter-Title`） |
| 选项配置 | `WithAPIKey`、`WithBaseURL`、`WithHTTPClient`、`WithHeader`、`WithAllowHTTP`、`WithAllowPrivateIPs`、`DefaultBaseURL` |

## `ai/kimi`

- Godoc：[`github.com/rsbin1178/pips/ai/kimi`](https://pkg.go.dev/github.com/rsbin1178/pips/ai/kimi)
- 源码：[`ai/kimi/`](../../ai/kimi)

月之暗面 Kimi（Moonshot AI）门面，OpenAI 兼容对话、工具调用与视觉输入：
| 任务 | 主要符号 |
| --- | --- |
| 对话模型 | `New(model, opts...)`（如 `kimi-k3`、`kimi-k2.6`、`moonshot-v1-128k`） |
| 区域端点 | `DefaultBaseURL`（国际 `api.moonshot.ai`）、`DefaultChinaBaseURL`（中国 `api.moonshot.cn`） |
| 选项配置 | `WithAPIKey`、`WithBaseURL`、`WithHTTPClient`、`WithHeader`、`WithAllowHTTP`、`WithAllowPrivateIPs` |

## `ai/qwen`

- Godoc：[`github.com/rsbin1178/pips/ai/qwen`](https://pkg.go.dev/github.com/rsbin1178/pips/ai/qwen)
- 源码：[`ai/qwen/`](../../ai/qwen)

阿里云百炼 DashScope / Qwen 门面：OpenAI 兼容的对话与文本向量，加上 DashScope 原生的文本重排与异步文生图：
| 任务 | 主要符号 |
| --- | --- |
| 对话模型 | `New(model, opts...)`（如 `qwen3.7-max`、`qwen-plus`、`qwen3-vl-plus`） |
| 向量嵌入 | `NewEmbeddingModel(model, opts...)`（如 `text-embedding-v4`） |
| 文本重排 | `NewRerankModel(model, opts...)`（`qwen3-rerank`、`gte-rerank-v2`、`qwen3-vl-rerank`）、`RerankOptions` |
| 文生图 | `NewImageModel(model, opts...)`（`qwen-image-plus`、`wan2.6-t2i` 等）、`ImageOptions` |
| 区域端点 | `DefaultBaseURL`（兼容面）、`DefaultChinaBaseURL`、`DefaultUSBaseURL`；`DefaultHTTPAPIURL`（原生面）、`DefaultChinaHTTPAPIURL` |
| 任务轮询 | `WithPollInterval`（默认 3s）、`WithPollTimeout`（默认 5m） |
| 选项配置 | `WithAPIKey`、`WithBaseURL`、`WithHTTPClient`、`WithHeader`、`WithAllowHTTP`、`WithAllowPrivateIPs` |

## `ai/minimax`

- Godoc：[`github.com/rsbin1178/pips/ai/minimax`](https://pkg.go.dev/github.com/rsbin1178/pips/ai/minimax)
- 源码：[`ai/minimax/`](../../ai/minimax)

MiniMax 门面，同时提供 OpenAI 兼容对话、Anthropic 兼容 Messages 与私有 schema 生图：
| 任务 | 主要符号 |
| --- | --- |
| OpenAI 兼容对话 | `New(model, opts...)`（如 `MiniMax-M3`） |
| Anthropic 兼容对话 | `NewAnthropic(model, opts...)` |
| 图片生成 | `NewImageModel(model, opts...)`（`image-01`）、`ImageOptions`、`SubjectReference` |
| 区域端点 | `DefaultBaseURL` / `DefaultChinaBaseURL`、`DefaultAnthropicBaseURL` / `DefaultChinaAnthropicBaseURL` |
| 选项配置 | `WithAPIKey`、`WithBaseURL`、`WithHTTPClient`、`WithHeader`、`WithAllowHTTP`、`WithAllowPrivateIPs` |

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

## `ai/middleware/capability`

- Godoc：[`github.com/rsbin1178/pips/ai/middleware/capability`](https://pkg.go.dev/github.com/rsbin1178/pips/ai/middleware/capability)
- 源码：[`capability.go`](../../ai/middleware/capability/capability.go)

公开入口：`New(ai.CapabilityOverride) ai.Middleware`。它只覆盖被包装模型的 `Capabilities()`，`Generate`、`Stream`、`Provider`、`ModelID` 原样透传；零值 override 返回原模型，因此可以无条件挂链。Coding Agent 用它把用户声明的能力叠加到适配器自带的能力表之上。

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

以下包虽然会出现在 `go list ./ai/...`，但不是公共使用面：`ai/internal/httpx`、`ai/internal/jsonx`、`ai/internal/sse`、`ai/internal/apierr`、`ai/internal/imagewire`。其他 `ai/internal` 目录也遵守相同边界。外部应用不得导入它们；需要新增通用能力时应先通过公开 API 设计，而不是复制 internal 类型（扩展路径见[扩展指南](extending.md)）。
