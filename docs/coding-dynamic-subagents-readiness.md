# Dynamic Subagent Beta readiness

This document is the release and rollback contract for Dynamic custom Agents.
It complements the [Coding CLI contract](coding-cli.md#dynamic-custom-agents-alpha)
and does not change the profile or durable event schemas.

Status on 2026-08-09: the repository can be evaluated for a selected Beta
distribution. GA is not approved. A real Beta observation window, an
independent release review, and zero unresolved P0/P1 incidents are still
required.

## Ownership and release boundary

Dynamic Subagent is a Coding Agent capability. Profile discovery/compilation,
exact recursive targets, child Sessions and approvals, private integration
bindings, model drafts, recovery, and release policy live in `internal/coding`
and `internal/coding/subagent`. The generic `agent` packages continue to supply
only reusable Harness, Tool, Catalog, cancellation, and usage primitives.

The existing default-off boolean is the kill switch:

```toml
dynamic_subagents = false
```

`PIPS_DYNAMIC_SUBAGENTS` and `--dynamic-subagents` keep their existing
environment/flag precedence. A rollback must remove any higher-precedence true
override and verify the result with `pips config show`. Pips does not implement
a local percentage cohort system or a second release-stage setting.

## Beta telemetry

Telemetry is content-free, synchronous, and fail-isolated. Applications own
the OpenTelemetry SDK, exporters, batching, retention, dashboards, and alerts.
Pips does not create an export goroutine or queue.

| Instrument | Purpose | Bounded dimensions |
| --- | --- | --- |
| `pips.coding.subagent.admissions` counter | Shared tree budget decisions before child creation | `coding.subagent.admission.outcome=accepted|rejected`, `coding.subagent.admission.reason=busy|capacity|spawn_limit|closed|invalid`, `coding.subagent.delivery=foreground|background`, `coding.subagent.depth=0..3` |
| `pips.coding.subagents` counter | Durable child lifecycle edges | role, state, delivery |
| `pips.coding.subagent.duration` histogram | Terminal child duration | role, state, delivery |
| `pips.coding.approvals` and `pips.coding.questions` counters | Input-control lifecycle | existing fixed state/tool dimensions |
| `pips.coding.integration.diagnostics` counter | Disabled integrations/observers and fixed diagnostic codes | component and code |

Admission is immediate success or error; there is no Subagent queue and
therefore no queue-depth metric. Approval/question events currently provide
counts but no safe wait-duration correlation contract. Generation/private
lease correctness is covered by lifecycle/diagnostic signals and race tests;
adding a new resource-lifecycle metric requires separate evidence and review.

The admission signal is not a durable `pips.coding.event/v1alpha1` event. A
capacity rejection happens before a child Session exists, so no placeholder
Session or journal entry is created solely for metrics.

The following values are forbidden as metric attributes or aggregate telemetry
fields: Agent/profile ID, definition source/digest, Session/interaction/run/Tool
call/root identity, prompt/task/result, path, command, URL, MCP/Hook ID or
arguments, headers, environment, and secrets.

## Beta SLIs and candidate SLOs

Compute these over the selected distribution and split foreground/background
and depth where relevant:

| SLI | Formula | Selected-Beta target |
| --- | --- | --- |
| Supported-load admission | accepted / all admission decisions, excluding an intentional saturation drill | >= 99%; investigate `capacity` and `spawn_limit` separately |
| Unexpected admission rejection | `closed` or `invalid` rejects outside a planned shutdown/test / all decisions | 0 |
| Child terminal success | completed / (completed + failed + interrupted); user cancellations are excluded | >= 99% after provider incidents are annotated |
| Crash interruption | interrupted terminals outside a planned crash-recovery drill | 0 |
| Telemetry health | `telemetry/observer_disabled` diagnostics | 0 |
| Cleanup/private binding health | terminal `binding_failed` and related fixed integration diagnostics | 0 unresolved events |
| Duration regression | terminal P95 against the same release cohort/model mix | no sustained regression above 20% from the Alpha baseline |

Candidate alerts:

- warn when admission rejection exceeds 5% for 5 minutes; page/revert when it
  exceeds 1% for 30 minutes under supported load;
- page on any sustained unexpected `closed`/`invalid`, interruption,
  observer-disabled, or private-binding/cleanup failure;
- warn when terminal failure exceeds 2% for 15 minutes after known provider
  incidents are separated;
- warn on a sustained P95 duration regression above 20%.

These are initial Beta thresholds, not historical claims. Record the exporter
query, time range, distribution version, model mix, and known incident
exclusions with every release decision.

## Distribution stages

| Stage | Entry requirement | Exit requirement |
| --- | --- | --- |
| Off | default build/config | explicit owner opts in |
| Opt-in Alpha | feature flag, local validation, bounded defaults | repository security/race/compatibility evidence |
| Selected Beta | telemetry exporter and rollback owner configured; Beta checklist passes | real observation window meets every SLO; no unresolved P0/P1 |
| Default-on candidate | independent security/code review and successful Beta window | rollback drill on the candidate artifact and release approval |
| GA | every GA item below has authoritative evidence | normal release governance |

The distribution channel selects participants. The local CLI never hashes
users, invents cohorts, or silently changes the feature flag.

## Rollback drill

Perform this on the exact candidate artifact before default-on or GA:

1. Capture `pips config show`, `pips agents list`, one builtin run, one custom
   run, and the resulting child IDs. Keep the Agent definition file in place.
2. Set `dynamic_subagents = false`, remove any higher-precedence true
   environment/flag override, and restart or reopen the Runtime.
3. Verify `pips config show` resolves the gate to false and identifies the
   intended source.
4. Verify `pips agents list` still reports the definition as unavailable,
   without instruction-body disclosure.
5. Verify new custom direct/model dispatch and Agent draft generation/promotion
   are rejected before model execution or file writes.
6. Verify builtin `explore`, `plan`, and `review` still execute, and prior child
   Sessions remain listable/inspectable.
7. Verify the definition bytes are unchanged. Rollback never deletes profiles,
   historical Sessions, or audit records.
8. Verify planned Runtime shutdown cancels/waits for in-flight children through
   the ordinary terminal cleanup contract; it does not mutate their frozen
   plan or generation in place.

Record the commands, artifact version, timestamps, child terminal states, and
operator. A failed drill blocks promotion to the next stage.

## GA checklist

Repository-controlled evidence:

- [x] bounded production execution/admission defaults and shared recursive
  budget;
- [x] authority-subset property tests, malicious definition fuzzing, and
  approval/recovery/private-integration red-team coverage;
- [x] bounded exact recursion, Agent-private configured MCP/Hook references,
  and user-promoted non-executable model drafts;
- [x] default-off bool compatibility, `pips.agent/v1alpha1`, legacy builtin and
  old-plan read compatibility;
- [x] content-free admission/lifecycle telemetry and automated rollback
  regression;
- [x] final repository gate outputs recorded in the Trellis readiness task.

Operational evidence required before GA:

- [ ] selected Beta observation window meets every SLO with saved queries;
- [ ] candidate-artifact rollback drill passes in a release environment;
- [ ] independent security and code review is complete;
- [ ] no unresolved P0/P1 incidents or unexplained interruption/cleanup events;
- [ ] competitor comparison is refreshed from official sources at GA time;
- [ ] default-on and GA are explicitly approved by the release owner.

Local tests can complete the first group. They cannot satisfy the second group
or justify a GA claim.
