# Provider、模型与韧性人工验收

[返回验收索引](index.md) · [证据记录模板](environment-evidence-record.md)

本分册区分“配置可解析”和“真实 Provider 可工作”。前者不能替代后者。每个目标 Provider/protocol 至少执行一次适用的实时用例，并记录模型、时间、区域/endpoint 和费用范围；不得记录 `API_KEY`。

## MA-MODEL-001：模型选择、Variant、Reasoning 与优先级

- 分类：本地确定性。变更等级：本地持久状态。
- 前置条件：隔离配置可编辑；无需有效模型 ID 或凭据。
- 隔离夹具：配置两个模型，其中一个 `default = true`；为默认模型声明两个 variants、reasoning levels 与不同的 `request.max_output_tokens`。
- 步骤：依次运行默认 `pips config show`，再以 `PIPS_MODEL`/`PIPS_VARIANT`/`PIPS_REASONING` 覆盖，最后以 `--model`/`--variant`/`--reasoning` 覆盖环境值。
- 预期证据：默认、配置、环境、flag 的选择和 `source=` 完整显示；显式切换 model 时不会继承低优先级 model 的 variant/reasoning；不存在的 variant、未声明的 reasoning、多个模型但无 default 均退出 2。
- 通过条件：有效组合解析正确，无效/歧义组合全部失败关闭。
- 失败条件：跨模型继承、静默降级、未声明 reasoning 被接受，或来源错误。
- 清理/回滚：恢复下一实时用例需要的有效配置。
- 来源/测试锚点：[config.go](../../internal/coding/config/config.go)、[load.go](../../internal/coding/config/load.go)、[modelcatalog](../../internal/coding/modelcatalog/catalog.go)、[config_test.go](../../internal/coding/cli/config_test.go)。
- 结果：`未执行`。

## MA-MODEL-002：协议、Endpoint 与自定义 Provider 安全约束

