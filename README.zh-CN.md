# Pips

[English](README.md) | 简体中文

[![Quality](https://github.com/rsbin1178/pips/actions/workflows/quality.yml/badge.svg?branch=main)](https://github.com/rsbin1178/pips/actions/workflows/quality.yml)

Pips 是一组用于构建 AI 应用、自主 Agent 和确定性 Workflow 的原生 Go
构建块。同一仓库还提供 `pips`：一个本地、终端优先的 Coding Agent，具有明确的
审批与 Sandbox 边界。

> **状态：v0。** 公开 API 仍在积极开发中，可能发生变化。依赖 Pips 时请固定
> module 版本或 commit。

## 为什么选择 Pips

- **原生 Provider 协议。** `ai` 适配器不依赖厂商 SDK，直接支持 OpenAI Chat
  Completions 与 Responses、Anthropic Messages 和 Gemini generateContent。
  可移植请求覆盖流式响应、Tools、结构化输出、推理、图片和 Embeddings，同时
  保留 Provider 专属逃生口。
- **分离的控制模型。** 当模型应自行决定调用哪个 Tool 时使用 `agent`；当流程
  必须遵循版本化图结构时使用 `workflow`。Workflow Runtime 不依赖 AI 或 Agent
  包。
- **基于显式权限的 Coding 产品。** Coding CLI 将持久会话、Plan Mode、工作区
  Tools、Subagents、Teams、MCP、ACP 和 SSH 与 Operation 级审批、原生命令
  Sandbox 组合起来。

## 选择使用路径

| 路径 | 从这里开始 | 适用场景 |
| --- | --- | --- |
| AI 与 Agent Library | [`ai`](https://pkg.go.dev/github.com/rsbin1178/pips/ai) · [`agent`](https://pkg.go.dev/github.com/rsbin1178/pips/agent) | 模型调用、类型化 Tools、受控 Agent 循环、Session 和应用自主管理的编排 |
| Workflow Runtime | [`workflow`](https://pkg.go.dev/github.com/rsbin1178/pips/workflow) | 版本化 DAG、固定流程、Schema 校验、失败路由、中断、Checkpoint 和节点调试 |
| Coding Agent | [`cmd/pips`](cmd/pips) · [Coding CLI 指南](docs/coding-cli.md) | 在本地或 SSH 工作区进行交互式、非交互式编码工作 |

## 快速开始

Pips 当前遵循 [`go.mod`](go.mod) 声明的 Go 工具链（Go 1.26.5）。

### 构建 Agent

在现有 Go module 中添加所需包：

```sh
go get github.com/rsbin1178/pips/agent \
  github.com/rsbin1178/pips/ai/openai
```

创建 `main.go`：

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
)

func main() {
	model := openai.New(
		"gpt-6-astra",
		openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
	)

	add := agent.NewTool(
		"add",
		"Add two integers.",
		func(_ context.Context, input struct {
			A int `json:"a" description:"First addend"`
			B int `json:"b" description:"Second addend"`
		}) (string, error) {
			return strconv.Itoa(input.A + input.B), nil
		},
	)

	a, err := agent.New(
		model,
		agent.WithSystem("Use the add tool for arithmetic."),
		agent.WithTools(add),
		agent.WithMaxTurns(8),
	)
	if err != nil {
		log.Fatal(err)
	}

	result, err := a.Run(
		context.Background(),
		agent.NewSession(),
		ai.UserText("What is 128 + 256?"),
	)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("%s (%s)\n", result.Text(), result.Stop)
}
```

通过环境或密钥管理系统提供凭据，然后运行：

```sh
export OPENAI_API_KEY='...'
go run .
```

仓库中的 [`examples/agent-basic`](examples/agent-basic) 在同一循环上增加了
Guardrails、Events 和 Usage 报告。直接调用模型时，从 [AI 快速开始](docs/ai/quickstart.md)
入手；需要 Tool 审批和持久会话时，继续阅读 [Agent 指南](docs/agent/index.md)。

### 运行 Coding CLI

安装最新发布的源码版本：

```sh
go install github.com/rsbin1178/pips/cmd/pips@latest
pips --help
```

创建 `~/.pips/config.toml` 并配置一个模型。只配置一个模型时会自动选中：

```toml
mode = "agent"
sandbox = "workspace-write"
approval = "on-request"

[providers.openai.models."your-model-id"]
```

Coding CLI 从 Provider 中立的 `API_KEY` 环境变量读取所选 Provider 的凭据。
启动 TUI 前检查配置、凭据和原生 Sandbox：

```sh
export API_KEY='...'
pips doctor
pips
```

无需 TUI 运行单次请求，或使用权限收窄的 Plan Mode 启动：

```sh
pips exec "review the current changes"
pips --mode plan
```

`pips doctor` 有意采用严格检查。在默认 `workspace-write` 模式下，Linux 需要
可用的 Bubblewrap 0.8.0+ Sandbox，macOS 使用系统 Seatbelt Sandbox。平台与
威胁模型边界见 [Coding 执行安全](docs/coding-security.md)。

### 界面截图

真实的 TUI 会话：模型探索工作区、修改文件，并在对话区说明改动结果。

![TUI 会话与工具活动](docs/assets/tui-session.png)

Plan Mode 把计划写入会话的 plan 文件，并在修改工作区之前请求审批。

![Plan 审批界面](docs/assets/plan-review.png)

斜杠命令（包括 `/plan`）都可以在命令面板中搜索。

![命令面板](docs/assets/command-palette.png)

## 能力地图

### Go Library

| 包 | 职责 |
| --- | --- |
| `ai` | Provider 中立消息、原生协议适配器、同步与流式生成、Tools、结构化输出、推理、Vision、图片、Embeddings、类型化错误、中间件和可观测 Hooks |
| `agent` | 有边界的模型/Tool 循环、类型化 Tools、Event 流、停止条件、Guardrails、审批、Steering、嵌套 Run 和可序列化 Session |
| `workflow` | 严格版本化 Definition、类型化 Binding 与 Schema、Actions、Conditions、Selectors、Merges、Sub-workflows、Batches、Loops、失败路由、Partial Runs、节点调试、中断、Checkpoint、重试和并发限制 |
| `agent/harness` | 追加式 Session 树、分支、上下文压缩、摘要、取消、Skills 和 Prompt 资源 |
| `agent/continuation`、`agent/goal`、`agent/loop` | 应用驱动的持久执行、基于证据的完成策略，以及不依赖常驻调度器的持久化激活决策 |
| `agent/team` | 持久成员、依赖任务、独占 Claim 与 Attempt、Mailboxes 和限定范围的协作 Tools；应用仍负责 Worker 与调度 |
| `agent/extension`、`agent/bundle`、`agent/mcp` | 可信编译期 Extension、有边界的本地声明式 Bundle，以及来自官方 MCP Client Session 的不可变 Tool Snapshot |

核心层有意保持可组合。裸 `ai` Provider 只尝试一次；Retry、限流和可观测性需要
显式加入。`Agent` 不可变且可安全共享；每个可变 `Session` 只允许一个活动 Run。
Workflow Definition 包含数据而非可执行代码；应用负责注册 Actions 并拥有其副作用。

### Coding Agent

`pips` 应用在 Library 之上增加产品层：

- 交互式 TUI、`exec`、持久 Resume/Fork 流程、Attachments、Themes 和 Shell
  Completion；
- 带结构化问题与显式 Plan Review 的 Agent 和 Plan 模式；
- 限定在 Workspace 内的 Read、Patch、Shell、Git Inspection 和 Image Tools；
- Operation 级审批、持久审批恢复，以及能力检查失败时关闭执行的原生命令
  Sandbox；
- Built-in Subagents、显式启用的 Alpha Custom Agents、具备依赖关系的 Coding
  Teams、Skills、MCP Servers、Agent Plugins、Extensions、Bundles 和生命周期
  Hooks；
- 基于 stdio 的 ACP v1，以及通过系统 OpenSSH 建立的精确版本 Remote TUI
  Session。

Workspace Trust 只为单次调用启用项目自有资源；它不会批准 Tools、Scripts、MCP
Servers、Full Access 或项目配置。可选集成可以贡献能力，但不会静默扩大权限。

## 项目状态与边界

- Pips 当前为 `v0`；兼容性工作仍在进行，不声明稳定 Release 契约。
- 不同 Provider 的能力和模型名称不同。静态能力元数据只是提示，不能替代所选
  Provider 的文档或一次真实请求。
- Workflow 是进程内 Runtime，不是远程 Worker 系统。宿主负责 Action 幂等性、
  Checkpoint 存储、加密、保留、授权以及稳定的 API 或数据库投影。
- Coding Sandbox 会限制模型生成的命令，但不是完整的密钥隔离或资源配额边界。
  当仓库或主机不可信时，请使用一次性环境、Container 或 VM。
- Dynamic Custom Coding Agents 是 Alpha 功能且默认关闭。其
  [Readiness Runbook](docs/coding-dynamic-subagents-readiness.md)规定了作出更广泛
  发布声明前所需的证据。

## 文档

| 主题 | 指南 |
| --- | --- |
| AI 接口、Providers、Tools、结构化输出、中间件与测试 | [AI 包指南](docs/ai/index.md) |
| Agent Runtime、控制、持久化、编排、Extensions 与 MCP | [Agent 包指南](docs/agent/index.md) |
| Agent 组合模式 | [Agent Composition](docs/agents.md) |
| Workflow 公开 API 与 Runtime 契约 | [Workflow Go Reference](https://pkg.go.dev/github.com/rsbin1178/pips/workflow) |
| Coding CLI、配置、Sessions、SSH 与 Custom Agents | [Coding CLI 契约](docs/coding-cli.md) |
| Sandbox、审批、凭据、Full Access 与威胁模型 | [Coding 执行安全](docs/coding-security.md) |
| Agent Client Protocol v1 | [ACP 指南](docs/coding-acp.md) |
| 本地生命周期命令的审阅与信任 | [Coding 生命周期 Hooks](docs/coding-hooks.md) |
| 端到端能力验收与证据收集 | [Coding Agent 人工验收](docs/manual-acceptance/index.md) |

## 示例

| 目标 | 示例 |
| --- | --- |
| 调用 OpenAI、Anthropic 或 Gemini | [`text-stream-openai`](examples/text-stream-openai) · [`text-stream-anthropic`](examples/text-stream-anthropic) · [`text-stream-gemini`](examples/text-stream-gemini) |
| 切换 Provider 或添加中间件 | [`provider-switch`](examples/provider-switch) · [`middleware`](examples/middleware) |
| 使用 Tools、结构化输出、Vision 或图片生成 | [`tools`](examples/tools) · [`structured`](examples/structured) · [`vision`](examples/vision) · [`image-gen`](examples/image-gen) |
| 构建 Agent 控制与持久化 | [`agent-basic`](examples/agent-basic) · [`agent-stream`](examples/agent-stream) · [`agent-approval`](examples/agent-approval) · [`agent-harness`](examples/agent-harness) |
| 组合 Agents | [`agent-subagent`](examples/agent-subagent) · [`team-agent`](examples/team-agent) |

## 开发与贡献

Clone 仓库后，使用项目已有的 Make Targets：

```sh
make help
make test-short
make vet
make lint
make build
```

`make p0-verify` 会运行更完整的 Coding Agent 质量与安全门，包括 Race Tests、
Provider Adapter Smoke Tests、依赖验证和漏洞审计。部分原生 Sandbox Tests 依赖
平台；作出平台或外部 Provider 声明前，请阅读[人工验收指南](docs/manual-acceptance/index.md)。

欢迎提交 Issues 和 Pull Requests。请保持变更范围清晰，为行为变化添加测试，运行
相关 Make Targets，并在记录用户可见契约时避免包含密钥或无法验证的产品声明。
