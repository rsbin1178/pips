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
    System:   "You are terse.",
    Messages: []ai.Message{ai.UserText("What is the capital of France?")},
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
// workerFactory resolves result.Dispatch.SessionRef to its independent Harness
// Session; resultProjector maps terminal evidence to Team result/messages.
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
if err != nil {
    return err
}
defer activation.Release(ctx)

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
