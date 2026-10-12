# Builtin 与 Dynamic Subagent 人工验收

[返回验收索引](index.md) · [证据记录模板](environment-evidence-record.md) · [Beta readiness](../coding-dynamic-subagents-readiness.md)

Dynamic Subagent 是 Coding Agent 应用层能力：定义发现/编译、child Session、审批/问题、私有集成、恢复和发布策略位于 `internal/coding`；通用 `agent` 包只提供 Harness、Tool、Catalog、取消和 usage 等可复用原语。Builtin `explore`、`plan`、`review` 与默认开启的 custom Agent 共用兼容调度基础，但 custom 能力不是 `agent/team`。

## 能力与非能力边界

当前可验收：Builtin 前台执行；custom profile 发现/校验、直接调用、model 前台/后台调用；并发/次数/turn/token/tool/duration/depth 上限；精确递归；child 独立审批/问题；配置好的 private MCP/Hook；draft 生成与人工 promotion；role/persona 与 model 优先级；worktree/cwd/resume 隔离与继承；`[subagents]` 配置面与逐类型 toggle；恢复、kill switch 和无内容 Telemetry。

当前没有实现、不得写成能力或验收通过：queue depth、百分比/cohort rollout、第二套 release-stage 配置、本地自动分发参与者、为了准入指标创建的持久化准入事件/占位 Session、`sampling_limit`/`workflow_max_concurrent`、远端 settings 层与 `[features]` 门、`Agent(...)` 形式的 tool allowlist 语法、profile 内 inline MCP/Hook/凭据/命令，以及跨用户共享 draft/草稿协作。Selected Beta/GA 是运营流程，不是另一套 Runtime stage。

## MA-SUB-001：Builtin explore、plan、review

- 分类：实时 Provider。变更等级：本地持久状态；可能产生费用。
- 前置条件：有效 Provider；custom gate 保持默认开启。
- 隔离夹具：Workspace 含一个小型 Go package 与已知 diff。
- 步骤：让 parent 调用 `spawn_subagent`（`run_in_background: false`）分别运行 builtin `explore` 查找证据、`plan` 给出实施步骤、`review` 审阅已知 diff；再省略 `subagent_type` 验证默认解析为 `general-purpose`。
- 预期证据：每次调用创建独立 child Session，默认把最终 assistant 文本作为结果返回（`result` 是字符串）；设置 `[subagents].structured_results = true` 后恢复严格结构结果，explore evidence、plan steps/files、review findings/residual risks 本地校验；`general-purpose` 得到完整可委托目录，`explore`/`plan`/`review` 只得到只读工具（无 `apply_patch`、无 `shell`）；builtin 不受 `dynamic_subagents = false` 影响；child 无 parent transcript/写权限扩张；child 通过流式路径驱动模型。
- 通过条件：三角色成功、结果契约与配置一致、上限正确；parent 只得到有界结果，不得到 hidden reasoning。
- 失败条件：关闭 custom 后 builtin 消失、只读类型拿到 `apply_patch`/`shell`、child 越权写入、或失败结果被当成功。
- 清理/回滚：等待/取消所有 child 后退出；child Session 随隔离 home 清理。
- 来源/测试锚点：[role.go](../../internal/coding/subagent/role.go)、[builtins.go](../../internal/coding/agentprofile/builtins.go)、[subagents_dispatch.go](../../internal/coding/subagent_dispatch.go)、[role_test.go](../../internal/coding/subagent/role_test.go)。
- 结果：`未执行`。

## MA-SUB-002：显式关闭、发现可见与 Dispatch 禁止

- 分类：本地确定性、显式关闭。变更等级：本地持久状态。
- 前置条件：`${PIPS_ACCEPT_HOME}/agents/go-checker.md` 是合法 custom profile；用下列任一层把 gate 显式关闭：`dynamic_subagents = false`、`[subagents] enabled = false`、`PIPS_DYNAMIC_SUBAGENTS=false`、`--dynamic-subagents=false` 或 `--no-subagents`。
- 隔离夹具：profile 只允许 `read`/`grep`，user/model visibility 为 true。
- 步骤：

  ```bash
  pips config show
  pips --no-subagents config show
  pips agents validate
  pips agents list --all
  pips agents show go-checker
  pips agents run go-checker "inspect fixture"
  ```

