# 扩展指南：新增 Provider 适配器

`ai` 只定义便携协议与接口，具体厂商由适配器包连接。本页说明扩展一个 Provider 时可以直接复用什么、必须自己实现什么、可选能力如何暴露，以及本项目在实测中确认的四条边界。每条限制都给出源码或实测依据。

## 先判断协议形状

| 目标端点 | 推荐路径 |
| --- | --- |
| OpenAI 形状的 Chat Completions / Responses（兼容服务、代理、私有网关） | 使用 `ai/openai/compat` 的 Profile，复用整套 chat wire |
| 非 OpenAI 形状（自有认证、自有端点族、私有媒体语义） | 新增独立适配器包，参考 `ai/agnes` |
| OpenAI 形状但需要新的端点族（图片、音频、Batch…） | 若该端点族已在 `ai/openai` 中有类型化实现，可继续组合；否则走独立包 |

两条路径的取舍：

| 维度 | `openai` + compat Profile | 独立适配器包 |
| --- | --- | --- |
| 复用的 wire 实现 | Chat Completions / Responses 全套（转换、流式、错误、推理续接） | 只有 `ai/internal` 的 seam（且仅限本模块内） |
| 厂商身份 | `compat.Profile.Provider` + Profile 专用环境变量 | 包自己的 `Provider()` |
| 私有请求参数 | `Compatibility` 只能表达已建模的协议差异，其余字段靠 `ExtraFields` | 类型化字段 + 自己的保留键列表 |
| 私有端点族与媒体编码 | 不支持（Profile 只产出 chat `*openai.Model`） | 自由实现，按需实现可选接口 |
| 内联大媒体（data URI、multipart） | 受 `ExtraFields` 大小上限阻挡 | 自己构造请求体，不经过 `ExtraFields` |
| 维护成本 | 低 | 高：wire、校验、测试、文档都要自建 |

本仓库的两个参照实现：

- `ai/openai/compat` —— 一个 OpenAI 形状文本厂商的最小接入（`compat.Profile` + `compat.New`）。
- `ai/agnes` —— 一个带私有媒体参数的厂商的完整适配器（本指南的参考实现）。

## 可复用的 seam

`ai/internal` 不是公开 API，但它就是同类适配器之间的共享实现。模块内的新适配器应当复用它们，而不是写第三份：

| 包 | 作用 | 使用者 |
| --- | --- | --- |
| `ai/internal/httpx` | 传输调优、SSRF 防护、JSON/SSE/multipart 执行、`Retry-After` 解析 | 全部适配器 |
| `ai/internal/jsonx` | 唯一的 JSON 编解码入口与有界 `ExtraFields` 合并 | 全部适配器 |
| `ai/internal/sse` | SSE 事件解析迭代器 | `openai`、`anthropic`、`gemini` |
| `ai/internal/apierr` | OpenAI 形状错误信封 → `*ai.Error`（保留 `RetryAfter`/`Raw`，按 `ClassifyStatus` 挂哨兵） | `openai`、`agnes` |
| `ai/internal/imagewire` | Images 形状响应条目 → `ai.GeneratedImage`，MIME 推导 | `openai`、`agnes` |

两个 seam 的用法：

```go
// 错误信封：供应商返回 {"error":{"message","type","code"}} 时，
// RetryAfter、Raw 与哨兵分类由 seam 统一处理。
func decodeError(status int, retryAfter time.Duration, body []byte) error {
	return apierr.Decode(ai.ProviderAgnes, status, retryAfter, body)
}

// 响应条目：data[].{b64_json,url,revised_prompt} → ai.GeneratedImage。
// 两者皆空的条目报错，绝不产出 0 字节图片。
image, err := imagewire.Image(datum, imagewire.MIMEFor(reported, requested), index)
```

## 必须自建的部分

