# Provider 指南

业务层应依赖 `ai.LanguageModel`；Provider 包负责模型构造、原生协议和不可移植选项。本页说明当前源码支持的协议与差异，不承诺厂商未来所有模型都具有相同能力。

## 快速选择

| 构造器 | 原生协议 | 默认密钥环境变量 | 额外接口 |
| --- | --- | --- | --- |
| `openai.New` | Chat Completions 或 Responses | `OPENAI_API_KEY` | 独立图片（含 `ai.ImageEditor`、`ai.ImageVariator`、`ai.ImageStreamer`）、Embedding 构造器 |
| `anthropic.New` | Messages | `ANTHROPIC_API_KEY` | `ai.TokenCounter` |
| `gemini.New` | generateContent / streamGenerateContent | `GEMINI_API_KEY`，其次 `GOOGLE_API_KEY` | `ai.TokenCounter`、独立图片、Embedding 构造器 |
| `agnes.NewImageModel` | Agnes Images | `AGNES_API_KEY` | 图片：`ai.ImageModel` + `ai.ImageEditor`（无流式/变体） |
| `compat.*` | 审阅后的 OpenAI 形状协议 | Profile 专用变量 | 返回 `*openai.Model` |

三个原生文本模型构造器均返回不可变、可并发复用的对象，且把配置错误延迟到首次调用。生产启动检查若需要立即失败，应主动执行一个受控 Smoke Test，而不是假定 `New` 会验证凭据。

## OpenAI

最小构造：

```go
model := openai.New("gpt-6-astra") // 默认读取 OPENAI_API_KEY
```

`openai.Model` 支持两个 API surface：

| `openai.API` | 路径 | 选择建议 |
| --- | --- | --- |
| `APIAuto` | 自动 | 默认；`o1`/`o3`/`o4`/`gpt-5*`/`gpt-6*` 走 Responses，其余走 Chat Completions |
| `APIChatCompletions` | `/v1/chat/completions` | 传统 Chat 或兼容服务 |
| `APIResponses` | `/v1/responses` | OpenAI 推理与 agentic 场景 |

显式固定协议：

```go
model := openai.New(
	"gpt-6-astra",
	openai.WithAPI(openai.APIResponses),
)
```

`openai.ResolveAPI(modelID, requested)` 可在构造注册表或测试配置时查看相同的自动路由决策。模型名称只是静态前缀判断；若代理使用自定义名称，应显式指定 API。

### OpenAI 请求差异

- Chat Completions 支持常见 sampling、Stop、Tools、Schema；OpenAI 原生 Chat 会拒绝 `TopK`。
- Responses 会拒绝 `TopK`、`Seed`、两类 penalty 和 `Stop`；Reasoning 不接受显式 `BudgetTokens` 或 adaptive。
- Chat Completions 的文件输入支持 Provider 文件 ID 或内联数据，不支持文件 URL。
- Responses 的文件输入支持文件 ID、URL 和内联数据。
- Responses 的 Tool Call 在 wire 上同时带 item id（`fc_…`）与 call id（`call_…`）；便携 `ToolCallPart.ID` 以 call id 为基底并追加 `|id=` 形式的 item id，回放历史时两个字段都会还原。严格校验输入项的兼容服务会拒绝缺少 item id 的 `function_call`，因此历史里没有该 id 时适配器会按 call id 派生一个。
- Responses 的推理文本有两种流：`response.reasoning_summary_text.*`（摘要）与 `response.reasoning_text.*`（原始推理，开源或代理服务常用）。流式响应里同一个 item 以先出现的流为准，另一种忽略，避免同一段推理被拼接两次；非流式响应优先取 `summary`，为空时用 `content`。回放时只还原 Provider 给的 `content`，不会再合成一份 `summary`。
- Responses 的请求体始终显式携带 `stream`（流式 `true`，非流式 `false`）。该字段在官方 schema 中可选，但存在省略后返回 500 的兼容服务；回放的 assistant 文本消息同样会补上 schema 标记为必填的 `status: "completed"`。
- `Response.Raw` 保存所选 API 的原始 JSON，但便携业务逻辑不应解析它。

图片与 Embedding 使用独立构造器：

```go
images := openai.NewImageModel("gpt-image-2")
embeddings := openai.NewEmbeddingModel("text-embedding-3-small")
```

`openai.ImageOptions` 和 `openai.RequestOptions` 各自是不同请求类型的 Provider escape hatch。OpenAI 没有实现 `ai.TokenCounter`。