- 预期证据：`config show` 在 `dynamic_subagents` 与 `subagents.enabled` 两行都显示 false，并标明决定该值的层（文件 / `PIPS_DYNAMIC_SUBAGENTS` / `--no-subagents`）；`[subagents]` 表存在但缺 `enabled` 时两行仍为 true；同一文件里两种写法取值冲突时校验失败并同时点名两个键；validate/list/show 可做静态管理并不输出完整 instruction body；run 在模型调用和 child 创建前被拒绝；profile bytes 不变；builtin 仍可运行。
- 通过条件：gate 只禁止新 custom dispatch/generate-promotion 能力，不删除定义或历史 child。
- 失败条件：默认启用、关闭时仍发 Provider 请求/写 Session、或 rollback 删除定义。
- 清理/回滚：保留 profile 给后续用例；确认无新增 child。
- 来源/测试锚点：[dynamic_subagent_test.go](../../internal/coding/dynamic_subagent_test.go)、[agents.go](../../internal/coding/cli/agents.go)、[readiness rollback](../coding-dynamic-subagents-readiness.md#rollback-drill)。
- 结果：`未执行`。

## MA-SUB-003：Profile schema、优先级与权限子集

- 分类：本地确定性、安全敏感、默认开启。变更等级：本地持久状态。
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

- 分类：实时 Provider、默认开启。变更等级：本地持久状态；可能产生费用。
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

- 分类：实时 Provider、交互终端、默认开启。变更等级：本地持久状态；可能产生费用。
- 前置条件：两个 custom profiles 分别允许 foreground/background；parent model 能调用相应工具。
- 隔离夹具：background child 等待一个可控制的测试点，不执行外部副作用。
- 步骤：要求 parent 前台调用一个 child 并等待结果；再启动 background child；打开 `/agents` 的 Runs，查看进度/详情；对 background child 执行 inspect、wait、follow-up、interrupt/cancel 中适用动作；用 Ctrl+A/Agents 直接启动 user-visible profile，再 Ctrl+R 返回 Runs。
- 预期证据：`spawn_subagent` 以 `run_in_background: false` 串行等待并返回结果 envelope 与 `<subagent_meta>` 文本；默认（`run_in_background` 省略或为 `true`）立即返回 stable child id，完成通知 durable 且恰一次；控制明确指向 child，approval/question 只解决该 child；parent/sibling 不受影响；敏感 task/result/reasoning 不进 compact timeline。
- 通过条件：ownership、delivery、通知与控制作用域均准确，取消后 child 终态稳定。
- 失败条件：background 阻塞 parent、通知丢失/重复、控制错 child、child prompt 泄露、或 parent 自动批准。
- 清理/回滚：等待所有 child terminal；关闭 root Runtime 验证 cleanup。
- 来源/测试锚点：[notification.go](../../internal/coding/subagent/notification.go)、[agents_overlay.go](../../internal/coding/tui/agents_overlay.go)、[dynamic_subagent_test.go](../../internal/coding/dynamic_subagent_test.go)。
- 结果：`未执行`。

## MA-SUB-006：并发、Spawn 与执行预算——queue / fail

- 分类：本地确定性、默认开启。变更等级：本地持久状态。
- 前置条件：配置较小但有效的 `[subagents]` limits；scripted model 能保持 child 运行；分别用 `limit_behavior = "queue"` 与 `"fail"` 各跑一轮。
- 隔离夹具：将 `max_concurrent`、`max_spawned_per_root_interaction`、`max_auto_follow_ups`、turn/token/tool/duration 设为易触发值。
- 步骤：并发启动直到 capacity；额外再启动；触发 spawn limit、turn/token/tool/duration limit；关闭 Runtime 时仍有 child；运行：

  ```bash
  go test ./internal/coding/subagent -run 'Test.*(Admission|Limit|Capacity|Close|Queue)' -count=1 -v
  go test -race ./internal/coding/subagent ./internal/coding -run 'Test.*Dynamic' -count=1
  ```

- 预期证据：foreground 预算在 child 创建前原子保留；`fail` 在 cap 上返回模型可见的 `Concurrent subagent limit reached: {limit} subagents are already running for this session. Do not retry; spawning succeeds again when a running subagent finishes.`；`queue` 按到达顺序排队，slot 释放后启动；background spawn 立即返回 `subagent_id`，其 slot 在 run goroutine 中等待；`max_concurrent` 至少为 1；spawn limit/closed/invalid 仍然同步失败；终态释放 permit 恰一次；`PIPS_SUBAGENT_LIMIT_BEHAVIOR` 覆盖配置，非法值回退 `queue`。
- 通过条件：所有上限和 race 测试通过；观测到 `queued` outcome 但没有 queue-depth 指标。
- 失败条件：`fail` 排队、`queue` 丢失请求、permit 泄漏/重复释放、queue-depth 或 task/child 身份进入遥测、或关闭后可新建 child。
- 清理/回滚：终止 scripted children；恢复 production defaults。
- 来源/测试锚点：[admission.go](../../internal/coding/subagent/admission.go)、[admission_test.go](../../internal/coding/subagent/admission_test.go)、[limits.go](../../internal/coding/subagent/limits.go)。
- 结果：`未执行`。

## MA-SUB-007：Depth 1 与 Agent-private MCP/Hook

- 分类：实时 Provider、安全敏感、默认开启。变更等级：本地持久状态与外部系统；可能产生费用。
- 前置条件：`max_depth` 使用默认 1；配置并独立信任一个 `agent_private` MCP 和 Hook；profiles 使用 exact IDs。
- 隔离夹具：两个 profile 都声明 `delegation`（应被拒绝）或直接尝试嵌套 spawn；private tool 返回固定信息。
- 步骤：确认带 `delegation` frontmatter 的 profile 解析失败；让 child 尝试调用 `spawn_subagent`（其工具集中不应存在该工具）；验证 parent Tool Search 看不到 private tool，只有 profile 精确选择的 child 能看到；变更 private fingerprint 后再启动。
- 预期证据：`max_depth` 默认为 1；child 的工具集中没有 `spawn_subagent`，无法继续委派；`delegation` frontmatter 被作为不支持字段拒绝；private integrations additive 且不替代 ambient policy；missing/stale/untrusted binding 在 child Session/control scope 创建前失败。
- 通过条件：child 无法委派，所有越界路径失败且无 child/connection/lease 泄漏。
- 失败条件：child 收到 delegation 工具、`delegation` frontmatter 被静默接受、private tool 暴露 parent、profile inline 定义 URL/command/header/env、或 private Hook 扩权。
- 清理/回滚：关闭 private server，移除 Hook/profile 夹具与 trust。
- 来源/测试锚点：[identity.go](../../internal/coding/subagent/identity.go)、[child scopes](../../internal/coding/child_control_scope.go)、[dynamic_subagent_test.go](../../internal/coding/dynamic_subagent_test.go)。
- 结果：`未执行`。

## MA-SUB-008：Draft 生成、人工审阅与 Promotion

- 分类：实时 Provider、安全敏感、默认开启。变更等级：本地持久状态；可能产生费用。
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

- 分类：默认开启、运营观察。变更等级：本地持久状态与外部 Telemetry 系统。
- 前置条件：候选 artifact、OTel exporter/dashboard/alert owner、Selected Beta cohort 由发布渠道选择；不是 CLI 自动 rollout。
- 隔离夹具：一个成功/失败/取消/background/depth child 和一次 saturation drill。
- 步骤：验证 `pips.coding.subagent.admissions`、`pips.coding.subagents`、`pips.coding.subagent.duration` 及 approval/question/diagnostic；扫描 attributes 无禁止字段；按 readiness 完整执行 rollback drill；在真实 Beta 窗口保存查询、版本、cohort、模型 mix、样本量、incident exclusions，并执行候选告警演练。
- 预期证据：Telemetry content-free、同步、observer failure 隔离；无 queue metric/准入 durable event；关闭 boolean 后新 custom dispatch/draft promotion 被拒，builtin/历史 Session 保留；Beta SLI/SLO 全满足、无未解决 P0/P1、独立安全/code review 完成，才可提 GA。
- 通过条件：仓库回归、候选 rollback、真实观察窗口与独立审阅全部有权威证据。
- 失败条件：用单元测试代替 Beta 窗口、CLI 自建百分比 cohort、指标含 ID/task/path/command/URL/secret、或 rollback 删除历史。
- 清理/回滚：按 runbook 将 gate 保持 false，撤销高优先级 overrides；保留运营查询和演练记录于发布系统，不提交 secrets。
- 来源/测试锚点：[OTel observer](../../internal/coding/observability/otel/otel.go)、[readiness](../coding-dynamic-subagents-readiness.md)、[rollback regression](../../internal/coding/dynamic_subagent_test.go)。
- 结果：`未执行`。

## MA-SUB-010：Role、Persona 与 model 优先级

- 分类：实时 Provider、默认开启。变更等级：本地持久状态；可能产生费用。
- 前置条件：有效 Provider 与至少两个已配置模型；`dynamic_subagents` 保持默认开启。
- 隔离夹具：`${PIPS_ACCEPT_HOME}/roles/explore.toml`（`prompt_file` 指向同目录的 markdown）、`${PIPS_ACCEPT_HOME}/personas/explore.toml`（`instructions` 与 `outputs`）、以及一个同名 inline `[subagents.roles.explore]` 覆盖。
- 步骤：让 parent 以 `subagent_type: "explore"` 前台委派一次，分别验证只有文件、文件+inline、以及 `model` 指向未配置值三种情形；再让 `default_isolation = "worktree"` 的 role 委派一次。
- 预期证据：child 的 plan 记录 `persona`、`persona_reminder` 与 `reasoning_effort`；system prompt 含 `<role-instructions>` 与 role prompt 正文；child transcript 在 task 之前恰有一条 `<system-reminder>` user 消息；inline 声明覆盖同名文件；`default_capability_mode` 与类型自身 mode 取交集（`explore` 仍是只读）；无法解析的 model 逐级回退并在 integration diagnostic 中报告，最终使用 parent 模型；`worktree` role 的 child 在独立 worktree 中运行，plan 记录 `isolation: "worktree"`。
- 通过条件：优先级与交集正确、persona 只注入一次、model 回退有可见诊断且不静默丢弃设置。
- 失败条件：persona 出现在 spawn schema、role 扩大权限、`worktree` default 被静默忽略或回退到 parent workspace、或未配置 model 导致 spawn 失败。
- 清理/回滚：移除验收用 role/persona 文件与 inline 配置；确认无新增 child。
- 来源/测试锚点：[subagentsprofile](../../internal/coding/subagentprofile)、[subagents_dispatch.go](../../internal/coding/subagent_dispatch.go)、[child_control_scope.go](../../internal/coding/child_control_scope.go)、[subagents_persona_test.go](../../internal/coding/subagent_persona_test.go)。
- 结果：`未执行`。

## MA-SUB-011：Isolation、cwd 与 resume_from

- 分类：实时 Provider、默认开启。变更等级：本地持久状态与 Git ref；可能产生费用。
- 前置条件：Workspace 是已有提交的 Git 仓库；parent 能用 `spawn_subagent` 前台委派。
- 隔离夹具：`${PIPS_ACCEPT_ROOT}` 下的一个 Git 仓库（含一个已提交文件），以及仓库外的一个空目录作为 `cwd`。
- 步骤：
  1. 让 parent 以 `isolation: "worktree"` 前台委派一个 bounded 编辑任务，记录返回的 `worktree_path`。
  2. 让 parent 以 `cwd` 指向仓库外目录委派一次，令 child 写入一个相对路径文件。
  3. 前台委派一个 child 并等其完成，然后用返回的 `subagent_id` 作为 `resume_from` 再委派一次。
  4. 分别用仍处 running 的 `subagent_id`、以及一个不存在的 id 作为 `resume_from` 委派一次。
- 预期证据：worktree child 的编辑只出现在 `git worktree list` 列出的 worktree 目录，parent 工作树保持原样，worktree 目录与其在 `refs/heads/pips/subagent/` 下的分支在 child 结束后保留，结果 envelope 含 `worktree_path`；`cwd` child 的相对写入落在该目录而非 parent workspace；resume child 的 transcript 以 source 的对话前缀开头，`plan.resume_from` 指向 source，system prompt 与 tool config 重新渲染，usage 为沿袭累计值；running、不存在、跨 parent 的 source 都在创建 child 前失败，不留下半成品 child。
- 通过条件：隔离与继承语义可复现，所有失败路径 fail closed。
- 失败条件：worktree 未创建却继续执行、parent 工作树被 child 修改、resume 丢弃 source transcript、resume 接受 running/跨 parent source、或 `cwd` 与 `isolation: "worktree"` 同时生效。
- 清理/回滚：删除验收用 worktree 目录与 `refs/heads/pips/subagent/*` 分支，清理验收用 `cwd` 目录。
- 来源/测试锚点：[subagent_worktree.go](../../internal/coding/subagent_worktree.go)、[teamworktree](../../internal/coding/teamworktree)、[manager_resume.go](../../internal/coding/subagent/manager_resume.go)、[subagent_isolation_test.go](../../internal/coding/subagent_isolation_test.go)。
- 结果：`未执行`。

## MA-SUB-012：`[subagents]` 配置面与逐类型 toggle

- 分类：本地确定性 + 实时 Provider、默认开启。变更等级：本地持久状态；可能产生费用。
- 前置条件：gate 保持默认开启；一个合法 custom profile（`go-checker`）；至少一个 builtin 类型作为对照。
- 隔离夹具：selected user 文件里写 `[subagents] enabled = false`、`[subagents.toggle]`（含一个 builtin、一个 custom、一个不存在的名字），以及指向被关掉类型的 `[subagents.models]` 与 `[subagents.roles]` 各一条。
- 步骤：

  ```bash
  pips config show
  pips --no-subagents config show
  pips --no-subagents --dynamic-subagents=true config show
  pips agents list
  ```

  随后把 `[subagents] enabled` 改回 true、只保留 toggle，重启 Runtime，让 parent 分别以被关掉的 custom 类型与被关掉的 builtin 类型前台委派一次；在 `/agents` 的 Agents 页签查看这两个类型。
- 预期证据：`config show` 逐字段报值与来源，`dynamic_subagents` 与 `subagents.enabled` 同值同源，`subagents.toggle` 按声明显示（含不存在的名字），被关掉类型的 `models`/`roles` 仍被解析并作为声明报出；`[subagents]` 表缺 `enabled` 时不会关闭子代理；同层两种写法冲突、以及 `--no-subagents --dynamic-subagents=true` 都显式失败并点名冲突的键；被关掉的类型委派返回 `Subagent '<type>' is disabled via [subagents.toggle] in config.toml`，不出现在模型可见的 `subagent_type` 列表里，在 `/agents` 显示为 `disabled by configuration`；`pips agents list` 仍按定义文件列出该类型（它不读运行时配置）；开关关闭时 builtin 仍可运行，但被 toggle 关掉的 builtin 不可运行。
- 通过条件：一个开关只有一个解析值且两种写法都可报告；缺 `enabled` 的表不关闭子代理；任何显式设置都不被静默丢弃；被关掉的类型在模型面、`/agents` 与运行入口一致地不可用。
- 失败条件：两种写法冲突时一方静默生效、缺 `enabled` 的表把子代理关掉、toggle 只影响文档而不影响 dispatch、或 `config show` 漏报新字段。
- 清理/回滚：删除验收用 `enabled`/`toggle` 配置并恢复默认；确认无新增 child。
- 来源/测试锚点：[load.go](../../internal/coding/config/load.go)、[config.go](../../internal/coding/config/config.go)、[cli/config.go](../../internal/coding/cli/config.go)、[subagents_dispatch.go](../../internal/coding/subagent_dispatch.go)、[dynamic_subagent_test.go](../../internal/coding/dynamic_subagent_test.go)、[load_test.go](../../internal/coding/config/load_test.go)。
- 结果：`未执行`。

## MA-SUB-013：子代理消息与唤醒（`active_agent_messages`）

- 分类：本地确定性 + 实时 Provider。变更等级：可能产生费用。仅在消息面开启时适用。
- 前置条件：配置文件或环境打开消息面：

  ```toml
  active_agent_messages = true
  ```

- 步骤：

  ```bash
  pips config show
  PIPS_ACTIVE_AGENT_MESSAGES=true pips config show
  ```

  随后在一个会话里依次：(1) 前台委派一个 `general-purpose` 子代理并让它完成，记下它的
  `subagent_id`；(2) 再委派第二个 `general-purpose` 子代理，让它对该 id 调用
  `send_subagent_message`（默认 `delivery: steer`）；(3) 由对话本身对第一个子代理的 id 调用
  `send_subagent_message`；用 `/agents` 的 Runs 页签与任务条观察被唤醒的子代理，并读它自己的
  transcript。再对一个 `read-only` 子代理确认它拿不到该工具，并让一个子代理尝试给自己发消息。
- 预期证据：`config show` 逐字段报出 `active_agent_messages` 的值与来源；gate 关闭时模型可见工具集
  里没有 `send_subagent_message`，调用被拒；子代理发起的唤醒返回
  `{"outcome":"accepted","disposition":"accepted"}`（而不是 `unsupported`）；被唤醒的子代理在
  Runs 页签与自己 transcript 里可见，但不出现在对话时间线上；对话本身发起的唤醒出现在对话时间线上；
  两种唤醒都不再额外抛出一次后台完成通知（对话不会为被唤醒的子代理再收到一条
  `[Pips background Agent completion]`）；被唤醒的子代理继承源子代理的类型与模型；
  `read-only` 子代理的工具集里没有 `send_subagent_message`；子代理给自己发消息返回
  `not_found_or_not_owned`。
- 通过条件：gate 的开关语义不变；子代理发起的唤醒被接受并落在自己的投影里；唤醒只报一次接受，
  不重复投递完成通知；其余 outcome 与配额行为与开启前一致。
- 失败条件：子代理发起的唤醒仍返回 `unsupported`；被唤醒的子代理进入对话时间线导致事件投影不一致；
  唤醒后又收到一次后台完成通知（重复投递）；`read-only` 子代理拿到消息工具。
- 清理/回滚：把 `active_agent_messages` 恢复默认（关闭）；确认无新增 child 与 pending 通知。
- 来源/测试锚点：[message.go](../../internal/coding/subagent/message.go)、[journal.go](../../internal/coding/subagent/journal.go)、[child_control_scope.go](../../internal/coding/child_control_scope.go)、[subagent_message_tool.go](../../internal/coding/subagent_message_tool.go)、[runtime_subagent.go](../../internal/coding/runtime_subagent.go)、[runtime_notification.go](../../internal/coding/runtime_notification.go)、[subagent_message_test.go](../../internal/coding/subagent_message_test.go)、[journal_test.go](../../internal/coding/subagent/journal_test.go)。
- 结果：`未执行`。

## 分册判定

Builtin 发布至少通过 `MA-SUB-001`。Custom Alpha 需通过 `002` 至 `008`、`010` 至 `012`；Selected Beta 还需完成 `009` 中的候选 artifact/Telemetry/rollback 条件。`MA-SUB-013` 覆盖默认关闭的消息面，只在 `active_agent_messages` 开启时适用，因此不进入 Custom Alpha 的必过集；一旦消息面作为发布面提供，它随该面一起验收。GA 只有在真实观察窗口和独立审阅完成后才能通过，仓库测试不能代替运营证据。
