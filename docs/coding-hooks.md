# Coding lifecycle hooks

Pips supports reviewed command hooks around the Coding Runtime. The lifecycle
events and their control effects follow the current [Codex Hooks](https://learn.chatgpt.com/docs/hooks)
model where Pips has the same Runtime boundary. The configuration and command
response schema are deliberately Pips-native: do not copy `.codex`, Claude
Code, or Pi configuration into Pips expecting it to run.

A trusted hook is local automation with the authority of the operating-system
user that starts Pips. It runs outside the Coding Sandbox and model-tool
approval flow. Review it as carefully as any shell script that can read the
invoking environment and access paths available to that user.

## Configure and trust hooks

Pips reads strict JSON from these sources, in this order:

1. `$PIPS_HOME/hooks.json` (normally `~/.pips/hooks.json`), when it is a
   private regular file.
2. `.pips/hooks.json` in the selected Workspace, only after that Workspace has
   existing Pips project trust.

An untrusted project hook file is not inspected. Discovery also does not grant
execution authority: each exact command definition must be explicitly trusted
in Pips-owned `$PIPS_HOME/hook-trust.json`. User definitions are trusted for
the user configuration; project definitions are additionally bound to the
current Workspace filesystem identity. Changing an event, matcher, command, or
timeout changes the definition fingerprint and requires review again.

```sh
pips hooks list
pips hooks list --json
pips hooks trust user/PreToolUse/0/0
pips hooks trust project/SessionStart/0/0 --yes
```

`trust` prints the exact source, event, matcher, command, timeout,
fingerprint, Workspace CWD, and host-authority warning before recording the
decision. It asks for terminal confirmation; automation must supply `--yes`
after that reviewable command line. `list` and `trust` never open a model
Runtime or execute a handler.

## Configuration format

Both files use `pips.coding.hooks/v1alpha1`, reject duplicate and unknown JSON
fields, and support command handlers only:

```json
{
  "schema": "pips.coding.hooks/v1alpha1",
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup|resume|compact",
        "hooks": [
          {
            "type": "command",
            "command": "./scripts/pips-session-start",
            "timeout": 10
          }
        ]
      }
    ],
    "PreToolUse": [
      {
        "matcher": "^(Bash|apply_patch)$",
        "hooks": [
          {
            "type": "command",
            "command": "./scripts/check-tool-call",
            "timeout": 30
          }
        ]
      }
    ]
  }
}
```

The supported events are:

| Event | Matcher target | Pips effect |
| --- | --- | --- |
| `SessionStart` | `startup`, `resume`, or `compact` | Adds trusted context to the next root request. |
| `SessionEnd` | Pips close reason | Advisory main-session cleanup only. |
| `UserPromptSubmit` | ignored | Adds context, blocks, or stops prompt admission. |
| `PreToolUse` | tool name | Blocks, adds context, or rewrites a proposed local tool call. |
| `PermissionRequest` | tool name | Allows, denies, or defers Pips's normal approval UI. |
| `PostToolUse` | tool name | Adds context or replaces the model-visible result after execution. |
| `PreCompact` | `manual` or `auto` | Can stop before compaction. |
| `PostCompact` | `manual` or `auto` | Can stop the next continuation after compaction. |
| `SubagentStart` | Pips child role | Adds trusted context to that child. |
| `SubagentStop` | Pips child role | Can continue a completed child with a follow-up. |
| `Stop` | ignored | Can continue a completed primary turn with a follow-up. |

`matcher` is an RE2 regular expression. Omit it, use `""`, or use `"*"` to
match every occurrence. `UserPromptSubmit` and `Stop` retain a configured
matcher for source fidelity but ignore it. Tool hooks match Pips's canonical
tool name and these Codex-compatible aliases:

- `shell` also matches `Bash`;
- `apply_patch` also matches `Edit` and `Write`;
- `run_subagent` and `spawn_agent` also match `Agent`.

MCP and other local function tools match their exact Pips tool name. Matching
groups from user and project files all run. Commands for one event start
concurrently, while their output is merged in deterministic source order.

`timeout` is a positive whole number of seconds. The normal default and upper
bound are 600 seconds. `SessionEnd` defaults to one second and has a
three-second upper bound so it cannot hold resource cleanup indefinitely.

## Command protocol

Pips invokes each trusted matching handler as:

```text
/bin/sh -c <reviewed command>
```

The command runs with the Workspace as CWD, inherits the invoking CLI process
environment snapshot, and receives one UTF-8 JSON object on standard input.
Pips never interpolates prompt, tool, or session data into the command string;
dynamic data travels only through stdin.

Every input begins with:

```json
{
  "schema": "pips.coding.hook-input/v1alpha1",
  "session_id": "s_...",
  "cwd": "/absolute/workspace",
  "hook_event_name": "PreToolUse"
}
```

The event-specific fields are:

| Event | Additional fields |
| --- | --- |
| `SessionStart` | `source` |
| `SessionEnd` | `reason` |
| `UserPromptSubmit` | `prompt`, `has_non_text_content`, optional `prompt_truncated` |
| `PreToolUse` | `tool_name`, `tool_use_id`, `turn`, `tool_input`, optional `tool_input_truncated` |
| `PermissionRequest` | Pre-tool fields plus `justification` |
| `PostToolUse` | Pre-tool fields plus bounded `tool_response` |
| `PreCompact` | `trigger`, `estimated_tokens`, `threshold_tokens` |
| `PostCompact` | `trigger`, `tokens_before`, `tokens_after`, `duration_ms` |
| `SubagentStart` | `agent_id`, `agent_type`, `task` |
| `SubagentStop` | `agent_id`, `agent_type`, `stop_hook_active`, `last_assistant_message` |
| `Stop` | `stop_hook_active`, `last_assistant_message` |

Prompt input excludes attachment bytes and hidden Runtime/model state.
`tool_response` is a bounded text-only projection with error and truncation
metadata; non-text parts are omitted. Inputs, stdout, stderr, reasons, and
model-visible hook context are bounded.

## Handler responses

Successful stdout may be an empty response or one strict JSON object:

```json
{"decision":"allow"}
{"decision":"block","reason":"repository policy requires a test"}
{"additional_context":"Use the repository release checklist."}
{"decision":"allow","updated_input":{"path":"safe/file.txt"}}
{"continue":false,"stop_reason":"do not continue after compaction"}
{"system_message":"The local policy script found an advisory issue."}
```

`deny` is an alias for `block`. `system_message` becomes a bounded hook
diagnostic. Plain non-JSON stdout adds context only for `SessionStart`,
`UserPromptSubmit`, and `SubagentStart`; it is ignored for the other events
except that `Stop` and `SubagentStop` require JSON and diagnose plain output.

Control behavior is event-specific:

- `PreToolUse`: `block` creates an ordinary model-visible tool denial.
  `decision:"allow"` plus `updated_input` replaces the arguments object
  before every remaining Pips guard, approval check, Sandbox policy, and tool
  validation. It does not grant any permission.
- `PermissionRequest`: `allow` resolves the one request without showing Pips's
	approval UI; `block` denies it; no decision defers to normal human approval.
	Its bounded block reason is preserved as model-visible approval-denial
	feedback. If multiple hooks decide, a denial wins.
- `PostToolUse`: `block` cannot undo side effects. It replaces the original
  model-visible tool result with hook feedback; context is appended to the
  model-visible result. `continue:false` also suppresses normal processing of
  the original result and continues from hook feedback.
- `UserPromptSubmit`: `block` rejects the submitted prompt before Pips creates
  an interaction record or sends a model request. `continue:false` also stops
  prompt admission.
- `PreCompact`, `PostCompact`, and `SessionStart` after root compaction:
  `continue:false` prevents the next model continuation. A pre-compaction stop
  prevents the compaction itself; a post-compaction stop leaves the durable
  compaction intact. `decision:"block"` and exit status 2 are not compaction
  controls and are diagnosed rather than interpreted as a stop.
- `SubagentStart`: context is injected into the child prompt. A
  `continue:false` response is accepted for compatibility but does not prevent
  child startup.
- `SubagentStop` and `Stop`: `block` creates a follow-up prompt with its
  reason. `continue:false` takes precedence and ends normally instead.
- `SessionEnd`: advisory only; output never keeps a session open.

Exit status 2 is equivalent to `block` for an event that supports blocking,
using bounded stderr as the reason. Any other non-zero exit, timeout, launch
error, oversized output, or malformed response emits an
`integration.diagnostic` with component `hook` and lets the Runtime continue.
Pips waits for all commands started for an event; parent-operation cancellation
cancels and joins them.

## Runtime coverage and boundaries

These hooks apply to the primary Coding Runtime used by interactive Pips,
`pips exec`, resume, SSH, and ACP. `SessionEnd` is primary-session only. Pips
uses parent-owned lifecycle adapters for `SubagentStart` and `SubagentStop`;
Team Worker is a Pips coordination runtime and is intentionally outside this
user-configurable hook surface.

The release does not add HTTP/direct-argv handlers, hot reload, automatic trust
prompts, a hook bypass flag, or a parser/executor for another product's hook
configuration. Remove a handler from its active `hooks.json` to disable it;
stale trust records are inert.
