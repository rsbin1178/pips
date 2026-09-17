# Coding CLI contract

`pips exec` runs exactly one non-interactive Coding Runtime interaction. It is
an adapter over the same durable session, approval, event, and sandbox layers
used by future interactive frontends; it does not contain a second Agent loop.

`pips acp` is the stdio editor protocol surface over the same Runtime. Its
launch, capability, authentication, and lifecycle contract is documented in
the [ACP v1 agent guide](coding-acp.md).

需要按当前实现逐项手工验收 CLI、TUI、权限、Subagent、Team、SSH、ACP 与集成能力时，从 [Coding Agent 人工验收手册](manual-acceptance/index.md)开始，并单独记录真实 Provider、平台和运营观察证据。

## Remote interactive SSH

`pips ssh` opens the ordinary interactive Pips TUI on a remote host while
system OpenSSH continues to own host configuration, authentication, host-key
verification, transport encryption, and connection multiplexing:

```sh
pips ssh dev-host --workspace /srv/project
pips ssh deploy@example.internal --workspace .
```

The destination is one bounded host, `user@host`, or SSH configuration alias.
Pips accepts no SSH option passthrough; put ports, jump hosts, identities, and
other transport choices in `~/.ssh/config`. Only `--workspace` applies to this
subcommand. `--model`, `--config`, sandbox, approval, and related flags are
rejected because the remote Pips process owns those choices.

Both hosts must run the exact same `pips version`, and `pips` must be on the
remote non-interactive SSH command PATH. The remote host also owns
`~/.pips/config.toml`, Workspace trust, sessions, Sandbox prerequisites, and
the provider-neutral `API_KEY`. The local API key, Pips paths, proxy variables,
and other secret-shaped environment entries are removed from the environment
given to OpenSSH and are never added to remote argv. The local SSH agent remains
usable for authentication, but agent and X11 forwarding are forcibly disabled.
An explicit `SetEnv` in the user's own OpenSSH configuration remains
user-controlled SSH policy and should not contain provider credentials.

Outside bracketed paste, Ctrl+V requests one local clipboard image. Pips
normalizes a PNG or JPEG entirely in memory, enforces the existing 1 MiB image
limit, and sends it over a second non-PTY session multiplexed onto the private
live connection. The remote Composer shows the same immutable image reference
used for local attachments. Ctrl+V bytes inside bracketed paste remain literal
paste content. One worker and one pending request are allowed; additional
requests receive a local busy notice. Failures show a fixed notice without
printing image content, nonce, socket paths, or remote diagnostics.

The bridge is push-only and lives only for the interactive session. It cannot
read the remote clipboard, upload arbitrary files or paths, start a daemon, or
create an image temporary file. Closing the main session cancels upload work,
reaps OpenSSH, restores the local terminal, and removes only bridge objects
owned by that invocation. The `__bridge-session` and `__bridge-upload` commands
are hidden fixed protocol endpoints, not public scripting interfaces.

If clipboard image bridging is unnecessary, ordinary SSH remains the simplest
fallback:

```sh
ssh -tt dev-host 'cd /srv/project && exec pips'
```

This fallback has ordinary SSH behavior and does not intercept local Ctrl+V.
For native CentOS 7 validation, follow the
[real-host checklist](coding-ssh-centos7.md).

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
  --mode plan \
  --sandbox workspace-write \
  "plan the build fix"
```

The default configuration is `~/.pips/config.toml`. `--config <path>` selects
one replacement file; it does not inherit the default file, and project
`.pips/config.toml` is never discovered implicitly. `PIPS_MODEL`,
`PIPS_REASONING`, `PIPS_VARIANT`, and `PIPS_MODE` mirror their selection flags.
Operating mode uses `agent < config.toml < PIPS_MODE < --mode`; supported values
are exactly `agent` and `plan`. Protocol and endpoint are registry data, not
command-line fields. The removed
`--provider`, `--model-api`, `PIPS_PROVIDER`, `PIPS_MODEL_API`, and `[model]`
table are rejected rather than normalized.

A minimal mode and model configuration is:

```toml
mode = "agent"

