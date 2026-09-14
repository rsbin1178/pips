# Coding Agent 人工验收手册

本目录用于验收当前源码所实现的 Coding Agent。它不是功能宣传页，也不代表文档作者已经替你执行了真实 Provider、远程主机或 Beta 运行观察。

开始前先复制并填写[环境与证据记录](environment-evidence-record.md)。所有写操作都应在一次性测试仓库和独立 `PIPS_HOME` 中完成。

## 结果定义

每个用例只能记录以下结果之一：

| 结果 | 含义 |
| --- | --- |
| `通过` | 所有步骤已执行，要求的证据完整，且全部通过条件成立 |
| `失败` | 已执行，但至少一项通过条件不成立；必须附失败证据 |
| `未执行` | 当前没有执行；不得计入通过率 |
| `不适用` | 已确认当前环境不满足适用条件，并记录理由；不得写成通过 |

一个分册只有在其适用用例全部为`通过`、不适用项均有理由、且没有`失败`或`未执行`时，才能判定为通过。整体产品验收还必须遵守以下规则：

- 本地确定性用例必须全部通过。
- 与目标发布平台相符的平台条件用例必须通过。
- 实际启用的 Provider、MCP、Hook、SSH、ACP 等外部能力必须完成对应条件用例。
- 标记为运营观察的用例不能由单次本地运行替代。
- Dynamic Subagent 的 GA 状态仍受独立 Beta 观察门约束。

## 分类与变更等级

| 分类 | 含义 |
| --- | --- |
| 本地确定性 | 不依赖外部服务，可在隔离仓库重复执行 |
| 实时 Provider | 需要真实模型凭据和网络，可能产生费用 |
| 平台条件 | 只适用于指定操作系统、Sandbox 或终端 |
| 安全敏感 | 涉及 Shell、网络、Hook、Full Access、远程系统或凭据边界 |
| 默认关闭 | 需要显式 feature flag 或配置启用 |
| 运营观察 | 需要一段时间的真实负载、告警或发布环境证据 |

变更等级为：`无`、`临时工作区`、`本地持久状态`或`外部系统`。执行前必须确认该等级可接受。

## 能力覆盖矩阵

| 能力 | 分册 | 主要用例 | 条件 |
| --- | --- | --- | --- |
| 安装、版本、帮助、诊断、配置、CLI | [安装、配置与 CLI](installation-configuration-cli.md) | `MA-CLI-*` | 本地；`doctor` 需要凭据 |
| Provider、模型、推理、重试、JSONL、Tool Search | [Provider、模型与韧性](providers-models-resilience.md) | `MA-MODEL-*` | 实时 Provider 为条件项 |
| TUI、会话、树、压缩、主题、状态栏 | [TUI、会话与呈现](tui-sessions-presentation.md) | `MA-TUI-*` | 交互终端 |
| Workspace 工具、Patch、Shell、Git、附件、图片 | [Workspace、工具、Git 与附件](workspace-tools-git-attachments.md) | `MA-WORK-*` | 写/Shell 用例安全敏感 |
| Sandbox、网络、权限与审批 | [安全、Sandbox、权限与审批](security-sandbox-permissions.md) | `MA-SEC-*` | 平台条件与安全敏感 |
| Plan Mode、计划文件与计划审批 | [Plan Mode](plan-mode.md) | `MA-PLAN-*` | 交互终端 |
| Skills、MCP、Extensions、Bundles、Plugins、Hooks | [集成能力](skills-mcp-extensions-plugins-hooks.md) | `MA-INT-*` | 按已配置集成选择 |
| Builtin 与 Dynamic Subagents | [Subagent](builtin-dynamic-subagents.md) | `MA-SUB-*` | Dynamic 默认关闭 |
| Coding Team | [Coding Team](coding-teams.md) | `MA-TEAM-*` | Git HEAD、交互确认 |
| Remote SSH 与 ACP | [远程 SSH 与 ACP](remote-ssh-acp.md) | `MA-REMOTE-*` | 外部系统/ACP 客户端 |
| 事件、Telemetry、恢复与兼容 | [可观测性、恢复与兼容](observability-recovery-compatibility.md) | `MA-OPS-*` | 含自动证据和运营观察 |

## 推荐执行顺序

1. 环境记录、CLI 和配置。
2. 安全/Sandbox 基线。
3. Provider 与 TUI 基础。
4. Workspace、Plan 和集成能力。
5. Subagent 与 Team。
6. SSH/ACP、可观测性和恢复。
7. 汇总证据，保留所有`失败`和`未执行`项，不得用文字结论覆盖原始状态。

## 来源边界

本手册以当前 CLI/config schema、测试和活跃代码规范为准。更详细的协议与安全说明见：[Coding CLI](../coding-cli.md)、[执行安全](../coding-security.md)、[生命周期 Hooks](../coding-hooks.md)、[ACP](../coding-acp.md)、[SSH](../coding-ssh-centos7.md)以及[Dynamic Subagent readiness](../coding-dynamic-subagents-readiness.md)。
