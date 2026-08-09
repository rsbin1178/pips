# Coding Team 人工验收

[返回验收索引](index.md) · [证据记录模板](environment-evidence-record.md)

Coding Team 是 `internal/coding` 应用层把 `agent/team` 的 durable coordination 原语与 Coding Session、Worker model/Harness、Git Worktree、审批、恢复和 Integration 组合后的能力。它不是 Dynamic Subagent：Team 有共享任务 DAG/邮箱/Worker Worktree 与显式 Integration；Subagent 是 parent-owned 的一次专门委派。

所有 Team 用例要求 Workspace 已有初始 commit。严禁在真实脏仓库尝试 cleanup/integration。

## MA-TEAM-001：Proposal、修订与人工 Admission

- 分类：实时 Provider、交互终端、安全敏感。变更等级：本地持久状态；可能产生费用。
- 前置条件：Git Workspace 有 HEAD；有效 Provider；Runtime idle。
- 隔离夹具：一个可拆成两个独立任务的目标；分别准备 clean 和预先 dirty 状态。
- 步骤：执行 `/team <objective>`；审阅成员、任务、依赖和 ownership；先要求修订/取消；重新 propose 并确认。clean repo 选择 clean admission；dirty repo 验证只能显式选择 HEAD-only 或取消。尝试 `pips exec` 发起 Team。
- 预期证据：普通 Prompt/model 无 Team admission authority；proposal 是 process/session-bound typed preview；确认前不建 Team/Worker/Worktree；repo state/revision 变化使 stale confirmation 失效；非交互返回 exit 3，不代选 clean/HEAD-only。
- 通过条件：只有 exact 当前 proposal + Workspace snapshot + 人工 admission 可创建 Team。
- 失败条件：模型自确认、取消仍建资源、dirty 自动忽略、无初始 commit 仍 admission，或 exec 代选。
- 清理/回滚：取消未确认 proposal；确认无 Team 目录/worktree/refs。
- 来源/测试锚点：[team_proposal_agent.go](../../internal/coding/team_proposal_agent.go)、[team_admission_test.go](../../internal/coding/team_admission_test.go)、[Team TUI spec](../../.trellis/spec/backend/team-tui.md)。
- 结果：`未执行`。

## MA-TEAM-002：Task DAG、Worker Session 与公平调度

- 分类：实时 Provider、安全敏感。变更等级：本地持久状态与临时 Worktree；可能产生费用。
- 前置条件：`MA-TEAM-001` 通过；proposal 含独立任务和依赖任务。
- 隔离夹具：每个 Worker ownership 不重叠；配置可观测的小并发上限。
- 步骤：确认 Team；观察无依赖任务按并发上限启动，依赖任务等待；检查每个 Worker stable identity、Session、Attempt 和 task claim/start/finish；制造多个可运行任务检查 FIFO/work-conserving；关闭/reopen 面板。
- 预期证据：每个 Attempt 有独立 Worker Session/owner generation；dependency 未满足不启动；同 identity key 不并发冲突；capacity 满时公平等待属于 Team scheduler（与 Subagent 无队列边界不同）；TUI 投影不暴露 child session/internal path。
- 通过条件：任务状态/DAG/调度与持久资源一致，无重复 claim/start。
- 失败条件：依赖越过、同一 Task 双 owner、UI 状态领先 durable state、或 parent Session 被当 Worker。
- 清理/回滚：等待 Team terminal 或在专门 cancel 用例处理；勿手工删 worktree。
- 来源/测试锚点：[team_scheduler.go](../../internal/coding/team_scheduler.go)、[team_scheduler_test.go](../../internal/coding/team_scheduler_test.go)、[agent/team](../../agent/team)。
- 结果：`未执行`。

## MA-TEAM-003：Attempt Worktree、Ownership 与 Capture

- 分类：平台条件、安全敏感。变更等级：临时 Worktree 与 Git refs。
- 前置条件：Git executable 与 repo identity 稳定；Team Work 已启动。
- 隔离夹具：Worker 修改各自 Worktree 的固定文件并提交/完成；另制造 dirty、identity drift、lock/branch/ref mismatch。
- 步骤：检查 Worktree 在 Pips-owned root、locked 且从 exact base OID 创建；Worker 完成后检查 capture/result ref/commit/tree；分别触发负向 identity 情况；运行：

  ```bash
  go test ./internal/coding/execution/gitcontrol ./internal/coding/teamworktree -count=1 -v
  go test -race ./internal/coding/execution/gitcontrol ./internal/coding/teamworktree -count=1
  ```