### OpenAI 图片

`openai.ImageModel` 覆盖三个端点，并把每个文档化参数映射到类型化字段：

| 端点 | 便携调用 | 编码 |
| --- | --- | --- |
| `POST /v1/images/generations` | `GenerateImages` | JSON |
| `POST /v1/images/edits` | `EditImage`（`ai.ImageEditor`） | multipart 或 JSON，由 `openai.ImageOptions.EditEncoding` 选择 |
| `POST /v1/images/variations` | `CreateVariations`（`ai.ImageVariator`） | multipart，仅 `dall-e-2` |

参数映射：

| OpenAI 参数 | 便携来源 |
| --- | --- |
| `model` | `NewImageModel` 的模型 ID |
| `prompt` | `ImageRequest.Prompt` / `ImageEditRequest.Prompt` |
| `n`、`size`、`quality` | 同名便携字段 |
| `output_format` | `ImageRequest.OutputFormat` / `ImageEditRequest.OutputFormat` |
| `response_format`、`background`、`moderation`、`style`、`user`、`output_compression`、`partial_images`、`input_fidelity` | `openai.ImageOptions` 同名字段 |
| `mask`、多图输入 | `ImageEditRequest.Mask` / `.Images`（1–16 张） |
| `stream` | 由 `StreamImages` / `StreamImageEdits` 置为 `true`；该键保留，不能经 `ExtraFields` 下发 |

未显式设置的字段不会出现在请求体里，厂商默认值照常生效。`ExtraFields` 是补充通道：图片请求使用独立的保留键列表（`model`、`prompt`、`n`、`size`、`quality`、`output_format`、`output_compression`、`partial_images`、`input_fidelity`、`moderation`、`style`、`user`、`stream`、`image`、`image[]`、`mask`、`images`），而 `background`、`response_format` 等未设置的类型化键不在该列表内，可继续通过它下发；`stream` 例外，只有 `ai.ImageStreamer` 能消费事件流响应，因此它保持保留状态。与类型化字段已设置的值冲突、或凭证形状的键依然被拒绝。编辑与变体的 multipart 请求的表单只能承载标量，因此这些请求的 `ExtraFields` 值必须是 string、bool 或数字，嵌套值会被拒绝。

响应侧：`data[].b64_json` 解码进 `GeneratedImage.Data`，`data[].url` 进入 `GeneratedImage.URL`，`revised_prompt` 进入 `GeneratedImage.RevisedPrompt`；两者都没有的条目会直接报错，不会产生 0 字节图片。`MIMEType` 依次取响应 `output_format`、请求 `OutputFormat`、URL 扩展名，内联字节的兜底是 `image/png`；无法判定时留空。顶层 `created`、`size`、`quality`、`background`、`output_format` 与 `usage`（含 `total_tokens`、`input_tokens_details`、`output_tokens_details`）都映射到 `ai.ImageResponse`。

编辑请求的编码规则：`EditEncodingAuto`（默认）在任一来源图带内联字节时选 multipart，全部为 URL 或文件 ID 时选 JSON 变体；`EditEncodingMultipart` 要求所有来源都是内联字节（multipart 无法表达 URL 或文件 ID），`EditEncodingJSON` 把内联字节编码为 `data:<mime>;base64,` 的 `image_url`。multipart 使用重复的 `image[]` 部件（单图也是 `image[]`）与 `mask` 部件，JSON 变体使用 `images[].image_url|file_id` 二选一。

流式（`ai.ImageStreamer`）：

```go
streamer := imageModel.(ai.ImageStreamer)
for event, err := range streamer.StreamImages(ctx, ai.ImageRequest{
	Prompt: "极简蓝色山脉图标",
	ProviderOptions: map[ai.Provider]any{
		ai.ProviderOpenAI: openai.ImageOptions{PartialImages: ai.Ptr(2)},
	},
}) {
	// ai.ImageStreamPartial：Index 为 0 基局部图下标。
	// ai.ImageStreamCompleted：最终图，且带 Usage。
}
```

generations 使用 `image_generation.*`、edits 使用 `image_edit.*` 命名空间；未识别的事件被忽略。完成事件缺少图片数据时直接报错，不会产出空图。首事件之前的失败（包括 4xx/5xx）在首次迭代产出错误并保持可重试，产出后的失败终止流且不重放。