[providers.anthropic.models."model-id"]
```

Plan Mode is a persisted state machine, not a reduced Tool catalog. The
ordinary catalog stays visible: Shell, MCP and Extension Tools, Subagents, and
`ask_user` all remain available, and the only restriction is an edit gate on
file writes. Plan Mode is a capability boundary for edits, not secret
isolation: files readable by the Pips process remain readable.

`pips exec --mode plan`, the interactive `/plan` command, `Shift+Tab`, and the
ACP `session/set_mode` or `mode` configuration option all drive the same four
states:

| State | Meaning |
| --- | --- |
| `Inactive` | Normal operation; no gate. |
| `Pending` | Plan mode was toggled on and no prompt has been sent yet. |
| `Active` | The edit gate is armed and the plan reminder is injected. |
| `ExitPending` | Plan mode was toggled off while a turn was in flight; the gate holds until that turn completes. |

A user toggle moves `Inactive` to `Pending`, and the first prompt of that
Session activates `Active`. An approved `enter_plan_mode` enters `Active`
directly and skips `Pending`. `Active` returns to `Inactive` on an approved or
abandoned `exit_plan_mode`, or when the user toggles plan mode off while idle;
toggling off during a turn defers the exit to `ExitPending`, which settles to
`Inactive` when the turn completes. Only `Active` is durable: the state is
stored in `<sessions-dir>/<session-id>/plan-mode.json`, and `Pending` or
`ExitPending` collapse to `Inactive` on restart because they depend on
interactions that did not survive it.

Two model-visible Tools gate the mode. Both take no arguments, must be the only
Tool call in their response, and pause for an explicit user decision:

- `enter_plan_mode` — "Use this tool when a task has ambiguity about the right
  approach or when the user asks you to write a plan. This tool enables a
  read-only plan mode where you explore the codebase and create an
  implementation plan for the user." An approved call returns "You have entered
  plan mode. You should now focus on exploring the codebase and creating an
  implementation plan." and arms `Active`; a declined call returns "The user
  declined to enter plan mode. Continue in normal mode without it." and leaves
  the state unchanged.
- `exit_plan_mode` — "Exit plan mode and present your plan to the user.\n\nUse
  this after you have finished writing your plan to the plan file in plan
  mode." It opens the plan approval view. The Runtime reads the plan from disk
  when the request is created; plan content is never passed as a Tool argument.

The plan itself is ordinary Markdown at `<sessions-dir>/<session-id>/plan.md`,
inside the Session-private directory. That directory is `0700` and the file is
a `0600` regular file; replacement is atomic, symlinks and special files are
rejected, UTF-8 and size bounds are enforced, and the file carries no metadata
header. The model is given the absolute path by the prompt and writes it with
the ordinary `apply_patch` Tool.

While the gate is armed, `apply_patch` may only target that exact file. Any
other target is rejected before execution with `Rejected: file edits are not
allowed in plan mode - the only editable file is the plan file (<plan path>).`
Plan-file writes are auto-approved: they create no permission request, are not
recorded as workspace changes, and never appear in `git status`. Shell is not
inspected, and Subagents are exempt from the gate because each Subagent
evaluates it from its own state.

Each request while plan mode is armed carries an ephemeral system suffix. It
starts with `Plan mode is active. Do not make any edits or writes to the
system.`, then a `## Plan File:` section that states either "A plan file exists
at <path>. You can read it and make edits using the apply_patch tool." or "No
plan written yet. Write your plan to <path> using the apply_patch tool.",
followed by the instruction that this is the only editable file and that the
turn should end with `ask_user` or `exit_plan_mode`. User turns append the
plan-iteration-versus-execution guidance that keeps feedback in the plan
instead of executing it, re-entering plan mode with a previous plan prepends a
`## Returning to Plan Mode` section, and leaving plan mode adds `You have
exited plan mode. You can now make edits, run tools, and take actions.` or
`You are now in Agent mode. Continue with the task in the new mode.` None of
these reminders are persisted to Session history.

Exit decisions return these exact Tool results:

| Outcome | Tool result |
| --- | --- |
| Approved, plan has content | `Your plan has been approved. You can now start coding.` |
| Approved, plan empty or missing | `Plan mode exit approved. No plan content was found - you can proceed.` |
| Request changes | `The user does not want to exit plan mode. Continue planning and ask the user what they would like to do.` |
| Abandoned | `The user chose to abandon the plan entirely (via the Abandon option in the plan approval dialog). Plan mode has been disabled. Do not call exit_plan_mode again unless the user explicitly asks to re-enter plan mode.` |

