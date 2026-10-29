# Coding Standards

How code in this repository is written, reviewed, and landed. This file is the Standards axis a
review reads against: it holds the rules a reviewer can cite, not the architecture they explain.
Deep design contracts live under [`docs/`](docs/) — [`docs/coding-cli.md`](docs/coding-cli.md),
[`docs/agents.md`](docs/agents.md), [`docs/ai/providers.md`](docs/ai/providers.md) — and the domain
vocabulary lives in [`GLOSSARY.md`](GLOSSARY.md).

Anything the toolchain already enforces (formatting, imports, most vet checks) is not repeated here.
The gate is [`Makefile`](Makefile): `make all` runs fmt, vet, lint, test, and build.

## 1. Language and layout

- Go 1.27.x; the module is `github.com/rsbin1178/pips`. Toolchain bumps are their own change with the
  vulnerability reason recorded in the commit.
- Top-level packages are real libraries and stay importable: `ai` (provider protocol and adapters),
  `agent` (runtime, `harness`, `continuation`, `goal`, `loop`, `team`, `catalog`), `workflow` (DAG
  engine). The terminal application lives in `internal/coding`; `cmd/pips` is only a `main`.
- Dependencies point inward: `ai` must not import `agent` or `internal/`; `internal/` may import the
  public libraries. `make deps-check` enforces the direction.
- A package name carries its domain, so exported names stay short inside it: `ai.Request`, not
  `ai.AIRequest`; `harness.Session`, not `harness.HarnessSession`.

## 2. Public API naming

The project test: read `package.Symbol` at a call site, without its declaration, and decide whether the
name alone identifies the object or the action. Add a role qualifier only when the package holds another
plausible meaning for the same word.

- `New` names the package's primary product (`agent.New`, `openai.New`). When a package exports several
  first-class constructible targets, name the target: `goal.NewController`, `goal.NewModelEvaluator`.
- `Option` belongs to a package's primary constructor family only. With more than one family, name the
  configured target (`loop.ControllerOption`, `loop.ModelPlannerOption`) and let the option functions
  name the setting, not the constructor scope (`WithMaxTokens`, not `WithModelMaxTokens`).
- Exported enum values carry their family prefix (`agent.QueueDrainAll`, `agent.ToolDecisionPause`).
- Preparation helpers return a named setup (`goal.Prepare` → `goal.Setup`) rather than claiming to start
  an execution.
- Preserve JSON tags when renaming a persisted field unless a protocol migration is part of the change.
- During v0 a reviewed source-breaking rename may replace an unreleased name directly. Do not add
  aliases just to avoid updating internal callers.
- Rename exported symbols with `gopls rename -w`, then update every active reference in code and docs;
  `go doc <package>` is the read-back check.

## 3. Errors

- Errors are values, wrapped with `%w`, and inspected with `errors.Is`/`errors.As`. Never match on error
  strings.
- Provider adapters translate vendor bodies into one structured `ai.Error` that wraps a class sentinel
  (`ErrAuth`, `ErrRateLimited`, `ErrOverloaded`, `ErrInvalidRequest`, `ErrUnsupported`) and keeps the raw
  body. Callers branch on the class, not on the provider.
- A tool failure is a result, not a run error: it becomes `ai.ToolResultPart{IsError: true}` and the run
  continues. Run errors are model-call failures and context cancellation only.
- Do not set `http.Client.Timeout`: it aborts long streams mid-body. Bound requests with `context` and
  the transport's header/dial timeouts.
- Do not retry a stream after its first event has been delivered.
- Control sentinels such as `agent.ErrTerminate` are not failures: match them with `errors.Is` and never
  log them as errors.
- Every error crossing a package boundary says where it came from: `fmt.Errorf("openai: chat
  completions: %w", err)`.

## 4. Concurrency and lifetime

- `context.Context` is the first parameter of anything that can block, and it flows from the caller.
  No package starts a goroutine without an owner that can stop it.
- A channel has one owner; the sender closes. Workers return errors through an `errgroup`, not by
  writing them to a shared field.
- Long-lived state is guarded by a mutex or owned by one goroutine, never both. The TUI's event loop
  owns its model state; anything crossing into it goes through a message.

## 5. Tests

- Poll for asynchronous results with `require.Eventually` / `assert.Eventually`: a wait of at least
  `30*time.Second` and a tick of at least `10*time.Millisecond`. `Eventually` returns as soon as the
  condition holds, so the long wait only bounds how long a genuinely unsatisfied condition takes to
  fail. Never assert an asynchronous result with a fixed `time.Sleep`.
