# 后台监视（`monitor`）人工验收

[返回验收索引](index.md) · [证据记录模板](environment-evidence-record.md)

本分册会启动长期运行的 Shell 命令并把它的 stdout 逐行送进对话。只允许使用一次性
Workspace 与独立 `PIPS_HOME`；命令不得触及真实仓库、真实凭据或生产日志。

## MA-WATCH-001：watch 事件进入进行中的回合与空闲会话

- 分类：实时 Provider、安全敏感。变更等级：临时工作区；会产生费用。
- 前置条件：`workspace-write`；已通过 `MA-WORK-003` 的 Shell 审批基线；Sandbox 允许运行 `sleep` 与 `printf`。
- 隔离夹具：Workspace 中不放任何真实日志；命令只 `printf` 固定测试行。
- 步骤：要求模型调用 `monitor`，命令为 `printf 'DONE-a\n'; sleep 20`，`description` 明确；随后立刻要求它继续做一件需要多轮工具调用的事，让回合保持进行。观察事件到达时回合仍在运行。再等该 watch 自然退出，观察完成文本。最后重复一次，但让会话在此之前回到空闲。
- 预期证据：进行中的回合里，事件以 hidden 的 `<system-reminder>` 用户消息在该回合的下一个回合边界出现，普通对话区域不把它当作用户发言；空闲场景下事件自成一个批次回合；两种情况下文本形状为单事件
  `<monitor-event task_id="…">[label] text</monitor-event>`；自然退出只产生一条完成文本，含原因、描述、耗时与有界尾部。
- 通过条件：事件均可达、批次形状正确、完成恰好一条、命令正文没有被回显。
- 失败条件：事件丢失、重复投递同一条事件、完成后仍继续产生事件、或把原始命令贴进完成文本。
- 清理/回滚：结束 watch；确认没有残留子进程；销毁一次性 Workspace。
- 来源/测试锚点：[monitor.go](../../internal/coding/tools/monitor.go)、[background](../../internal/coding/background)、[runtime_notification.go](../../internal/coding/runtime_notification.go)、[runtime_background.go](../../internal/coding/runtime_background.go)。
- 结果：`未执行`。

## MA-WATCH-002：行/批上限、限流、追赶提示与自动停止

- 分类：实时 Provider、安全敏感。变更等级：临时工作区；会产生费用。
- 前置条件：同 MA-WATCH-001。
- 隔离夹具：命令产生一行超过 500 字节的文本、一个超过 3000 字节的批次、若干空行，以及一段每 50 ms 输出一行的突发输出（持续 40 s 以上）。
- 步骤：分四轮启动 watch，分别覆盖长行、大批次、空行、突发输出。记录事件文本、被抑制事件后的第一条事件文本，以及突发持续 30 s 之后的文本与进程状态。
- 预期证据：长行按 rune 边界截断并带 `...(truncated)`；批次同样截断；空行不产生事件；突发期间超出 10 个 / 2 s 的事件被静默丢弃；突发结束后第一条事件前带一条
  `[{N} events suppressed -- output rate too high. …]`；持续 30 s 时出现
  `[Monitor stopped -- your script produced too much output …]`，pipeline 停止读取，而被监视的进程本身未被限流器杀死。
- 通过条件：五处上限都成立，两条提示文本与契约一致，限流器只停 pipeline。
- 失败条件：行/批无界、空行产生事件、抑制期间继续投递、自动停止时把进程也杀掉。
- 清理/回滚：`kill_background_task` 停止仍在运行的 watch；确认无残留进程。
- 来源/测试锚点：[background/line.go](../../internal/coding/background/line.go)、[background/rate_limiter.go](../../internal/coding/background/rate_limiter.go)、[monitor_test.go](../../internal/coding/tools/monitor_test.go)。
- 结果：`未执行`。

## MA-WATCH-003：停止 watch、单一完成与 TUI 两个动作

- 分类：实时 Provider、安全敏感。变更等级：临时工作区；会产生费用。
- 前置条件：同 MA-WATCH-001；交互终端；终端支持鼠标上报。
- 隔离夹具：一个 `persistent: true` 的 watch（例如 `sleep 600`），不产生任何输出。
- 步骤：要求模型启动该 watch，随后要求它调用 `kill_background_task` 并记录结果；再启动第二个 watch，按 `Ctrl+B` 聚焦 tasks pane，确认它出现在 `Watchers` 分组并标着 `Monitor`，按 `x` 停止、按 `Enter` 打开详情。
- 预期证据：模型侧结果是 `killed`（或对该 id 而言 `already_exited`、`not_found`）；每个 watch 的终止只产生一条模型可见完成，且模型自己 kill 的那次不会再被额外唤醒；tasks pane 行显示描述与自身耗时、运行态有 spinner、两个控制 `[✗]`/`[↗]` 可点；详情页是整宽容器（无边框、无 `[✗]`）：描述与状态、`$ command` 前言、有界输出尾部；运行中的 watch 打开时停在尾部并随新输出刷新，向上滚动后保持阅读位置，`Esc` 关闭返回对话。
- 通过条件：一条控制路径生效；模型 kill 不产生第二次唤醒；pane 的 kill 与 inspect 都经同一控制方法。
- 失败条件：模型无法停止 `persistent` watch、出现两条完成、或 pane 走了第二套控制路径。
- 清理/回滚：确认两组 watch 都已终止；销毁一次性 Workspace。
- 来源/测试锚点：[kill_background.go](../../internal/coding/tools/kill_background.go)、[tui/tasks_pane.go](../../internal/coding/tui/tasks_pane.go)、[runtime_background_test.go](../../internal/coding/runtime_background_test.go)、[tui/tasks_pane_test.go](../../internal/coding/tui/tasks_pane_test.go)。
- 结果：`未执行`。

## MA-WATCH-004：`Stop` 负载的 `background_tasks`

- 分类：本地确定性、安全敏感。变更等级：本地持久状态（写入独立 `PIPS_HOME`）。
- 前置条件：一个已信任的 `Stop` handler，把 stdin 原样写入日志文件。
- 隔离夹具：独立 `PIPS_HOME`；handler 与日志都在其中。
- 步骤：启动一个 `persistent` watch，触发一次普通回合结束，读取 handler 日志中的 `background_tasks`；再停止该 watch，重复一次。
- 预期证据：运行中存在 `{"id":"monitor-1","type":"monitor","status":"running","description":"<被监视命令>"}`，且没有 `command` 字段；超过 1000 字符的字段被裁剪并以 `… [+N chars]` 结尾；没有在运行的 watch 时，`background_tasks` 不出现在负载里。
- 通过条件：条目形状、类型、状态与裁剪标记都与契约一致。
- 失败条件：携带 `command`、在无运行 watch 时仍出现空数组、或文本未按 1000 字符裁剪。
- 清理/回滚：停止全部 watch；删除夹具 `PIPS_HOME`。
- 来源/测试锚点：[runtime_hooks.go](../../internal/coding/runtime_hooks.go)、[runtime_hooks_test.go](../../internal/coding/runtime_hooks_test.go)、[coding-hooks.md](../coding-hooks.md)。
- 结果：`未执行`。

## 分册判定

本分册全部用例为`通过`时，才能认为后台监视面通过验收；`未执行`不得计入通过率。若某个
Provider 或平台不适用，按索引的`不适用`规则记录理由。
