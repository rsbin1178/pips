# Session storage

pips keeps one session in one directory, under a workspace group, with each kind
of durable state in its own layer. Listing reads a small index file and never
opens a transcript.

The design keeps a workspace group holding one directory per session, a small
metadata index, an authoritative protocol stream, model-input snapshots, and
rebuildable derived state. The layer table below is the contract; the research
notes that led to it are not part of the repository.

## Layout

```
<PIPS_HOME>/sessions/
  <workspace-key>/
    workspace.json                # {schema, key, path, created_at}
    <session-id>/
      session.json                # index record: identity, timestamps, title, counts
      transcript.jsonl            # authoritative conversation tree (harness entries)
      events.jsonl                # authoritative coding event journal
      model/
        system_prompt.txt         # exactly what the model was sent
        prompt_context.json       # the inputs that prompt was rendered from
        tool_definitions.json     # the tools offered on the latest call
      state/                      # derived, rebuildable
        plan.md
        plan-mode.json
        usage.json
        signals.json
        rewind_points.jsonl
        goal/                     # Goal continuation ledger
      archives/<sha256>.archive   # content-addressed compaction archives
      compaction/<checkpoint>.json
      subagents/
        index.json                # completeness marker for the parent-side child index
        <child-id>/entry.json     # parent-side child record (the child session is a sibling)
      terminal/<call-id>.log
      search.json                 # derived search document
      session.lock
```

`<workspace-key>` is the URL-escaped workspace path (`url.PathEscape`, so
`/Users/me/app` becomes `%2FUsers%2Fme%2Fapp`). When that name would exceed 255
bytes the store uses `ws-<sha256(path)[:16]>` and `workspace.json` carries the
original path.

## Layers

| Layer | Files | Authority |
| --- | --- | --- |
| Layout | — | `session.StoreLayout` is the only place a session path is built; `session.StoreRoot` is the only place the tree is read or created |
| Index | `session.json` | authoritative for listing; the writer keeps `counts` current |
| Conversation | `transcript.jsonl` | authoritative; a harness entry tree |
| Events | `events.jsonl` | authoritative for durable lifecycle facts; live projections stay out |
| Model input | `model/` | audit snapshot of what the model was sent |
| Derived state | `state/` | rebuildable; never authoritative for conversation content |
| Content | `archives/`, `compaction/` | content-addressed, digest-verified |
| Subagents | `subagents/` | the parent-side child index; one entry per child session, which stays a sibling in the normal tree |
| Diagnostics | `terminal/` | bounded tool output logs |
| Search | `search.json` | derived; rebuild with `pips session index rebuild` |

### Event journal policy

The journal records every committed coding event **except** `message.delta`,
`model.retry`, and `session.tree_changed`. The first two exist only for the live
TUI projection, and the message they build is already recorded by
`message.committed` and by the transcript. `session.tree_changed` carries a
full transcript snapshot; the transcript is already authoritative, so
persisting the snapshot would duplicate content and make one journal record grow
with the whole conversation. All three stay live-only.

Journal writes are best-effort: a real I/O failure is remembered and reported
when the session closes, so a slow or full disk never aborts a turn whose
transcript entry is already durable. Durable records are written at their
natural size; a large committed message is never silently dropped. The only
byte guard is a 256 MiB corruption bound on one unbroken line, far above normal
event payloads, and exceeding it means the file is damaged.

### A session with no conversation leaves nothing

A session is provisional until it has an entry, and a provisional session has no
directory, no writer lock, and no index record. `Create` resolves every path from
the layout — a pure value — and writes nothing; the group, the directory, the
lock, the index record, and the transcript are created together with the first
entry.

Starting the TUI and quitting without sending a prompt therefore leaves nothing
behind. Bootstrap artifacts do not count as content: the runtime writes its
journal, its model snapshot, and its state sidecars as soon as it starts, so a
session that still has no entries is discarded when it closes. A caller that needs
the session to exist before its first entry passes `RetainEmpty`, and a session
holding archive objects is never discarded because those objects are durable
content a checkpoint can reference.

The workspace group directory and its `workspace.json` remain once a workspace has
been used: a group is the store's organizational layer, one per workspace, so it
does not accumulate per launch.

### Parent-side child index