- 预期证据：使用固定 argv Git control，不调用 shell/checkout/reset/clean/force remove/prune/branch -D；capture 只在 clean/known identity；dirty/drift 返回 retained recovery evidence，不销毁资源；exact result ref 在 cleanup 后仍可读。
- 通过条件：正向 capture 完整、负向 fail closed、测试/race 通过。
- 失败条件：force cleanup、base/owner mismatch 仍捕获、repo-controlled hooks/helpers 执行、或证据 ref 丢失。
- 清理/回滚：只通过 Team cleanup flow；保留异常对象到 recovery/cleanup 用例。
- 来源/测试锚点：[teamworktree](../../internal/coding/teamworktree)、[gitcontrol](../../internal/coding/execution/gitcontrol)、[team-runtime spec](../../.trellis/spec/backend/team-runtime.md)。
- 结果：`未执行`。

## MA-TEAM-004：Worker 审批、问题、消息与控制

- 分类：实时 Provider、交互终端、安全敏感。变更等级：临时 Worktree；可能产生费用。
- 前置条件：至少两个 active Workers；一个停在 approval，一个停在 structured question。
- 隔离夹具：approval 只操作 Worker Worktree；question 使用无秘密选择。
- 步骤：Team panel 中选择 exact Worker/Task；分别 resolve approval、answer/reject question；发送 message、follow-up；interrupt attempt；对另一个 Worker 验证状态不变；制造 response-delivery unknown 后尝试重复响应。
- 预期证据：每个 control 有 stable command ID、target/revision/owner generation；stale/duplicate 被拒绝；Worker approval 只授权该 child operation；question 只回答该 child；delivery unknown 要求人工复核，不自动重发；敏感 prompt/command/result 不进 Team telemetry/timeline。
- 通过条件：所有 control 精确作用于目标且 durable lifecycle 显示 pending/applied/rejected/unknown。
- 失败条件：控制串 Worker、parent grant 复用、unknown 自动重试、或取消 Team 被小写/普通输入误触。
- 清理/回滚：使 Workers 到 terminal；保留 control IDs 的脱敏记录。
- 来源/测试锚点：[teamcontrol](../../internal/coding/teamcontrol)、[team_interaction.go](../../internal/coding/tui/team_interaction.go)、[team_control_lifecycle_test.go](../../internal/coding/team_control_lifecycle_test.go)。
- 结果：`未执行`。

## MA-TEAM-005：取消、失败、Retry 与重启 Recovery

- 分类：恢复、安全敏感。变更等级：本地持久状态与临时 Worktree。
- 前置条件：可在 claim/start/work/capture 多个点做受控中断；操作均可验证且无真实外部副作用。
- 隔离夹具：一个失败 Task、一个 interrupted Work、一个已完成 Task。
- 步骤：分别取消 task、取消 Team、retry eligible failed Task；在 Work 阶段 kill 后 resume；Recovery UI 先取消，再明确选择“恢复但不重试”或“恢复并显式重试 interrupted Work”；重复 resume 检查幂等。
- 预期证据：已完成 Work 不重放；只有 eligible failed Task 可 retry；interrupted Work 的恢复与 retry 是两个决定；recovery token/process-local confirmation 不能复用；orphan running child 终结为 interrupted；资源 identity 变化保留证据。
- 通过条件：每个 crash window 收敛到唯一 durable 状态，无重复提交/消息/Tool side effect。
- 失败条件：resume 自动重试、取消后继续 Worker、新 attempt 复用旧 owner generation、或未知资源被删。
- 清理/回滚：完成 recovery 后进入显式 cleanup；不手改 Team/Session JSON。
- 来源/测试锚点：[team_recovery.go](../../internal/coding/team_recovery.go)、[team_resume.go](../../internal/coding/team_resume.go)、[route_team_integration_test.go](../../internal/coding/tui/route_team_integration_test.go)。
- 结果：`未执行`。

## MA-TEAM-006：Integration Preview、Apply、Reject 与 Recovery