Review comments attached to an approval append `\n\nThe user approved the plan
with the following review comments:\n` and one comment per line; revision notes
attached to a request-changes decision append `\n\nUser revision notes:\n`. A
request-changes decision keeps plan mode `Active`. Approval resolves mid-turn:
the model receives the Tool result in the same turn and continues implementing.
It runs no extra model request, grants no Tool approval, and creates no Sandbox
exception.

In the interactive TUI the approval view shows a scrollable preview with
line-level comments: `a` approves (the label reads `approve w/ comments` when
comments are pending), `s` requests changes, `c` comments on the selected line
or range, `v` toggles a range anchored at the cursor, `x` drops the last
comment, `y` copies the plan, `q` abandons the plan and turns plan mode off,
`Tab` switches between preview and input, and `Esc` returns. An empty plan
shows `No plan written yet.` with the approve, request-changes, and quit
explanation. An agent-initiated `enter_plan_mode` opens a confirmation prompt
("Enter plan mode?" / "a approve · d decline") instead. `/view-plan` (aliases
`/show-plan` and `/plan-view`) reopens the
plan preview, the status line shows the `plan` flag while plan mode is on, and
the transcript prints `Agent entered plan mode`, `file edits outside session
plan.md blocked until plan mode exits`, and `Plan mode off`.

Non-interactive `pips exec --mode plan` never chooses on the user's behalf: a
plan decision or structured question that needs user input returns exit code `3`
(input required). Compaction preserves plan mode and injects the reminder again
on the compacted context. A Team Worker and a Lead with an active Team cannot
enter plan mode.

The earlier Plan protocol was removed and has no compatibility layer:
`read_plan`, `write_plan`, `present_plan`, `submit_plan`, and
`plan_checkpoint` no longer exist, the `plandoc` and `planflow` packages are
gone, plans are no longer stored under `~/.pips/plans/`, and the historical
`PlanProposal` projection is no longer produced. A Session parked on one of
those calls is not resumed by this binary. Do not edit the Session JSONL or any
plan file to force recovery, and do not expect the Runtime to treat a
historical Tool call or its arguments as authority.

A higher-priority `--model` or `PIPS_MODEL` selection starts from that target
model's default variant and reasoning; it never inherits those choices from the
lower-priority model. Pass `--variant`/`--reasoning` (or their environment
counterparts) together when an explicit target preset is wanted.

A minimal model on a built-in provider needs only its nested model table:

```toml
[providers.anthropic.models."model-id"]
```

When the file contains exactly one model, Pips selects it automatically. For
multiple models, set `default = true` on one model, or select one for the
process with `PIPS_MODEL`/`--model`. OpenAI uses Responses by default. Select
Chat Completions for one model with a model-level protocol override:

```toml
[providers.openai.models."chat-model-id"]
protocol = "openai/chat_completions"
```

One file can define multiple selectable models, model-specific reasoning
levels, and named request presets:

```toml
[providers.openai.models."gpt-model-id"]
default = true
context_window = 200000
reasoning_levels = ["low", "medium", "high", "xhigh"]
default_reasoning_level = "medium"
default_variant = "balanced"

[providers.openai.models."gpt-model-id".request]
max_output_tokens = 8192
temperature = 0.2
top_p = 0.95

[providers.openai.models."gpt-model-id".variants.balanced]
reasoning_level = "medium"

[providers.openai.models."gpt-model-id".variants.deep]
reasoning_level = "xhigh"

[providers.openai.models."gpt-model-id".variants.deep.request]
max_output_tokens = 32768

[providers.anthropic.models."claude-model-id"]
context_window = 200000

[providers.anthropic.models."claude-model-id".request]
max_output_tokens = 64000
```

Connection settings are defined once per provider and inherited by its models:

```toml
[providers.local]
protocol = "openai/chat_completions"
base_url = "http://127.0.0.1:11434/v1"
allow_http = true
allow_private_ips = true

[providers.local.models."qwen3-coder:7b"]
context_window = 32768

[providers.local.models."qwen3-coder:7b".request]
max_output_tokens = 4096
top_k = 40
seed = 7

[providers.local.models."qwen3-coder:7b".request.extra_body]
service_tier = "flex"
```

