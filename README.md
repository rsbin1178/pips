# pips

Go building blocks for AI applications. Current packages:

- **`ai`** — unified, provider-agnostic LLM client speaking the native wire
  protocols of **OpenAI** (Chat Completions + Responses), **Anthropic**
  (Messages), and **Google Gemini** (generateContent). No vendor SDKs.
- **`agent`** — agent runtime core on top of `ai`: the autonomous loop (model
  → tools → results → model), typed tools, event streaming, stop conditions,
  approval gates with pause/resume, and serializable sessions.

> **Status: v0.** The API is under active development and may change without
> notice. Pin a commit if you depend on it.

## Design highlights

- **Zero third-party runtime dependencies** in the core (`stdlib` +
  `golang.org/x` only) — verified by `make deps-check`.
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
human approval (`Session.ResolvePending` resumes it). Sessions serialize to
JSON for persistence. See `examples/agent-*`.

## Development

```sh
make help        # list targets
make all         # fmt + vet + lint + test + build
make deps-check  # enforce the zero-dependency policy
```

Requires Go 1.26+.
