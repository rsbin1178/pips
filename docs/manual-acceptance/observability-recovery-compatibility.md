# 可观测性、恢复与兼容人工验收

[返回验收索引](index.md) · [证据记录模板](environment-evidence-record.md)

本分册验证跨能力的“失败之后发生什么”以及可见信号是否安全。自动测试是必要证据，但运营观察、真实进程中断和版本兼容不能只靠单元测试代替。

## MA-OPS-001：Coding Event 编码、顺序与安全投影

- 分类：本地确定性、安全敏感。变更等级：无。
- 前置条件：Go 测试环境；准备人工 secret/path/command/reasoning 测试串。
- 隔离夹具：覆盖 session/interaction/tool/approval/question/plan/subagent/team/diagnostic 事件。
- 步骤：

  ```bash
  go test ./internal/coding -run 'Test.*(Event|Projection|Writer|Publisher)' -count=1 -v
  go test -race ./internal/coding -run 'Test.*EventHub' -count=1
  ```

  另执行一次 `pips exec --output jsonl`，逐行解析并扫描禁止串。
- 预期证据：sealed event taxonomy 严格验证 type/payload；sequence 单调、clone/observer 隔离；content projection 与 safe projection 按消费方区分；JSONL schema 固定且不含 raw tool args/results/reasoning/secret/internal paths。
- 通过条件：测试/race 通过、实际 JSONL 可解析且扫描无泄露。
- 失败条件：unknown payload 接受、observer 可 mutate、sequence 重复倒退、或安全投影包含禁止内容。
- 清理/回滚：销毁含人工测试串的输出。
- 来源/测试锚点：[event.go](../../internal/coding/event.go)、[event_codec.go](../../internal/coding/event_codec.go)、[event_projection.go](../../internal/coding/event_projection.go)。
- 结果：`未执行`。

## MA-OPS-002：OTel Metrics/Trace 与 Observer 故障隔离

- 分类：本地确定性、运营观察、安全敏感。变更等级：外部 Telemetry 系统。
- 前置条件：in-memory exporter 与发布环境 exporter；应用负责 SDK/batching/retention/shutdown。
- 隔离夹具：覆盖成功/失败/取消、usage、approval/question、diagnostic、subagent admission/lifecycle、Team/control/integration。
- 步骤：运行 `go test ./internal/coding/observability/... -count=1 -v`；采集真实候选运行；枚举 metric/span 名、attributes/cardinality；注入 observer/exporter failure。
- 预期证据：计数/时长/usage 与事件一致；维度只有 bounded categories，不含 profile/session/tool-call/root ID、prompt/result/path/command/URL/header/env/secret；Pips 不自建 exporter goroutine/queue；observer panic/error 被隔离并产生有界 diagnostic，不改变 Runtime 结果。
- 通过条件：自动与真实 exporter 证据一致，cardinality/隐私审计通过，shutdown flush 由应用完成。
- 失败条件：Telemetry 影响主流程、含内容维度、重复计数、或文档声称 Pips 自带后台 export queue。
- 清理/回滚：关闭 exporter/provider；按组织策略删除人工敏感测试数据。
- 来源/测试锚点：[observability](../../internal/coding/observability)、[OTel observer](../../internal/coding/observability/otel/otel.go)、[otel_test.go](../../internal/coding/observability/otel/otel_test.go)。
- 结果：`未执行`。

## MA-OPS-003：Session 单写者、截断/损坏与恢复

- 分类：恢复、安全敏感。变更等级：本地持久状态。
- 前置条件：复制隔离 Session fixture 后再注入故障，绝不编辑真实 Session。
- 隔离夹具：正常 Session、尾部截断、invalid event、mode 非 0600、同时 open、unmatched interaction start。
- 步骤：正常 resume；第二进程并发 resume；逐个打开损坏夹具；权限过宽夹具；对 unmatched interaction 启动恢复；运行 `go test ./internal/coding/session ./internal/coding -run 'Test.*(Bootstrap|Resume|Interrupted|Lock)' -count=1 -v`。
- 预期证据：single-writer lock 阻止并发；Darwin/Linux JSONL 必须 0600 且给出 manual chmod guidance，不自动改；invalid history fail closed；可识别 unmatched work terminalize interrupted，不重放/fabricate prompt；修复/append 前重检文件 identity。
- 通过条件：正常可恢复、每个损坏条件结果确定且不覆盖原始证据。
- 失败条件：忽略 corruption、自动 chmod/删/复制广权限文件、双写、或 unmatched work 自动执行。
- 清理/回滚：只销毁复制夹具；真实格式问题需保留证据并人工决策。
- 来源/测试锚点：[session](../../internal/coding/session)、[bootstrap.go](../../internal/coding/bootstrap.go)、[bootstrap_test.go](../../internal/coding/bootstrap_test.go)。
- 结果：`未执行`。