Supported protocol values are `openai/auto`, `openai/chat_completions`,
`openai/responses`, `anthropic/messages`, and `gemini/generate_content`.
`openai/auto` is resolved once from the model ID before Runtime construction.
Custom provider IDs require both `base_url` and `protocol`; their IDs remain the
connection identity shown in the TUI and events. `extra_body` is bounded and
add-only: it cannot replace authentication, model/input/tool/session fields or
typed request options. Pips does not contact a remote model catalog; only the
current model and locally configured nested model tables appear in `/model`.
Unknown model capacity remains unknown rather than being guessed.

Capabilities are declared, never fetched. Pips ships no model capability
database; declare what a model supports on the provider (inherited by its
models) or on a single model:

```toml
[providers.zhipu.capabilities]
tools = true

[providers.zhipu.models."glm-4v-plus".capabilities]
vision = true
reasoning = true
structured_output = false
```

Every field is optional and presence-aware: a declared `false` is distinct
from "not declared", and a model overrides only the fields it declares. The
available names are `text`, `vision`, `documents`, `audio_input`,
`video_input`, `tools`, `structured_output`, `reasoning`, `image_generation`,
`embeddings`, `reranking`, `prompt_caching`, `token_counting`, `web_search`,
and `code_execution`. An unknown name fails configuration loading.

Declarations layer on top of whatever the protocol adapter already reports for
the bound model, so a third-party model the adapter only knows as plain chat
can still be declared vision-capable. Two declarations change runtime behavior
today: `vision` gates image attachments in the composer, and
`structured_output` selects a native JSON Schema response format for subagents
instead of prompt-only JSON. Other declarations are accepted and reported as
`advisory` by `pips config show` until a consumer reads them. `tools = false`
is rejected, because Pips requires native tool calling.

`context_window` is local context-capacity metadata. Request output is
configured only with a model or variant `request.max_output_tokens`; that value
is compiled into the provider request and may be overridden by an explicit
per-call limit.

Reasoning levels are model capabilities, but native protocol vocabularies are
still validated before a request. For budget-based Anthropic or Gemini models,
use `reasoning_budgets = { low = 2048, high = 8192 }`; selecting a mapped level
sends the numeric budget instead of an incompatible native effort enum.

Which wire field carries a reasoning level on an OpenAI-shaped endpoint is a
property of the provider family, not of the model name. Every reviewed profile
declares the knob its family reads, and a model that reads a different one
overrides it by declaration — Pips never dispatches on the model string:

```toml
[providers.kimi.compatibility]
chat_reasoning = "reasoning_effort"

[providers.kimi.models."kimi-k2.6".compatibility]
chat_reasoning = "omit"
```

Accepted values are `reasoning_effort` (OpenAI-native, Groq, Cerebras,
Together, Qwen tier-native, Kimi k3, GLM-5.2 and later), `reasoning_object`
(`reasoning{effort}`: OpenRouter, xAI chat), `deepseek` (`reasoning_effort`
plus `thinking{type}`), and `omit`. A model layer overrides the provider layer,
which overrides the reviewed profile. `omit` is reserved for a protocol that
has no such field at all; it is never a way to say "this model may not read
it". A configured level is forwarded otherwise, and anything that is *not* sent
as configured is reported rather than dropped.

Family-specific thinking knobs that are not a portable reasoning level —
`enable_thinking`/`thinking_budget` on Qwen hybrid models, `thinking{type}` on
MiniMax and Kimi k2.x — travel through the model's `request.extra_body`.

`pips config show` prints the resolved protocol, endpoint origin, context capacity,
every effective typed request option, the resolved reasoning level and the
compatibility encoding that carries it (`resolved.reasoning.encoding`), plus a
`resolved.reasoning.warning` when the profile cannot encode the configured
selection. It also prints every declared capability (marked `declared`, or
`declared advisory` when no consumer reads it yet). Raw `extra_body` values
stay hidden; only their top-level key count and encoded byte count are shown.

`pips doctor` reports the same unencoded selection as a `reasoning warn` line
and exits successfully: only a `fail` changes the exit code. Doctor diagnoses
without modifying anything, and it stays runnable on a configuration it cannot
resolve — it prints `configuration fail` with the reason and next steps instead
of exiting before it can say what is wrong.