1. **包结构与构造器**：`<provider>.go`（构造器、Option 族、`Provider`/`ModelID`/`Capabilities`、认证头）、`wire.go`（请求/响应结构）、`options.go`（Provider options 与错误解码）、按端点族拆分的 `<surface>.go`。
2. **wire 结构与请求映射**：未设置的字段一律省略，让厂商默认值生效；厂商私有参数进入类型化 options，不进 `ExtraFields` 的默认路径。
3. **本地校验**：结构性错误（空 prompt、缺必填参数、来源图缺失）返回包装 `ai.ErrInvalidRequest` 的错误且不发请求；厂商无法表达的值返回 `ai.ErrUnsupported`。两者都必须在发请求之前判定。
4. **传输细节**：模块外无法导入 `ai/internal/*`（见边界 1），所以模块外的适配器必须自己处理 HTTP、SSE、JSON 与错误信封。
5. **测试与文档**：见下文两节。
6. **可选接口实现**：只在厂商真正支持时实现，见下一节。

构造器样板（`WithAPIKey`/`WithBaseURL`/`WithHTTPClient`/…）目前在每个适配器包里各写一遍。这是现状而非定论：重复的选项让每个包的构造项在 godoc 里独立可读，也避免一个跨包配置类型把厂商差异抹平；代价是新增一个 Option 要在每个包改一次。目前有三个包承担这份重复，量还可控——当第四个适配器落地、或同一选项在不同包里开始出现语义漂移时，再评估抽出共享的配置类型。

## 可选能力与接口

便携层把能力拆成窄接口，用类型断言发现；缺失的能力**不实现**，而不是返回错误：

| 接口 | 含义 | 参照 |
| --- | --- | --- |
| `ai.ImageModel` | 文生图 | `openai`、`gemini`、`agnes` |
| `ai.ImageEditor` | 图生图/编辑 | `openai`、`agnes` |
| `ai.ImageVariator` | 变体 | `openai`（`dall-e-2`） |
| `ai.ImageStreamer` | 图片流式事件 | `openai` |
| `ai.EmbeddingModel` | 向量 | `openai`、`gemini` |
| `ai.RerankModel` | 重排 | `cohere` |
| `ai.TokenCounter` | 服务端 token 计数 | `anthropic`、`gemini` |

- 厂商没有的能力用“省略”表达（Agnes 没有 `New` 文本构造器、没有 `ImageVariator`），调用方在编译期就得到拒绝。
- 厂商有该端点但不支持某个取值时，本地返回 `ai.ErrUnsupported`（例如 Agnes 的 `N > 1`、`Mask`）。
- `Capabilities()` 是静态提示，不是调用闸门；未知模型给保守值，不要据此在适配器外复刻校验。

图片模型的 `Capabilities()` 目前并不一致：`agnes.ImageModel` 暴露 `Capabilities()`（并接受 `WithCapabilities` 覆盖），而 `openai`/`gemini` 的图片模型没有这个方法，只有它们的文本 `Model` 有。`ai.ImageModel` 接口本身不要求它，因此调用方只能用类型断言探测，不能假定任何图片模型都具备。把这件事统一——扩展 `ai.ImageModel`，或把 `Capabilities()` 定为所有图片模型的约定——是尚未决定的开放项，不属于新增适配器的必需步骤。

## 实测边界

以下四条是影响架构选择的硬限制，都有源码或实测依据。

### 1. 模块外无法导入 `ai/internal/*`

