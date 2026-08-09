# Workspace、工具、Git 与附件人工验收

[返回验收索引](index.md) · [证据记录模板](environment-evidence-record.md)

本分册会让模型读写文件并运行受控命令。只允许使用一次性 Workspace；执行前记录 Git HEAD 与 `git status --short`，不要把真实仓库作为夹具。

## MA-WORK-001：只读 Workspace 工具

- 分类：实时 Provider。变更等级：无；会产生费用。
- 前置条件：Workspace 内准备 `src/a.go`、`src/b_test.go`、`notes/readme.txt` 与一个 `.gitignore` 排除文件。
- 隔离夹具：文件只包含公开测试文字；另在 Workspace 外放置一个不可读取的 sentinel。
- 步骤：分轮要求模型使用 `ls` 列目录、`glob` 匹配 `**/*.go`、`grep` 搜索固定 token、`read` 读取带行号片段；再要求读取 `../<outside-sentinel>`。
- 预期证据：前四项返回稳定、有界的 Workspace 相对结果；忽略/不支持类型按工具契约处理；外部路径被拒绝且不返回 sentinel 内容；Git 状态不变。
- 通过条件：结果准确、路径边界生效、无写入、错误为结构化安全错误。
- 失败条件：目录逃逸、symlink 跟随到外部、读结果错误或 Workspace 变化。
- 清理/回滚：删除 Workspace 外 sentinel；记录其摘要，不记录敏感内容。
- 来源/测试锚点：[catalog.go](../../internal/coding/tools/catalog.go)、[tools_test.go](../../internal/coding/tools/tools_test.go)、[workspace](../../internal/coding/workspace)。
- 结果：`未执行`。

## MA-WORK-002：`apply_patch` 原子修改与失败回滚

- 分类：实时 Provider、安全敏感。变更等级：临时工作区；会产生费用。
- 前置条件：`workspace-write`；approval 设置符合安全分册；目标文件已提交。
- 隔离夹具：`fixture.txt` 含三行固定文本，记录修改前 SHA-256。
- 步骤：要求模型只用 `apply_patch` 精确替换第二行并新增 `created.txt`；复核 diff。再让模型尝试一个上下文不匹配或越界路径 patch。
- 预期证据：成功 patch 只改变声明文件，换行和未触及内容保持；TUI/JSONL 不显示完整敏感 patch；失败 patch 不产生部分写入，外部路径被拒绝。
- 通过条件：成功变更与预期 diff 完全一致；失败时两个文件摘要均保持失败前状态。
- 失败条件：部分写入、非目标文件变化、目录逃逸、失败仍返回 success。
- 清理/回滚：用新的反向 patch 或在夹具仓库提交/恢复预期状态；不得对真实仓库运行破坏性 Git 命令。
- 来源/测试锚点：[apply_patch.go](../../internal/coding/tools/apply_patch.go)、[patch package](../../internal/coding/tools/patch)、[tools_test.go](../../internal/coding/tools/tools_test.go)、[workspace tree](../../internal/coding/workspace/tree.go)。
- 结果：`未执行`。

## MA-WORK-003：Shell 的 cwd、环境、输出与审批边界

- 分类：实时 Provider、安全敏感、平台条件。变更等级：临时工作区；会产生费用。
- 前置条件：先通过 `MA-SEC-001` 至 `MA-SEC-004`；使用 `workspace-write`，不得在真实 home 执行。
- 隔离夹具：Workspace 中有子目录；设置人工测试环境变量 `PIPS_SECRET_FIXTURE`，值不得是真实秘密。
- 步骤：要求模型运行安全命令打印当前目录、创建 `shell-output.txt`、产生超过展示上限的 stdout；再尝试不存在 cwd、Workspace 外 cwd 和读取测试环境变量。每次记录审批卡片与选择。
- 预期证据：cwd 只能是 Workspace 内目录；写入受 Sandbox/approval 控制；输出被有界截断但命令结果有明确 exit status；不允许的环境变量不传入或不在视图/Session 泄露；无效 cwd 返回可重试的结构化错误。
- 通过条件：允许操作只影响夹具，拒绝操作无副作用，审批范围不扩大到后续不同命令。
- 失败条件：cwd 逃逸、秘密环境透传、拒绝后仍执行、输出无限增长或泄露完整敏感内容。
- 清理/回滚：删除人工变量；恢复/提交夹具变更；确认无子进程遗留。
- 来源/测试锚点：[shell.go](../../internal/coding/tools/shell.go)、[execution](../../internal/coding/execution)、[approval](../../internal/coding/approval)。
- 结果：`未执行`。

## MA-WORK-004：Git 状态、Diff 与不受信任配置隔离

- 分类：本地确定性、安全敏感。变更等级：临时工作区。
- 前置条件：Git 可用；夹具仓库有 tracked、untracked、deleted、binary、mode change 和超大 diff 场景。
- 隔离夹具：在 repo config 中设置一个若被执行会写 sentinel 的 `diff.external`、自定义 diff driver 与 fsmonitor 命令。
- 步骤：启动 TUI 执行 `/status` 并请求 review 当前 changes；对照 `git status --short`；运行聚焦测试：

  ```bash
  go test ./internal/coding/changes/git -count=1 -v
  ```