`--trust-workspace` records the canonical workspace identity in `~/.pips` and
enables project `.pips` Skills/Bundles/MCP plus shared `.agents/skills` for
this invocation. It does not enable project configuration, grant a tool or
Skill-script approval, approve an MCP server, or enable Full Access. The trust
decision is audited on stderr.

Reviewed local lifecycle commands are configured and approved separately from
Workspace trust. See the [Coding lifecycle hooks guide](coding-hooks.md) for
the native JSON schema, command protocol, handler trust review, and host
authority boundary.

### Dynamic custom Agents (Alpha)

Custom Coding Agents are reusable Markdown definitions. They are an Alpha
feature and are disabled by default: discovery and validation still work while
the gate is off, but Pips will not expose custom definitions to the parent
model or dispatch them. Enable the gate at the highest-precedence layer that
fits the use case:

```toml
# ~/.pips/config.toml (or the file selected by --config)
dynamic_subagents = true
```

```sh
PIPS_DYNAMIC_SUBAGENTS=true pips agents list
pips --dynamic-subagents agents run go-checker "inspect internal/coding"
```

`pips config show` reports the resolved `dynamic_subagents` value and its
source. Disabling the gate stops new custom dispatch without deleting
definitions or making historical child Sessions unreadable. Built-in
`explore`, `plan`, and `review` remain available through their compatible
protocol. Selected-Beta telemetry, staged distribution, rollback drill, and GA
evidence are defined in the
[Dynamic Subagent readiness runbook](coding-dynamic-subagents-readiness.md).

Production child admission and execution budgets are configured independently
in the same selected user file:

```toml
[subagent]
max_concurrent = 4
max_spawned_per_root_interaction = 8
max_auto_follow_ups = 4
max_turns = 64
max_tokens = 256000
max_tool_calls = 128
max_duration_minutes = 30
max_depth = 0
```

Every value is positive and hard-bounded; invalid or unknown fields make
configuration validation fail before Runtime open. A partial table changes only
the named values, and `pips config show` reports each effective value and source.
These are production defaults, not new authority: profile selectors, child-owned
approval, Sandbox, and the frozen generation capability intersection still apply.

Pips discovers `*.md` definitions from these roots in increasing precedence:

| Scope | Root | Trust requirement |
| --- | --- | --- |
| Shared user | `~/.agents/agents/` | none |
| Pips user | `$PIPS_HOME/agents/` (normally `~/.pips/agents/`) | none |
| Shared project | `.agents/agents/` | trusted Workspace only |
| Pips project | `.pips/agents/` | trusted Workspace only |

The filename stem is the canonical ID, so `go-checker.md` defines
`go-checker`. IDs use bounded lowercase ASCII letters, digits, and hyphens.
Higher-precedence definitions with the same ID suppress lower ones; conflicting,
invalid, or reserved builtin IDs are retained only as diagnostics. Pips does
not inspect either project root before the Workspace is trusted, and opens user
roots and files with safe regular-file and permission checks.

Each definition uses strict YAML front matter followed by a non-empty Markdown
instruction body. Unknown and duplicate fields are rejected rather than
ignored:

```markdown
---
schema: pips.agent/v1alpha1
name: Go checker
description: Inspect one bounded Go target and report evidence.
model: inherit
visibility:
  user: true
  model: false
delivery: [foreground]
limits:
  max_turns: 8
  max_tool_calls: 16
  max_duration: 2m
tools:
  allow: ["tool:read", "tool:grep"]
  require: ["tool:read"]
skills:
  allow: [golang-code-style]
  preload: [golang-code-style]
delegation:
  allow: [test-runner]
mcp:
  private: [source_control]
hooks:
  private: [review-child-shell]
output:
  format: text
---
Inspect only the task supplied by the caller. Report evidence and uncertainty.
```

`visibility.user` controls the Library and direct user invocation;
`visibility.model` controls whether the parent model can discover the profile.
`delivery` can contain `foreground`, `background`, or both. A profile's
`model` is either `inherit` or a configured Pips `provider/model` reference;
it cannot specify an endpoint, header, credential, or provider policy.
`output.format` is `text` or `json_schema`; JSON Schema output is validated
locally in addition to any provider structured-output support.