- 分类：安全敏感、恢复。变更等级：临时 Workspace、Integration Worktree 与 Git commit。
- 前置条件：一个或多个 captured result refs；parent Workspace 基线已记录。
- 隔离夹具：一组无冲突结果、一组冲突结果、parent unrelated dirty changes。
- 步骤：在 Team route 按 `g` 准备 Integration；审阅 selected refs、base/target tree、changed paths、unrelated changes、token/revision；先 reject；重新 prepare 后 apply；制造 apply crash window并走 recover/rollback；尝试 stale token 与 preview 后 Workspace 变化。
- 预期证据：prepare 无 parent mutation；apply 只消费 exact process-local approval；stale/revision/identity/unrelated-overlap 失败关闭；固定 git plumbing 创建 Integration commit/tree 后才更新 parent；冲突/中断返回 retained 或 rolled-back evidence；不使用 model/Shell 修冲突。
- 通过条件：成功结果与 preview 完全对应；reject 零写入；所有 crash/recovery 幂等且不覆盖 unrelated changes。
- 失败条件：未确认 apply、preview 后变更仍套用、冲突自动丢弃、force reset/clean，或 result ref 选择被模型更改。
- 清理/回滚：只用 typed Integration recovery/cleanup；记录最终 parent HEAD/status。
- 来源/测试锚点：[team_integration.go](../../internal/coding/team_integration.go)、[teamintegration](../../internal/coding/teamintegration)、[team_integration_test.go](../../internal/coding/team_integration_test.go)。
- 结果：`未执行`。

## MA-TEAM-007：显式 Cleanup 与保留策略

- 分类：安全敏感、恢复。变更等级：Worktree/lease 清理。
- 前置条件：分别准备 clean terminal resource、dirty resource、identity-drift resource、另一个 live Team resource。
- 隔离夹具：每类资源有 exact owner/identity evidence。
- 步骤：从 Team UI 发起 cleanup，先检查预览/确认；clean resource 清理；对 dirty/drift 执行并观察 retained/orphaned；并发 Close/cleanup；检查另一个 Team 不受影响。
- 预期证据：cleanup 只删除 exact owned clean Worktree/lease，不删 result refs；dirty/conflicted/changed/unknown 保留并报告人工恢复信息；没有 recursive broad deletion、force remove 或 prune；并发调用只有一个 cleanup owner，其他等待同结果。
- 通过条件：clean 对象完全清理且证据保留；异常对象不被破坏；跨 Team 隔离。
- 失败条件：无需确认、force 清理、误删其他资源、清理失败仍标 completed，或孤儿进程/lock 未报告。
- 清理/回滚：异常资源由人工按记录处理；删除前再次核对 exact path/type/owner/inode，手册不提供宽泛递归删除命令。
- 来源/测试锚点：[team_cleanup.go](../../internal/coding/team_cleanup.go)、[cleanup.go](../../internal/coding/teamworktree/cleanup.go)、[Team TUI cleanup](../../.trellis/spec/backend/team-tui.md)。
- 结果：`未执行`。

## MA-TEAM-008：Team 投影、Telemetry 与关闭收敛

- 分类：运营观察、安全敏感。变更等级：无。
- 前置条件：OTel test exporter；覆盖成功/失败/取消/中断/Integration/control。
- 隔离夹具：人工 private path/task/result/child ID 测试串。
- 步骤：观察 TUI compact/detail/status；收集 `pips.coding.teams`、duration、controls、integrations 指标；扫描属性；在 active Team 时关闭 root Runtime，多次 Close。
- 预期证据：UI 提供 worker/task/state/tool count/duration 等有界事实但不暴露 private paths、child Session、prompt/result/reasoning；Telemetry 只有 bounded state/action/outcome/code；Close 禁止新 work、取消/等待 owned work、flush terminal state并 cleanup 一次。
- 通过条件：内容最小化、生命周期计数与 durable state 一致、关闭无 goroutine/resource leak。
- 失败条件：敏感维度进入指标、重复 terminal、Close 返回后仍启动 Worker、或 cleanup owner 竞争。
- 清理/回滚：关闭 exporter 和 Runtime；保存脱敏聚合结果。
- 来源/测试锚点：[team_projection.go](../../internal/coding/tui/team_projection.go)、[OTel observer](../../internal/coding/observability/otel/otel.go)、[Team lifecycle spec](../../.trellis/spec/backend/coding-application.md#coding-team-interactive-boundary)。
- 结果：`未执行`。

## 分册判定

声明支持 Coding Team 的发布必须通过全部八项。任何自动 admission/integration/retry/cleanup、跨 Worker 授权、force Git 操作或覆盖 unrelated changes 均阻断发布。
