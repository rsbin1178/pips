# 0001 — Speak each provider's wire protocol directly

- **Status:** accepted
- **Date:** 2026-07-18
- **Supersedes:** —

## Context

`pips` talks to OpenAI, Anthropic, Gemini, and OpenAI-compatible gateways. The obvious shortcut is to
depend on each vendor's SDK, or on one library that wraps them all. Both were rejected on evidence from
the first weeks of the project:

- Vendor SDKs disagree about context handling, retries, and error shapes, so a caller ends up with three
  unrelated ways to ask "was this rate limited".
- The SDKs pull large dependency trees into a module whose public surface is libraries other people embed;
  every version conflict becomes the embedder's problem.
- The features that matter here — streaming tool-call deltas, reasoning signatures, prompt caching,
  structured output — are the newest and least-covered parts of the SDKs, and the versions that support
  them lag the wire.
- A gateway that imitates OpenAI is not OpenAI: it needs its own compatibility profile, which no SDK
  exposes.

## Decision

Each provider gets a hand-written adapter over `net/http` and SSE, in its own package (`ai/openai`,
`ai/anthropic`, `ai/gemini`), behind the provider-neutral protocol in `ai`. Requests and responses are
typed Go values defined in `ai`; the adapter's whole job is to translate between those and the vendor's
JSON.

Compatibility policy is explicit rather than inferred: an unknown vendor field is forwarded, a known
impossible one fails early with an actionable message, and anything not sent as configured shows up in
`ai.Response.Warnings` (see [`CODING_STANDARDS.md`](../CODING_STANDARDS.md) §7).

## Consequences

- The module graph stays small: no vendor SDK, no transitive surface to audit for a library consumer.
- Every wire detail is ours to test. Each adapter is exercised against a scripted HTTP server, and the
  response bytes are asserted, so a provider's change is a failing test rather than a runtime surprise.
- Streaming is ours to keep correct: bounded header waits, idle detection, and no replay after the first
  event. This is the part SDKs get wrong most often for long-running agents.
- We pay for it in maintenance: a new vendor field or endpoint family is code we write. The scripts and
  the policy above are what keep that cost linear instead of surprising.
- A vendor that changes its wire without a version bump breaks a test here, not a user's session in
  production.