本地校验只做结构性检查：空 prompt、edits 无图或超过 16 张、`partial_images` 不在 0–3、`output_compression` 不在 0–100、变体缺少内联图、来源图没有引用或同时给出多种引用、显式 multipart 遇到 URL/文件 ID 来源、未知的 `EditEncoding` 值。失败返回包装 `ai.ErrInvalidRequest` 的错误且不发送请求。是否支持某个参数仍由厂商按模型判定：能力表只作提示，适配器不会按模型名拦截。

### OpenAI 构造选项

| 选项 | 用途 |
| --- | --- |
| `WithAPIKey` | 显式密钥；优先于环境变量 |
| `WithBaseURL` | 代理或兼容端点，保留其路径前缀 |
| `WithAPI` | 固定 Chat Completions / Responses |
| `WithProvider` | 保持真实服务身份，不改变 wire 协议 |
| `WithCapabilities` | 覆盖静态模型能力表 |
| `WithCompatibility` | 精确描述兼容端点 wire 差异 |
| `WithCompatMode` | 简化兼容模式：旧 `max_tokens` 且不发送流 Usage 选项 |
| `WithHeader` | 添加每请求 Header；同名时覆盖适配器值 |
| `WithHTTPClient` | 使用调用方拥有的客户端 |
| `WithAllowHTTP` | 明确允许明文 HTTP Base URL |
| `WithAllowPrivateIPs` | 明确允许私网/Loopback 目标 |
| `WithMaxStreamLineSize` | 调高默认 1 MiB 的单行 SSE 上限 |

## OpenAI 兼容服务

优先使用 `ai/openai/compat` 的命名构造器，而不是仅组合 `WithBaseURL` 与 `WithCompatMode`。Profile 同时保存真实 Provider 身份、环境变量、API surface、能力基线和已审阅的 wire 差异。

| 构造器 | Provider | 环境变量 | API | 能力/协议重点 |
| --- | --- | --- | --- | --- |
| `compat.DeepSeek` | `deepseek` | `DEEPSEEK_API_KEY` | Chat | JSON object 结构输出、DeepSeek reasoning、旧 max tokens |
| `compat.Groq` | `groq` | `GROQ_API_KEY` | Chat | 能力按模型名保守推断 |
| `compat.XAI` | `xai` | `XAI_API_KEY` | Responses | 返回可续接的加密 reasoning 状态 |
| `compat.OpenRouter` | `openrouter` | `OPENROUTER_API_KEY` | Chat | 上游模型不固定，能力保守；保留 reasoning details |
| `compat.Cerebras` | `cerebras` | `CEREBRAS_API_KEY` | Chat | 能力按模型名保守推断 |
| `compat.Together` | `together` | `TOGETHER_API_KEY` | Chat | 关闭便携 Chat reasoning 编码 |
| `compat.Mistral` | `mistral` | `MISTRAL_API_KEY` | Chat | 结构输出；按模型名推断视觉/推理；保留 content chunks |

示例：

```go
model := compat.DeepSeek("deepseek-v4.1-flash")
resp, err := model.Generate(ctx, req)
```

`compat.Lookup(provider)` 返回内置 Profile 的防御性副本，适合构造应用注册表。自定义代理可使用 `compat.New`：

```go
profile := compat.Profile{
	Provider:  ai.Provider("company-gateway"),
	BaseURL:   "https://llm.example.com/v1",
	APIKeyEnv: []string{"COMPANY_LLM_API_KEY"},
	API:       openai.APIChatCompletions,
	Capabilities: ai.Capabilities{
		Text:  true,
		Tools: true,
	},
}
model := compat.New(profile, "company-model")
```

自定义 Profile 的空 API 默认使用 Chat Completions；空能力默认是 Text + Tools。Profile 密钥为空是有意状态，绝不会退回读取 `OPENAI_API_KEY`。传给 `compat.New` 的额外 `openai.Option` 在 Profile 默认值之后应用，因此可覆盖端点、密钥或协议。

`WithCompatibility` 可精确调整 max-token 字段、流 Usage、结构输出形态、Chat reasoning 形态、历史 reasoning 字段以及 Responses 的加密 reasoning include。只在目标端点协议已有测试证据时自定义这些值；它不负责 Provider 身份、认证、URL 或能力表。

## Agnes

```go
images := agnes.NewImageModel("agnes-image-2.5-flash") // 默认读取 AGNES_API_KEY
```

`ai/agnes` 只覆盖 Agnes 图片系列（`agnes-image-2.5-flash` 等）；Agnes 的文本模型不属于该包的目标。适配器与 OpenAI Images 同形但不相同，代码注释与本节都记录了两者的差异。