`tools.allow` and `tools.require` accept exact `tool:<wire-name>`,
`source:<kind>/<id>`, and `tag:<tag>` selectors. `require` must also be in
`allow`; a missing required capability makes admission fail instead of silently
weakening the profile. Skills select only already loaded, trusted Skills, and
`preload` must be a subset of `allow`.

`delegation.allow` names exact model-visible custom Agent IDs. Recursion is
foreground-only, serial, cycle-checked, and disabled unless
`[subagent].max_depth` is set to `1..3`; a recursive child receives only an
exact `run_subagent` target list, never `spawn_agent`.

`mcp.private` and `hooks.private` bind exact, already configured and trusted
Agent-private integration IDs. They do not accept inline commands, URLs,
headers, environment values, or credentials. A private MCP server is declared
in `$PIPS_HOME/mcp.json` or trusted-project `.pips/mcp.json` with
`"visibility":"agent_private"`:

```json
{
  "schema": "pips.mcp/v1alpha1",
  "servers": [
    {
      "id": "source_control",
      "type": "streamable_http",
      "visibility": "agent_private",
      "url": "https://mcp.example.test/api"
    }
  ]
}
```

The profile must also select individual tools through `tools.allow`, commonly
with `source:mcp/source_control`; binding the server alone grants no Tool.
Private server tools are absent from the parent model catalog and Tool Search.
Project MCP permission and Hook trust continue to use their existing exact
semantic fingerprints. See [Coding lifecycle hooks](coding-hooks.md) for the
private Hook declaration and trust flow.

Definitions describe a specialization; they never grant authority. At launch,
Pips freezes the intersection of the profile's selections with the active
Runtime's delegable catalog, operating mode, Workspace trust, Sandbox,
approval policy, model catalog, Skill/MCP generation, and per-call checks.
Consequently a profile may request `apply_patch`, `shell`, an eligible MCP Tool,
Tool Search, `ask_user`, an exact recursive target, or an exact configured
private integration, but it receives the capability only when every Runtime
and generation check permits it. It cannot define credentials, endpoints,
environment variables, commands, Sandbox or permission overrides, or a new
MCP/Hook process inline.

Every admitted child receives its own approval, question, change-audit, and
generation-lease scope. A profile's declared selectors are shown separately
from the dispatch-time effective plan; the child Session keeps the immutable
identity, digest, source, model, limits, and capability snapshot. Reloads
affect later interactions, not an already running child.

Use the explicit commands to manage and invoke definitions:

```sh
# Creates $PIPS_HOME/agents/go-checker.md only if it does not already exist.
pips agents init go-checker

pips agents validate
pips agents list --all
pips agents show go-checker

# Runs one named user-visible profile and prints a bounded JSON result envelope.
pips --dynamic-subagents agents run go-checker "inspect internal/coding"

# Parses an explicitly supplied Markdown definition for this run only.
pips --dynamic-subagents agents run ad-hoc-check \
  "review this diff" --definition ./reviewer.md

# Generates an isolated process-local draft, prints its full authority preview,
# and waits for exact promote/edit/discard input before writing anything.
pips --dynamic-subagents agents generate go-checker \
  "Review bounded Go changes and report evidence"
```

`--definition` never writes the supplied file into an Agent root and never
publishes it to the parent model. `agents run` is non-interactive: if its child
needs a Shell/patch approval, an unknown-outcome decision, or a structured
answer, it returns the matching classified error rather than auto-approving or
waiting indefinitely.

`agents generate` gives the proposal model no Workspace, Shell, MCP, Hook,
Skill, Subagent, approval, question, registry, or promotion Tool. Its only Tool
submits one strict Markdown proposal into an in-memory Session. Pips parses and
compiles that proposal against the current frozen Runtime generation, prints
the full Markdown plus declared/effective authority preview, and then accepts
only `promote`, `edit <markdown-file>`, or `discard`; EOF also discards. The
model cannot choose the action or path.

Promotion is digest-bound and never overwrites a file. User scope writes only
to `$PIPS_HOME/agents/<id>.md`; `--scope project` additionally requires a
trusted Workspace and targets `.pips/agents/<id>.md`. Edited bytes are parsed
and compiled again. A failed review or write retains only the process-local
draft until the command exits; a successful write becomes discoverable on the
next normal Runtime open or reload, not in the generation that reviewed it.

