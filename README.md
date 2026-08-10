# pips

Go building blocks for AI applications. Current packages:

- **`ai`** — unified, provider-agnostic LLM client speaking the native wire
  protocols of **OpenAI** (Chat Completions + Responses), **Anthropic**
  (Messages), and **Google Gemini** (generateContent), plus reviewed profiles
  for seven OpenAI-shaped services. No vendor SDKs.
- **`agent`** — agent runtime core on top of `ai`: the autonomous loop (model
  → tools → results → model), typed tools, event streaming, stop conditions,
  input/output guardrails, durable partial approvals, correlated nested runs,
  and serializable sessions.
- **`workflow`** — provider- and Agent-independent declarative DAG runtime for
  fixed processes: strict versioned definitions, typed bindings and schemas,
  versioned host Actions, binary conditions, ordered selectors, exact-revision
  sub-workflows, bounded array batches, bounded stateful loops, explicit merge
  semantics, concurrency/retry limits, and process-local lifecycle events.
- **`agent/harness`** — stateful orchestration over the runtime: persistent
  session trees (JSONL) with branching, automatic context compaction, branch
  summaries, streaming with active cancellation, and skill/prompt-template
  resources.
- **`agent/continuation`** — durable, application-driven execution across bounded
  agent runs: optimistic lifecycle state, cumulative limits, explicit retries,
  pause/cancel, and time/signal wakeups without a resident scheduler.
- **`agent/team`** — durable coordination for a flat Team of independent Agent
  sessions: fixed Lead authority, coordinator-registered members, dependency tasks,
  exclusive claims and attempts, direct mailboxes, and scoped Agent tools.
- **`agent/goal`** — optional evidence-based completion policy over
  continuation, with custom or structured-output model evaluators.
- **`agent/loop`** — optional fixed or dynamic activation policy that persists
  waits while leaving timers and wake delivery to the application.
- **`agent/mcp`** — optional bridge from official Model Context Protocol client
  sessions to immutable `agent.Tool` snapshots, including progress and tool-list
  change notifications.
- **`agent/extension`** — trusted, compiled-in contribution composition with
  capability negotiation, deterministic hooks, lifecycle rollback, and leased
  immutable generations.
- **`agent/bundle`** — bounded local bundle manifests that select registered
  Extensions and declarative Skills, prompts, and typed assets without loading
  code or executing bundled scripts.

> **Status: v0.** The API is under active development and may change without
> notice. Pin a commit if you depend on it.

## 文档

- [AI 包使用指南](docs/ai/index.md)：Provider 中立接口、OpenAI/Anthropic/Gemini、Tools、结构化输出、中间件与测试。
- [Agent 包使用指南](docs/agent/index.md)：Agent/Session、工具控制、Harness、Extension/Bundle/MCP、可观测性与持久编排。
- [Coding Agent 人工验收手册](docs/manual-acceptance/index.md)：按能力划分的手工步骤、通过标准、证据要求和真实环境验收边界。

现有 Coding CLI、安全、ACP、SSH、Hook 与 Dynamic Subagent 发布文档仍保留在 `docs/`；上述三个入口提供中文学习和验收路径。

## Workflow core

`workflow` 用于构建确定的、非 ReAct 的通用流程。内置节点遵循 Coze
命名风格，但运行时不包含 AI、图像、RAG 或产品画布依赖：

| Node | Type key | Purpose |
|---|---|---|
| Start | `start` | 声明并传入 Workflow inputs |
| End | `end` | 校验并返回 Workflow outputs |
| Action | `action` | 调用宿主注册的版本固定 Action |
| Condition | `condition` | 二分条件路由 |
| Selector | `selector` | 有序多分支，first-match，带 default |
| Merge | `merge` | 独占或并行分支汇合 |
| SubWorkflow | `sub_workflow` | 同步调用精确 ID/revision/fingerprint 的子流程 |
| Batch | `batch` | 对数组执行串行或有界并行的内联子流程 |
| Loop | `loop` | 以数组、次数或无限模式串行执行有界内联流程 |
| Break | `break` | 提交当前迭代后退出所属 Loop |
| Continue | `continue` | 结束当前迭代并进入下一轮 |
| Set Variable | `set_variable` | 原子更新 Loop 局部变量 |