| 站点 | Base URL |
| --- | --- |
| 中国站（默认，`agnes.BaseURLCN`） | `https://api.agnes-ai.cn/v1` |
| 国际站（`agnes.BaseURLGlobal`） | `https://apihub.agnes-ai.com/v1`，用 `agnes.WithBaseURL` 切换 |

`NewImageModel` 返回的对象实现 `ai.ImageModel` 与 `ai.ImageEditor`：文生图与图生图/多图合成都走同一个端点 `POST /v1/images/generations`，Agnes 没有 `/v1/images/edits`。

参数映射：

| Agnes 参数 | 便携来源 |
| --- | --- |
| `model` | `NewImageModel` 的模型 ID |
| `prompt` | `ImageRequest.Prompt` / `ImageEditRequest.Prompt` |
| `size` | `ImageRequest.Size` / `ImageEditRequest.Size`（厂商必填：档位 `1K`/`2K`/`3K`/`4K`，也接受 `1024x768` 旧写法；适配器只在文生图缺失时本地拦截） |
| `ratio` | `agnes.ImageOptions.Ratio`；文档枚举 `1:1`、`3:4`、`4:3`、`16:9`、`9:16`、`2:3`、`3:2`、`21:9`，空值省略并使用厂商默认 `1:1` |
| `extra_body.response_format` | `agnes.ImageOptions.ResponseFormat`（`url` / `b64_json`），绝不放在顶层 |
| `return_base64` | `agnes.ImageOptions.ReturnBase64`（仅文生图发送） |
| `extra_body.image[]` | `ImageEditRequest.Images`：公共 HTTPS URL 直传；内联字节编码为 `data:<mime>;base64,…`，缺少 MIME 时按 `image/png` |

厂商文档分歧：参数表把 `image` 列在顶层，但三处示例、错误排查与接入检查清单都要求放在 `extra_body.image`；适配器按后者实现（`ai/agnes/wire.go` 有对应注释），字段位置由测试锁定。