In the interactive TUI, open `/agents`, press `Ctrl+L` for the Library, choose
an available user-visible profile with Enter, then enter its task in the
Composer. `Ctrl+R` returns to durable Runs. Child approval and question prompts
identify the exact child and resolve only that child Session; they do not grant
or answer anything for the parent interaction.

### Interactive TUI themes and status line

The interactive TUI supports the automatic selection `auto` and these built-in
IDs:

- `default-dark` — the Pips default dark palette;
- `default-light` — the Pips default light palette;
- `dracula`;
- `nord`;
- `gruvbox-dark`;
- `catppuccin-mocha`;
- `one-dark`;
- `solarized-light`.

Open the command picker with `/` and choose `/theme`. The picker lists `auto`,
built-ins, and discovered user themes in stable order. Press Enter to apply a
selection immediately; Esc cancels. `auto` starts with the dark default and
then follows Bubble Tea's terminal background detection. An explicit ID is
not changed by later background messages. Theme changes update the managed
TUI view, input styles, Markdown rendering, and theme-separated Markdown
cache entries without restarting the session.

All TUI selections are stored together in the active configuration file under
`[tui]`:

```toml
[tui]
theme = "nord"
status_line = ["workspace", "session", "model", "phase"]
```

Theme and status-line selection are file-only: there is no `PIPS_THEME`,
`--theme`, status-line environment variable, or status-line flag. An absent
`[tui]` table or absent `theme` means `auto`; an absent `status_line` uses the
built-in default order. An explicit `status_line = []` is a valid empty
configurable line; transient safety and operation indicators may still appear.
A well-formed but unavailable theme ID is accepted by configuration loading,
but the TUI falls back to `auto` when the registry cannot resolve it and shows
a bounded notice.

`/statusline` keeps the existing picker interactions: toggle and reorder items,
preview the result, press Enter to save, or press Esc to cancel. `/theme` and
`/statusline` each update only their own `[tui]` field, so saving one never
removes the other. A failed save leaves the file and active presentation state
unchanged. If an atomic replacement has already committed but its
parent-directory sync is uncertain, the selected value is applied, the
relevant picker stays open, and a bounded durability warning is shown.

`--config <path>` selects the active file for loading and for both `/theme` and
`/statusline` persistence. It is a replacement path, not an overlay. An
existing explicit target must be a safe regular non-symlink file; a missing
explicit target is not created. The default configuration may be created when
saving a TUI preference. The source-preserving writer changes only the selected
`[tui]` scalar or array while preserving unrelated bytes, comments, and
settings. Malformed, unknown-field, ambiguous, unsafe, or concurrently changed
files are rejected rather than overwritten. To keep the editor fail-closed,
files containing TOML multiline strings or multiline `status_line` arrays are
rejected rather than risking a match inside unrelated content.

User themes are independent files below `$PIPS_HOME/themes` (normally
`~/.pips/themes`). Discovery does not create the directory. A theme file's
basename is its ID, so `themes/ocean.toml` defines `ocean`; the file cannot
replace a built-in ID. Files must be regular non-symlinks without group/world
write bits and use this schema:

```toml
schema = "pips.tui.theme/v1alpha1"
name = "Ocean Night"       # optional
inherits = "nord"          # optional built-in or user theme
background = "dark"        # optional: dark or light

[palette]
separator = "#3B4252"
composer_prompt = "#81A1C1"
muted = "#81A1C1"
workspace = "#D8DEE9"
session = "#88C0D0"
model = "#B48EAD"
idle = "#A3BE8C"
active = "#EBCB8B"
warning = "#D08770"
error = "#BF616A"
change = "#8FBCBB"
code = "#D8DEE9"
code_background = "#2E3440"
diagnostic = "#81A1C1"
```

