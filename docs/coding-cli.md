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
  --provider openai \
  --model model-id \
  --model-api responses \
  --sandbox workspace-write \
  "fix the build"
```

`--trust-workspace` records the canonical workspace identity in `~/.pips` and
enables project `.pips` configuration, Skills, Bundles, and MCP definitions for
this invocation. It does not grant a tool approval, approve an MCP server, or
enable Full Access. The trust decision is audited on stderr.

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
