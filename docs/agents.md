# Agent composition

`agent` is a general runtime, not a workflow engine or a coding-agent shell.
Its stable responsibility is one controllable model/tool loop. The harness
adds durable conversation state. Applications compose multiple loops with
ordinary Go control flow.

This boundary follows the small, composable patterns in Anthropic's
[Building effective agents](https://www.anthropic.com/engineering/building-effective-agents),
the control and handoff distinctions in the OpenAI Agents SDK
[orchestration guide](https://openai.github.io/openai-agents-python/multi_agent/),
and the durable-state guidance in Anthropic's
[long-running agent harness](https://www.anthropic.com/engineering/effective-harnesses-for-long-running-agents).

## Layers

| Layer | Owns | Does not own |
| --- | --- | --- |
| `agent` runtime | Model/tool loop, typed tools, ordered events, run identity, guardrails, approval interruption, deterministic stops, progress, steering, context hooks | Persistence backend, workflow graph, business routing, tracing backend |
| `agent/harness` | Append-only session tree, JSONL storage, context reconstruction, compaction, branches, streaming prompt lifecycle, active cancellation, custom checkpoints | Autonomous scheduling, conversation ownership policy, vendor control plane |
| Application | Routing, parallel requests, evaluator criteria, approval UI/policy, credential/model selection, handoffs, traces/evals storage | Runtime protocol internals |

## Control and visibility

Name agents with `agent.WithName`. Every event and `RunResult` has a `RunID`,
`ParentRunID`, and agent name; events also have a UTC timestamp. A tool can
read the same identity with `agent.RunMetadataFromContext`. `agent.AsTool`
propagates the parent context, so a shared event observer can reconstruct a
manager/subagent tree without an SDK-specific tracer.

Use conversation and side-effect controls at different boundaries:

```go
a, err := agent.New(model,
    agent.WithName("support"),
    agent.WithInputGuardrail("tenant-policy", checkInput),
    agent.WithOutputGuardrail("claims", checkFinalAnswer),
    agent.WithBeforeTool(approveSideEffects),
)
```

Input guardrails run once before transcript mutation or model I/O. Output
guardrails validate every assistant answer candidate (a response with no tool
calls) before it is committed; queued steering or follow-up can extend the run
after an answer passes. This covers the eventual final answer without trying
to predict concurrent queue changes. In a stream, deltas are provisional and
may already be visible; use a buffered presentation layer when output must not
be shown before validation. Tool gates remain the correct place for
side-effect approval.

## Durable approvals

A gate returns `agent.Pause` to stop with `agent.StopPaused`. Resolve any
approved or rejected subset by call ID:

```go
err := sess.ResolveToolCalls(agent.ToolResolution{
    ToolCallID: pending.ID,
    Content:    agent.TextResult("approved and completed"),
})
```

Omitted calls remain pending through session JSON and harness storage. A new
run is rejected until all calls have results. The callback-based
`ResolvePending` remains useful when one policy resolves the complete batch.
Applications should persist policy decisions separately when they need an
audit record; `harness.Session.AppendCustom` stores data that must not enter
model context.

## Harness streaming and cancellation

`Harness.PromptStream` and `PromptMessagesStream` persist the same turn save
points as blocking prompts. Breaking iteration cancels the runtime and restores
the harness to idle. Caller context cancellation is the normal ownership path;
`Harness.Cancel` lets a remote UI or supervisor stop the active prompt from
another goroutine.

The harness session is durable state, not the context window. `Session.Context`
derives the model-visible history; automatic compaction and branch summaries
can change that view without discarding the append-only record. Use
`AppendCustom` for progress/checkpoint artifacts that must survive a process
restart but should not consume context.

## Composition patterns

**Routing.** Classify in application code, then select an `Agent` or model.
`WithPrepareTurn` can swap a configured model between turns. Credential lookup
belongs to the provider/model layer, not the loop.

**Parallelization.** Mark only concurrency-safe tools with `agent.Parallel`.
For independent agent requests, run separate sessions in goroutines and join
them in the caller. An `Agent` is immutable and concurrent-safe; a `Session`
allows only one active run.

**Manager and workers.** Wrap specialists with `agent.AsTool` when the manager
should retain conversation ownership and synthesize the answer. Each tool
invocation has an isolated child session. A paused child becomes an explicit
tool error because the simple tool contract cannot durably expose child
approval state.

**Evaluator-optimizer.** Keep the success criterion and iteration budget in a
caller loop. Feed evaluator feedback through a new message or
`Session.FollowUp`; bound the loop with application limits plus
`WithMaxTurns`, `WithMaxTokens`, or `WithStopWhen`.

**Handoffs.** When a specialist should own the conversation, the application
must choose the destination session, filter or summarize transferred history,
and transfer policy explicitly. pips intentionally has no first-class handoff
until those ownership rules are known; `AsTool` is manager-owned delegation,
not a handoff.
