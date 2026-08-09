# Builtin 与 Dynamic Subagent 人工验收

[返回验收索引](index.md) · [证据记录模板](environment-evidence-record.md) · [Beta readiness](../coding-dynamic-subagents-readiness.md)

Dynamic Subagent 是 Coding Agent 应用层能力：定义发现/编译、child Session、审批/问题、私有集成、恢复和发布策略位于 `internal/coding`；通用 `agent` 包只提供 Harness、Tool、Catalog、取消和 usage 等可复用原语。Builtin `explore`、`plan`、`review` 与默认关闭的 custom Agent 共用兼容调度基础，但 custom 能力不是 `agent/team`。

## 能力与非能力边界

当前可验收：Builtin 前台执行；custom profile 发现/校验、直接调用、model 前台/后台调用；并发/次数/turn/token/tool/duration/depth 上限；精确递归；child 独立审批/问题；配置好的 private MCP/Hook；draft 生成与人工 promotion；恢复、kill switch 和无内容 Telemetry。

当前没有实现、不得写成能力或验收通过：等待式调度队列、queue depth、百分比/cohort rollout、第二套 release-stage 配置、本地自动分发参与者、为了准入指标创建的持久化准入事件/占位 Session、profile 内 inline MCP/Hook/凭据/命令，以及跨用户共享 draft/草稿协作。Selected Beta/GA 是运营流程，不是另一套 Runtime stage。

## MA-SUB-001：Builtin explore、plan、review

- 分类：实时 Provider。变更等级：本地持久状态；可能产生费用。
- 前置条件：有效 Provider；`dynamic_subagents = false` 保持默认关闭。
- 隔离夹具：Workspace 含一个小型 Go package 与已知 diff。
- 步骤：让 parent 分别调用 builtin `explore` 查找证据、`plan` 给出实施步骤、`review` 审阅已知 diff；也可通过项目现有 Runtime smoke fixture 验证 legacy `role` wire shape。
- 预期证据：三者均创建独立 child Session，返回各自严格结构结果；explore evidence、plan steps/files、review findings/residual risks 本地校验；builtin 不受 custom gate 关闭影响；child 无 parent transcript/写权限扩张。
- 通过条件：三角色成功且结果 schema/上限正确；parent 只得到有界结果，不得到 hidden reasoning。
- 失败条件：关闭 custom 后 builtin 消失、角色结果串型、child 越权写入、或失败结果被当成功。
- 清理/回滚：等待/取消所有 child 后退出；child Session 随隔离 home 清理。
- 来源/测试锚点：[role.go](../../internal/coding/subagent/role.go)、[manager.go](../../internal/coding/subagent/manager.go)、[role_test.go](../../internal/coding/subagent/role_test.go)。
- 结果：`未执行`。

## MA-SUB-002：默认关闭、发现可见与 Dispatch 禁止

- 分类：本地确定性、默认关闭。变更等级：本地持久状态。
- 前置条件：`${PIPS_ACCEPT_HOME}/agents/go-checker.md` 是合法 custom profile；resolved gate 为 false。
- 隔离夹具：profile 只允许 `read`/`grep`，user/model visibility 为 true。
- 步骤：

  ```bash
  pips config show
  pips agents validate
  pips agents list --all
  pips agents show go-checker
  pips agents run go-checker "inspect fixture"
  ```