Batch 是可并行的数组 map，Loop 是带事务局部变量的串行状态循环；两者不
允许互相嵌套。父流程与子流程共享 Run ID、取消信号、总步数、叶子并发额度
和串行化事件输出。

## Coding agent status

`cmd/pips` is being built as a local, terminal-first coding agent. Its P0
execution foundation now includes Workspace-confined file tools, durable Shell
approval, native OS command isolation, and read-only Git change attribution.
It also includes durable, read-only Explore, Plan, and Review specialists.
The synchronous `run_subagent` Tool waits for one specialist; the asynchronous
`spawn_agent` Tool returns an `agent_id` immediately and delivers its terminal
result back to the parent Agent automatically. Both paths share the same
bounded manager, journal, permissions, result validation, and ordinary child
Session timeline. Built-in specialists receive only `read`, `ls`, `glob`, and
`grep`.

The disabled-by-default Dynamic custom Agents Alpha adds user and trusted-project
Markdown profiles without changing that authority model. A profile can select
currently delegable read, write, Shell, existing MCP, Skill, Tool Search, and
question capabilities, but only the active Runtime can authorize them and each
high-risk action retains the child-owned approval/audit path. See
[Dynamic custom Agents](docs/coding-cli.md#dynamic-custom-agents-alpha) for
the feature gate, definition format, trust roots, direct invocation, and the
explicit preview-before-promotion model-assisted draft workflow. Release
operators should also use the
[Dynamic Subagent Beta readiness runbook](docs/coding-dynamic-subagents-readiness.md)
for content-free admission signals, rollback, and the still-pending GA gate.
The single-session Runtime now composes durable Harness state, immutable
Extension/Skill/MCP generations, approval continuation, change attribution,
typed product events, and optional telemetry. The default command now opens a
single-column, chat-first TUI; `exec` remains the non-interactive adapter for
scripts and CI. The command also exposes configuration, session inspection,
and `doctor`.

The default `workspace-write` mode never falls back to an unsandboxed command.
The formal P0 support matrix is macOS Seatbelt and native Linux Bubblewrap, and
both fail closed when the real capability probe does not pass. WSL2 uses the
Linux backend but remains conditional until the same native smoke matrix passes
in the target environment. Native Windows and WSL1 are not supported.
Linux requires a trusted executable Bubblewrap 0.8.0 or newer at
`/usr/bin/bwrap` or `/bin/bwrap`; PATH-only installations are intentionally
ignored. CentOS 7 and other older distributions are conditional on that binary
and the complete runtime namespace/seccomp probe, not on distribution name.
`full-access` is an explicit, unsandboxed user override.

Configure a model in `~/.pips/config.toml`, for example:

```toml
mode = "agent"

[providers.openai.models."model-id"]
```

A single configured model is selected automatically; with multiple models,
mark one `default = true` or use `PIPS_MODEL`/`--model`. Then run
`API_KEY=... go run ./cmd/pips doctor`
to validate the selected model credential and the actual local Sandbox. The
probe also reports process-isolation strength. See
[Coding execution security](docs/coding-security.md) for the threat model,
runtime requirements, HOME-read limitation, and container guidance.

Run one request with a plain final answer on stdout:

```sh
API_KEY=... go run ./cmd/pips exec \
  --model <provider>/<model> \
  "explain the failing tests"
```

Prompts can also come from stdin (`printf 'review this change' | pips exec` or
`pips exec -`). Use `--output jsonl` for safe, schema-versioned Runtime events,
`--session <id>` to continue an exact session, and `--trust-workspace` to
persist the explicit decision that enables project `.pips` resources. See the
[Coding CLI contract](docs/coding-cli.md) for output, exit-code, signal, and
security details. Reviewed local command automation is documented separately
in [Coding lifecycle hooks](docs/coding-hooks.md).

ACP-capable editors can launch Pips as a stdio agent with `pips acp`. It speaks
stable ACP v1, uses the same configuration, credentials, Workspace trust,
durable sessions, Runtime, and Sandbox as the TUI, and keeps stdout
protocol-only. See the [ACP v1 agent guide](docs/coding-acp.md) for editor
process configuration, supported capabilities, and explicit exclusions.

Coding observability has two deliberate inputs. Raw `agent.Event` observers
receive run/turn/tool signals through the Harness boundary; content-free
`coding.TelemetryEvent` observers receive session, interaction, subagent,
approval, change, and integration signals. The optional Coding OpenTelemetry adapter
accepts application-owned providers and never creates exporters or shuts down
providers:

```go
productOTel, _ := codingotel.New(codingotel.Config{
    TracerProvider: tracerProvider,
    MeterProvider:  meterProvider,
})

runtime, _ := coding.Open(ctx, coding.OpenOptions{
    // Workspace, Config, Paths, Model/Credentials, and Execution omitted.
    AgentObservers:     []func(context.Context, agent.Event){recorder.Observe, rawOTel.Observe},
    TelemetryObservers: []coding.TelemetryObserver{productOTel},
})
```

Callbacks are synchronous and isolated per observer. Applications should use
OTel batch processors for slow export; the Runtime intentionally owns no
unbounded observability queue.

### Interactive coding agent

Configure `~/.pips/config.toml`, export the provider-neutral `API_KEY`, and run
`pips` in a real terminal:

```sh
API_KEY=... go run ./cmd/pips
```

The first visit to a Workspace asks whether project resources may be loaded.
Trust enables Pips-native `.pips` resources, shared `.agents/skills`, and the
project Agent roots `.pips/agents` and `.agents/agents`, but does not approve
tools, Skill scripts, MCP servers, Shell commands, or full access. Main
configuration always comes from `~/.pips/config.toml` or the one file selected
by `--config`; project trust does not change it.
Non-TTY input and `TERM=dumb` fail before Workspace configuration or
credentials are acquired and direct the caller to `pips exec`.

The TUI is intentionally single-column: transcript, a 1–8 line composer, and a
compact status line. All operations have keyboard paths; the mouse is used only
for transcript scrolling.

Run the same TUI in an existing remote Workspace through system OpenSSH:

```sh
pips ssh dev-host --workspace /srv/project
```

`dev-host` may be a bounded host or `~/.ssh/config` alias; SSH options remain in
OpenSSH configuration and are not accepted on the Pips command line. Local
Ctrl+V pushes one clipboard image into the live remote draft. The remote host
must have the exact same Pips version, its own model configuration and
`API_KEY`, and `pips` available to non-interactive SSH commands. Local provider
credentials are intentionally not forwarded. See the
[SSH CLI contract](docs/coding-cli.md#remote-interactive-ssh) and
[CentOS 7 host checklist](docs/coding-ssh-centos7.md).

| Action | Key |
|---|---|
| Send / steer while running | `Enter` |
| Queue follow-up while running | `Tab` |
| Toggle Agent / Plan Mode while idle | `Shift+Tab` |
| Insert newline | `Ctrl+J` or `Shift+Enter` when supported |
| Open command palette | `/` on an empty composer or `Ctrl+K` |
| Open latest tool / child Agent timeline; return to parent | `Ctrl+T` |
| Scroll / return to latest | `PageUp`, `PageDown`, `End`, or mouse wheel |
| Cancel operation / clear draft / confirm exit | `Ctrl+C` or `Esc` |

In the parent conversation, `run_subagent` and `spawn_agent` remain ordinary
inline Tool activities; no duplicate Agent card is created. `Ctrl+T` opens the
selected child's full-width ordinary Session timeline, including visible
assistant output and Tool activity, and toggles back to the parent. Other Tools
retain ordinary detail behavior.

The command palette provides `/new`, `/resume`, `/plan`, `/mode`, `/agents`, `/team`,
`/skills`, `/model`, `/permissions`, `/statusline`, `/theme`, `/tree`, `/fork`,
`/compact`, `/review`, `/reload`, `/status`, `/help`, and `/quit`. `/skills` browses user-invocable Skills from native
`~/.pips/skills`/`.pips/skills` and shared
`~/.agents/skills`/`.agents/skills` roots. Type `$` at a token boundary to
filter the same list and insert an exact `$skill-name` reference. Explicit
references apply only to that request; unknown tokens such as `$HOME` remain
ordinary text.
`/agents` opens durable Runs by default: it orders running work first, opens
the full child timeline with Enter, and cancels the selected running child with
`c`. Press `Ctrl+L` for the definition Library and `Ctrl+R` to return to Runs.
The Library shows safe provenance, declared capability requests, and
availability; Enter selects an available user-visible Agent, then the Composer
submits its direct task without asking the parent model to delegate it. Approval
is fail-closed: review defaults to deny, Enter applies only the highlighted
Runtime-provided choice, and an unknown outcome exposes only retry, mark-failed,
or acknowledge when the Runtime declares them. A custom child's approval or
question is resolved against that exact child Session, never the parent.

`/team [objective]` proposes a Coding Team through an explicit review before
admission; `/team` inspects current or recoverable Team work. Worker input,
interrupted-Work retry, Integration Apply/Reject/recovery, and cleanup each use
their own exact confirmation. `/resume` may show a bounded Team recovery badge,
but Enter resumes only the conversation; no Worker starts until the later Team
recovery confirmation. Private routing IDs, Worktree paths, Git refs/OIDs,
journal hashes, and approval tokens are never shown in these views.

`/model` changes the effective model only for the current pips process. Later
new or resumed sessions in that process inherit the selection, but Session
history and `config.toml` are not modified. Restarting pips returns to the
normal default/user-file/env/flag selection. Existing Session model metadata
is tolerated for compatibility and ignored.

`/permissions` changes only the current process's Sandbox profile and Approval
policy. The window presents human-facing choices: Read only, Workspace write,
or Full access; Network is Off, Ask when needed, or On for the two Sandbox
profiles, and is shown as unrestricted rather than editable under Full access.
Approval is separate: Ask before risky actions or Block actions that need
approval. A transition to Full access requires explicit confirmation, but does
not require restarting Pips: the idle Runtime is replaced in place with the
same Session/model. Changes rebuild the execution policy, do not write
configuration files or Session history, and read-only policy rejects
workspace/external writes before Approval or session grants. The window may
note that changes apply to the current process and reset on restart; internal
configured values and provenance are not part of the normal UI.

Operating mode is process-local. Configure `mode = "agent"` or `mode = "plan"`,
or override it with `PIPS_MODE`/`--mode`. `/plan` enters Plan Mode and `/mode`
selects either mode; neither action edits configuration or Session history.
Plan Mode removes workspace writes, arbitrary Shell, privileged MCP/Extension
tools, and other external side effects at the Runtime capability boundary. Its
only write exception is the current private document at
`~/.pips/plans/<session-id>.md`; the model cannot select that path. New creates
the document lazily, Resume reuses it, and Fork copies an existing snapshot.
Planning follows an evidence → material decisions → implementation design flow.
Material choices pause through `ask_user`; if a provider returns malformed
structured arguments, Pips narrows one retry to `ask_user_text` and ultimately
falls back to a Runtime-owned free-form prompt instead of silently planning
past the missing answer. Once requirements are decision-complete, the model
calls `plan_checkpoint` and then `present_plan`. `present_plan` atomically
persists the complete Markdown document and opens a path-free review that shows
the full Plan. You can keep planning (with optional feedback) or explicitly
approve that exact revision. Approval performs no extra model turn and grants
no Tool or Sandbox permission; only after the Plan interaction reaches idle
does the current process switch to Agent Mode. A restart never replays
historical Plan approval as authority. `pips exec --mode plan` cannot answer a
question or review and exits with input-required status 3.
Plan Mode can still read files visible to the process, so it is not a secret
isolation boundary.

The Shell Tool requires only `command`. `cwd`, `timeout_ms`, `permissions`, and
`justification` are optional; omit `cwd` for the Workspace root. `permissions`
may be `null` or a complete object such as
`{"write_paths":[],"network":false}`. Pips also normalizes the known
once-JSON-encoded form, but rejects incomplete, unknown, duplicate, or
recursively encoded permission values with a bounded actionable error.

`/status` may asynchronously refresh a compact Git branch/dirty summary
without modifying the repository. Product metadata under `.pips` is omitted.
Pips never automatically stages, commits, resets, cleans, stashes, switches
branches, or rewrites history. Interaction-only changes stay separately
labeled as `Pips-attributed changes` in the conversation timeline.

Set `NO_COLOR=1` for an ASCII, color-free view. The TUI stays in the terminal's
main buffer so stable conversation output remains selectable and scrollable,
and restores terminal mode before closing its Runtime, including
Ctrl+C, SIGINT/SIGTERM, startup failure, and normal exit paths.

## Design highlights

- **Zero third-party runtime dependencies** in the core (`stdlib` +
  `golang.org/x` only) — verified by `make deps-check`. Optional integrations
  such as `agent/mcp` declare their protocol SDK dependencies separately.
- **One request/response model** across providers: multi-modal messages
  (text, images, files), tool calling, structured output (JSON Schema),
  reasoning/thinking, usage accounting, normalized finish reasons and errors.
- **Streaming as iterators** (`iter.Seq2[StreamEvent, error]`) — `break`
  cancels the underlying HTTP request.
- **Bare clients never retry.** Retries, rate limiting, and observability are
  opt-in middleware that wrap the `ai.LanguageModel` interface.
- **Escape hatches everywhere**: bring your own `*http.Client`, override base
  URLs (OpenAI-compatible endpoints work out of the box), pass
  provider-specific options, and read raw responses.

## Quick start

```go
import (
    "github.com/rsbin/pips/ai"
    "github.com/rsbin/pips/ai/openai"
)

model := openai.New("gpt-4o", openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")))

resp, err := model.Generate(ctx, ai.Request{
    Messages: ai.Messages{
        ai.SystemText("You are terse."),
        ai.UserText("What is the capital of France?"),
    },
})
fmt.Println(resp.Text())
```

Switching to Anthropic or Gemini is a one-line change:

```go
model := anthropic.New("claude-sonnet-4-5", anthropic.WithAPIKey(key))
model := gemini.New("gemini-2.5-flash", gemini.WithAPIKey(key))
```

### OpenAI-shaped providers

`ai/openai/compat` reuses the OpenAI protocol implementation while preserving
the actual provider in responses, streams, errors, request options, endpoint
defaults, and capability reports:

```go
import "github.com/rsbin/pips/ai/openai/compat"

model := compat.DeepSeek("deepseek-reasoner")
model := compat.XAI("grok-4") // Responses API, including encrypted reasoning replay
```

| Provider | Constructor | Default API | Credential |
|---|---|---|---|
| OpenAI | `openai.New` | Auto: Chat or Responses | `OPENAI_API_KEY` |
| DeepSeek | `compat.DeepSeek` | Chat Completions | `DEEPSEEK_API_KEY` |
| Groq | `compat.Groq` | Chat Completions | `GROQ_API_KEY` |
| xAI | `compat.XAI` | Responses | `XAI_API_KEY` |
| OpenRouter | `compat.OpenRouter` | Chat Completions | `OPENROUTER_API_KEY` |
| Cerebras | `compat.Cerebras` | Chat Completions | `CEREBRAS_API_KEY` |
| Together AI | `compat.Together` | Chat Completions | `TOGETHER_API_KEY` |
| Mistral | `compat.Mistral` | Chat Completions | `MISTRAL_API_KEY` |

Profiles encode documented wire differences such as token-limit fields,
reasoning history, structured-output shape, and stream usage. Capabilities for
routers and unknown model IDs are intentionally conservative. Native protocols
such as Bedrock Converse, Vertex AI, Azure deployments, and Mistral
Conversations are not represented as compatibility profiles.

Provider-specific request options use the real provider key. For example,
DeepSeek extras belong under `ai.ProviderDeepSeek`, not `ai.ProviderOpenAI`.
Uploaded file IDs are provider-scoped; OpenAI Responses also accepts file URLs,
Anthropic file IDs opt into its Files API beta automatically, and Gemini uses
Files API URIs through `ai.FileURL`.

Run the provider switcher with the matching credential in the environment:

```sh
go run ./examples/provider-switch -provider deepseek -model deepseek-chat
```

See `examples/` for streaming, vision, tool calling, structured output, and
image generation.

## Agents

The `agent` package turns any `ai.LanguageModel` into an autonomous loop:

```go
import "github.com/rsbin/pips/agent"

add := agent.NewTool("add", "Add two integers.",
    func(ctx context.Context, args struct {
        A int `json:"a"`
        B int `json:"b"`
    }) (string, error) {
        return strconv.Itoa(args.A + args.B), nil
    })

a, _ := agent.New(model, agent.WithTools(add))
sess := agent.NewSession()

result, err := a.Run(ctx, sess, ai.UserText("What is 2+3?"))
fmt.Println(result.Text()) // model called add(2,3), saw "5", answered
```

Tool failures become error results the model can react to — they never abort
the run. `Agent.Stream` yields the loop as events (deltas, tool lifecycle,
turn boundaries); a `WithBeforeTool` gate can deny calls or pause the run for
human approval (`Session.ResolveToolCalls` can resolve any durable subset).
Named input/output guardrails validate the first input and each answer
candidate before it is committed.
Every event carries a run ID, parent run ID, agent name, and timestamp, so an
`agent.AsTool` child can be attributed without a tracing dependency.

A running loop can be steered (`Session.Steer`) or given follow-up work
(`Session.FollowUp`) without restarting. `WithTransformContext` and
`WithPrepareTurn` provide context curation and mid-run model-swap injection
points. The harness adds append-only persistence, compaction, branching,
`PromptStream`, and concurrent `Cancel`.

Routing, parallel agents, manager-owned subagents, in-run evaluator loops,
durable checkpoints, and application-owned handoffs are ordinary Go
composition, not a workflow DSL. See [Agent composition](docs/agents.md) and
`examples/agent-*`.

Cross-run autonomy composes through `agent/continuation`: a Worker performs one
bounded unit (the Harness adapter performs exactly one prompt run), then a
Controller chooses continue, wait, block, complete, fail, or cancel.
`agent/goal` implements durable completion evaluation and `agent/loop`
implements fixed or model-planned activation. Both are optional Agent-layer
Controllers: Team and Workflow runtimes can use continuation without importing
either package. The application remains responsible for calling `ResumeDue` or
delivering a signal; no package starts a scheduler or retry goroutine.

Policy setup names the continuation payloads explicitly:

```go
goalSetup, _ := goal.Prepare("all tests pass")
goalController, _ := goal.NewController(evaluator)

loopSetup, _ := loop.Prepare(ai.JSON(`{"prompt":"check deployment"}`))
fixedLoop, _ := loop.Every(5 * time.Minute)
```

Use each setup's `ControllerState` and `WorkInput` in
`continuation.CreateRequest`; register the corresponding Controller in
`continuation.Handlers`. Package examples show complete creation, advancement,
and explicit Loop wakeup.

`agent/team` coordinates multiple independent member Sessions without starting
them. The application Coordinator registers member resource references. For a
standard durable task attempt, `AttemptRuntime` removes the repeated Team and
Continuation choreography while retaining application ownership of the worker,
model/Harness, and result schema:

```go
attempts, _ := team.NewAttemptRuntime(teamRuntime, continuations,
    workerFactory, resultProjector,
    team.WithAttemptCoordinator(coordinator),
    team.WithAttemptHandlers(workerRef, controllerRef, controller),
)
result, err := attempts.Run(ctx, team.AttemptRunRequest{
    TeamID: teamID, TaskID: "review", AttemptID: "review-1",
    ContinuationID: "review-execution-1",
})
// workerFactory resolves result.Dispatch.MemberID/CapabilityProfileRef through
// the application resource registry; resultProjector maps terminal evidence.
```

Member and Lead toolsets bind Team/member identity and derive durable command
keys from provider ToolCall IDs. The model never supplies its actor, sender,
revision, command ID, or IDs for model-created tasks/messages. Member
registration and attempt start remain Coordinator-only. Team has no scheduler,
provisioner, Workflow graph, remote runtime, or distributed control plane;
applications compose those concerns above its finite command API.

### Extensions and local Bundles

`agent/extension` is the atomic activation boundary for trusted Go behavior.
An Extension returns declarative tools, Skills, prompt templates, AI
middleware, Agent hooks, typed assets, and an optional lifecycle. The Runtime
validates and starts the complete next generation before publishing it;
in-flight `Activation` leases keep the prior generation alive until released.
There is deliberately no application framework or generic `Host` object.

`agent/bundle` is the distribution layer above it. A strict
`pips.bundle/v1alpha1` JSON manifest may select only Extensions registered by
the embedding application and may load bounded local resources:

```go
loader, err := bundle.Open("./bundles/review")
if err != nil {
    return err
}
defer loader.Close()

bundle, err := loader.Load(ctx, bundle.DefaultManifestPath, bundle.Settings{
    Scope: bundle.ScopeProject,
    Trust: bundle.TrustApproved, // required explicitly for project Bundles
})
if err != nil {
    return err
}

runtime, err := extension.New(extension.WithExtensions(reviewExtension))
if err != nil {
    return err
}
activation, err := bundle.Activate(ctx, runtime)
if activation != nil {
    defer activation.Release(ctx)
}
if err != nil {
    return err
}

snapshot := activation.Snapshot()
model = snapshot.Model(model)
agentOptions, err := snapshot.AgentOptions(ctx, toolPolicy)
```

Skill `scripts/` directories are never executable capabilities by themselves.
A script becomes callable only when trusted application code explicitly wraps
it as an `agent.Tool`, or exposes it through an application-owned MCP adapter.
The P0 Bundle loader starts no goroutines, subprocesses, network clients,
package installers, shared libraries, or WASM runtimes.

### MCP tools

`agent/mcp` uses the official Go MCP SDK for lifecycle negotiation, stdio,
Streamable HTTP, cancellation, and all non-tool capabilities. The bridge maps
one server's paginated tool list into a portable snapshot:

```go
import (
    "os/exec"

    sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
    "github.com/rsbin/pips/agent"
    agentmcp "github.com/rsbin/pips/agent/mcp"
)

transport := &sdkmcp.CommandTransport{
    Command: exec.CommandContext(ctx, "my-mcp-server"),
}
client, err := agentmcp.Connect(ctx,
    &sdkmcp.Implementation{Name: "pips-runtime", Version: "v0.1.0"},
    transport,
    agentmcp.WithToolNamePrefix("workspace"),
)
if err != nil {
    return err
}
defer func() {
    _ = client.Close()
}()

tools, err := client.Tools(ctx)
if err != nil {
    return err
}
runtime, err := agent.New(model,
    agent.WithTools(tools...),
    agent.WithBeforeTool(approvalGate),
)
```

MCP annotations are untrusted hints, so the bridge never enables parallel
execution or bypasses `WithBeforeTool` automatically. `ToolListChanged()` is a
coalescing signal: fetch a new snapshot and construct the next immutable Agent
when it fires. `Session()` exposes the official SDK session directly for
prompts, resources, completions, roots, sampling, elicitation, and logging.
Use `mcp.StreamableClientTransport` in place of `CommandTransport` for HTTP.

## Development

```sh
make help        # list targets
make all         # fmt + vet + lint + test + build
make deps-check  # enforce core dependency policy and compile optional integrations
```

Requires Go 1.26+.
