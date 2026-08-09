# 安装、配置与 CLI 人工验收

[返回验收索引](index.md) · [先填写环境与证据记录](environment-evidence-record.md)

本分册验证二进制入口、帮助面、配置分层、诊断、非交互输入和持久会话。所有命令都在隔离的 `PIPS_HOME` 与 Workspace 中执行。除 `MA-CLI-004` 的成功交互外，不需要真实 Provider。

## MA-CLI-001：构建、版本、帮助与补全

- 分类：本地确定性。变更等级：无。
- 前置条件：源码 checkout 完整；Go 版本满足 `go.mod`；已记录目标 commit。
- 隔离夹具：使用验收 Workspace；本用例不写入它。
- 步骤：

  ```bash
  go build -o "${PIPS_ACCEPT_ROOT}/pips" ./cmd/pips
  "${PIPS_ACCEPT_ROOT}/pips" version
  "${PIPS_ACCEPT_ROOT}/pips" --help
  "${PIPS_ACCEPT_ROOT}/pips" completion bash >"${PIPS_ACCEPT_ROOT}/pips.bash"
  test -s "${PIPS_ACCEPT_ROOT}/pips.bash"
  ```

- 预期证据：构建退出码为 0；`version` 是单行、非空且不包含凭据；帮助列出 `acp`、`agents`、`completion`、`config`、`doctor`、`exec`、`hooks`、`plugin`、`resume`、`session`、`ssh`、`version`；根 flags 至少包含 `--workspace`、`--config`、`--model`、`--variant`、`--reasoning`、`--tool-search`、`--dynamic-subagents`、`--mode`、`--sandbox`、`--approval`；补全文件非空。
- 通过条件：以上命令全为 0，命令与 flags 无遗漏，补全脚本对应 Bash 且不混入日志。
- 失败条件：构建失败、版本为空、公开帮助面缺项，或补全输出为空/污染。
- 清理/回滚：保留脱敏输出后删除或移入回收站的对象仅限 `${PIPS_ACCEPT_ROOT}/pips` 与 `${PIPS_ACCEPT_ROOT}/pips.bash`。
- 来源/测试锚点：[root.go](../../internal/coding/cli/root.go)、[completion.go](../../internal/coding/cli/completion.go)、[root_test.go](../../internal/coding/cli/root_test.go)。
- 结果：`未执行`。

## MA-CLI-002：配置路径、校验、解析值与来源

- 分类：本地确定性。变更等级：本地持久状态。
- 前置条件：`MA-CLI-001` 通过；`${PIPS_ACCEPT_HOME}` 为空。
- 隔离夹具：创建 `${PIPS_ACCEPT_HOME}/config.toml`，内容如下；模型 ID 只是配置夹具，不发请求。

  ```toml
  mode = "agent"
  tool_search = true

  [providers.openai.models."acceptance-model"]
  default = true
  reasoning_levels = ["low", "high"]
  default_reasoning_level = "low"
  ```

- 步骤：用安全编辑器保存夹具，然后执行：

  ```bash
  pips --workspace "${PIPS_ACCEPT_WORKSPACE}" config path
  pips --workspace "${PIPS_ACCEPT_WORKSPACE}" config validate
  pips --workspace "${PIPS_ACCEPT_WORKSPACE}" config show
  PIPS_REASONING=high pips --workspace "${PIPS_ACCEPT_WORKSPACE}" config show --tool-search=false
  ```

