# Skills、MCP、Extensions、Bundles、Plugins 与 Hooks 人工验收

[返回验收索引](index.md) · [证据记录模板](environment-evidence-record.md)

只执行发布实际启用的集成用例，但未启用项必须记为`不适用`并说明产品是否仍对外声明该能力。Go Extensions 是 embedding/composition-root 能力，不是 CLI 安装系统；Bundle 只能选择已注册 Extension，不能加载代码。

## MA-INT-001：Skill 发现、优先级与显式调用

- 分类：实时 Provider、安全敏感。变更等级：本地持久状态；可能产生费用。
- 前置条件：准备同名 Skill 于 shared user、Pips user、shared project、Pips project 四个 root；项目尚未信任。
- 隔离夹具：每个 `SKILL.md` 有不同无秘密标记；一个 invalid Skill 与一个含 text/binary/symlink resource 的 Skill。
- 步骤：未信任时启动并检查 `/skills`；信任后 `/reload`；验证优先级 `project/.pips > project/.agents > user/.pips > user/.agents > Bundle/Extension`；用 Space/Enter 禁用/启用；在 Composer 用 `$name` 选择并提交；输入未知 `$HOME`。
- 预期证据：未信任不检查项目 root；winner/diagnostic 稳定；policy 写入 untracked、mode 0600 的 `.pips/skills.toml` 且只存 opaque identity；`$name` 只影响当前 interaction，原用户消息不变；未知 token 原样保留；script/resource 不自动变成工具或执行。
- 通过条件：发现、信任、切换、精确调用、资源安全与 generation 更新均正确。
- 失败条件：项目未信任即读取、disabled Skill 仍注入、symlink 跟随、脚本自动执行、或 policy 污染 `.gitignore`。
- 清理/回滚：移除夹具 Skill/policy；共享 root 夹具应使用专用测试账号。
- 来源/测试锚点：[resource](../../internal/coding/resource)、[route_skills.go](../../internal/coding/tui/route_skills.go)、[Agent Skills contract](../../.trellis/spec/backend/coding-application.md#skill-bundle-and-mcp-integrations)。
- 结果：`未执行`。

## MA-INT-002：MCP 定义、连接、权限与 Reload

- 分类：实时 Provider、安全敏感。变更等级：本地持久状态与外部系统。
- 前置条件：受控 MCP stdio server 使用绝对普通可执行文件，或受控 HTTPS Streamable HTTP server；工具只返回固定数据。
- 隔离夹具：用户 `${PIPS_ACCEPT_HOME}/mcp.json` 和项目 `.pips/mcp.json`；包含一个有效 server、一个连接失败 server、一个 unknown-field/duplicate-ID 负向夹具。
- 步骤：验证 user server 可连接；项目未信任时不读取；信任后项目 server 为 pending，通过 TUI 分别 deny/allow exact fingerprint；调用工具；修改 definition 后 `/reload`；制造 list-changed refresh 失败。
- 预期证据：schema 为 `pips.mcp/v1alpha1`；项目批准同时写严格 `.pips/permissions.toml` 与用户 Workspace record，缺一/stale 均 pending；失败 server 只禁用自身；reload 原子发布新 generation，失败保留旧 registry；stdio 子环境不含 secrets/proxy/agent/OTel/Pips 变量。
- 通过条件：连接、工具调用、双记录 permission、隔离失败与 reload 均符合契约；重复 ID/authority 字段失败关闭。
- 失败条件：项目自动启用、one-sided record 生效、shell command 拼接、HTTP auth/env inline 被接受、或失败清空所有 server。
- 清理/回滚：关闭 server；撤销人工 permission；确认 private temp/child process 已清理。
- 来源/测试锚点：[mcp](../../internal/coding/mcp)、[mcpstdio](../../internal/coding/execution/mcpstdio)、[MCP contract](../../.trellis/spec/backend/coding-application.md#skill-bundle-and-mcp-integrations)。
- 结果：`未执行`。

## MA-INT-003：Extension 生命周期与不可变 Generation

- 分类：本地确定性、embedding 条件。变更等级：无。
- 前置条件：只适用于嵌入 `agent/extension` 的应用/测试；普通 `pips` CLI 没有安装任意 Go Extension 的命令。
- 隔离夹具：仓库的测试 Extension，贡献一个工具/Skill/hook，并可注入 Prepare/Start/Stop failure。
- 步骤：

  ```bash
  go test ./agent/extension -count=1 -v
  go test ./internal/coding -run 'Test.*IntegrationGeneration' -count=1 -v
  ```

  复核 activation、Acquire/Release、replacement 与 Shutdown 的生命周期顺序和 snapshot。
- 预期证据：只有预注册、能力满足的 Extension 可激活；候选 generation 成功后原子发布；已有 interaction 持有旧 immutable generation；失败 candidate 不污染 active；最后 lease release 后才 stop/cleanup。
- 通过条件：测试全通过，生命周期恰一次且错误合并/回滚完整。
- 失败条件：Bundle 加载 Go code、失败 candidate 部分发布、running interaction 被 reload 改写、或 cleanup 泄漏。
- 清理/回滚：测试进程退出即释放；无用户文件安装动作。
- 来源/测试锚点：[extension](../../agent/extension)、[integration_generation_test.go](../../internal/coding/integration_generation_test.go)、[Extension spec](../../.trellis/spec/backend/extension-bundle.md)。
- 结果：`未执行`。

## MA-INT-004：Bundle 选择、信任与资源边界

- 分类：本地确定性、安全敏感。变更等级：本地持久状态。
- 前置条件：应用 composition root 已注册测试 Extension；用户 `${PIPS_ACCEPT_HOME}/bundles` 与 trusted project `.pips/bundles` 可用。
- 隔离夹具：合法 `pips.bundle/v1alpha1` manifest、未知 Extension、duplicate ID、symlink escape、超限资源夹具。
- 步骤：未信任时确认 project Bundle 不打开；信任后 reload；运行：

  ```bash
  go test ./agent/bundle -count=1 -v
  go test ./internal/coding/resource -count=1 -v
  ```

- 预期证据：Bundle 只提供 bounded metadata/resource 与已注册 Extension ID 选择；不运行命令、不连接 MCP、不加载代码；project trust、schema、filter、duplicate、resource/symlink/limit 错误失败关闭且不污染旧 generation。
- 通过条件：合法选择正确，所有负向夹具按类型拒绝，空选择不隐式变成“全部”。
- 失败条件：manifest 获得代码/命令权限、未信任读取、symlink 逃逸、或失败 reload 清空 active generation。
- 清理/回滚：移除 Bundle 夹具，退出 runtime 释放资源。
- 来源/测试锚点：[bundle](../../agent/bundle)、[resource](../../internal/coding/resource)、[Bundle spec](../../.trellis/spec/backend/extension-bundle.md)。
- 结果：`未执行`。

## MA-INT-005：Agent Plugin 发现与校验

- 分类：本地确定性、安全敏感。变更等级：本地持久状态。
- 前置条件：在 `${PIPS_ACCEPT_HOME}/plugins/<id>/` 创建 portable package；项目 plugin 仅在 trusted Workspace 测试。
- 隔离夹具：合法 root `plugin.json`、可选 `skills/`、`mcp.json`；另备 manifest 错误、一个坏 Skill、一个坏 MCP sibling。
- 步骤：

  ```bash
  pips plugin list
  pips plugin list --json
  pips plugin validate "${PIPS_ACCEPT_HOME}/plugins/<id>"
  pips plugin validate "${PIPS_ACCEPT_HOME}/plugins/<id>" --json
  ```

  对每个负向夹具重复 validate，再启动/reload Runtime 检查有效组件投影。
- 预期证据：text/JSON 显示 portable identity、root、Skill/MCP counts 与有界 diagnostics；manifest 错误使 validate 失败；单个坏组件不删除有效 sibling；插件不提供 registry/install/enable/upgrade/rollback/logs/remove 或可执行 capability manifest。
- 通过条件：CLI 结果稳定、JSON 可解析、隔离诊断正确、项目包受 Workspace trust 控制。
- 失败条件：无效组件导致越界加载、legacy executable target 被接受、或 CLI 暗示不存在的生命周期操作。
- 清理/回滚：移除夹具 plugin 目录；不修改 marketplace/registry，因为该能力不存在。
- 来源/测试锚点：[agentplugin](../../internal/coding/agentplugin)、[plugin.go](../../internal/coding/cli/plugin.go)、[Agent Plugins 说明](../coding-cli.md#agent-plugins)。
- 结果：`未执行`。

## MA-INT-006：Hook 发现、精确 Trust 与 Host Authority

- 分类：安全敏感。变更等级：本地持久状态与临时工作区。
- 前置条件：阅读[Hooks 指南](../coding-hooks.md)；脚本只写验收 Workspace 中的固定日志；确认其在 Coding Sandbox/approval 外以用户权限运行。
- 隔离夹具：用户与项目 `hooks.json`，至少一个 `SessionStart` 和一个 `PreToolUse` handler。
- 步骤：项目未信任时 `pips hooks list`；信任后 `list --json`；执行 `pips hooks trust <exact-ref>`，先拒绝，再 `--yes` 明确信任；修改 command/timeout 并重新列出；触发对应 event。
- 预期证据：trust 前打印 source/event/matcher/command/timeout/fingerprint/cwd/host warning，但不执行；项目未信任不读取；定义语义变化后旧 fingerprint 失效；输入只经 stdin JSON，动态值不插入 shell command；日志触发次数准确。
- 通过条件：Workspace trust 与 Hook trust 独立、精确绑定，未信任/changed handler 不执行。
- 失败条件：仅信任 Workspace 即运行、命令变化沿用旧 trust、prompt 插入 command string、或 Hook 被 Sandbox 保护的错误安全假设掩盖。
- 清理/回滚：撤销/移除夹具定义与 trust store，删除固定日志，确认无 handler process。
- 来源/测试锚点：[hooks](../../internal/coding/hooks)、[hooks.go](../../internal/coding/cli/hooks.go)、[Hooks 指南](../coding-hooks.md)。
- 结果：`未执行`。

## MA-INT-007：Hook 控制语义、超时与 Child Scope

- 分类：实时 Provider、安全敏感。变更等级：临时工作区；可能产生费用。
- 前置条件：`MA-INT-006` 通过；脚本可返回严格 JSON。
- 隔离夹具：分别配置 block、additional_context、updated_input、permission allow/deny/defer、post-tool replacement、stop continuation、timeout/exit failure；另有 agent-private child hook。
- 步骤：逐事件触发 `UserPromptSubmit`、`PreToolUse`、`PermissionRequest`、`PostToolUse`、`Pre/PostCompact`、`SubagentStart/Stop`、`Stop`；检查冲突决定、顺序和次数；运行：

  ```bash
  go test ./internal/coding/hooks ./internal/coding -run 'Test.*Hook' -count=1 -v
  ```

- 预期证据：deny 胜出；PreTool updated input 仍经过工具验证/approval/Sandbox；PostTool block 不声称撤销已发生副作用；Stop follow-up 有上限且防递归；ambient 先于 child private；private `PermissionRequest allow` 只按已定义安全语义处理；超时/异常不会泄露 stdin/stderr。
- 通过条件：每个 event 仅产生规定控制效果，bounded output/context、顺序、timeout 和 child ownership 正确。
- 失败条件：Hook 直接扩权、PostTool 回滚假象、无限 continuation、private handler 影响 parent/sibling，或异常绕过 guards。
- 清理/回滚：移除所有 handler、日志和人工 trust；确认 Session 正常关闭。
- 来源/测试锚点：[runtime_hooks.go](../../internal/coding/runtime_hooks.go)、[runtime_hooks_test.go](../../internal/coding/runtime_hooks_test.go)、[Hook 控制协议](../coding-hooks.md#handler-responses)。
- 结果：`未执行`。

## 分册判定

实际发布中存在的每类集成都必须通过其用例；未暴露 Go Extension/Bundle 的普通 CLI 可将 `003`/`004` 标记为`不适用`，但不得因此在产品声明中宣传可安装 Extension。任何未信任执行、secret 透传、symlink escape 或 Hook 扩权均阻断发布。