- 预期证据：`config show` 显示 false 及来源；validate/list/show 可做静态管理并不输出完整 instruction body；run 在模型调用和 child 创建前被拒绝；profile bytes 不变；builtin 仍可运行。
- 通过条件：gate 只禁止新 custom dispatch/generate-promotion 能力，不删除定义或历史 child。
- 失败条件：默认启用、关闭时仍发 Provider 请求/写 Session、或 rollback 删除定义。
- 清理/回滚：保留 profile 给后续用例；确认无新增 child。
- 来源/测试锚点：[dynamic_subagent_test.go](../../internal/coding/dynamic_subagent_test.go)、[agents.go](../../internal/coding/cli/agents.go)、[readiness rollback](../coding-dynamic-subagents-readiness.md#rollback-drill)。
- 结果：`未执行`。

## MA-SUB-003：Profile schema、优先级与权限子集

- 分类：本地确定性、安全敏感、默认关闭。变更等级：本地持久状态。
- 前置条件：custom gate 可保持关闭做静态校验；项目 root 只在信任后检查。
- 隔离夹具：四个 Agent roots 的同名定义；unknown/duplicate fields、reserved builtin ID、invalid selector、require-not-allowed、unsafe file/symlink、超限 body 等负向定义。
- 步骤：未信任/信任后分别运行 `agents validate` 与 `agents list --all`；检查 precedence；随后开启 gate，用 `agents show` 对照 declared selectors 与 effective plan；要求一个超出 Runtime ceiling 的 tool/Skill/model/private binding。
- 预期证据：优先级为 shared user < Pips user < shared project < Pips project；invalid/suppressed 定义只产生安全诊断；unknown/duplicate 严格拒绝；require 缺失时 admission fail，不静默弱化；effective authority 是 profile 与 trust/mode/Sandbox/catalog/generation 的交集。
- 通过条件：所有恶意定义不能扩大权限或影响合法 sibling；项目未信任不被读取。
- 失败条件：profile 能设 endpoint/credential/env/command/Sandbox、未知 selector 变成通配、或 required 能力缺失仍启动。
- 清理/回滚：移除负向夹具，保留一个合法 profile。
- 来源/测试锚点：[agentprofile](../../internal/coding/agentprofile)、[identity.go](../../internal/coding/subagent/identity.go)、[Coding CLI profile contract](../coding-cli.md#dynamic-custom-agents-alpha)。
- 结果：`未执行`。

## MA-SUB-004：Direct run、临时 Definition 与 Resume

- 分类：实时 Provider、默认关闭。变更等级：本地持久状态；可能产生费用。
- 前置条件：gate 开启；合法 user-visible profile；记录有效配置。
- 隔离夹具：persisted `go-checker` 与显式 `${PIPS_ACCEPT_ROOT}/reviewer.md`。
- 步骤：

  ```bash
  pips --dynamic-subagents agents run go-checker "inspect the fixture"
  pips --dynamic-subagents agents run ad-hoc-check \
    "review the fixture" --definition "${PIPS_ACCEPT_ROOT}/reviewer.md"
  pips --dynamic-subagents agents run go-checker --session "<CHILD_SESSION_ID>" \
    "continue with one bounded check"
  ```

- 预期证据：每次成功输出一行 `pips.coding.agent.run/v1alpha1` JSON envelope，含 agent_id/child_session_id/outcome/code/result；临时 definition 不写入 Agent root、不发布给 parent；resume 只接受同一 profile/Workspace 身份；需审批/问题时非交互退出 3。
- 通过条件：输出可解析、identity 固定、Session 可恢复且无 authority drift。
- 失败条件：临时文件被安装、resume 换 profile/Workspace、stdout 混入 progress/secret、或自动处理 child input。
- 清理/回滚：删除临时 definition；Session/profile 随隔离 home 清理。
- 来源/测试锚点：[agents.go](../../internal/coding/cli/agents.go)、[agents_test.go](../../internal/coding/cli/agents_test.go)、[dispatch.go](../../internal/coding/subagent/dispatch.go)。
- 结果：`未执行`。

## MA-SUB-005：Foreground、Background 与 Child 控制

- 分类：实时 Provider、交互终端、默认关闭。变更等级：本地持久状态；可能产生费用。
- 前置条件：两个 custom profiles 分别允许 foreground/background；parent model 能调用相应工具。
- 隔离夹具：background child 等待一个可控制的测试点，不执行外部副作用。
- 步骤：要求 parent 前台调用一个 child 并等待结果；再启动 background child；打开 `/agents` 的 Runs，查看进度/详情；对 background child 执行 inspect、wait、follow-up、interrupt/cancel 中适用动作；用 Ctrl+L/Library 直接启动 user-visible profile，再 Ctrl+R 返回 Runs。
- 预期证据：foreground 使用 `run_subagent` 串行返回；background 使用 `spawn_agent` 立即返回 stable child id，完成通知 durable 且恰一次；控制明确指向 child，approval/question 只解决该 child；parent/sibling 不受影响；敏感 task/result/reasoning 不进 compact timeline。
- 通过条件：ownership、delivery、通知与控制作用域均准确，取消后 child 终态稳定。
- 失败条件：background 阻塞 parent、通知丢失/重复、控制错 child、child prompt 泄露、或 parent 自动批准。
- 清理/回滚：等待所有 child terminal；关闭 root Runtime 验证 cleanup。
- 来源/测试锚点：[notification.go](../../internal/coding/subagent/notification.go)、[agents_overlay.go](../../internal/coding/tui/agents_overlay.go)、[dynamic_subagent_test.go](../../internal/coding/dynamic_subagent_test.go)。
- 结果：`未执行`。

## MA-SUB-006：并发、Spawn 与执行预算——无队列

- 分类：本地确定性、默认关闭。变更等级：本地持久状态。
- 前置条件：配置较小但有效的 `[subagent]` limits；scripted model 能保持 child 运行。
- 隔离夹具：将 `max_concurrent`、`max_spawned_per_root_interaction`、`max_auto_follow_ups`、turn/token/tool/duration 设为易触发值。
- 步骤：并发启动直到 capacity；额外再启动；触发 spawn limit、turn/token/tool/duration limit；关闭 Runtime 时仍有 child；运行：

  ```bash
  go test ./internal/coding/subagent -run 'Test.*(Admission|Limit|Capacity|Close)' -count=1 -v
  go test -race ./internal/coding/subagent ./internal/coding -run 'Test.*Dynamic' -count=1
  ```

- 预期证据：预算在 child 创建前原子保留；超限立即返回 busy/capacity/spawn_limit/closed/invalid，不等待队列且不创建 placeholder Session；已有 child 继续 authoritative；终态释放 permit 恰一次；invalid config 在 Runtime open 前失败。
- 通过条件：所有上限和 race 测试通过；观察不到排队或 queue-depth 状态。
- 失败条件：额外请求等待/后来自动执行、超限仍创建 Session、permit 泄漏/重复释放、或关闭后可新建 child。
- 清理/回滚：终止 scripted children；恢复 production defaults。
- 来源/测试锚点：[admission.go](../../internal/coding/subagent/admission.go)、[admission_test.go](../../internal/coding/subagent/admission_test.go)、[limits.go](../../internal/coding/subagent/limits.go)。
- 结果：`未执行`。

## MA-SUB-007：精确递归与 Agent-private MCP/Hook

- 分类：实时 Provider、安全敏感、默认关闭。变更等级：本地持久状态与外部系统；可能产生费用。
- 前置条件：`max_depth` 在 1..3；配置并独立信任一个 `agent_private` MCP 和 Hook；profiles 使用 exact IDs。
- 隔离夹具：A 仅可 delegation 到 B；B 不允许回 A；private tool 返回固定信息。
- 步骤：A 前台递归调用 B；尝试 A→未允许 C、B→A cycle、超过 depth、background recursion；验证 parent Tool Search 看不到 private tool，A/B 中只有 profile 精确选择的 child 能看到；变更 private fingerprint 后再启动。
- 预期证据：递归只有 foreground、serial、exact allowlist、cycle/depth checked；recursive child 只收到 `run_subagent` target enum，不收到 `spawn_agent`；private integrations additive 且不替代 ambient policy；missing/stale/untrusted binding 在 child Session/control scope 创建前失败。
- 通过条件：合法 A→B 成功，所有越界路径失败且无 child/connection/lease 泄漏。
- 失败条件：private tool 暴露 parent、profile inline 定义 URL/command/header/env、cycle/深度绕过、或 private Hook 扩权。
- 清理/回滚：关闭 private server，移除 Hook/profile 夹具与 trust。
- 来源/测试锚点：[identity.go](../../internal/coding/subagent/identity.go)、[child scopes](../../internal/coding/child_control_scope.go)、[dynamic_subagent_test.go](../../internal/coding/dynamic_subagent_test.go)。
- 结果：`未执行`。

## MA-SUB-008：Draft 生成、人工审阅与 Promotion

- 分类：实时 Provider、安全敏感、默认关闭。变更等级：本地持久状态；可能产生费用。
- 前置条件：gate 开启；proposal model 可用；user/project scope 分别测试。
- 隔离夹具：一个新 ID、一个已有文件 ID、一个 edit 后非法定义；project scope 先未信任再信任。
- 步骤：执行 `pips agents generate <id> <intent>`；检查完整 Markdown 与 declared/effective authority preview；分别输入 EOF/discard、`edit <file>`、promote；测试已有目标、未信任 project 和 write failure。
- 预期证据：proposal model 只有严格 `propose_agent`，无 Workspace/Shell/MCP/Hook/Skill/Subagent/approval/question/promotion tool；action/path 只能由人输入；promotion digest-bound、no-overwrite、原子写；edited bytes 重新 parse/compile；成功后需下一次 open/reload 才可发现；没有共享 draft store。
- 通过条件：discard/EOF 零写入；合法 promote 写 exact path；所有负向路径保留旧文件且不部分发布。
- 失败条件：模型自行 promote/选路径、覆盖文件、draft 跨进程共享、或当代 generation 立即自加载。
- 清理/回滚：移除仅属于验收的新 profile；保留失败证据摘要。
- 来源/测试锚点：[agent_draft.go](../../internal/coding/agent_draft.go)、[agent_draft_promotion.go](../../internal/coding/agent_draft_promotion.go)、[agents_test.go](../../internal/coding/cli/agents_test.go)。
- 结果：`未执行`。

## MA-SUB-009：Kill switch、Telemetry 与 Beta 运营门

- 分类：默认关闭、运营观察。变更等级：本地持久状态与外部 Telemetry 系统。
- 前置条件：候选 artifact、OTel exporter/dashboard/alert owner、Selected Beta cohort 由发布渠道选择；不是 CLI 自动 rollout。
- 隔离夹具：一个成功/失败/取消/background/depth child 和一次 saturation drill。
- 步骤：验证 `pips.coding.subagent.admissions`、`pips.coding.subagents`、`pips.coding.subagent.duration` 及 approval/question/diagnostic；扫描 attributes 无禁止字段；按 readiness 完整执行 rollback drill；在真实 Beta 窗口保存查询、版本、cohort、模型 mix、样本量、incident exclusions，并执行候选告警演练。
- 预期证据：Telemetry content-free、同步、observer failure 隔离；无 queue metric/准入 durable event；关闭 boolean 后新 custom dispatch/draft promotion 被拒，builtin/历史 Session 保留；Beta SLI/SLO 全满足、无未解决 P0/P1、独立安全/code review 完成，才可提 GA。
- 通过条件：仓库回归、候选 rollback、真实观察窗口与独立审阅全部有权威证据。
- 失败条件：用单元测试代替 Beta 窗口、CLI 自建百分比 cohort、指标含 ID/task/path/command/URL/secret、或 rollback 删除历史。
- 清理/回滚：按 runbook 将 gate 保持 false，撤销高优先级 overrides；保留运营查询和演练记录于发布系统，不提交 secrets。
- 来源/测试锚点：[OTel observer](../../internal/coding/observability/otel/otel.go)、[readiness](../coding-dynamic-subagents-readiness.md)、[rollback regression](../../internal/coding/dynamic_subagent_test.go)。
- 结果：`未执行`。

## 分册判定

Builtin 发布至少通过 `MA-SUB-001`。Custom Alpha 需通过 `002` 至 `008`；Selected Beta 还需完成 `009` 中的候选 artifact/Telemetry/rollback 条件。GA 只有在真实观察窗口和独立审阅完成后才能通过，仓库测试不能代替运营证据。