`schema` is required. `name`, `inherits`, `background`, and palette fields are
optional; omitted values inherit from the parent, or from the default dark or
light built-in selected by `background`. Colors accept only `#RGB` and
`#RRGGBB`. IDs use lowercase letters, digits, and hyphens, are at most 32
bytes, and cannot be `auto`. Invalid files are skipped individually and do not
prevent startup. Inheritance supports built-ins and other user themes, with a
maximum depth of eight; missing parents and cycles are skipped. The picker
reports only a bounded count/category of ignored files, not paths or parser
details. Directory identity is rechecked throughout discovery; if the theme
directory is replaced during a scan, custom entries are discarded for that
scan. The active resolved theme remains usable for the current run even if its
file is later changed or removed; the next picker scan discovers the new state.

Theme definitions remain display-only. They are never written to Runtime
events or Session history; only the selected theme ID is stored in
`config.toml`. Existing `$PIPS_HOME/tui.json` files are ignored completely:
they are not read, migrated, written, or deleted. If `[tui].status_line` is
absent, Pips uses the built-in default instead. `NO_COLOR` still suppresses
ANSI output while preserving the same layout and width behavior. See the
[TUI config example](examples/tui-config.toml) and the small
[custom theme example](examples/tui-theme.toml).

User Skills are discovered from `~/.pips/skills` and the ecosystem-standard
`~/.agents/skills`; `PIPS_HOME` moves only the native Pips root. In interactive
mode, `/skills` browses the effective user-invocable set and `$skill-name`
selects a Skill for one request. The same exact-reference semantics apply to
`pips exec`; unknown dollar tokens remain unchanged.

`--session <id>` reopens only that durable session in the current workspace. A
reopened pending operation is reconciled first. If a human decision is still
needed, `exec` exits with code 3 without approving, retrying, or resolving it.
The same applies when `ask_user` requests structured input: non-interactive
execution returns classified `input_required` rather than selecting an answer
or waiting indefinitely. Team admission is also interactive-only:
`interaction_required` maps to exit 3, does not consume the proposal, and never
chooses clean versus HEAD-only admission, interrupted-Work retry, Integration,
or cleanup on the caller's behalf.

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

### Agent Plugins

pips implements the portable
[Agent Plugins 1.0.0](https://agent-plugins.org/specification) directory format.
Each immediate child below `$PIPS_HOME/plugins/` is one package with a required
root `plugin.json` and optional fixed `skills/` and `mcp.json` components.
Trusted workspaces may provide the same format below `.pips/plugins/`.

Use `list` to inspect user packages and `validate` to check one explicit package:

```sh
pips plugin list
pips plugin list --json
pips plugin validate /path/to/my-plugin
pips plugin validate /path/to/my-plugin --json
```

The JSON form contains `plugins` package projections and a `diagnostics` array.
Package projections include portable identity, the resolved root, and accepted
Skill and MCP server counts. A manifest error makes `validate` fail. Invalid
individual Skills or MCP server entries are reported but do not make otherwise
valid sibling components disappear.

Agent Plugins does not define an archive, registry, enablement database, or
upgrade/rollback transaction. Install by placing the portable directory below
a discovery root, then open or reload the Coding Runtime. pips does not accept
its former executable-target/capability manifest and provides no legacy
install, trust, grant, enable, upgrade, rollback, logs, or remove commands.

## Exit codes and signals

| Code | Meaning |
| ---: | --- |
| 0 | Successful interaction and cleanup |
| 1 | Runtime, provider, output, or cleanup failure |
| 2 | Invalid input, flags, configuration, credential, workspace, or session |
| 3 | Approval/recovery decision or structured user input required/denied |
| 4 | Sandbox, policy, authorization, or workspace-integrity failure |
| 5–129, 131–142, 144–255 | Exit status preserved from the remote OpenSSH/Pips session |
| 130 | Interrupted (`SIGINT`) |
| 143 | Terminated (`SIGTERM`) |

Cancellation still gives the Runtime a bounded 10-second cleanup context.
Darwin/Linux stdin reads are cancellable while waiting for EOF, and broken
stdout pipes return through normal cleanup rather than terminating as exit
141. Errors are printed once on stderr; a plain cancellation does not add a
redundant `context canceled` line.

The default `workspace-write` sandbox fails closed. Review
[Coding execution security](coding-security.md) before using `full-access` or
running against an untrusted repository. Linux requires Bubblewrap 0.8.0 or
newer at `/usr/bin/bwrap` or `/bin/bwrap` and must pass the real capability
probe; a successful upstream build test alone is insufficient. CentOS 7 and
other older distributions are supported only when this same probe passes.
