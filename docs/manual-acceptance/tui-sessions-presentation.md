# TUI、会话与呈现人工验收

[返回验收索引](index.md) · [证据记录模板](environment-evidence-record.md)

本分册必须在真实交互终端执行。截图可用于证明布局和状态，但必须遮盖 Workspace 私有内容；功能正确性仍以 Session、Git 状态和可重复动作共同判定。

## MA-TUI-001：启动、Workspace 信任与终端恢复

- 分类：交互终端、安全敏感。变更等级：本地持久状态。
- 前置条件：有效配置；隔离 Workspace 尚未信任；stdin/stdout 都是 TTY，`TERM` 不是 `dumb`。
- 隔离夹具：干净验收 Workspace。
- 步骤：运行 `pips --workspace "${PIPS_ACCEPT_WORKSPACE}"`；在信任 overlay 中先取消，重新启动后明确选择信任；再执行 `/quit`。另执行 `TERM=dumb pips ...` 作为负向检查。
- 预期证据：未信任时 project Skills/MCP/Plugins/Hooks 不加载；取消不写 trust；确认后只信任当前 Workspace identity；启动 banner 显示 Workspace/Session/model；正常退出恢复光标、终端模式和 alternate screen；`TERM=dumb` 退出 2 并提示使用 `pips exec`。
- 通过条件：信任是显式、持久且 Workspace 绑定；退出后 shell 输入/回显正常。
- 失败条件：自动信任、取消仍落盘、加载未信任项目资源、或终端残留 raw mode。
- 清理/回滚：退出进程；trust 记录随隔离 `PIPS_HOME` 清理。
- 来源/测试锚点：[run.go](../../internal/coding/tui/run.go)、[model_test.go](../../internal/coding/tui/model_test.go)、[interactive_test.go](../../internal/coding/cli/interactive_test.go)。
- 结果：`未执行`。

## MA-TUI-002：Composer、命令面与流式呈现

- 分类：实时 Provider、交互终端。变更等级：本地持久状态；可能产生费用。
- 前置条件：`MA-TUI-001` 通过。
- 隔离夹具：无秘密 prompt。
- 步骤：在 Composer 输入多行文本，使用 `Ctrl+J` 或 `Shift+Enter` 换行并以 Enter 提交；模型响应期间观察 streaming；按 `/` 打开命令 picker，核对 `new`、`resume`、`plan`、`mode`、`agents`、`team`、`skills`、`model`、`permissions`、`statusline`、`theme`、`tree`、`fork`、`compact`、`review`、`reload`、`status`、`help`、`quit`；用 Esc 取消 picker 并确认草稿恢复。
- 预期证据：换行不会提前发送；一次 Enter 只提交一次；assistant 文本稳定增量呈现且最终只固化一次；命令过滤可用；Esc 恢复原草稿与附件；idle-only 命令在 busy 状态不可执行。
- 通过条件：Composer、stream、picker 和取消行为均无丢字、重复或状态串线。
- 失败条件：paste/换行误提交、最终消息重复、取消丢草稿、busy 时启动冲突控制。
- 清理/回滚：等待请求完成或明确 Ctrl+C 取消，再退出。
- 来源/测试锚点：[composer.go](../../internal/coding/tui/composer.go)、[command_picker.go](../../internal/coding/tui/command_picker.go)、[streaming_test.go](../../internal/coding/tui/streaming_test.go)。
- 结果：`未执行`。

## MA-TUI-003：新会话、恢复、树与 Fork

- 分类：实时 Provider、交互终端。变更等级：本地持久状态；可能产生费用。
- 前置条件：已有至少两轮对话和一个已完成工具调用。
- 隔离夹具：工具调用只能写验收 Workspace 中的固定夹具文件。
- 步骤：依次查看 `/tree`；选择非叶子节点后取消；重新打开 `/fork` 并确认从该节点 fork；在 fork Session 添加可辨识消息；使用 `/resume` 在原 Session 与 fork 之间切换；最后 `/new` 创建空 Session。
- 预期证据：tree 能区分节点与当前分支；fork 获得新 Session ID、复制所选节点之前的历史但不复制之后内容；原/fork 后续互不污染；resume 只列当前 Workspace；new 不继承 transcript。
- 通过条件：Session identity、历史边界、选择/取消和恢复全部正确；已完成工具副作用不因浏览/fork 自动重放。
- 失败条件：共用 Session ID、复制错误边界、恢复跨 Workspace 条目、或重放副作用。
- 清理/回滚：保留 Session 元数据作为证据；Workspace 文件恢复到约定夹具状态。
- 来源/测试锚点：[route_tree.go](../../internal/coding/tui/route_tree.go)、[session_picker.go](../../internal/coding/tui/session_picker.go)、[session repository](../../internal/coding/session/repository.go)。
- 结果：`未执行`。

## MA-TUI-004：Compact 预览、确认与上下文连续性

- 分类：实时 Provider、交互终端。变更等级：本地持久状态；可能产生费用。
- 前置条件：Session 有足够历史可压缩；记录关键事实和当前 Git 状态。
- 隔离夹具：在早期消息提供一个无秘密、可核验的标记事实。
- 步骤：打开 `/compact`，先取消并确认 transcript 未变；再次打开，检查预览边界后确认；压缩完成后询问早期标记事实并继续一轮正常请求；退出再 resume。
- 预期证据：取消不写入 compaction；确认只压缩预览范围；摘要不包含隐藏 reasoning/秘密工具参数；当前工作状态、关键事实和未完成交互保持；resume 后压缩状态一致。
- 通过条件：有明确预览与确认、无意外消息删除、连续性可验证、无重复工具副作用。
- 失败条件：未确认即压缩、摘要泄密、活动交互被压缩、恢复后历史损坏。
- 清理/回滚：Session 随隔离 home 清理；记录压缩前后 Session ID/节点数而非完整私有文本。
- 来源/测试锚点：[runtime_structure.go](../../internal/coding/runtime_structure.go)、[command_picker.go](../../internal/coding/tui/command_picker.go)、[runtime_test.go](../../internal/coding/runtime_test.go)。
- 结果：`未执行`。

