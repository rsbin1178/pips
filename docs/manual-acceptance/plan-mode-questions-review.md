# Plan Mode、问题与审阅人工验收

[返回验收索引](index.md) · [证据记录模板](environment-evidence-record.md)

Plan Mode 是能力边界，不是 secret 隔离。它允许读取可读文件并维护 Runtime 绑定的 Plan 文档，但不能通过 Shell、Patch、MCP/Extension write 或伪造同名工具绕过只读限制。

## MA-PLAN-001：模式进入、退出与工具能力边界

- 分类：实时 Provider、安全敏感。变更等级：本地持久状态；可能产生费用。
- 前置条件：有效 Provider；Workspace 有可读文件和已提交基线。
- 隔离夹具：准备一个模型可能尝试修改的文件。
- 步骤：以 `--mode plan` 启动；用 `/status` 确认；要求读取并制定修改方案，再明确要求直接 patch/shell；用 `/mode` 在 idle 时切回 Agent，先取消再确认。
- 预期证据：Plan Mode 仅暴露只读 workspace/external 能力、path-free `read_plan` 及受控问题/审阅工具；当前流程拒绝 legacy `write_plan`/`submit_plan`，只接受 `plan_checkpoint` 后的 `present_plan`；`apply_patch`、Shell、privileged MCP/Extension 和 same-name forged tools 在 hooks/approval/执行前被拒绝；确认切 Agent 只是 process-local mode change，不自动再跑模型或授权写入。
- 通过条件：模式边界严格、取消不切换、确认后下一轮才按 Agent 能力运行。
- 失败条件：Plan 中发生 Workspace 写入/外部副作用、模型调用触发模式切换、或切换被持久化为更高权限默认。
- 清理/回滚：确认 `git status --short` 与基线一致；退出进程。
- 来源/测试锚点：[planflow](../../internal/coding/planflow)、[runtime_mode_test.go](../../internal/coding/runtime_mode_test.go)、[runtime_plan_test.go](../../internal/coding/runtime_plan_test.go)、[Plan Mode 契约](../coding-cli.md#configuration-trust-and-sessions)。
- 结果：`未执行`。

## MA-PLAN-002：结构化问题、单选/多选与自由文本

- 分类：实时 Provider、交互终端。变更等级：本地持久状态；可能产生费用。
- 前置条件：Plan Mode；任务包含一个会实质改变方案的选择。
- 隔离夹具：例如在两种存储后端中选择，并要求一个可多选的兼容目标。
- 步骤：要求模型在决定前提问；在 UI 中验证单选、多选、推荐项、自由文本入口、上下导航、Space、Enter、Shift+Tab/返回；先 Esc 取消，再重新回答；确认回答后继续。
- 预期证据：问题只包含有界、互斥选项和可选推荐说明；取消保持 checkpoint 锁定且不代答；回答与 exact pending call 绑定并持久化一次；模型收到选择但 UI/Session 不泄露额外内部状态。
- 通过条件：每种输入交互可用，stale/重复回答被拒绝，回答后只继续一次。
- 失败条件：模型自选、Esc 丢失 pending 状态、answer 绑定错误、或重复 continuation。
- 清理/回滚：保留脱敏问题/答案摘要，勿记录业务秘密。
- 来源/测试锚点：[question](../../internal/coding/question)、[question_prompt_test.go](../../internal/coding/tui/question_prompt_test.go)、[runtime_question_test.go](../../internal/coding/runtime_question_test.go)。
- 结果：`未执行`。

## MA-PLAN-003：畸形问题的受控降级

- 分类：本地确定性。变更等级：本地持久状态。
- 前置条件：可运行 scripted-model 测试。
- 隔离夹具：模型依次提交畸形 `ask_user`、畸形 `ask_user_text`，再停止。
- 步骤：

  ```bash
  go test ./internal/coding/planflow -count=1 -v
  go test ./internal/coding -run 'Test.*Question' -count=1 -v
  ```

  复核事件/状态：第一次畸形后仅允许 one-field text retry；再次畸形后由 Runtime 生成自由文本 prompt。
- 预期证据：降级路径有界、确定且仍需要人工输入；checkpoint 不解锁；工具名/参数不可被模型借机扩权；无无限重试。
- 通过条件：测试全通过，状态机只沿规定路径前进。
- 失败条件：畸形输入跳过问题、循环重试、自动填答案或错误关闭 Session。
- 清理/回滚：无外部清理；测试临时目录自动回收。
- 来源/测试锚点：[controller.go](../../internal/coding/planflow/controller.go)、[controller_test.go](../../internal/coding/planflow/controller_test.go)、[question](../../internal/coding/question)。
- 结果：`未执行`。

## MA-PLAN-004：Checkpoint、完整 Plan 呈现与修订

- 分类：实时 Provider、交互终端。变更等级：本地持久状态；可能产生费用。
- 前置条件：`MA-PLAN-002` 已回答所有实质问题。
- 隔离夹具：任务要求至少三个步骤、风险与验收项。
- 步骤：让模型完成 `plan_checkpoint` 后调用 `present_plan`；检查完整 Markdown 和 revision；选择“继续规划/修改”，提出一项具体修改；再次呈现并比较 revision 与文件；最后批准切换 Agent。
- 预期证据：Plan 仅在首次持久化内容时创建于 `${PIPS_ACCEPT_HOME}/plans/<session-id>.md`；路径不由模型选择；每次 `present_plan` 原子替换完整文档并绑定 exact revision；继续规划不会丢失旧内容；批准只在 idle 切模式且不执行 Plan。
- 通过条件：Plan 文件、UI 内容、revision 和 Session pending call 一致，旧/stale approval 无效。
- 失败条件：append 导致残留、模型控制路径、批准自动执行/授权、或修订覆盖失败后仍显示成功。
- 清理/回滚：Plan 随隔离 home 清理；记录摘要而非完整私有方案。
- 来源/测试锚点：[plandoc](../../internal/coding/plandoc)、[planreview](../../internal/coding/planreview)、[plan_review_prompt_test.go](../../internal/coding/tui/plan_review_prompt_test.go)。
- 结果：`未执行`。

## MA-PLAN-005：非交互 input-required

- 分类：实时 Provider。变更等级：本地持久状态；可能产生费用。
- 前置条件：模型稳定地产生问题或 `present_plan` review。
- 隔离夹具：两个新 Session，分别停在 question 和 plan review。
- 步骤：

  ```bash
  pips --workspace "${PIPS_ACCEPT_WORKSPACE}" exec --mode plan \
    "在继续前询问我必须选择的方案，不要替我决定"
  pips --workspace "${PIPS_ACCEPT_WORKSPACE}" exec --session "<PAUSED_ID>" "继续"
  ```

- 预期证据：pending question/review 时退出 3；不会等待 stdin、选推荐项、批准 Plan 或继续 Provider；JSONL 若启用仍是合法安全前缀；Session 可在 TUI 中人工恢复。
- 通过条件：所有需人决策的非交互路径快速、分类正确地失败。
- 失败条件：自动选择、永久挂起、退出 0/1/2/4，或 pending 状态丢失。
- 清理/回滚：在 TUI 中明确解决或随隔离 home 清理 paused Session。
- 来源/测试锚点：[exec.go](../../internal/coding/cli/exec.go)、[exit.go](../../internal/coding/cli/exit.go)、[exec_test.go](../../internal/coding/cli/exec_test.go)。
- 结果：`未执行`。

## MA-PLAN-006：Resume、幂等 reconcile 与降级边界

- 分类：恢复、兼容、安全敏感。变更等级：本地持久状态。
- 前置条件：Session 停在 `present_plan` pending review；保存同 commit 的当前二进制和不理解新 handshake 的旧测试二进制（如有）。
- 隔离夹具：Plan 文件与 Session 都备份摘要，不直接编辑。
- 步骤：强制终止后用当前二进制 `pips resume <id>`；重复关闭/恢复；若发布矩阵含旧版，尝试旧版只读打开并记录拒绝，再回当前版恢复。
- 预期证据：当前版幂等 reconcile 同一个 pending call，不重复创建 Plan/revision；旧版不会把 `present_plan` 当完成或静默降级；回当前版仍可审阅。只有 review 已解决且 interaction settled 后才可安全降级。
- 通过条件：无重复 continuation/Plan 写入，兼容失败明确且数据可恢复。
- 失败条件：Session/Plan 人工修复才可打开、旧版破坏 pending 状态、或恢复自动批准。
- 清理/回滚：不得手改 JSONL/Plan 强制恢复；测试结束后随隔离 home 清理。
- 来源/测试锚点：[bootstrap.go](../../internal/coding/bootstrap.go)、[runtime_plan_review_test.go](../../internal/coding/runtime_plan_review_test.go)、[Plan 恢复说明](../coding-cli.md#configuration-trust-and-sessions)。
- 结果：`未执行`。

## MA-PLAN-007：`/review` 只读变更审阅

- 分类：实时 Provider、安全敏感。变更等级：无；可能产生费用。
- 前置条件：Workspace 有已知的 staged/unstaged/untracked 测试变更；当前为 Plan Mode。
- 隔离夹具：包含一个明显 correctness issue 和一个无关安全文件名，内容无秘密。
- 步骤：执行 `/review`；观察模型使用只读 Git/Workspace 能力；在 Agent Mode 再尝试 `/review` 负向检查。
- 预期证据：Plan Mode 产出针对 correctness/security/tests 的有证据审阅，不修改文件；Agent Mode 命令显示 only-in-Plan 错误；Git inspector 不执行 repo-controlled helper。
- 通过条件：报告能对应已知夹具，工作区摘要不变，命令模式限制准确。
- 失败条件：审阅修改文件、虚构变更、泄露不相关文件内容，或 Agent Mode 仍执行该命令。
- 清理/回滚：销毁变更夹具或恢复到初始 commit。
- 来源/测试锚点：[command_picker.go](../../internal/coding/tui/command_picker.go)、[changes/git](../../internal/coding/changes/git)、[print_test.go](../../internal/coding/tui/print_test.go)。
- 结果：`未执行`。

## 分册判定

全部七项必须通过。任何 Plan 写越界、自动代答/审批、stale revision 被接受或恢复时重复副作用都直接阻断发布。