- 分类：本地确定性、安全敏感。变更等级：本地持久状态。
- 前置条件：无需连接 endpoint。
- 隔离夹具：分别配置内建 Provider 和名为 `local` 的自定义 Provider。
- 步骤：验证以下协议各自可被解析：`openai/auto`、`openai/chat_completions`、`openai/responses`、`anthropic/messages`、`gemini/generate_content`。随后依次测试：自定义 Provider 缺 `base_url`、缺 `protocol`、HTTP 未设置 `allow_http`、私网地址未设置 `allow_private_ips`、URL 含 userinfo 或 fragment。
- 预期证据：支持的协议在相容 Provider/model 上通过；每个不安全或不完整组合退出 2，并指出具体字段；`config show` 展示 endpoint origin/protocol，但不输出凭据或 Authorization header。
- 通过条件：明确定义的合法配置通过，所有负向配置失败关闭；不会因为 `allow_http` 自动允许私网。
- 失败条件：未知协议被接受、HTTP/私网保护可被隐式绕过、输出秘密。
- 清理/回滚：删除负向夹具；不向这些 endpoint 发请求。
- 来源/测试锚点：[config.go](../../internal/coding/config/config.go)、[load_test.go](../../internal/coding/config/load_test.go)、[模型配置示例](../coding-cli.md#configuration-trust-and-sessions)。
- 结果：`未执行`。

## MA-MODEL-003：真实 Provider 文本流

- 分类：实时 Provider。变更等级：本地持久状态；会产生外部请求与费用。
- 前置条件：目标 Provider 的有效 `API_KEY`、真实 model ID、网络许可与预算；`doctor` 通过。
- 隔离夹具：干净 Workspace；prompt 不包含仓库或个人秘密。
- 步骤：

  ```bash
  pips --workspace "${PIPS_ACCEPT_WORKSPACE}" exec \
    --model "<provider/model>" \
    "只输出字符串 PIPS-PROVIDER-OK，不调用工具"
  ```

  对产品声明支持的每种 native protocol 重复一次；兼容 endpoint 另执行 `MA-MODEL-004`。
- 预期证据：退出 0；stdout 有非空最终文本且无事件 envelope；stderr 只包含安全进度；Session 中记录完成状态和 usage（若 Provider 提供）；没有 reasoning 内容或请求体泄露。
- 通过条件：请求成功关闭，结果可读，Session 可恢复，Workspace 无变化。
- 失败条件：模型构造/协议错误、流中断却退出 0、明文 reasoning/secret 泄露，或产生未请求的工具变更。
- 清理/回滚：从 shell 清除临时 key；记录 Provider request ID 时仅保留可公开/可脱敏部分。
- 来源/测试锚点：[generation.go](../../internal/coding/generation/generation.go)、[model](../../internal/coding/model)、[provider_smoke_test.go](../../internal/coding/tui/provider_smoke_test.go)。
- 结果：`未执行`。

## MA-MODEL-004：OpenAI-compatible 自定义 Endpoint

- 分类：实时 Provider、安全敏感。变更等级：外部系统。
- 前置条件：受控的 OpenAI-compatible 测试服务；明确其协议是 Chat Completions 或 Responses；仅在负责人批准的 endpoint 上执行。
- 隔离夹具：自定义 provider 必须显式配置 `base_url` 与 `protocol`；仅当实际需要时设置 `allow_http`/`allow_private_ips`。
- 步骤：先用 `config validate` 和 `config show` 复核解析结果；再执行与 `MA-MODEL-003` 相同的固定文本请求；最后故意改成不匹配的 protocol 并记录失败。
- 预期证据：匹配协议时退出 0；服务端只收到目标 model/endpoint 的一次逻辑请求（重试除外并须计数）；不匹配协议时退出非 0 且不伪造成功；Authorization 不出现在 CLI/Session/Telemetry。
- 通过条件：endpoint/protocol 选择准确，安全 opt-in 明确，错误协议不会静默切换 wire format。
- 失败条件：请求发往非目标 host、静默协议 fallback、凭据泄露、错误响应被当成成功。
- 清理/回滚：关闭测试服务，撤销临时 key，恢复正式配置。
- 来源/测试锚点：[modelcatalog](../../internal/coding/modelcatalog)、[OpenAI provider](../../ai/openai)、[兼容配置说明](../coding-cli.md#configuration-trust-and-sessions)。
- 结果：`未执行`。

## MA-MODEL-005：JSONL 安全事件流

- 分类：实时 Provider。变更等级：本地持久状态；可能产生费用。
- 前置条件：`MA-MODEL-003` 已通过。
- 隔离夹具：prompt 为固定无秘密文本；输出写到验收根目录。
- 步骤：

  ```bash
  pips --workspace "${PIPS_ACCEPT_WORKSPACE}" exec --output jsonl \
    "只回复 JSONL-OK，不调用工具" >"${PIPS_ACCEPT_ROOT}/events.jsonl"
  while IFS= read -r line; do printf '%s\n' "$line" | jq -e . >/dev/null; done \
    <"${PIPS_ACCEPT_ROOT}/events.jsonl"
  ```

- 预期证据：每个非空行都是独立 JSON；schema 为 `pips.coding.event/v1alpha1`；sequence 保持递增但无需从 1 开始；没有 CLI header/synthetic result；不含 API key、Authorization、原始工具参数/结果、私有 reasoning 或完整诊断。
- 通过条件：进程退出 0，所有行可解析且只属于安全投影；最终事件与完成状态一致。
- 失败条件：混入 plain 文本、JSON 不完整、敏感字段泄露或伪造 header/result。
- 清理/回滚：对事件文件脱敏复核后随验收根目录清理。
- 来源/测试锚点：[event_projection.go](../../internal/coding/event_projection.go)、[event_codec.go](../../internal/coding/event_codec.go)、[presenter_test.go](../../internal/coding/cli/presenter_test.go)。
- 结果：`未执行`。

## MA-MODEL-006：重试、取消与失败分类

- 分类：本地确定性；取消分支为平台条件。变更等级：无。
- 前置条件：Go 测试环境可用；不要用真实 429 故障轰击生产 Provider。
- 隔离夹具：仓库内 scripted model 测试夹具。
- 步骤：

  ```bash
  go test ./internal/coding/model -run 'Test.*Retry' -count=1 -v
  go test ./ai/middleware/retry -count=1 -v
  go test ./internal/coding/cli -run 'Test.*(Cancel|Signal|Broken|Exit)' -count=1 -v
  ```

  若目标发布流程提供受控 fault-injection endpoint，再补充一次 `429/Retry-After -> 成功`，记录请求次数和总延迟；否则该补充项记为`不适用`而不是通过。
- 预期证据：仅 retryable 且尚未产出 stream 数据的失败会按上限重试；非 retryable、context 取消和已开始 stream 的错误不进行不安全重放；SIGINT/SIGTERM 映射为 130/143；失败输出仍是合法 JSONL 前缀。
- 通过条件：上述测试全通过；任何 fault injection 结果符合相同契约，且请求次数不超过实现上限。
- 失败条件：无限重试、取消后继续发请求、stream 中途重放，或失败退出码错误。
- 清理/回滚：确认没有遗留测试服务或进程。
- 来源/测试锚点：[retry.go](../../internal/coding/model/retry.go)、[retry_test.go](../../internal/coding/model/retry_test.go)、[AI retry middleware](../../ai/middleware/retry)。
- 结果：`未执行`。

## MA-MODEL-007：Deferred Tool Search 开关

- 分类：实时 Provider、默认关闭。变更等级：本地持久状态；可能产生费用。
- 前置条件：至少配置一个可安全调用的 Extension 或 MCP tool；该工具在后续集成分册已验证。
- 隔离夹具：同一模型、同一 Workspace，分别以关闭和开启 `tool_search` 启动两个新 Session。
- 步骤：先用 `config show` 记录 resolved 值；关闭时要求模型“查找并使用指定延迟工具”；开启时重复；再用 `--tool-search=false` 覆盖启用配置。
- 预期证据：关闭时目录没有 `tool_search`；开启时 `tool_search` 可发现允许的 Extension/MCP tool，但内建工具仍直接可见；flag 覆盖后再次关闭。私有 server tool、Plan Mode 禁止项和 child 不可见项不会因搜索而越权出现。
- 通过条件：开关与 provenance 一致，搜索只扩大“已获准但延迟公开”的工具可发现性，不扩大权限。
- 失败条件：关闭时仍出现搜索工具、开启后暴露私有/越权工具，或搜索结果含秘密配置。
- 清理/回滚：关闭测试集成；恢复默认 `tool_search` 值。
- 来源/测试锚点：[system_prompt.go](../../internal/coding/system_prompt.go)、[catalog](../../agent/catalog)、[Coding CLI Tool Search 契约](../coding-cli.md)。
- 结果：`未执行`。

## 分册判定

本地 `MA-MODEL-001`、`002`、`006` 必须通过；每个实际支持的 Provider/protocol 必须有 `003` 或 `004` 的真实证据；启用 JSONL 与 Tool Search 的发布还必须通过 `005`、`007`。
