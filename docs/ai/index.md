# AI 包使用指南

`github.com/rsbin/pips/ai` 是 Pips 的 Provider 中立模型层。它用同一组 Go 类型表示消息、流、工具调用、结构化输出、推理、图片与向量，并通过适配包连接 OpenAI、Anthropic、Gemini 及经过审阅的 OpenAI 兼容服务。

本指南面向熟悉 Go、第一次使用 Pips 的开发者。建议先按顺序阅读前三篇，再按任务选择后续章节。

## 从哪里开始

| 目标 | 文档 |
| --- | --- |
| 发出第一次请求 | [快速开始](quickstart.md) |
| 理解接口、消息、多模态、能力与生命周期 | [核心概念](core-concepts.md) |
| 正确处理同步与流式生成 | [生成与流式响应](generation-streaming.md) |
| 实现工具循环或类型化 JSON 输出 | [Tools 与结构化输出](tools-structured-output.md) |
| 选择 Provider、协议和兼容配置 | [Provider 指南](providers.md) |
| 添加重试、限流、错误分类和监控 | [错误、中间件与可观测性](errors-middleware-observability.md) |
| 在不使用真实密钥时测试业务逻辑 | [测试指南](testing.md) |
| 查找公开包、类型、示例和源码 | [API 导航](api-navigation.md) |

## 公开包一览

| 包 | 用途 |
| --- | --- |
| [`ai`](https://pkg.go.dev/github.com/rsbin/pips/ai) | Provider 中立接口、请求、响应、流、Tools、Schema、错误与中间件组合 |
| [`ai/openai`](https://pkg.go.dev/github.com/rsbin/pips/ai/openai) | OpenAI Chat Completions、Responses、图片和 Embeddings |
| [`ai/openai/compat`](https://pkg.go.dev/github.com/rsbin/pips/ai/openai/compat) | 七个经过审阅的 OpenAI 兼容服务配置 |
| [`ai/anthropic`](https://pkg.go.dev/github.com/rsbin/pips/ai/anthropic) | Anthropic Messages、流式响应、Tools、结构化输出、Prompt Cache 与 Token Counting |
| [`ai/gemini`](https://pkg.go.dev/github.com/rsbin/pips/ai/gemini) | Gemini generateContent、流式响应、图片、Embeddings、缓存与 Token Counting |
| [`ai/middleware/retry`](https://pkg.go.dev/github.com/rsbin/pips/ai/middleware/retry) | 有边界的指数退避重试 |
| [`ai/middleware/ratelimit`](https://pkg.go.dev/github.com/rsbin/pips/ai/middleware/ratelimit) | 客户端 RPM/TPM 限流 |
| [`ai/observability`](https://pkg.go.dev/github.com/rsbin/pips/ai/observability) | 与遥测后端无关的调用生命周期 Hooks |

`ai/internal/...` 是适配器之间共享的实现细节。Go 的 `internal` 规则本身也会阻止外部模块导入；不要复制、依赖或在配置中引用这些包。

## 设计边界

AI 包负责：

- 把便携请求翻译为各 Provider 的原生协议，并把响应规范化。
- 提供单次同步调用、流式事件、图片生成、向量、Token Counting 和中间件接口。
- 保留 `Raw`、`ProviderOptions` 等受控逃生口，以便使用未抽象的 Provider 字段。
- 提供错误类别、重试判断、RPM/TPM 限流和可插拔观测 Hooks。

AI 包不负责：

- 自动执行 Tool、决定 Tool 权限或无限循环调用模型。
- 保存对话、恢复运行、调度任务或管理 Agent。
- 自动选择模型、联网发现模型能力，或保证不同 Provider 功能完全一致。
- 默认重试。裸 Provider 客户端每次调用只尝试一次。
- 创建日志、指标、Trace Exporter，或替应用关闭这些资源。

需要自主 Tool 循环、Guardrail、Approval、Session 或多 Agent 编排时，请使用 [Agent 包使用指南](../agent/index.md)；直接调用模型时保留在 `ai` 层。

## 稳定性与版本

项目当前为 `v0`，公开 API 仍可能变化。生产项目应固定模块版本或提交，并在升级时运行本项目的编译与测试。仓库声明的最低 Go 工具链以 [`go.mod`](../../go.mod) 为准。

本文档描述当前源码，而不是某一家模型厂商全部可能提供的能力。模型名称、服务配额和厂商能力会变化；`Capabilities` 仅是静态、尽力而为的提示，最终应以实际调用结果和 Provider 文档为准。

## 最小生产检查表

- 从环境或密钥管理器读取凭据，不把密钥写入源码、日志或 `ProviderOptions`。
- 为请求设置 `context` 超时，并在流式循环中处理事件错误。
- 根据幂等性和成本显式添加重试；确认流输出后不会重放。
- 把 Tool 循环限制为有限轮次，验证 Tool 名称和参数，并把权限判断放在应用或 `agent` 层。
- 同时记录 Provider、实际模型、完成原因、耗时和 `Usage`；不要记录敏感 Prompt 或 Tool 结果。
- 用假模型测试业务分支，用 `httptest.Server` 测试适配器集成；把真实 Provider Smoke Test 独立标记。

## 权威来源

每个符号的字段和签名以 [Go 文档](https://pkg.go.dev/github.com/rsbin/pips/ai)与仓库源码为准；本目录提供学习路径和生产工作流。可执行示例位于 [`examples/`](../../examples)，包级示例位于 [`ai/example_test.go`](../../ai/example_test.go)。