## MA-OPS-004：跨 Subagent/Team/Integration 的崩溃收敛

- 分类：恢复、安全敏感。变更等级：本地持久状态、Worktrees 与 Git refs。
- 前置条件：完成 `MA-SEC-005`、`MA-SUB-005/006`、`MA-TEAM-005/006`；使用 scripted side-effect counters。
- 隔离夹具：child-first/parent-index crash、background completion notification crash、Worker work/capture crash、Integration apply crash。
- 步骤：在每个持久化边界终止进程并重复 open/resume 两次；核对 child journal、parent index、notification、Team resources、refs/commits 与 counters；运行相关 recovery/race tests。
- 预期证据：child journal 为 authoritative，缺 parent records 按序补齐；orphan running child 变 interrupted；terminal notification 恰一次；Team/Integration 使用 exact resource/revision recovery，不重复 Work/apply；cleanup 不销毁 unknown evidence。
- 通过条件：每个 crash point 最终收敛到唯一状态，重复恢复幂等，side-effect count 符合人工选择。
- 失败条件：自动重放、重复通知/commit、lost child、跨 generation 恢复、或 unknown resource force cleanup。
- 清理/回滚：完成 typed recovery/cleanup 后再处理 retained fixture；记录 exact identity。
- 来源/测试锚点：[subagent journal](../../internal/coding/subagent/journal.go)、[team_recovery.go](../../internal/coding/team_recovery.go)、[teamintegration](../../internal/coding/teamintegration)。
- 结果：`未执行`。

## MA-OPS-005：配置、Wire 与 Profile 兼容/迁移

- 分类：本地确定性、兼容。变更等级：本地持久状态。
- 前置条件：保存上一支持版本和候选版本；使用复制的 fixture state。
- 隔离夹具：legacy config keys/env/flags、builtin subagent records、旧 Plan handshake、Agent profile v1alpha1、ACP stable-v1 fixtures、Session/Team history。
- 步骤：候选读取受支持旧状态；明确验证 removed `--provider`、`--model-api`、`PIPS_PROVIDER`、`PIPS_MODEL_API`、`[model]`、旧 `api`/request 字段报 migration error；运行 config/profile/event/ACP compatibility tests；做前后版本 open/resume matrix。
- 预期证据：只承诺的旧记录可读；removed surface 不静默 normalize；unsupported schema/version fail closed 且原始文件不变；Dynamic bool default-off、builtin legacy wire 与 stable ACP v1 保持；pending `present_plan` 不允许不理解它的旧版破坏。
- 通过条件：兼容矩阵每项有结果与恢复路径，无 silent authority change/data loss。
- 失败条件：旧配置变成不同 provider/protocol、unknown field 忽略、旧版写坏新状态、或测试 fixture 与实际 release artifact 不一致。
- 清理/回滚：销毁复制 state；不要用旧版写真实新格式数据。
- 来源/测试锚点：[config load tests](../../internal/coding/config/load_test.go)、[agentprofile tests](../../internal/coding/agentprofile/load_test.go)、[ACP wire tests](../../internal/coding/acp/wire_test.go)。
- 结果：`未执行`。

## MA-OPS-006：进程信号、Close、资源回收与退出码

