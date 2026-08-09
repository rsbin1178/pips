# 人工验收环境与证据记录

[返回验收索引](index.md)

复制本页或将以下模板粘贴到独立验收记录中。不要把 API key、Authorization header、MCP 参数、Hook 输入、完整提示词或用户文件内容提交到仓库。

## 环境记录模板

```text
验收批次 ID：
验收人：
开始/结束时间（含时区）：
源码 commit：
pips version 输出：
Go 版本：
操作系统/架构：
终端与版本：
Workspace 路径（可脱敏）：
Workspace 初始 Git HEAD：
Workspace 初始是否干净：
PIPS_HOME 路径（可脱敏）：
配置文件路径：
Provider/模型：
Sandbox 模式及实际 backend：
Network 模式：
Approval 模式：
启用的 feature flags：
启用的 Skills/MCP/Plugins/Hooks：
外部服务/远程主机范围：
已知环境限制：
```

## 创建隔离环境

分类：本地确定性。变更等级：临时工作区与本地持久状态。

在源码根目录执行：

```bash
PIPS_ACCEPT_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/pips-accept.XXXXXX")"
PIPS_ACCEPT_HOME="${PIPS_ACCEPT_ROOT}/home"
PIPS_ACCEPT_WORKSPACE="${PIPS_ACCEPT_ROOT}/workspace"
mkdir -p "${PIPS_ACCEPT_HOME}" "${PIPS_ACCEPT_WORKSPACE}"
git -C "${PIPS_ACCEPT_WORKSPACE}" init
git -C "${PIPS_ACCEPT_WORKSPACE}" config user.name "Pips Acceptance"
git -C "${PIPS_ACCEPT_WORKSPACE}" config user.email "pips-acceptance@example.invalid"
printf '%s\n' '# Acceptance workspace' >"${PIPS_ACCEPT_WORKSPACE}/README.md"
git -C "${PIPS_ACCEPT_WORKSPACE}" add README.md
git -C "${PIPS_ACCEPT_WORKSPACE}" commit -m "acceptance fixture"
export PIPS_HOME="${PIPS_ACCEPT_HOME}"
```

记录 `PIPS_ACCEPT_ROOT` 的实际值。`PIPS_HOME` 必须是干净的绝对路径；它隔离原生 Pips 状态，但共享 `~/.agents/skills` 与 `~/.agents/agents` 仍可能被发现，因此在证据中记录共享资源，或在没有共享资源的专用测试账号中执行。

如果使用已安装二进制，请先记录 `command -v pips` 和 `pips version`。如果直接从源码验收，以下文档中的 `pips` 可替换为：

```bash
go run ./cmd/pips
```

替换时应保持命令其余参数不变。

## 用例结果模板

```text
用例 ID：
结果：通过 | 失败 | 未执行 | 不适用
执行时间：
适用性理由：
实际命令/UI 动作：
退出码：
关键可见结果：
证据文件或截图引用：
清理结果：
偏差/缺陷链接：
```

## 证据最低要求

- CLI：完整命令、退出码、经过脱敏的 stdout/stderr。
- TUI：开始状态、关键动作后的状态和终态截图或终端录屏；不得包含秘密。
- 文件：执行前后路径、权限、摘要或 Git diff；敏感内容只记录摘要。
- 外部服务：服务名称、目标范围、请求时间和脱敏结果；不要记录凭据。
- 恢复：中断点、重启命令、恢复后的状态和是否发生重复副作用。
- 运营观察：查询、时间范围、版本/cohort、样本量、排除项和告警演练记录。

## 清理规则

先退出所有 Pips/Worker/SSH/ACP 进程，再记录以下检查：

```bash
git -C "${PIPS_ACCEPT_WORKSPACE}" status --short
find "${PIPS_ACCEPT_HOME}" -maxdepth 2 -type f -print
printf '验收根目录：%s\n' "${PIPS_ACCEPT_ROOT}"
```

本手册不会给出基于未复核变量的递归删除命令。保存证据后，确认打印路径确实是本次 `mktemp` 创建的目录，再通过文件管理器或明确的安全流程移入回收站。

## 汇总模板

| 用例 ID | 结果 | 证据 | 备注 |
| --- | --- | --- | --- |
|  | 未执行 |  |  |

```text
适用用例总数：
通过：
失败：
未执行：
不适用：
分册结论：通过 | 不通过 | 尚未完成
```

只要存在`失败`或适用的`未执行`，结论就不能是`通过`。
