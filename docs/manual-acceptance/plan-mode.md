# Plan Mode 人工验收

[返回验收索引](index.md) · [证据记录模板](environment-evidence-record.md)

Plan Mode 是「状态机 + Session 私有计划文件」的模型，不是受限工具集。常规 Tool catalog、Shell、MCP、Subagent 与 `ask_user` 全部保留；唯一的限制是编辑门控：`apply_patch` 的目标只要不全是 `plan.md` 就会在执行前被拒绝。Plan Mode 是能力边界，不是 secret 隔离；Pips 进程可读的文件仍然可读。

计划文件位于 `$PIPS_HOME/sessions/<session-id>/plan.md`，目录权限 0700、文件权限 0600、无元数据头；模型按提示词中给出的绝对路径用 `apply_patch` 写入，plan 状态另存于同目录的 `plan-mode.json`。

## MA-PLAN-001：进入/退出与状态机

- 分类：交互终端。变更等级：本地持久状态。
- 前置条件：有效配置；已信任的隔离 Workspace；记录基线 `git status --short`。
- 隔离夹具：一个模型可能尝试修改的源文件。
- 步骤：用 `/plan` 或 `Shift+Tab` 打开 plan mode，先不发送 prompt，确认停在 `Pending`；发送首个 prompt 观察进入 `Active`；再让模型显式调用 `enter_plan_mode`，分别用批准和拒绝各跑一次；最后分别在 idle 和回合进行中按 `Shift+Tab` 关闭 plan mode。
- 预期证据：`Inactive → Pending → Active` 与 `Active → ExitPending → Inactive` 都按状态机收敛；`enter_plan_mode` 批准后跳过 `Pending` 直接进入 `Active` 并返回 "You have entered plan mode. You should now focus on exploring the codebase and creating an implementation plan."，拒绝时返回 "The user declined to enter plan mode. Continue in normal mode without it." 且状态不变；idle 关闭立即回到 `Inactive`，回合中关闭先进入 `ExitPending`；每个切换产生 `plan_mode.changed`，可见 mode 翻转时另有 `mode.changed`。
- 通过条件：四种状态的进入、保持与退出都能从 UI、事件和 `plan-mode.json` 三方对上，没有隐式切换。
- 失败条件：`Pending` 被跳过（除 `enter_plan_mode` 批准路径外）、批准后仍停留在 plan、回合中关闭立即解锁编辑、或状态在重启后与磁盘不一致。
- 清理/回滚：`git status --short` 与基线一致；退出进程。
- 来源/测试锚点：[planmode.go](../../internal/coding/planmode/planmode.go)、[runtime_plan.go](../../internal/coding/runtime_plan.go)、[planmode_test.go](../../internal/coding/planmode/planmode_test.go)、[Plan Mode 契约](../coding-cli.md#configuration-trust-and-sessions)。
- 结果：`未执行`。

## MA-PLAN-002：编辑门控与拒绝文本

- 分类：实时 Provider、安全敏感。变更等级：临时工作区；可能产生费用。
- 前置条件：`MA-PLAN-001` 通过；Workspace 有已提交基线。
- 隔离夹具：一个普通源文件与一个无关文件。
- 步骤：在 `Active` plan mode 中依次要求模型 patch 普通源文件、patch 计划文件本身、在一个批次的 patch 里同时改计划文件与工作区文件，并用 Shell 重定向写文件。
- 预期证据：非计划文件目标的 patch 在执行前被拒绝，模型收到的原文是 `Rejected: file edits are not allowed in plan mode - the only editable file is the plan file (<plan path>).`；混合目标的整个 patch 被拒绝且什么都不写入；计划文件写入自动批准，不进入审批、不进入工作区变更跟踪、不改变 `git status --short`；Shell 不被检查，仍可运行；子代理不继承门控。
- 通过条件：门控只拒绝编辑而不隐藏工具，拒绝发生在 hooks/审批/归因/执行之前。
- 失败条件：普通文件被写入、拒绝发生在执行之后、拒绝文本与约定不一致、或计划文件写入出现在 `git status`/变更跟踪里。
- 清理/回滚：恢复 Workspace 到基线；确认计划文件随隔离 `PIPS_HOME` 清理。
- 来源/测试锚点：[runtime_plan_gate.go](../../internal/coding/runtime_plan_gate.go)、[apply_patch.go](../../internal/coding/tools/apply_patch.go)、[plan_file_test.go](../../internal/coding/tools/plan_file_test.go)。
- 结果：`未执行`。

## MA-PLAN-003：计划文件写入与审批视图

- 分类：实时 Provider、交互终端。变更等级：本地持久状态；可能产生费用。
- 前置条件：`MA-PLAN-002` 通过。
- 隔离夹具：一个要求多步骤、含风险与验收项的任务。
- 步骤：让模型按提示词给出的绝对路径写计划文件；调用 `exit_plan_mode`；在审批视图中滚动预览、按 `y` 复制、用 `Tab` 在预览与输入之间切换、按 `Esc` 返回；再对一个尚未写计划的情况重复一次。
- 预期证据：文件确实位于 `$PIPS_HOME/sessions/<session-id>/plan.md`，权限 0700/0600，无元数据头，内容与审批视图完全一致（运行时读盘，计划内容从不作为工具参数传入）；空计划显示 `No plan written yet.` 与 Approve/Request changes/Quit 说明，而不是空白预览；审批视图打开期间状态为 paused，`Esc` 返回不改变 plan 状态。
- 通过条件：磁盘内容、审批视图内容和批准后模型收到的结果三者一致。
- 失败条件：模型可以自选路径、审批视图展示的不是磁盘内容、空计划显示为空预览、或 `Esc` 后状态被改变。
- 清理/回滚：计划文件随隔离 `PIPS_HOME` 清理；记录摘要而非完整私有方案。
- 来源/测试锚点：[store.go](../../internal/coding/planmode/store.go)、[plan_review.go](../../internal/coding/tui/plan_review.go)、[plan_review_prompt_test.go](../../internal/coding/tui/plan_review_prompt_test.go)。
- 结果：`未执行`。

## MA-PLAN-004：请求修改、评论与放弃

- 分类：实时 Provider、交互终端。变更等级：本地持久状态；可能产生费用。
- 前置条件：`MA-PLAN-003` 通过。
- 隔离夹具：一个至少有三级标题、便于选中行区间评论的计划。
- 步骤：在审批视图中用 `c` 对选中行或行区间加评论，检查 `a` 的标签变为 `approve w/ comments` 并批准；再发起一次，用 `s` 输入修订说明并发送；模型据此更新计划后再次 `exit_plan_mode`，这次用 `q` 放弃并确认。
- 预期证据：批准且带评论时模型收到 "Your plan has been approved. You can now start coding." 并追加 `The user approved the plan with the following review comments:` 与逐行评论，随后在同一回合继续实施；请求修改时模型收到 "The user does not want to exit plan mode. Continue planning and ask the user what they would like to do."、追加 `User revision notes:` 与 notes，且 plan mode 保持 `Active`；放弃时收到 abandon 文本并关闭 plan mode。
- 通过条件：三种决定互不混淆，评论/说明与实际输入逐字一致，只有批准会解锁实施。
- 失败条件：请求修改后解锁编辑、放弃后仍在 plan mode、评论丢失或错位、或批准后必须由用户再发一条 prompt 才会继续实施。
- 清理/回滚：`git status --short` 与基线一致；退出进程。
- 来源/测试锚点：[controller.go](../../internal/coding/planreview/controller.go)、[prompt.go](../../internal/coding/planmode/prompt.go)、[runtime_plan_gate.go](../../internal/coding/runtime_plan_gate.go)。
- 结果：`未执行`。

## MA-PLAN-005：非交互 fail closed

- 分类：实时 Provider。变更等级：本地持久状态；可能产生费用。
- 前置条件：计划文件与 Session 均由非交互进程创建或恢复。
- 隔离夹具：三个新 Session，分别停在 `enter_plan_mode` 审批、`exit_plan_mode` 计划审批与 `ask_user` 提问。
- 步骤：

  ```bash
  pips --workspace "${PIPS_ACCEPT_WORKSPACE}" exec --mode plan \
    "先调用 enter_plan_mode，再写计划并调用 exit_plan_mode"
  pips --workspace "${PIPS_ACCEPT_WORKSPACE}" exec --mode plan \
    "先把方案写进计划文件，然后调用 exit_plan_mode"
  pips --workspace "${PIPS_ACCEPT_WORKSPACE}" exec --mode plan \
    "在继续前用 ask_user 询问我必须选择的方案"
  ```

- 预期证据：需要用户决策时立即以退出码 3 失败，不等待 stdin、不自动批准、不自动选择、不继续向 Provider 发请求；`enter_plan_mode` 审批不会让进程停在 plan mode 继续执行，`exit_plan_mode` 审批不会自动采纳计划；stdout 若启用 JSONL 仍是合法安全前缀；Session 可在 TUI 中人工恢复并正常展示待决策视图。
- 通过条件：所有需要人工决定的非交互路径都快速、分类正确地失败。
- 失败条件：自动代答/自动批准、永久挂起、退出 0/1/2/4，或 pending 状态丢失。
- 清理/回滚：在 TUI 中明确解决或随隔离 `PIPS_HOME` 清理 paused Session。
- 来源/测试锚点：[exec.go](../../internal/coding/cli/exec.go)、[exit.go](../../internal/coding/cli/exit.go)、[reducer.go](../../internal/coding/reducer.go)。
- 结果：`未执行`。

## MA-PLAN-006：重启恢复与 compaction

- 分类：恢复、本地确定性。变更等级：本地持久状态。
- 前置条件：`MA-PLAN-001` 与 `MA-PLAN-003` 通过。
- 隔离夹具：一次在 `Active`（计划已写、待审批）时强杀进程，一次在 `Pending` 与 `ExitPending` 时强杀进程。
- 步骤：强杀后用 `pips resume <id>` 重新打开；核对 `plan-mode.json` 与 `plan.md`；在 `Active` 会话中执行 `/compact`；再次核对状态与提醒。
- 预期证据：只有 `Active` 会被恢复，`Pending` 与 `ExitPending` 在加载时折叠为 `Inactive`；`plan.md` 内容不丢且不被截断（进入 `Active` 只在文件不存在时写入空文件）；compaction 保留 plan 状态并在压缩后的上下文里重新注入计划提醒；计划内容不进入 transcript 或稳定 system prefix。
- 通过条件：重启与压缩都不改变已持久化的计划，也不产生幽灵门控。
- 失败条件：`Pending`/`ExitPending` 被恢复成 `Active`、计划文件被截断/重建、compaction 后门控消失或提醒缺失、或需要人工手改 Session JSONL 才能恢复。
- 清理/回滚：不要手改 JSONL 或计划文件强制恢复；随隔离 `PIPS_HOME` 清理。
- 来源/测试锚点：[store.go](../../internal/coding/planmode/store.go)、[runtime.go](../../internal/coding/runtime.go)、[planmode_test.go](../../internal/coding/planmode/planmode_test.go)。
- 结果：`未执行`。

## MA-PLAN-007：`/view-plan`、状态栏与转录行

- 分类：交互终端。变更等级：本地持久状态。
- 前置条件：存在一份已保存的计划；当前不处于审批视图。
- 隔离夹具：无秘密计划内容。
- 步骤：依次执行 `/view-plan`、`/show-plan`、`/plan-view`；滚动预览并复制；比较执行前后的 plan 状态；进入与退出 plan mode 时观察状态栏与转录。
- 预期证据：三个命令打开同一份计划预览且不改变 plan 状态、不触发模型请求；状态栏在 plan mode 期间显示 `plan` 标志；转录出现 `Agent entered plan mode`、`file edits outside session plan.md blocked until plan mode exits`、`Plan mode off`；私有计划路径不出现在对话、managed footer 或状态栏。
- 通过条件：查看计划与实际状态一致，别名行为一致，转录行语义准确。
- 失败条件：查看计划导致状态变化、预览内容过期、或状态栏/转录缺失或泄露私有路径。
- 清理/回滚：关闭预览并退出进程。
- 来源/测试锚点：[command_picker.go](../../internal/coding/tui/command_picker.go)、[plan_review.go](../../internal/coding/tui/plan_review.go)、[scrollback.go](../../internal/coding/tui/scrollback.go)、[model.go](../../internal/coding/tui/model.go)。
- 结果：`未执行`。

## MA-PLAN-008：计划模式中的结构化提问与取消

- 分类：实时 Provider、交互终端。变更等级：本地持久状态；可能产生费用。
- 前置条件：`MA-PLAN-001` 通过。
- 隔离夹具：一个会实质改变方案的二选一问题，外加一个可多选的兼容目标。
- 步骤：要求模型在写计划前用 `ask_user` 提问；验证单选、多选、选项说明、自由文本入口、上下导航、Space、Enter；先 Esc 取消一次，再重新提问并回答；确认回答后继续。
- 预期证据：问题只包含有界、互斥的 2–4 个选项与可选推荐说明；取消不代答、不改变 plan 状态；回答与 exact pending call 绑定并只持久化一次，模型收到选择但 UI/Session 不泄露额外内部状态；畸形 `ask_user` 只返回有界错误，不会把模型没要求过的自由文本提示伪装成用户输入。
- 通过条件：每种输入方式都可用，stale/重复回答被拒绝，回答后只继续一次。
- 失败条件：模型自选、Esc 丢失 pending 状态、回答绑定错误、或同一问题重复 continuation。
- 清理/回滚：保留脱敏的问题/答案摘要，勿记录业务秘密。
- 来源/测试锚点：[controller.go](../../internal/coding/question/controller.go)、[runtime_question_test.go](../../internal/coding/runtime_question_test.go)、[question_prompt_test.go](../../internal/coding/tui/question_prompt_test.go)。
- 结果：`未执行`。

## MA-PLAN-009：`/review` 只读变更审阅

- 分类：实时 Provider、安全敏感。变更等级：无；可能产生费用。
- 前置条件：Workspace 有已知的 staged/unstaged/untracked 测试变更；当前为 plan mode。
- 隔离夹具：包含一个明显 correctness issue 和一个无关安全文件名，内容无秘密。
- 步骤：执行 `/review`；观察模型使用只读 Git/Workspace 能力；在 Agent Mode 再尝试 `/review` 负向检查。
- 预期证据：plan mode 产出针对 correctness/security/tests 的有证据审阅，不修改文件；Agent Mode 命令显示 only-in-Plan 错误；Git inspector 不执行 repo-controlled helper。
- 通过条件：报告能对应已知夹具，工作区摘要不变，命令模式限制准确。
- 失败条件：审阅修改文件、虚构变更、泄露不相关文件内容，或 Agent Mode 仍执行该命令。
- 清理/回滚：销毁变更夹具或恢复到初始 commit。
- 来源/测试锚点：[command_picker.go](../../internal/coding/tui/command_picker.go)、[changes/git](../../internal/coding/changes/git)、[print_test.go](../../internal/coding/tui/print_test.go)。
- 结果：`未执行`。

## 分册判定

全部九项必须通过。任何 plan 写越界、自动代答/审批、计划路径由模型选择、`Pending`/`ExitPending` 在重启后被恢复成 `Active`，或拒绝文本与约定不一致，都直接阻断发布。