`agnes.ImageOptions` 先按 `ai.ProviderAgnes` 查找，找不到时回退到 `ai.ProviderOpenAI`；两个 key 都要求 `agnes.ImageOptions` 类型，其它类型（例如 `openai.ImageOptions`）会被静默忽略，不会退化成 OpenAI 语义——与 [ProviderOptions 与 ExtraFields](#provideroptions-与-extrafields) 的约定一致。

本地校验（失败不发请求）：空/空白 prompt、文生图缺少 `size`、`ratio` 不在文档枚举内、编辑无来源图、来源为空、来源同时带 URL 与内联字节 → `ai.ErrInvalidRequest`；`N > 1`、`Mask != nil`、来源为文件 ID → `ai.ErrUnsupported`；`N < 0` → `ai.ErrInvalidRequest`。图生图不发送 `return_base64`，其余未设置字段一律省略。

响应：`created` → `CreatedAt`；`data[].url` → `URL`（适配器不代下载）；`data[].b64_json` → `Data`；`revised_prompt` → `RevisedPrompt`；两者皆空的条目直接报错，不会产出 0 字节图片。Agnes 未文档化 `usage` 与输出格式回显，`ImageResponse.Usage` 保持零值，原始响应体仍可从 `Raw` 读取。

`ExtraFields` 使用 Agnes 专用保留键列表（`model`、`prompt`、`size`、`ratio`、`image`、`extra_body`、`return_base64`、`response_format`），这些键始终被拒绝，避免绕过文档化的字段位置（顶层的 `response_format` 正是厂商列为最常见的集成错误）；`seed` 等未建模参数可自由下发，凭证形状与超限值仍被拒绝。

错误与超时：Agnes 的错误信封与 OpenAI 同形（实测 401 返回 `{"error":{...}}`），401/403 → `ErrAuth`、402 等其他 4xx → `ErrInvalidRequest`、429 → `ErrRateLimited`、5xx → `ErrOverloaded`，`Retry-After` 与 `Raw` 保留。生成耗时可达数十秒，官方建议客户端超时 60–360s，请用 `context.WithTimeout` 设置；当前所有档位与参考图免费，以厂商页面为准。

Agnes 没有流式、变体与 mask，这些能力以“省略”表达：`ImageModel` 不实现 `ai.ImageStreamer`、`ai.ImageVariator`，调用方用类型断言发现。构造选项与 OpenAI 对齐：`WithAPIKey`、`WithBaseURL`、`WithHTTPClient`、`WithHeader`、`WithAllowHTTP`、`WithAllowPrivateIPs`、`WithMaxStreamLineSize`（Agnes 无流式，仅为构造器对齐保留）、`WithCapabilities`。

## Anthropic

```go
model := anthropic.New("claude-sonnet-5")
```

适配器使用 `/v1/messages`，支持文本、视觉、文件、Tools、原生结构输出、扩展思考、流式响应、Prompt Cache 和 `/v1/messages/count_tokens`。

Anthropic Messages 要求 `max_tokens`。当 `Request.MaxTokens == nil` 时，适配器发送默认 4096；可用构造选项调整：

```go
model := anthropic.New(
	"claude-sonnet-5",
	anthropic.WithMaxTokens(2_000),
)
```

当前适配器会拒绝 `Seed`、`FrequencyPenalty`、`PresencePenalty` 和 `LogProbs`。消息序列开头的 `SystemMessage` 会转换为顶层 system blocks；Tool Result 在 wire 上转换为 user 消息。文件 ID 会自动追加当前实现需要的 Files API Beta Header。

### Anthropic Prompt Cache

Provider options 可设置显式或自动缓存断点：

```go
req.ProviderOptions = map[ai.Provider]any{
	ai.ProviderAnthropic: anthropic.RequestOptions{
		CacheSystem:    true,
		AutomaticCache: true,
		CacheTTL:       anthropic.CacheTTL1Hour,
	},
}
```

可用项为 `CacheSystem`、`CacheLastMessage`、`AutomaticCache`、`CacheTTL` 和 `ExtraFields`。TTL 可选 `CacheTTL5Minutes` 或 `CacheTTL1Hour`；空值不发送 TTL，使用 Provider 默认。Token 计数端点不携带 cache breakpoint，也不会预热 Prompt Cache。

### Anthropic 构造选项

除通用的密钥、Base URL、HTTP Client、Header、HTTP/私网允许和 SSE 行上限外，还支持：

- `WithAPIVersion`：覆盖默认 `anthropic-version`，当前默认值为 `2023-06-01`。
- `WithBeta`：追加 `anthropic-beta` 值，可调用多次。
- `WithMaxTokens`：设置请求未给出上限时的默认值。
- `WithProvider`：为兼容网关保留真实身份。

## Gemini

```go
model := gemini.New("gemini-3.8-flash")
```

适配器使用 `generateContent` / `streamGenerateContent`，支持文本、图像、文档、音频/视频输入、Tools、结构化输出、Thinking、显式缓存资源和 `countTokens`。

Gemini 会在发送前校验部分数值范围，包括 Temperature `0..2`、TopP `0..1`、正数 int32 TopK/MaxTokens、int32 Seed、`-2..2` penalties、`0..20` Top LogProbs 以及非负 int32 Thinking budget。越界返回 `ai.ErrInvalidRequest`，不发送 HTTP 请求。adaptive reasoning 返回 `ai.ErrUnsupported`。

显式缓存资源放在 Provider options：

```go
req.ProviderOptions = map[ai.Provider]any{
	ai.ProviderGemini: gemini.RequestOptions{
		CachedContent: "cachedContents/abc123",
	},
}
```

Gemini Provider 文件 URI 使用 `FileURL` 的 URL 字段。当前适配器不接受 `FileID` 作为 Gemini 输入。

图片和向量：

```go
images := gemini.NewImageModel("gemini-2.5-flash-image") // 或 "imagen-3.0-generate-002"
embeddings := gemini.NewEmbeddingModel("gemini-embedding-001")
```

`gemini.NewImageModel` 返回的对象同时实现 `ai.ImageModel` 与 `ai.ImageEditor`，支持双端点自动路由：

1. **Gemini 原生多模态生图/编辑**（如 `gemini-2.5-flash-image`, `gemini-3.1-flash-image`, `gemini-3-pro-image`）：
   - 调用 `:generateContent` 端点。
   - 文生图与多图图生图编辑（`EditImage` 支持最多 14 张参考图多模态输入，不支持 `Mask`）。
   - 智能解析便携 `Size` 为 `aspectRatio`（`16:9`、`1:1`、`4:3` 等）与 `imageSize`（`1K`、`2K`、`4K`）。
   - 类型化 `gemini.ImageOptions` 支持 `AspectRatio`、`ImageSize`、`ThinkingLevel`、`SearchGrounding`（自动挂载 Google Search Grounding 工具）、`SafetySettings` 与 `ExtraFields`。
   - 自动过滤思考过程草图（`thought: true`），只返回最终成品图；安全风控拦截（`finishReason: SAFETY` 等）或无图输出时显式报错（包装 `ai.ErrInvalidRequest`），不静默掩盖。
   - 单次调用生成 1 张图，`N > 1` 返回 `ai.ErrUnsupported`，`N < 0` 返回 `ai.ErrInvalidRequest`。
2. **Imagen 专用生图模型**（如 `imagen-3.0-generate-002`, `imagen-4.0-*`）：
   - 自动路由至 `:predict` 端点，构建 `instances` 与 `parameters`。
   - `N` 映射至 `sampleCount`（1~4，超出范围返回 `ai.ErrInvalidRequest`）。
   - 支持 `aspectRatio` 与 `personGeneration`（`dont_allow`, `allow_adult`, `allow_all`）。
   - Imagen 模型不支持图生图编辑（`EditImage` 返回 `ai.ErrUnsupported`）。

Embedding 使用 batch endpoint，因此多个输入在一个请求中发送，并按返回顺序输出。

Gemini 构造选项包括 `WithAPIKey`、`WithBaseURL`、`WithHTTPClient`、`WithHeader`、`WithProvider`、`WithAllowHTTP`、`WithAllowPrivateIPs` 和 `WithMaxStreamLineSize`。

## ProviderOptions 与 ExtraFields

三种请求 options 类型不能混用：

```go
req.ProviderOptions = map[ai.Provider]any{
	ai.ProviderOpenAI: openai.RequestOptions{
		ExtraFields: map[string]any{"service_tier": "flex"},
	},
	ai.ProviderAnthropic: anthropic.RequestOptions{
		CacheSystem: true,
	},
	ai.ProviderGemini: gemini.RequestOptions{
		ExtraFields: map[string]any{"safetySettings": settings},
	},
}
```

OpenAI 兼容模型会先按它的真实 Provider key 查找 `openai.RequestOptions`，再兼容查找 `ProviderOpenAI`。Anthropic/Gemini 的自定义 Provider 身份也有类似回退到原生 Provider key 的行为。

`ExtraFields` 是只添加、不覆盖的有界递归合并。与保留字段或保留 dotted path 冲突会失败；可用 `ai.ValidateRequestBodyExtension` 对自定义扩展做提前校验，但适配器发送时仍会再次验证。不要用 ExtraFields 注入 `model`、消息、Tools、认证、流开关或其他已建模字段。图片请求使用独立的保留键列表，见 [OpenAI 图片](#openai-图片)。

## 自定义端点与网络安全

默认只允许 HTTPS 公网端点。使用本地模型或私网 Gateway 时必须显式选择相应能力：

```go
model := openai.New(
	"local-model",
	openai.WithBaseURL("http://127.0.0.1:8080/v1"),
	openai.WithAllowHTTP(),
	openai.WithAllowPrivateIPs(),
	openai.WithCompatMode(),
)
```

允许 HTTP 会失去传输机密性；允许私网会扩大 SSRF 范围，只应对受控地址启用。若传入自定义 `http.Client`，适配器会原样使用，调用方负责 Transport、Proxy、重定向、超时、TLS 和 SSRF dial guard。不要让不可信用户直接控制 Base URL 或 Header。

## 能力矩阵如何阅读

内置能力来自静态表或 Profile 的保守推断。Anthropic/Gemini 报告当前适配器的广泛协议能力；OpenAI 根据模型名前缀区分文本、视觉、文档、推理、图片或 Embedding；兼容服务可能按模型名片段推断。

能力表适合提示，不适合授权或硬校验。Unknown model 仍允许调用；账户、区域、服务版本和上游路由都可能让运行结果与静态表不同。生产系统应记录实际失败并对每个启用模型做独立 Smoke Test。

## Provider 切换检查表

- 业务函数只依赖 `ai.LanguageModel`，Provider 构造集中在 Composition Root。
- 检查目标协议是否接受正在使用的采样参数、文件来源和 reasoning 配置。
- 保留完整 `Response.Message` 只用于同一 Provider 的后续对话；不要跨 Provider 搬运 opaque reasoning state。
- 为每个目标 Provider 验证 Tools、结构输出、流 Usage 和错误映射。
- 使用正确的 Provider options 类型和 key，不依赖错误类型被静默忽略。
- 更新成本、超时、RPM/TPM 与真实模型 Smoke Test。

可运行的统一切换示例见 [`examples/provider-switch`](../../examples/provider-switch/main.go)。各适配器公开符号入口见 [API 导航](api-navigation.md)。