- 预期证据：`config path` 指向 `${PIPS_ACCEPT_HOME}/config.toml`；校验输出为成功；第一次 `show` 的 model 为 `openai/acceptance-model`、reasoning 为 `low`、tool search 为 `true`；第二次 reasoning 来源为环境变量 `PIPS_REASONING`、tool search 来源为 flag `--tool-search`。输出只显示配置值与来源，不显示 `API_KEY`。
- 通过条件：解析值、选择结果和每个覆盖项的 provenance 均准确，且 project `.pips/config.toml` 未被隐式加载。
- 失败条件：默认路径错误、分层优先级错误、未知字段被静默忽略、或输出泄露凭据。
- 清理/回滚：保留原始夹具摘要；此文件随隔离 `PIPS_HOME` 清理。
- 来源/测试锚点：[load.go](../../internal/coding/config/load.go)、[config.go](../../internal/coding/cli/config.go)、[config_test.go](../../internal/coding/cli/config_test.go)、[Coding CLI 配置契约](../coding-cli.md#configuration-trust-and-sessions)。
- 结果：`未执行`。

## MA-CLI-003：迁移错误与无效调用的失败关闭

- 分类：本地确定性。变更等级：本地持久状态。
- 前置条件：可临时替换隔离配置；不得在真实用户配置上执行。
- 隔离夹具：分别准备包含 `[model]`、provider/model `api` 字段、未知顶层字段的三个无效 TOML 文件。
- 步骤：逐个通过 `pips --config <file> config validate` 校验；再执行：

  ```bash
  pips --provider openai config show
  PIPS_PROVIDER=openai pips config show
  pips exec first second
  pips exec --output xml hello
  printf 'stdin' | pips exec argument
  ```

- 预期证据：每条命令退出码均为 2；`--provider`、`PIPS_PROVIDER`、`[model]` 和旧 `api` 字段明确报告已移除或迁移方向；多 prompt、不支持的 output mode、位置参数与 stdin 同时有内容均被拒绝；stderr 只有一次有界错误，stdout 无成功结果。
- 通过条件：所有无效入口均失败关闭，未创建 Session，未发 Provider 请求，未修改 Workspace。
- 失败条件：旧配置被自动接受、退出码不是 2、产生 Session/外部请求，或错误包含秘密/堆栈噪声。
- 清理/回滚：删除仅属于本夹具的无效配置文件；确认 `git status --short` 未变化。
- 来源/测试锚点：[input.go](../../internal/coding/cli/input.go)、[state.go](../../internal/coding/cli/state.go)、[load_test.go](../../internal/coding/config/load_test.go)、[input_test.go](../../internal/coding/cli/input_test.go)。
- 结果：`未执行`。

## MA-CLI-004：Doctor 预检

- 分类：实时 Provider、平台条件。变更等级：无。
- 前置条件：有效模型配置；通过安全注入方式为当前进程设置 provider-neutral `API_KEY`；目标平台的 Sandbox 依赖已安装。该用例不应产生模型费用。
- 隔离夹具：沿用有效配置和验收 Workspace。
- 步骤：先在不设置 `API_KEY` 时执行一次并记录失败；再设置有效 key 后执行：

  ```bash
  pips --workspace "${PIPS_ACCEPT_WORKSPACE}" doctor
  ```

- 预期证据：缺凭据时退出 2 且只提示设置 `API_KEY`；成功时输出 Workspace、活动配置文件、`configuration ok`、`credential ok API_KEY` 与 Sandbox 状态。Linux/Darwin 的 `workspace-write` 应报告实际 backend/capability；`full-access` 可报告不执行隔离探测的 notice。
- 通过条件：失败与成功路径符合预期，任何输出均不含 key 值；成功路径退出 0。
- 失败条件：接受 `OPENAI_API_KEY` 等 provider-specific 变量作为替代、泄露 key、或 Sandbox 不可用却报告成功。
- 清理/回滚：立即从 shell 环境移除临时 `API_KEY`；不要把环境转储作为证据。
- 来源/测试锚点：[doctor.go](../../internal/coding/cli/doctor.go)、[environment.go](../../internal/coding/credential/environment.go)、[config_test.go](../../internal/coding/cli/config_test.go)。
- 结果：`未执行`。

## MA-CLI-005：非交互输入与输出边界

- 分类：实时 Provider。变更等级：本地持久状态；可能产生费用。
- 前置条件：`MA-CLI-004` 通过；选择低成本测试模型；Workspace 已信任或本次显式允许信任。
- 隔离夹具：创建不含秘密的 `${PIPS_ACCEPT_ROOT}/prompt.txt`，文本为“只回复 ACCEPTANCE-OK，不调用工具”。
- 步骤：

  ```bash
  pips --workspace "${PIPS_ACCEPT_WORKSPACE}" exec --trust-workspace "只回复 ACCEPTANCE-OK，不调用工具"
  pips --workspace "${PIPS_ACCEPT_WORKSPACE}" exec - <"${PIPS_ACCEPT_ROOT}/prompt.txt"
  printf '%s\n' '只回复 ACCEPTANCE-OK，不调用工具' | pips --workspace "${PIPS_ACCEPT_WORKSPACE}" exec
  ```

- 预期证据：三次退出 0；plain stdout 仅包含本次最终 assistant 文本，运行进度只在 stderr；第一次 stderr 最多出现一次 Workspace trusted 提示；后两次不重复信任提示；输入字符保持有效 UTF-8。
- 通过条件：三个输入入口都完成同一语义请求，没有 ANSI/事件 envelope 混入 stdout，没有更改 Workspace 文件。
- 失败条件：输出通道污染、自动审批/提问、重复信任副作用、输入截断或 Workspace 变化。
- 清理/回滚：保留 Session ID 与输出摘要；删除 prompt 夹具；不要删除 Session，留给下一用例。
- 来源/测试锚点：[exec.go](../../internal/coding/cli/exec.go)、[presenter.go](../../internal/coding/cli/presenter.go)、[exec_test.go](../../internal/coding/cli/exec_test.go)。
- 结果：`未执行`。

## MA-CLI-006：会话列出、恢复与 Workspace 绑定

- 分类：实时 Provider、交互终端。变更等级：本地持久状态；可能产生费用。
- 前置条件：`MA-CLI-005` 至少生成一个已完成 Session；记录其 ID。
- 隔离夹具：原 Workspace，再创建一个空的第二 Workspace 用于负向检查。
- 步骤：

  ```bash
  pips --workspace "${PIPS_ACCEPT_WORKSPACE}" session list
  pips --workspace "${PIPS_ACCEPT_WORKSPACE}" exec --session "<SESSION_ID>" "只回复 RESUMED"
  pips --workspace "<SECOND_WORKSPACE>" exec --session "<SESSION_ID>" "不要执行"
  ```

  另在真实终端执行 `pips --workspace "${PIPS_ACCEPT_WORKSPACE}" resume <SESSION_ID>`，确认 transcript 后用 `/quit` 正常退出。
- 预期证据：列表仅显示当前 Workspace 的 Session ID 与 UTC RFC3339Nano 时间；同 Workspace 可恢复并继续；第二 Workspace 的恢复退出 2 且未打开/复制 Session；TUI transcript 与之前内容一致。
- 通过条件：Workspace 绑定严格生效，恢复不会重放已完成副作用，正常退出后 Session 仍可再次列出。
- 失败条件：跨 Workspace 恢复成功、重复工具副作用、同一 Session 被并发打开、或历史丢失。
- 清理/回滚：退出所有恢复进程；Session 随隔离 `PIPS_HOME` 清理。
- 来源/测试锚点：[session.go](../../internal/coding/cli/session.go)、[interactive.go](../../internal/coding/cli/interactive.go)、[session repository](../../internal/coding/session/repository.go)。
- 结果：`未执行`。

## 分册判定

`MA-CLI-001` 至 `003` 必须通过；发布环境支持的 `004` 至 `006` 必须通过。缺少真实凭据时它们记为`未执行`，本分册不能据此宣称端到端通过。