- 预期证据：变更分类与 Git 一致；binary/mode/large diff 有界表示；输出出现 `[diff truncated]` 时仍列出受影响文件；恶意 repo Git config 未执行，sentinel 不存在。
- 通过条件：状态完整且稳定，安全固定 Git 配置覆盖不受信任扩展，测试通过。
- 失败条件：执行 repo hook/diff helper、漏报删除/未跟踪文件、无界输出、或扫描写入仓库。
- 清理/回滚：删除仅属于夹具的恶意配置和 sentinel；销毁夹具 repo。
- 来源/测试锚点：[command.go](../../internal/coding/changes/git/command.go)、[inspector_more_test.go](../../internal/coding/changes/git/inspector_more_test.go)、[status.go](../../internal/coding/changes/git/status.go)。
- 结果：`未执行`。

## MA-WORK-005：文本/图片文件附件与变更检测

- 分类：实时 Provider、安全敏感。变更等级：无；会产生费用。
- 前置条件：交互 TUI；Workspace 含小型 UTF-8 文本、PNG/JPEG/GIF 与超限/不支持类型夹具。
- 隔离夹具：所有附件都无秘密；记录文件摘要。
- 步骤：在 Composer 输入 `@`，过滤并选择文本文件，确认不可变引用后提交；对图片重复；选择后、提交前修改原文件，验证 changed 错误；尝试 symlink、Workspace 外路径、超限文件和不支持类型。
- 预期证据：picker 只显示可发现的 Workspace 候选；文本和图片在提交时重新安全解析；图片规范化为 PNG/JPEG inline part，GIF 只取首帧；文件变化、越界、symlink、大小/像素超限均失败关闭；失败不会把原始 bytes 打印到 UI。
- 通过条件：合法附件到达模型且内容/类型正确，所有负向夹具无泄露、无部分提交。
- 失败条件：选择时读取后不复核、symlink 逃逸、TOCTOU 未检测、图片超限仍发送。
- 清理/回滚：删除附件夹具；Session/录屏不得作为公开制品保留其原始 bytes。
- 来源/测试锚点：[attachment](../../internal/coding/attachment)、[file_picker.go](../../internal/coding/tui/file_picker.go)、[file_picker_test.go](../../internal/coding/tui/file_picker_test.go)。
- 结果：`未执行`。

## MA-WORK-006：本地剪贴板图片

- 分类：交互终端、平台条件、安全敏感。变更等级：无；可能产生费用。
- 前置条件：目标平台有受支持剪贴板读取能力；剪贴板仅放人工测试图片。
- 隔离夹具：小型 PNG/JPEG；另准备非图片 clipboard 内容。
- 步骤：在本地 TUI Composer 按 `Ctrl+V`；确认图片引用后提交；在 bracketed paste 中包含 Ctrl+V 等价输入，确认作为普通 paste；快速重复请求验证 busy 行为；非图片内容验证失败提示。
- 预期证据：只读取一次图片、内存中规范化、最多一个 worker 和一个 pending request；bracketed paste 不触发图片读取；busy/失败提示有界且不显示 bytes、临时路径或诊断秘密。
- 通过条件：合法图片可发送，所有控制边界正确，未创建意外临时图片文件。
- 失败条件：读取文本剪贴板/任意文件、paste 误触发、并发无界、失败泄露内容。
- 清理/回滚：清空测试剪贴板；确认进程退出后无 clipboard worker。
- 来源/测试锚点：[clipboard](../../internal/coding/clipboard)、[imagebridge](../../internal/coding/imagebridge)、[attachment image tests](../../internal/coding/attachment/image_test.go)。
- 结果：`未执行`。

## MA-WORK-007：Workspace `AGENTS.md` 与 Agent task list

- 分类：实时 Provider、安全敏感。变更等级：本地持久状态；可能产生费用。
- 前置条件：trusted 验收 Workspace；Agent Mode；准备 root 与 nested `AGENTS.md`，内容只含无秘密、可观察约束。
- 隔离夹具：root 要求回答包含 `ROOT-MARKER`，nested 要求更近作用域包含 `NESTED-MARKER`；另准备 binary、symlink、超限 instruction 负向夹具。任务要求模型维护三项 task list。
- 步骤：分别在 root/nested scope 发请求并核对适用指令顺序；验证普通源码/README 中伪装的指令只被视为 data；运行 `go test ./internal/coding/instructions -count=1 -v`。在 Agent Mode 要求创建三项 task list、只允许一项 `in_progress`、逐项更新完成状态；切 Plan Mode 检查该 Agent task tool 不可用。
- 预期证据：Resolver 只加载精确 `AGENTS.md`，从 Workspace root 到 active scope 排序且更近来源在后；不跟随逃逸 symlink，binary/non-regular/超过 32 KiB 总预算失败关闭；task update 是完整有界 plan，1..N 项、至多一个 in progress，TUI status 正确显示完成计数；Plan Mode 不暴露该 Agent-mode task tool。
- 通过条件：作用域/优先级与 task 状态准确，恶意文件不能注入或越界，invalid task update 不改变前一 snapshot。
- 失败条件：扫描任意文件当指令、symlink 逃逸、invalid instructions 被忽略后继续、task 增量合并出重复项、或 Plan Mode 仍可写 Agent task state。
- 清理/回滚：移除 instruction/task 夹具；Session 随隔离 home 清理。
- 来源/测试锚点：[instructions](../../internal/coding/instructions)、[tasklist](../../internal/coding/tasklist)、[tasklist tool](../../internal/coding/tools/tasklist.go)。
- 结果：`未执行`。

## 分册判定

`MA-WORK-001` 至 `005` 和 `007` 是本地 Coding Agent 发布必验项；`006` 在声明支持剪贴板图片的平台必验。任一越界读取、拒绝后写入或秘密泄露均为阻断发布的失败。