- 分类：平台条件、安全敏感。变更等级：临时本地/远端资源。
- 前置条件：覆盖 idle、Provider streaming、Shell、MCP、Subagent background、Team Worker、SSH、ACP active 场景。
- 隔离夹具：每类操作都有可检测 child process/temp/socket/worktree/lock。
- 步骤：分别正常 close、SIGINT、SIGTERM、broken stdout/disconnect；并发调用 Close；等待 bounded cleanup；检查 exit codes、resume hint、进程/FD/socket/temp/lease。
- 预期证据：停止 admission、取消/等待 owned work、flush durable terminal state、reverse-order cleanup 且一次；取消给 bounded 10 秒 Runtime cleanup；130/143 准确；broken pipe 走 cleanup 不变 141；只有 clean resumable close 打印 exact resume hint；cleanup failure join 主错误并返回非 0。
- 通过条件：无 goroutine/process/resource leak，terminal 恢复，退出码和 durable 状态一致。
- 失败条件：Close 返回后仍有新工作/孤儿、重复 cleanup、错误被吞、或失败仍打印成功 resume hint。
- 清理/回滚：逐类检查 exact owned 资源；不要用宽泛进程杀戮/递归删除代替缺陷记录。
- 来源/测试锚点：[runtime.go](../../internal/coding/runtime.go)、[run.go](../../internal/coding/tui/run.go)、[exit.go](../../internal/coding/cli/exit.go)。
- 结果：`未执行`。

## MA-OPS-007：发布观察、告警与回滚证据

- 分类：运营观察。变更等级：外部发布/Telemetry 系统。
- 前置条件：候选 artifact、发布负责人、回滚负责人、真实选定 cohort、保存查询/告警渠道；只适用于待 Beta/GA 能力。
- 隔离夹具：计划的 saturation、provider incident annotation、observer failure 与 rollback drill。
- 步骤：在约定真实观察窗口统计支持负载 admission、terminal success、unexpected reject/interruption、cleanup/private binding、observer health、P95 duration；执行告警演练和候选 artifact kill-switch rollback；独立 reviewer 检查安全/code 与竞品官方资料时效。
- 预期证据：查询含时区/窗口/version/cohort/model mix/sample/exclusions；所有目标满足且 P0/P1 为 0；告警实际触发到 owner；rollback 后新能力关闭而历史/兼容能力保留；审批记录明确。
- 通过条件：运营负责人和独立 reviewer 均签署，且没有用本地测试/短 smoke 替代时间窗口。
- 失败条件：窗口未到、样本不足却判 GA、指标缺失/含秘密、rollback 未演练、未解决 P0/P1，或竞品比较使用过期二手信息。
- 清理/回滚：若任一失败，维持/恢复 default-off；保存发布系统证据，不向仓库提交秘密。
- 来源/测试锚点：[Dynamic readiness](../coding-dynamic-subagents-readiness.md)、[observability](../../internal/coding/observability)、组织发布治理记录（外部证据）。
- 结果：`未执行`。

## MA-OPS-008：仓库质量门与文档/实现一致性

- 分类：本地确定性。变更等级：无。
- 前置条件：目标 commit 已冻结；工作树中的用户变更已识别并排除。
- 隔离夹具：无。
- 步骤：

  ```bash
  go test ./... -count=1
  go test -race ./internal/coding/... ./agent/... -count=1
  go vet ./...
  git diff --check
  ```

  另检查所有手册相对链接、命令帮助、config/env/flag/schema 名称；把文档“当前没有实现”列表与代码搜索/测试复核。
- 预期证据：命令全通过；无死链；用例 ID 唯一；所有公开声明都有 source/test anchor；不存在把 test-only、embedding-only、operational gate 写成普通 CLI 已实现能力。
- 通过条件：质量门与覆盖审计均通过，例外有 owner/理由/截止时间。
- 失败条件：测试/lint/link 错误、文档引用不存在 symbol/file、或能力边界漂移。
- 清理/回滚：无；保存 CI/job URL 与 commit SHA。
- 来源/测试锚点：[项目规范](../../.trellis/spec)、[Coding tests](../../internal/coding)、本目录[索引](index.md)。
- 结果：`未执行`。

## 分册判定

`MA-OPS-001` 至 `006` 和 `008` 是发布工程门；`007` 对需要 Beta/GA 运营批准的能力是强制门。任何内容泄露、自动重放、兼容数据损坏或 cleanup 泄漏都判不通过。