A parent session answers "which child sessions do I own" from its own directory:
`subagents/index.json` marks the index complete, and `subagents/<child-id>/entry.json`
is one minimal record per child. The child session itself stays a sibling of its
parent in the same workspace group, so this is a lookup layer, not a second home
for the child's metadata — title, counts, model, and identity still come from the
child's `session.json` and transcript prefix.

The entry records lineage and existence only (`child_session_id`,
`parent_session_id`, `parent_run_id`, `agent_id`, `agent_kind`, `created_at`), plus
a lifecycle `state`/`ended_at` cache. Only the lifecycle mirror writes a state, so
an entry published at creation or backfilled from a scan carries none rather than
guessing one.

The write order inside a spawn is child session directory, then the parent's entry,
then the child's own `session.json`: a later failure leaves a detectable entry
rather than a child nothing short of a full scan could see. Only a completed scan
writes `index.json`; a spawn never does, and removing any entry removes the marker.

`ListSubagents` reads the index when the marker is present and every entry is
readable; otherwise it scans the store and rebuilds the index. An entry whose child
directory is gone, or whose readable record contradicts it, is dropped and removed;
a child whose record is present but unreadable is excluded while its entry is left
alone. Both read paths project through the same code, so the indexed result equals
the scan. The index versions itself in its own files, so `session.json` and the
store version are unchanged and an older build simply ignores `subagents/`.

### Durability

Small JSON files are published with write-to-temp, `fsync`, `rename`, then a
directory `fsync`. The transcript is append-only. Deleting a session renames its
directory to a quarantine name, syncs the group, then removes the quarantine, so
a crash cannot leave a session half-deleted under its own name. Files are
owner-only (`0600`) inside owner-only directories (`0700`).

A journal whose last line has no terminating newline was interrupted mid-append.
Opening it truncates that partial line, so the next append cannot create a
corrupt interior line that a reader would have to reject.

### Naming

`StoreLayout` and `StoreRoot` split the two halves of "where things live":
`StoreLayout` is a pure path scheme (it never reads or creates anything), and
`StoreRoot` is the store on disk (it enumerates groups, creates a group, and
locates a session). Everything else is named after the layer it owns —
`metadata.go`, `journal.go`, `state.go`, `snapshot.go`, `search.go`,
`migrate.go` — with `fileio.go` holding the shared filesystem primitives.

### Resume and search

`session.json` carries the workspace identity and path, the session directory's
contents are self-describing, and a resume locates the session by id across
workspace groups. `search.json` holds a bounded extract of the title, summary,
and first user prompts so `pips session search` never opens a transcript.

## Commands

```
pips session list                    # sessions owned by the current workspace
pips session path <id>               # print the session directory
pips session show <id> [--json]      # index record + which layers exist
pips session search <query> [--json] # keyword search over the derived documents
pips session index rebuild           # rebuild every search.json
pips session delete <id> [--yes]     # remove the whole session directory
pips session migrate [--apply]       # convert a pre-v2 flat store
```

`pips session list` shows the sessions owned by the **current** workspace. Run it
from the project directory; `pips session search` spans every group.

### What decides a session's workspace

A session belongs to the workspace whose **canonical path** it was created in.
The recorded workspace identity — a hash of that path with the volume's device
number and the directory's inode — is provenance, not the key.

The distinction matters because a device number can change without anything the
user did (a remount, a volume renumbering after an OS update). If attribution
keyed on the identity, every session created before the change would vanish from
`list` and refuse to resume, even though nothing about the project changed.
Identity matching is still consulted, so a session stored before a path was
recorded resolves by identity.

### Recovering a workspace path

A pre-v2 header could record the workspace identity without the path. The
migration reads the pre-v2 workspace registry (`workspaces.json`, beside the
store root) to turn that identity into the path, and preserves the identity even
when the path cannot be recovered. `--workspaces PATH` selects a registry from
another location.

## Migrating a pre-v2 store

pips 2.0 moved sessions from flat files (`<root>/s-<hex>.jsonl` plus
`<id>.lock`, `<id>/`, `<id>.history/`, and Goals under `<root>/goals/<id>/`) to
the layout above. Historical sessions are not read by the new store, so:

```
pips session migrate --dry-run        # report only, writes nothing
pips session migrate --apply          # convert, leaving legacy files in place
pips session migrate --apply --delete-legacy
```

`pips update --migrate-sessions` runs the same migration as part of an upgrade.
The migration is idempotent: a session that already has `session.json` is
skipped. A store you do not care about can simply be deleted instead.
