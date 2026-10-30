# GLOSSARY

The project's terms, one line each. Code, tests, commit messages, specs, and tickets use these words and
no synonyms: a shared vocabulary is what keeps a sentence short here. The long form of any entry lives
in [`docs/`](docs/) — this file is the word, not the behaviour.

## Models and providers

- **Provider** — a vendor wire protocol pips speaks: OpenAI, Anthropic, Gemini, or an OpenAI-compatible
  gateway.
- **Adapter** — the package that encodes and decodes one provider's wire format; `New` returns a model.
- **Model** — an adapter bound to a model ID and its configuration, ready to generate or stream.
- **Knob** — the vendor field that actually drives a feature for that family; the thing R4 sends.
- **Profile** — the reviewed per-vendor record of which knob and which capabilities apply to a family.
- **Capability** — a declared property of a model (`vision`, `documents`, `structured_output`) that selects
  an implementation path; it never rejects a call on its own.
- **Omit** — "this protocol has no such field". It never means "we are unsure" or "the enums differ".
- **Class sentinel** — the `ai` error value a caller branches on with `errors.Is` (`ErrRateLimited`, …).
- **Warning** — an `ai.Response` entry reporting that something was downgraded or not sent.

## Agent runtime

- **Agent** — one bounded model/tool loop (`agent.New`).
- **Run** — one execution of an agent, identified by `RunID` and `ParentRunID`.
- **Turn** — one model call plus the tool calls it asks for.
- **Tool call / tool result** — the model's request and the result fed back to it; a tool failure is a
  result, not a run error.
- **Gate** — a `BeforeTool` policy that approves, denies, or pauses a tool call.
- **Stop reason** — why a run ended cleanly (`agent.StopReason`), as opposed to an error.
- **Guardrail** — input or output validation around a turn (`agent.GuardrailError` on rejection).
- **Steering** — a message injected while a run is in flight, as distinct from a follow-up after it.

## Sessions and storage

- **Harness** — the layer that owns durable conversation state around the runtime.
- **Session** — an append-only event tree with JSONL storage; the durable record, not the context window.
- **Branch** — a fork of a session at a point, with its own continuation.
- **Checkpoint** — a saved position a session can resume or fork from.
- **Compaction** — replacing the model-visible history with a summary at a visible boundary; the record
  is untouched.
- **Journal** — the durable event stream under a session directory.
- **Sidecar** — content-addressed media stored beside a session, removed by `pips session gc`.

## Continuation, goal, loop, team

- **Continuation** — the durable cross-run lifecycle: limits, retry boundaries, waits, wakeups.
- **Goal** — a completion condition checked by an evaluator; a controller decides whether to continue.
- **Loop** — a repeating activation with a persisted delay; the package never sleeps or polls itself.
- **Controller** — the policy object in `goal` or `loop` that returns the next decision.
- **Setup** — a prepared payload (`goal.Prepare` → `goal.Setup`); it does not start anything.
- **Team** — fixed lead and member identities with shared tasks, claims, and mailboxes.
- **Attempt** — one execution of a team task by one member, from claim to result.

## Workflow

- **Workflow** — a versioned DAG of nodes with schema contracts and conditional branches.
- **Node** — one step in a workflow with typed input and output.
- **Checkpoint resume** — restarting a workflow from its last durable step.

## Terminal agent

- **Exec mode** — `pips exec`: one non-interactive interaction from an argument, stdin, or `-`.
- **Goal mode** — `pips exec --goal`: continuation toward a completion condition under a token budget.
- **Plan mode** — the model drafts into an isolated session file; edits stay blocked until approved.
- **Hunk** — one reviewable change in a plan draft.
- **Sandbox scope** — `workspace-write`, `read-only`, or `full-access`.
- **Approval** — the explicit decision a scope-escaping operation waits for.
- **Frame** — one rendered screen of the TUI.
- **Live block** — the streaming answer or thinking block, rendered incrementally.
- **Frozen prefix / live tail** — the part of a live block whose rows are final, and the part that still
  grows.
- **Row segment** — the paired frozen and tail rows a live block renders into.
- **Scrollback** — the transcript retained above the viewport in inline mode.
- **User turn** — one prompt and everything it caused, from that prompt to the next; the unit the turn
  rail marks. Not the same as a **Turn**, which is one model call.
- **Turn rail** — the column of per-user-turn marks down the transcript's right margin: a position
  indicator, not a scrollbar, and one mark per user turn.
- **Watcher** — a background command or file watch that outlives the tool call that started it.

## Extensions

- **Skill** — a directory with a `SKILL.md` manifest that an agent can activate by name.
- **Skill catalog** — the loaded, validated set of skills a session advertises.
- **Activation** — loading one skill's full instructions, optionally with one text resource.
- **Bundle** — locally distributed declarative metadata an agent can load; not executable code.
- **Extension** — trusted Go code registered at build or install time, behind a generation boundary.
- **MCP server** — an external tool server spoken to over stdio or HTTP.
- **Plugin** — a portable Agent Plugins package that projects skills and MCP servers.

## Configuration

- **Layer** — where a setting comes from: user config, project config, or CLI flags, in that precedence
  order.
- **Trusted workspace** — a workspace identity recorded in `~/.pips`, required before project-scoped
  configuration takes effect.
- **Resolved model** — the model ID, provider, and settings after layering and catalog lookup.
