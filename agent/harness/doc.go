// Package harness provides the stateful orchestration layer over the agent
// runtime: persistent session trees with branching, automatic context
// compaction, branch summaries, and skill/template resources — the pieces an
// agent application needs beyond a single run.
//
// A [Session] is an append-only entry tree over a [Store] ([MemoryStore] for
// ephemeral use, [JSONLStore]/[Repo] for durable files). Appending advances
// the active leaf; [Session.MoveTo] branches from any earlier entry, and
// [Session.Context] reconstructs the model-visible conversation, applying
// the latest compaction and any branch summaries.
//
// A [Harness] drives an [agent.Agent] over that tree:
//
//	store, _ := harness.Repo{Dir: "sessions"}.Create("", nil)
//	sess, _ := harness.NewSession(store)
//	h, _ := harness.New(model, sess,
//	    harness.WithTools(myTools...),
//	    harness.WithCompaction(harness.Settings{ContextTokens: 200_000}),
//	)
//	result, err := h.Prompt(ctx, "Let's get to work.")
//
// Each prompt reconstructs context from the tree, runs the loop, and persists
// every accepted message at turn boundaries (assistant entries carry their
// turn's token usage), so a process can stop and resume mid-project.
// [Harness.PromptStream] exposes the same lifecycle incrementally;
// [Harness.Cancel] can stop the active prompt from another goroutine. When
// automatic compaction is enabled, oversized context is summarized — with
// pi-style cut points that never separate a tool result from its call — before
// the prompt runs.
//
// The package has no third-party runtime dependencies.
package harness