- When the component under test advances on a signal, prepare both sides of the state before raising it,
  so no wakeup can land mid-setup.
- A test is valid only if a production defect can make it fail. Judge that from the production path and
  the observable contract, not from fixture names or test size.
- Delete or rewrite a test that checks test-authored data against itself, verifies a helper or the
  standard library without asserting a project contract, or repeats a stronger test without a distinct
  input, error path, state transition, or layer boundary.
- Test external behaviour at a seam. Assert at the public interface, not through unexported state.
- `t.Parallel()` unless the test measures the whole process (`testing.AllocsPerRun`) or drives a
  process-wide resource.
- The fast loop is a package and a `-run` pattern; the full suite runs once before the PR.

## 6. Commits, branches, and delivery

- Conventional Commits: `type(scope): summary`, where `type` is one of `feat`, `fix`, `perf`, `refactor`,
  `test`, `docs`, `build`, `chore`, `ci`. The body explains what a reader can now do, or no longer has to
  do; it never narrates the session.
- `main` is linear: changes land by squash merge, so the pull request title becomes the commit subject
  and must read like one (`type(scope): summary`).
- One pull request per coherent batch. Small conventional commits stay on the branch; a pull request per
  commit only multiplies CI cost.
- Branch from `main` as `type/short-slug` (`fix/workspace-relative-tool-paths`, `perf/tui-streaming-cpu`).
- Before merging, lint the batch incrementally against its base:
  `golangci-lint run --new-from-rev=origin/main ./...` (use the real base for a stacked branch).
- Rebase onto `main` after an unrelated fix lands, with `--force-with-lease`, never a bare force.
- When a check fails for a reason that is not the branch's, reproduce it on the pristine base and put the
  command and output in the pull request.
- Release notes are one file per tag, `docs/releases/<tag>.md`, with the structure recorded in
  [`docs/releases/TEMPLATE.md`](docs/releases/TEMPLATE.md). The release workflow reads the file and
  appends the compare link.

## 7. Provider compatibility

One judgement decides every "the user configured something this model may not accept" case: **if the
setting were dropped, could the agent still do the work?** If not, fail early with an actionable message.
If it could, forward it verbatim and let the provider decide. The rules below apply to every adapter,
vendor shim, and compat profile, and to any new vendor or protocol before it is added.

- **R1 — fail open on uncertainty.** Never delete, rewrite, or intercept a configured field because a
  provider "probably" ignores it. Unknown or unmapped models default to supported. A provider 400 that
  names the parameter is a better outcome than silently changing what the user asked for.
- **R2 — fail locally only with evidence.** Local failure needs both a documented or measured failure and
  a message that names the parameter and the way out. Today the only case is `tools = false`: a
  tool-driven coding agent cannot work without native tool calls.
- **R3 — contradictory configuration fails locally.** Two settings in the same layer that cannot both
  hold (`top_logprobs` without `logprobs`, `reasoning_mode = disabled` together with a budget, a
  Responses-only field on Chat) are a user error, not a capability gap. Keep the startup validation.
- **R4 — send the knob the family actually reads.** When a vendor's control is not OpenAI's
  `reasoning_effort`, translate to that family's field instead of dropping it, and record the mapping in
  a declared knob table (`compatibility.chat_reasoning`) rather than dispatching on model names in code.
- **R5 — never silent.** Anything not sent as configured stays visible: `pips config show` echoes the
  resolved settings, `pips doctor` reports declared-but-inactive items, and adapters fill
  `ai.Response.Warnings` (`unsupported` / `compatibility` / `deprecated`), including on the terminal
  event of a stream. A debug log is not visibility.
- **R6 — capability declarations choose a path, not a gate.** `providers.*.capabilities` selects which
  implementation runs (native JSON Schema versus schema-in-prompt, with identical validation and
  verdicts on both). It never rejects a path another reference implementation accepts.

`Omit` means exactly one thing: the field does not exist in that protocol. It never means "we are unsure",
and never means "the model's enum differs" — that is R4.

## 8. The gate before a pull request

```sh
make all          # fmt, vet, lint, test (race + coverage), build
make audit        # govulncheck
make mod-verify   # go.sum integrity
make deps-check   # import-direction and dependency policy
make p0-verify    # the reproducible P0 gate: tests, race subset, provider smoke, vet
```

The quality workflow runs the same checks plus the three-OS harness matrix, the sandbox matrix, and the
installer proof. Green locally is not green in CI until the matrix has run on the branch.
