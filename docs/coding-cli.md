# Coding CLI contract

`pips exec` runs exactly one non-interactive Coding Runtime interaction. It is
an adapter over the same durable session, approval, event, and sandbox layers
used by future interactive frontends; it does not contain a second Agent loop.

## Input

```sh
pips exec "explain this package"
printf 'review the current changes' | pips exec
pips exec - < prompt.txt
```

At most one positional argument is accepted. With no argument, stdin must be a
pipe or redirected file. `-` explicitly reads stdin to EOF, including from a
terminal. A normal argument and non-empty redirected stdin are rejected as
ambiguous. Prompt text is preserved as supplied, must contain non-whitespace
content, must be valid UTF-8, and is limited to 1 MiB. NUL and control
characters other than LF, CR, and TAB are rejected.

## Configuration, trust, and sessions

The root configuration flags remain available to `exec`, for example:

```sh
API_KEY=... pips exec \
  --model openai/model-id \
  --reasoning high \
  --variant deep \
  --sandbox workspace-write \
  "fix the build"
```

The default configuration is `~/.pips/config.toml`. `--config <path>` selects
one replacement file; it does not inherit the default file, and project
`.pips/config.toml` is never discovered implicitly. `PIPS_MODEL`,
`PIPS_REASONING`, and `PIPS_VARIANT` mirror the three selection flags. API
surface and endpoint are registry data, not command-line fields. The removed
`--provider`, `--model-api`, `PIPS_PROVIDER`, `PIPS_MODEL_API`, and `[model]`
table are rejected rather than normalized.

A higher-priority `--model` or `PIPS_MODEL` selection starts from that target
model's default variant and reasoning; it never inherits those choices from the
lower-priority model. Pass `--variant`/`--reasoning` (or their environment
counterparts) together when an explicit target preset is wanted.

A minimal official model is one line:

```toml
model = "anthropic/model-id"
```

OpenAI uses Responses by default. Select Chat Completions for one model with a
model-level API override:

```toml
model = "openai/chat-model-id"

[[models]]
id = "openai/chat-model-id"
api = "chat_completions"
```

One file can define multiple selectable models, model-specific reasoning
levels, and named request presets:

```toml
model = "openai/gpt-model-id"

[[models]]
id = "openai/gpt-model-id"
context_window = 200000
reasoning_levels = ["low", "medium", "high", "xhigh"]
default_reasoning_level = "medium"
default_variant = "balanced"

[models.options]
max_output_tokens = 8192
temperature = 0.2
top_p = 0.95

[models.variants.balanced]
reasoning_level = "medium"

[models.variants.deep]
reasoning_level = "xhigh"
max_output_tokens = 32768

[[models]]
id = "anthropic/claude-model-id"
context_window = 200000

[models.options]
max_output_tokens = 64000
```

Connection settings are defined once per provider and inherited by its models:

```toml
model = "local/qwen2.5-coder:7b"

[providers.local]
api = "chat_completions"
base_url = "http://127.0.0.1:11434/v1"
allow_http = true
allow_private_ips = true

[[models]]
id = "local/qwen2.5-coder:7b"
context_window = 32768

[models.options]
max_output_tokens = 4096
top_k = 40
seed = 7

[models.options.extra_body]
service_tier = "flex"
```

Supported protocol values are `responses`, `chat_completions`,
`anthropic_messages`, and `generate_content`. `extra_body` is bounded and
add-only: it cannot replace authentication, model/input/tool/session fields or
typed request options. Pips does not contact a remote model catalog; only the
current model and local `[[models]]` entries appear in `/model`. Unknown model
capacity remains unknown rather than being guessed.

`context_window` is local context-capacity metadata. Request output is
configured only with `[models.options].max_output_tokens` (or a variant
override); that value is compiled into the provider request and may be
overridden by an explicit per-call limit.

Reasoning levels are model capabilities, but native protocol vocabularies are
still validated before a request. For budget-based Anthropic or Gemini models,
use `reasoning_budgets = { low = 2048, high = 8192 }`; selecting a mapped level
sends the numeric budget instead of an incompatible native effort enum.

`pips config show` prints the resolved API, endpoint origin, context capacity,
and every effective typed request option. Raw `extra_body` values stay hidden;
only their top-level key count and encoded byte count are shown.

`--trust-workspace` records the canonical workspace identity in `~/.pips` and
enables project `.pips` Skills, Bundles, and MCP definitions for this
invocation. It does not enable project configuration, grant a tool approval,
approve an MCP server, or enable Full Access. The trust decision is audited on
stderr.

`--session <id>` reopens only that durable session in the current workspace. A
reopened pending operation is reconciled first. If a human decision is still
needed, `exec` exits with code 3 without approving, retrying, or resolving it.

## Output

`--output plain` is the default. Stdout contains only the final assistant text
from the newly requested interaction and is written only after the Runtime
closes successfully. Human progress goes to stderr without ANSI escapes. Tool
progress exposes names and lifecycle only; arguments, results, reasoning,
prompts, diffs, and diagnostic messages are not rendered.

`--output jsonl` writes one `pips.coding.event/v1alpha1` safe-projected Runtime
event per line. It emits only events delivered by the actual Continue/Prompt
iterators—there is no CLI header, synthetic session event, or result record.
The first sequence number can therefore be greater than one after open or
resume. If a later operation or close fails, stdout remains a valid JSONL
prefix and the process exits nonzero.

## Exit codes and signals

| Code | Meaning |
| ---: | --- |
| 0 | Successful interaction and cleanup |
| 1 | Runtime, provider, output, or cleanup failure |
| 2 | Invalid input, flags, configuration, credential, workspace, or session |
| 3 | Approval/recovery decision required or denied |
| 4 | Sandbox, policy, authorization, or workspace-integrity failure |
| 130 | Interrupted (`SIGINT`) |
| 143 | Terminated (`SIGTERM`) |

Cancellation still gives the Runtime a bounded 10-second cleanup context.
Darwin/Linux stdin reads are cancellable while waiting for EOF, and broken
stdout pipes return through normal cleanup rather than terminating as exit
141. Errors are printed once on stderr; a plain cancellation does not add a
redundant `context canceled` line.

The default `workspace-write` sandbox fails closed. Review
[Coding execution security](coding-security.md) before using `full-access` or
running against an untrusted repository.