## MA-TUI-005：Theme、Status Line 与 `NO_COLOR`

- 分类：交互终端、本地确定性。变更等级：本地持久状态。
- 前置条件：活动配置是安全的普通文件；记录其原始摘要。
- 隔离夹具：可选地把 [theme 示例](../examples/tui-theme.toml)复制为 `${PIPS_ACCEPT_HOME}/themes/ocean.toml`，确保权限不允许 group/world 写。
- 步骤：用 `/theme` 选择 `nord`，用 `/statusline` 切换并重排 `workspace`、`session`、`model`、`phase` 后保存；退出检查 TOML，再启动确认持久化；用 Esc 验证取消不保存；最后以 `NO_COLOR=1` 启动。
- 预期证据：UI 立即换主题；两次保存只修改各自 `[tui]` 字段，保留注释和其他配置；重启选择保持；不存在 `--theme`/`PIPS_THEME`；`NO_COLOR` 无 ANSI 但布局与内容保留；无效 custom theme 被有界忽略且不阻止启动。
- 通过条件：显示、持久化、取消和 source-preserving 写入均符合预期。
- 失败条件：保存覆盖无关配置、symlink/不安全文件被写入、取消仍修改、或 `NO_COLOR` 仍输出颜色控制码。
- 清理/回滚：恢复配置原始内容或随隔离 home 清理。
- 来源/测试锚点：[theme_picker.go](../../internal/coding/tui/theme_picker.go)、[statusline_picker.go](../../internal/coding/tui/statusline_picker.go)、[theme_store_test.go](../../internal/coding/config/theme_store_test.go)。
- 结果：`未执行`。

## MA-TUI-006：Status、Help、详情与敏感信息投影

- 分类：实时 Provider、安全敏感。变更等级：无。
- 前置条件：至少有一次 read 和一次 shell/tool 生命周期；prompt/输出使用人工标记的“敏感测试串”，不得用真实秘密。
- 隔离夹具：测试串形如 `PIPS-SENSITIVE-FIXTURE-<批次号>`。
- 步骤：运行 `/status` 与 `/help`，检查它们进入 scrollback；观察 compact tool activity，再按 `Ctrl+T` 打开详情；取消/关闭详情；搜索录屏、终端保存和 Session 安全投影是否包含测试串。
- 预期证据：status 显示有界 Workspace/model/mode/permissions/changes 状态；help 与实际按键一致；tool activity 只暴露允许的名称、状态和有界摘要；shell 命令、完整输出、reasoning、凭据与内部 child 路径不会进入不应出现的视图。
- 通过条件：操作信息足够定位状态，同时敏感测试串只出现在用户明确输入/允许显示的位置。
- 失败条件：详情泄露原始 shell/secret/reasoning、status 阻塞交互、或帮助与行为不一致。
- 清理/回滚：销毁含测试串的本地验收录屏；不要提交到仓库。
- 来源/测试锚点：[print.go](../../internal/coding/tui/print.go)、[tool_activity.go](../../internal/coding/tui/tool_activity.go)、[tool_activity_test.go](../../internal/coding/tui/tool_activity_test.go)。
- 结果：`未执行`。

## MA-TUI-007：Model 切换、Steer、Follow-up 与取消

- 分类：实时 Provider、交互终端。变更等级：本地持久状态；可能产生费用。
- 前置条件：配置至少两个本地 catalog models；其中一个请求能保持 streaming 足够久以输入控制消息。
- 隔离夹具：三个带顺序标记的无秘密 prompt；记录原 Session ID、model 和 transcript 长度。
- 步骤：idle 时打开 `/model`，先 Esc 取消，再选择另一 model；确认 Session ID 不变并发一轮请求。下一轮 running 时在 Composer 输入 steer 文本并按 Enter；再输入 follow-up 并按 Tab；另起一轮按 Ctrl+C 取消；测试 bracketed multiline paste 与普通输入的边界。
- 预期证据：model 只来自本地配置 catalog，不请求远端 `/models`，切换原子 reopen 同一 Session，失败时可恢复旧 model；Enter 在 running 时调用 Steer，Tab 只排一个 Follow-up，二者在 Controller 接受后才清草稿；Ctrl+C 终止当前 interaction 但不损坏 Session；粘贴多行不拆成多次提交。
- 通过条件：model attribution 按 interaction 固定，控制消息顺序正确、无丢失/重复，取消后可继续使用同一 Session。
- 失败条件：model switch 新建 Session/改写旧 attribution、失败后旧 Runtime 丢失、Steer/Follow-up 串型、多个 follow-up 无界排队、或取消仍完成原请求。
- 清理/回滚：等待取消收敛，切回验收默认 model 并正常退出。
- 来源/测试锚点：[control.go](../../internal/coding/tui/control.go)、[model.go](../../internal/coding/tui/model.go)、[model_test.go](../../internal/coding/tui/model_test.go)。
- 结果：`未执行`。

## 分册判定

真实 TUI 发布必须完成全部七项。只运行 `internal/coding/tui` 测试不能替代终端恢复、布局、输入法/粘贴和真实 streaming 的人工证据。
