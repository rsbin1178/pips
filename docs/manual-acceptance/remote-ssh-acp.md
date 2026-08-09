# Remote SSH 与 ACP 人工验收

[返回验收索引](index.md) · [证据记录模板](environment-evidence-record.md)

SSH 与 ACP 都复用同一个 Coding Runtime，但信任边界不同：SSH 在远端拥有配置/凭据/Session/Sandbox；ACP 客户端通过 stdio lifecycle 驱动本机 Runtime，不能获得额外文件根或静默信任 Workspace。

## MA-REMOTE-001：SSH 参数、版本与秘密隔离

- 分类：平台条件、安全敏感。变更等级：外部系统。
- 前置条件：本地/远端有 fixed root-owned OpenSSH 与 `pips`；专用远程账号/Workspace；远端配置自己的 `API_KEY`。
- 隔离夹具：本地设置人工 secret/proxy/Pips-path 形状变量；远端不含该值。
- 步骤：验证两端 `pips version` 完全相同；运行 `pips ssh <destination> --workspace <remote-path>`；负向测试本地 `--model`/`--config`/`--sandbox`/`--approval`、SSH option passthrough、非法 destination；远端版本不匹配时再测试。
- 预期证据：公开 subcommand 只接受一个 bounded destination 和 `--workspace`；ports/jump/identity 由 `~/.ssh/config`；remote argv 无 local secret；子环境不含 API key/proxy/Pips paths，`SSH_AUTH_SOCK` 仅供本地 client 且强制 `ForwardAgent=no`；版本不匹配拒绝启动 TUI。
- 通过条件：目标/argv/环境/版本约束准确，不调用 shell 拼接或备用 ssh binary。
- 失败条件：local API key 到远端、任意 ssh args/remote command 被接受、agent/X11/port/local command 转发启用、或版本 mismatch 继续。
- 清理/回滚：关闭连接并撤销人工变量；不修改用户 SSH 配置。
- 来源/测试锚点：[ssh.go](../../internal/coding/cli/ssh.go)、[sshclient](../../internal/coding/execution/sshclient)、[SSH 安全模型](../coding-security.md#remote-ssh-image-bridge)。
- 结果：`未执行`。

## MA-REMOTE-002：远程交互、PTY、Resize 与退出码

- 分类：平台条件、实时 Provider。变更等级：远端本地持久状态与费用。
- 前置条件：`MA-REMOTE-001` 通过；远端 `doctor` 通过；真实本地 TTY。
- 隔离夹具：远端一次性 Git Workspace。
- 步骤：连接并完成远端 Workspace trust；发送固定文本请求；调整本地终端尺寸；正常退出；重连后 SIGINT；制造远端已知非 0 退出；最后用普通 `ssh -tt '<cd && exec pips>'` fallback。
- 预期证据：远端 owns trust/config/model/Sandbox/Session；resize 立即传递；正常/信号/远端 status 按 0、130/143 或保留 5..255（排除本地保留码）映射；每次恢复 local terminal；fallback 可用且无图片 bridge。
- 通过条件：PTY/信号/status/terminal cleanup 正确，远端 Session 可恢复。
- 失败条件：本地配置覆盖远端、终端 raw mode 残留、status 被吞、或 fallback 被 bridge 依赖破坏。
- 清理/回滚：退出所有 SSH/Pips 进程，检查远端 Session/Workspace 归属。
- 来源/测试锚点：[ssh_test.go](../../internal/coding/cli/ssh_test.go)、[runner_unix.go](../../internal/coding/execution/sshclient/runner_unix.go)、[CentOS 7 checklist](../coding-ssh-centos7.md)。
- 结果：`未执行`。

## MA-REMOTE-003：SSH Clipboard 图片桥与临时对象

- 分类：平台条件、安全敏感、实时 Provider。变更等级：外部系统与费用。
- 前置条件：本地剪贴板图片支持；两端版本一致；远端模型支持图片。
- 隔离夹具：小 PNG/JPEG、超限图片、普通多行文本。
- 步骤：远程 TUI 中 Ctrl+V 一次并提交；重复按键检查 busy；bracketed paste 检查字节 `0x16` 不触发；中途断开；检查本地 ControlMaster 与远端 `/tmp/pips-bridge-<uid>` exact socket/type/owner/mode/inode。
- 预期证据：fresh 256-bit nonce、private non-persistent ControlMaster、单帧 strict protocol、1 MiB image limit、one worker/pending；只 push local image 到 remote draft，不读远端 clipboard/传任意文件/创建图片 temp；断开取消 upload/reap ssh/删除本 invocation socket。
- 通过条件：正向图片可用，所有负向/cleanup 边界正确，无 bytes/nonce/socket path/remote diagnostic 泄露。
- 失败条件：generic upload/reverse channel、trailing/malformed frame accepted、socket broad mode/wrong owner、或完成后遗留 invocation socket。
- 清理/回滚：按 [CentOS checklist cleanup](../coding-ssh-centos7.md#4-check-ephemeral-cleanup)只检查 exact 对象；不盲删共享目录。
- 来源/测试锚点：[imagebridge](../../internal/coding/imagebridge)、[SSH bridge checklist](../coding-ssh-centos7.md#3-exercise-the-interactive-bridge)。
- 结果：`未执行`。

## MA-REMOTE-004：ACP Initialize 与 Session lifecycle

- 分类：ACP 客户端、实时 Provider。变更等级：本地持久状态与费用。
- 前置条件：ACP v1 client/conformance harness；以 `pips acp` 启动，stderr 与 stdout 分离。
- 隔离夹具：canonical Workspace cwd；两个 Session。
- 步骤：发送 `initialize`；依次 `session/new`、`session/list`、`session/close`、`session/load`、`session/resume`、`session/delete`；空 Session 也重复；对 absent ID 再 delete；同时打开两个 Session。
- 预期证据：protocol version 1；new ID 立即 durable；load 先 replay history，resume 不 replay；一个 connection 最多 32 active sessions，各 Session 单 prompt、不同 Session 可并发；delete active 会 cancel/close 后永久删除 ordinary conversation，重复 delete idempotent；不删 subagent/Team Worker transcript。
- 通过条件：lifecycle 顺序、IDs、locks、并发与 replay 语义全部正确；stdout 只有 JSON-RPC。
- 失败条件：stderr 混入 wire、空 Session 不 durable、同 Session 并发 prompt、delete 越界 child Session。
- 清理/回滚：`session/close` 所有 active Session，关闭 ACP process；验收 Session 随隔离 home 清理。
- 来源/测试锚点：[server.go](../../internal/coding/acp/server.go)、[session.go](../../internal/coding/acp/session.go)、[server_test.go](../../internal/coding/acp/server_test.go)。
- 结果：`未执行`。

## MA-REMOTE-005：ACP Prompt、Mode/Model、Permission 与 Elicitation

- 分类：ACP 客户端、实时 Provider、安全敏感。变更等级：临时工作区与费用。
- 前置条件：client 宣告 form elicitation 与 permission 能力；Workspace trust 状态已知。
- 隔离夹具：文本 prompt、一次安全 tool approval、一次 Plan question；至少两个 configured models。
- 步骤：发送 `session/prompt`；收集 update/message IDs/usage；用 `session/set_mode` 及 select config option 在 agent/plan 间切换；用 model select option 切 model；触发 `session/request_permission` 与 form elicitation；中途 `session/cancel`。
- 预期证据：mode 两种 API 同步；model 切换保留 exact Session ID 并返回完整配置 state；live/replay message IDs 稳定；permission/elicitation exact-session-bound；cancel 停 owned work且 Session 可 resume；客户端不能通过 ACP 静默 trust Workspace。
- 通过条件：所有控制与同一 Runtime/TUI 语义一致，无第二 Agent loop 或权限捷径。
- 失败条件：mode/model 创建新 Session、client 自动 trust、permission 跨 Session、cancel 删除历史或继续 side effect。
- 清理/回滚：显式关闭 Session，恢复 Workspace 夹具。
- 来源/测试锚点：[config_options.go](../../internal/coding/acp/config_options.go)、[elicitation.go](../../internal/coding/acp/elicitation.go)、[ACP guide](../coding-acp.md#supported-protocol-surface)。
- 结果：`未执行`。

## MA-REMOTE-006：ACP Content blocks 与 Session MCP

- 分类：ACP 客户端、实时 Provider、安全敏感。变更等级：外部 MCP 与费用。
- 前置条件：client 支持 inline image/embedded resource/resource link；受控 stdio MCP server 的 executable 是 clean absolute path。
- 隔离夹具：bounded text/blob/image；超过 block/decoded-byte 限制夹具；合法/非法 MCP env overlays。
- 步骤：逐类发送 prompt content；在 `session/new`/lifecycle 供应一个 session-scoped stdio MCP 并调用其 tool；close 后检查未持久化；测试 relative command、危险 env、超过 64 servers 和 HTTP/SSE transport。
- 预期证据：支持内容安全转换为 Runtime parts；binary/blocks/text 在调用前有界；MCP args shell-free、env allowlist，定义只在内存/session 生命周期；unsupported transport/超限返回 typed invalid params 且无 partial open。
- 通过条件：合法内容/MCP 可用，负向输入均失败关闭且 wire 无 bytes/secret/raw internal event 泄露。
- 失败条件：ACP MCP 写入 user/project config、relative/shell command accepted、危险 env 透传、超限部分创建。
- 清理/回滚：关闭 ACP/MCP 进程，确认 private temp 清理。
- 来源/测试锚点：[content.go](../../internal/coding/acp/content.go)、[mcp.go](../../internal/coding/acp/mcp.go)、[mcp_test.go](../../internal/coding/acp/mcp_test.go)。
- 结果：`未执行`。

## MA-REMOTE-007：ACP Unsupported、Malformed、Disconnect 与兼容

- 分类：ACP 客户端、安全敏感、兼容。变更等级：本地持久状态。
- 前置条件：raw line-delimited JSON-RPC fixture harness；保存目标 SDK/客户端版本。
- 隔离夹具：unknown method、ACP v0/draft v2、audio/terminal/filesystem/additional-directory/auth/HTTP-MCP 等请求；malformed JSON/params；active prompt disconnect。
- 步骤：逐项发送；确认 connection 是否按 JSON-RPC 规则保持；disconnect active connection 后重新启动 `pips acp` 并 resume durable Session；运行：

  ```bash
  go test ./internal/coding/acp -count=1 -v
  ```

- 预期证据：unsupported method 为 method not found；malformed/unsupported params 为 typed bounded error且不泄露 stack/prompt/MCP env；connection 可在允许情况继续；disconnect/close 取消 owned Runtime、保留 Session；不宣传 Registry auth/官方 editor certification。
- 通过条件：raw stable-v1 wire tests 与目标真实 ACP client launch/lifecycle 均通过；unsupported 明确。
- 失败条件：接受额外 filesystem authority、错误关闭/污染 framing、disconnect 丢 Session、或本地测试被写成 editor certification。
- 清理/回滚：关闭 harness/进程；记录客户端版本和差异。
- 来源/测试锚点：[wire_test.go](../../internal/coding/acp/wire_test.go)、[types.go](../../internal/coding/acp/types.go)、[ACP compatibility](../coding-acp.md#compatibility-status)。
- 结果：`未执行`。

## 分册判定

声明 Remote SSH 时 `001` 至 `003` 必须在真实目标主机完成；声明 ACP v1 时 `004` 至 `007` 必须在 raw harness 和至少一个目标真实 client 完成。Cross-build、mock SSH 或 SDK unit test 不能替代真实终端/客户端证据。
