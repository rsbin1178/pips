# pips

Go building blocks for AI applications. Current packages:

- **`ai`** — unified, provider-agnostic LLM client speaking the native wire
  protocols of **OpenAI** (Chat Completions + Responses), **Anthropic**
  (Messages), and **Google Gemini** (generateContent). No vendor SDKs.
- `agent` — planned; will build on `ai`.

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

## Development

```sh
make help        # list targets
make all         # fmt + vet + lint + test + build
make deps-check  # enforce the zero-dependency policy
```

Requires Go 1.26+.
