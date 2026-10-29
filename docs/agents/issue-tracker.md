# Issue tracker: local markdown

Specs and tickets for this repository live as markdown files under `.scratch/`, which is git-ignored:
the workflow is local state, the code and `docs/` are the published record.

## Conventions

- One feature per directory: `.scratch/<feature-slug>/`
- The spec is `.scratch/<feature-slug>/spec.md`
- One file per ticket at `.scratch/<feature-slug>/issues/<NN>-<slug>.md`, numbered from `01`, never a
  single combined tickets file
- A `Status:` line near the top of each ticket records where it stands: `ready`, `blocked`,
  `in-progress`, or `done`. A `Blocked by:` line lists the tickets that gate it, or says
  `None (can start immediately)`
- Comments and conversation history append to the bottom of the file under a `## Comments` heading
- Finished features are moved to `.scratch/archive/<date>-<feature-slug>/` rather than deleted, so a
  later session can still read why a decision was made

## When a skill says "publish to the issue tracker"

Create a new file under `.scratch/<feature-slug>/` (creating the directory if needed). A spec becomes
`spec.md`; a ticket becomes `issues/<NN>-<slug>.md`.

## When a skill says "fetch the relevant ticket"

Read the file at the referenced path. The user normally passes the path or the ticket number directly.

## Wayfinding operations

Used by `/wayfinder`. The **map** is a file with one **child** file per ticket.

- **Map**: `.scratch/<effort>/map.md` (the Notes / Decisions-so-far / Fog body).
- **Child ticket**: `.scratch/<effort>/issues/NN-<slug>.md`, numbered from `01`, with the question in the
  body. A `Type:` line records the ticket type (`research` / `prototype` / `grilling` / `task`); a
  `Status:` line records `claimed` / `resolved`.
- **Blocking**: a `Blocked by: NN, NN` line near the top. A ticket is unblocked when every file it lists
  is `resolved`.
- **Frontier**: scan `.scratch/<effort>/issues/` for files that are open, unblocked, and unclaimed; first
  by number wins.
- **Claim**: set `Status: claimed` and save before any work.
- **Resolve**: append the answer under an `## Answer` heading, set `Status: resolved`, then append a
  context pointer (gist + link) to the map's Decisions-so-far in `map.md`.
