# 安全、Sandbox、权限与审批人工验收

[返回验收索引](index.md) · [证据记录模板](environment-evidence-record.md) · [安全模型](../coding-security.md)

本分册包含高风险负向测试。只能在一次性 Workspace、专用测试账号或外层容器/VM 中执行。`workspace-write` 的 HOME 可读性不构成秘密隔离；不得用真实凭据文件验证“是否能读到”。

## MA-SEC-001：原生 Sandbox 能力探测

- 分类：平台条件、安全敏感。变更等级：无。
- 前置条件：目标发布平台原生 runner；Linux/WSL2 有 root-owned、非 setuid、版本不低于 0.8.0 的 Bubblewrap；macOS 有系统 `/usr/bin/sandbox-exec`。
- 隔离夹具：有效配置、人工 `API_KEY`、干净 Workspace。
- 步骤：以默认 `workspace-write` 执行 `pips doctor`；记录平台/backend/version/filesystem/network/process 探测。分别在发布矩阵的 macOS/Linux amd64/arm64 原生环境重复。
- 预期证据：真实 namespace/mount/PID/network/filesystem/seccomp 或 Seatbelt capability probe 全部通过；缺依赖、版本过低、namespace 被容器策略阻断时失败关闭，不降级为无 Sandbox。
- 通过条件：每个声明支持的平台都有本机证据；单机 `doctor` 不代替其他 runner。
- 失败条件：只检查版本即判成功、探测失败仍启动 Shell、或自动改主机设置。
- 清理/回滚：不修改主机 Sandbox 安装；若环境不满足，记录为失败/不适用并交由平台管理员处理。
- 来源/测试锚点：[execution backend](../../internal/coding/execution)、[doctor.go](../../internal/coding/cli/doctor.go)、[安全平台要求](../coding-security.md#platform-requirements)。
- 结果：`未执行`。

## MA-SEC-002：Read-only 与 Workspace-write 文件边界

- 分类：实时 Provider、安全敏感、平台条件。变更等级：临时工作区。
- 前置条件：`MA-SEC-001` 通过；外层隔离环境中准备 Workspace 内外各一个人工 sentinel。
- 隔离夹具：Workspace 已提交；外部 sentinel 不含秘密；记录两个摘要。
- 步骤：在 `--sandbox read-only --approval never` 下要求 Shell 写 Workspace；在 `--sandbox workspace-write` 下写 Workspace；再尝试写 Workspace 外、`.git`、`${PIPS_ACCEPT_HOME}` 和系统临时位置。
- 预期证据：read-only 写入被策略/Sandbox 拒绝；workspace-write 只允许 Workspace 基线写入；`.git`、Pips 状态和未获准外部目录保持保护；拒绝均无部分副作用。
- 通过条件：允许/拒绝矩阵与模式一致，外部 sentinel/Pips 状态摘要不变。
- 失败条件：read-only 可写、外部/`.git` 可写、Backend 失败后直接执行，或审批可绕过 protected path。
- 清理/回滚：恢复 Workspace 夹具；删除外部人工 sentinel；检查无遗留 mount/process。
- 来源/测试锚点：[policy.go](../../internal/coding/execution/policy.go)、[path_policy_test.go](../../internal/coding/execution/path_policy_test.go)、[executor_test.go](../../internal/coding/execution/executor_test.go)。
- 结果：`未执行`。

## MA-SEC-003：Network deny、on-request 与 allow

- 分类：实时 Provider、安全敏感、平台条件。变更等级：外部系统。
- 前置条件：受控 HTTP 测试 endpoint，只返回固定文本；Provider 自身网络不等同于 Shell child network。
- 隔离夹具：配置 `[sandbox_workspace_write] network = "deny"`、`"on-request"`、`"allow"` 的三个文件；`approval = "on-request"`。
- 步骤：让 Shell 对受控 endpoint 发一个 GET。deny 下执行；on-request 下先拒绝、再允许一次；allow 下执行。记录 endpoint 请求计数。
- 预期证据：deny 不出现审批且 endpoint 计数为 0；on-request 显示绑定目标 operation 的审批，拒绝后为 0，允许后恰有一次；allow 无额外 network 扩权审批。所有模式都不能访问宿主 Unix socket；`NetworkNone` 仅允许 private Plan root 内本地 IPC。
- 通过条件：网络 enforcement 与配置一致，无请求走漏、无跨命令授权复用。
- 失败条件：deny 仍出站、拒绝后服务收到请求、一次授权扩大到其他 host/argv，或 host socket 可达。
- 清理/回滚：关闭测试 endpoint，撤销临时网络规则，记录最终请求数。
- 来源/测试锚点：[policy.go](../../internal/coding/execution/policy.go)、[backend_linux_test.go](../../internal/coding/execution/backend_linux_test.go)、[安全文件与网络契约](../coding-security.md#files-credentials-and-home)。
- 结果：`未执行`。

## MA-SEC-004：审批 Allow once、Allow session 与 Deny

- 分类：实时 Provider、安全敏感。变更等级：临时工作区。
- 前置条件：`workspace-write`、`approval = "on-request"`；准备两个不同 fingerprint 的安全外部写操作。
- 隔离夹具：外部目录位于一次性验收根目录，且不是 Pips home；文件名/命令固定。
- 步骤：第一次操作按 `o`（Allow once）；相同操作再次请求并按 `s`（Allow session）；第三次相同 operation 应使用 exact session grant；改变 argv 或 write dir 后应再次询问并按 deny。另以 `pips exec` 触发审批。
- 预期证据：Allow once 只执行一次；Allow session 只复用于完全相同 fingerprint/Workspace/ceiling；任何字段变化重新审阅；Deny 产生持久、模型可见但有界的拒绝结果；非交互 `exec` 不自动选择并退出 3。
- 通过条件：决策持久化顺序正确，授权精确绑定，无跨 Session/Workspace/operation 扩散。
- 失败条件：模糊匹配复用、模型能自批、deny 后执行、或 exec 卡住等待输入。
- 清理/回滚：删除夹具外部文件；approval journal 随隔离 Session 清理。
- 来源/测试锚点：[controller.go](../../internal/coding/approval/controller.go)、[journal.go](../../internal/coding/approval/journal.go)、[overlay_test.go](../../internal/coding/tui/overlay_test.go)。
- 结果：`未执行`。

## MA-SEC-005：未知结果恢复不自动重放

- 分类：安全敏感、恢复。变更等级：临时工作区。
- 前置条件：允许通过受控测试/fault injection 在 operation `started` 已持久化、result 未持久化时终止进程；不得对不可逆外部命令执行。
- 隔离夹具：幂等测试脚本每次运行向独立计数文件追加一次；另执行确定性测试：

  ```bash
  go test ./internal/coding/approval -run 'Test.*(Unknown|Retry|Acknowledge)' -count=1 -v
  ```

- 步骤：制造未知结果后 resume；检查 UI 给出 retry、mark failed/acknowledge 等适用选择，而不是自动运行；先选择不重试并核对计数；在新夹具重复并明确 retry。
- 预期证据：resume 首先显示 exact tool/fingerprint/attempt 的 unknown barrier；非交互恢复退出 3；仅明确 retry 会产生下一 attempt；计数与人工选择一致。
- 通过条件：没有静默重放，所有恢复选择有 durable receipt，stale choice 被拒绝。
- 失败条件：启动即重试、同 attempt 执行两次、unknown 可被普通 prompt 绕过，或 journal 损坏被忽略。
- 清理/回滚：终止注入脚本并保存计数；销毁夹具目录。
- 来源/测试锚点：[state.go](../../internal/coding/approval/state.go)、[controller_test.go](../../internal/coding/approval/controller_test.go)、[journal_test.go](../../internal/coding/approval/journal_test.go)。
- 结果：`未执行`。

## MA-SEC-006：Full Access 显式来源与进程内切换

- 分类：安全敏感。变更等级：外部系统潜在；本用例仅做无副作用命令。
- 前置条件：只在专用容器/VM；先记录外层隔离边界。
- 隔离夹具：无写入的 `pwd`/版本查询命令。
- 步骤：验证 project 配置、Bundle、Extension、tool arguments 都不能启用 Full Access；用 CLI `--sandbox full-access` 启动并观察 doctor warning；TUI `/permissions` 从 workspace-write 选择 Full Access，先取消确认，再确认；切回 workspace-write；重启检查未持久化。
- 预期证据：Full Access 只来自 user config、`PIPS_SANDBOX`、CLI flag 或明确的 process-local session override；切入有二次警告；Network 显示 unrestricted/not enforced；重启恢复配置值。
- 通过条件：来源限制、确认、process-local 生命周期和状态显示均正确。
- 失败条件：项目内容/模型可启用、取消仍切换、选择被写入配置/Session、或 doctor 把 unsandboxed 报为 probe 成功。
- 清理/回滚：退出 Full Access 进程；销毁外层隔离环境。
- 来源/测试锚点：[permissions_picker.go](../../internal/coding/tui/permissions_picker.go)、[policy.go](../../internal/coding/execution/policy.go)、[Full Access 契约](../coding-security.md#full-access-and-stronger-isolation)。
- 结果：`未执行`。

## MA-SEC-007：环境清洗、输出/时间上限与进程清理

- 分类：平台条件、安全敏感。变更等级：临时工作区。
- 前置条件：设置只用于测试的 `TOKEN`/`PASSWORD`/proxy/agent/OTel/dynamic-loader 形状变量；不得设置真实值。
- 隔离夹具：脚本尝试列出环境名、产生超限 stdout/stderr、超过 timeout，并创建子进程。
- 步骤：通过受控 Shell 运行各分支，再 Ctrl+C 取消一次；检查 private temp/cache 路径、子进程和输出。运行：

  ```bash
  go test ./internal/coding/execution -run 'Test.*(Environment|Output|Timeout|Cancel|Process)' -count=1 -v
  ```

- 预期证据：secret/proxy/agent/OTel/Pips/Git 危险变量被移除；cache/temp 强制位于 private Plan root；输出按字节/行上限截断并标识；timeout/cancel 终止 process group（Linux PID namespace 证据按平台记录）；真实 provider key 从不进入 child。
- 通过条件：测试通过且人工夹具没有环境值泄露、孤儿进程或 Pips home 写入。
- 失败条件：任一危险变量透传、无限输出、取消后子进程继续、或 cleanup 错误仍返回 0。
- 清理/回滚：unset 所有人工变量，确认无夹具进程，检查 private root 生命周期。
- 来源/测试锚点：[environment.go](../../internal/coding/execution/environment.go)、[output.go](../../internal/coding/execution/output.go)、[runner_unix_test.go](../../internal/coding/execution/runner_unix_test.go)。
- 结果：`未执行`。

## 分册判定

目标发布平台的全部适用项必须通过。越界写、拒绝后执行、自动重放未知结果、secret 泄露或 Sandbox 降级中的任一项都直接判定分册不通过。
