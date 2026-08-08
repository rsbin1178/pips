# ACP v1 agent

`pips acp` exposes the Pips Coding Runtime as a stable Agent Client Protocol
(ACP) v1 agent over line-delimited JSON-RPC on stdin/stdout. It is intended for
editors and other ACP clients; it is not a human-readable command.

## Editor launch configuration

Configure a custom stdio ACP agent in the editor with these process settings:

```json
{
  "command": "pips",
  "args": ["acp"]
}
```

The exact outer configuration key is editor-specific. The client supplies the
canonical workspace through ACP `cwd`; no shell wrapper or `cd` command is
needed. If `pips` is not on the editor's PATH, use the absolute executable path.
Root selection flags still work, for example:

```json
{
  "command": "/absolute/path/to/pips",
  "args": ["--config", "/absolute/path/to/config.toml", "--model", "openai/model-id", "acp"]
}
```

Do not merge stderr into stdout. Stdout is reserved for ACP messages; bounded
human diagnostics use stderr.

## Authentication and trust

ACP authentication methods are not advertised. Start the editor so the Pips
process can obtain the same provider credentials used by the TUI and
`pips exec`, such as the configured provider environment variable. Credentials
are never sent as ACP metadata.

Each ACP `cwd` is checked against the existing Pips Workspace trust store.
Trusted workspaces may load project `.pips` resources; untrusted workspaces use
the same restricted behavior as other Pips surfaces. The ACP client cannot
silently mark a workspace trusted.

## Supported protocol surface

Pips implements ACP protocol version `1` and supports:

- `initialize`;
- `session/new`, `session/load`, `session/resume`, `session/list`, and
  `session/close`;
- stable, idempotent `session/delete` for durable conversation history;
- `session/prompt`, `session/cancel`, `session/update`, and
  `session/request_permission`;
- `agent` and `plan` session modes through `session/set_mode`;
- the same `agent`/`plan` choice as a stable select config option through
  `session/set_config_option` (both v1 APIs remain synchronized);
- the configured local model catalog as a stable `model` select option through
  `session/set_config_option`;
- text and resource-link prompts;
- inline images and embedded text/blob resources;
- stable form elicitation for Pips questions when the client advertises it;
- session-scoped stdio MCP servers supplied in lifecycle requests;
- stable message IDs on live and replayed user/assistant chunks;
- session title/time metadata and context-window usage updates.

`session/load` replays durable user/assistant history before returning.
`session/resume` reopens the same session without replay. A connection may own
multiple active sessions; each session permits one prompt at a time, while
different sessions may run concurrently.

The ID returned by `session/new` is durable immediately, even before the first
prompt. It can be closed, listed, loaded/resumed, or deleted while still empty.
Changing the model preserves that exact session ID and returns the complete
mode/model configuration state.

Deleting an active session cancels and closes its Runtime before permanently
removing the durable conversation. Deleting an absent/already-deleted ID is a
successful no-op. The delete boundary uses the normal single-writer lock and
will not delete Pips subagent or Team Worker transcripts.

ACP-supplied MCP servers are in-memory and session-scoped. Commands must be
clean absolute executable paths. Arguments are shell-free, and explicit
environment overlays use the same bounded allowlist and dangerous-name
rejection as other Pips child processes. These definitions are never written
to user/project MCP files.

## Explicitly unsupported

Pips does not advertise or implement:

- ACP v0 compatibility or draft ACP v2;
- ACP authentication, audio prompts, terminals, or client filesystem methods;
- additional workspace directories, reasoning/permission config options, or
  obsolete fork artifacts that are not in the current stable v1 schema;
- HTTP, SSE, or MCP-over-ACP server transports;
- experimental provider, NES, document, or extension methods.

Unsupported methods return JSON-RPC `method not found`. Malformed or unsupported
parameters return a typed error without closing the connection. Cancellation,
disconnect, and close stop owned Runtime work and preserve durable sessions for
later resume.

Additional directories remain disabled because Pips' reviewed filesystem
authority is one canonical Workspace; accepting extra roots without extending
the sandbox and tool-policy model would be unsafe. Provider credentials still
come from the Pips process environment/configuration, so Pips does not yet
satisfy the ACP Registry requirement for Agent Auth or Terminal Auth and should
be configured as a custom stdio Agent.

## Compatibility status

The adapter is tested both through SDK-typed unit tests and raw line-delimited
JSON-RPC fixtures. The raw fixtures pin current stable-v1 wire names for delete,
mode/model select config options, message IDs, usage, metadata, and request/
notification ordering. This matters because the newest released community Go SDK is
`v0.13.5`; its generated shapes are compatible, but some comments predate the
features' later stable-v1 announcements.

The official Registry matrix is a useful launch/lifecycle probe, not a complete
v1 conformance suite: its current matrix does not cover several newer stable-v1
features above. Pips does not claim editor certification from local wire tests;
real Zed/JetBrains packaging and Registry authentication remain separate
release checks.

## Operational notes

The process may host up to 32 active sessions. Lifecycle requests accept at
most 64 session MCP servers. Prompt text, block count, and decoded binary data
are bounded before Runtime invocation. Provider continuation signatures,
system messages, credentials, MCP environment values, and raw internal Coding
events are never projected onto the ACP connection.