Go 的 `internal` 规则禁止模块外导入，本项目也明确要求不要复制这些包（见 [API 导航](api-navigation.md#非公开边界)）。后果：seam 只服务本仓库内的适配器；模块外的第三方适配器必须自建传输与解码。把 seam 提升为公开包是已知的扩展性限制，不在当前范围内。

依据：`ai/internal/{httpx,jsonx,sse,apierr,imagewire}` 的导入路径与 `docs/ai/index.md` 的声明。

### 2. `ExtraFields` 单串上限 32 KiB、整包上限 64 KiB

`ai/internal/jsonx` 的默认限制是 `defaultMaxBytes = 64 << 10`、`defaultMaxStringBytes = 32 << 10`，`MergeExtraFields` 在超限时返回 `jsonx.ErrUnsafeExtension`。实测：8 KiB/24 KiB 的字符串或 data URI 通过，40 KiB 被拒。

后果：约 100 KB 的 PNG 编码为 base64 约 137 KB，**无法**通过 `ExtraFields` 下发。带内联媒体的厂商参数必须由适配器自己构造请求体——这正是 `ai/agnes` 选择独立包而不是 `openai` + `ExtraFields` 的原因。

### 3. 中间件只覆盖 `LanguageModel`

`ai.Middleware` 的类型是 `func(ai.LanguageModel) ai.LanguageModel`；`retry`、`ratelimit` 与 `observability` 都按文本调用语义定义。图片、Embedding 调用没有对应的横切能力。

后果：图片适配器不要假装接受中间件；为图片补重试/限流是独立的后续工作。业务侧目前只能自己包裹图片调用。

### 4. `compat` 只产出 chat `*openai.Model`

`compat.Profile` 描述的是 Provider 身份、Base URL、凭据环境变量、API surface、能力基线与已审阅的协议差异；`compat.New` 返回 `*openai.Model`，即一个文本模型。

后果：兼容服务如果只有图片端点、或图片参数是私有的，Profile 表达不了，只能写独立适配器。Protocol shape 与 provider identity 分离的设计目标在这里也意味着：Profile 不是“任意厂商接入器”。

## 测试约定

- 协议样本写成**未导出的、按行为命名的**字符串常量，放在消费它的测试文件里（`agnesURLResponse`、`agnesInlineResponse`），通过 `httptest.Server` 走完整请求路径。
- 不新增 `testdata/`，不读相对路径 fixture；大体积二进制资产是唯一例外，且要在引入时写明理由。
- 请求侧断言**精确的 wire body**（`assert.Equal` 整个 `map[string]any`），这样“未设置字段不出现”“字段不在错误的层级”才有约束力。
- 响应侧覆盖：URL 响应、Base64 响应、两者皆空的条目报错、MIME 推导优先级。
- 每条本地校验单独一例，handler 在被调用时让测试失败，证明请求没有发出。
- 错误映射按 401/402/429/5xx 逐条断言哨兵、`IsRetryable`、`RetryAfter` 与 `Raw`。
- 接口与能力用编译期断言 + `Capabilities()` 断言；能力覆盖用 `WithCapabilities` 验证。
- 共享 seam 的回归由既有适配器测试承担；新 seam 自己补最小单测（信封变体、MIME 优先级、缺载荷）。

## 文档约定

- 每个导出符号都有 godoc，注释以句号结尾。
- 在 [`providers.md`](providers.md) 增加一节：构造示例、端点、默认密钥环境变量、参数映射表、本地校验规则、与厂商文档的分歧、超时建议。
- 在 [`api-navigation.md`](api-navigation.md) 增加包小节与符号入口；在 [`index.md`](index.md) 的公开包一览里加一行。
- 厂商文档自相矛盾时：按多处一致的示例实现，并在代码注释与 `providers.md` 里记录分歧和取舍（`ai/agnes/wire.go` 的 `extra_body.image` 是范例）。
- 导出符号的命名让调用点自解释：`package.Symbol` 必须能脱离声明读出作用；同一包出现多个可选配置目标时才加角色限定词。

## 新增适配器检查表

- [ ] 协议形状判断过：Profile 够用吗？不够的理由写进包文档注释。
- [ ] `Provider()` 返回真实身份；密钥优先显式选项，其次专用环境变量（不要回退到别人的变量）。
- [ ] 未设置的字段不出现在请求体；`ExtraFields` 使用本请求族自己的保留键列表。
- [ ] 本地校验先于网络调用，`ErrInvalidRequest` 与 `ErrUnsupported` 分流正确。
- [ ] 错误信封经 `apierr` 或等价的供应商实现解码，`Raw`/`RetryAfter` 保留。
- [ ] 响应不产出零值工件；URL 不代下载。
- [ ] 能力缺失用省略表达；可选接口只在真实支持时实现。
- [ ] 测试覆盖 wire body、响应变体、校验矩阵、错误映射与能力断言。
- [ ] `providers.md`、`api-navigation.md`、`index.md` 已更新；`make deps-check` 与 lint 全绿。
