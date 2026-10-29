# Domain docs

How the engineering skills consume this repository's domain documentation when exploring the codebase.

## Before exploring, read these

- **`GLOSSARY.md`** at the repo root: the project's terms, one line each. Use them in code, tests, commit
  messages, and specs; they are what makes a sentence short here.
- **`docs/adr/`**: read the decisions that touch the area you are about to work in. An ADR records a
  choice that is expensive to reverse; when a change contradicts one, say so before writing code.

If any of these files is missing, **proceed silently**. Don't flag the absence and don't propose creating
them upfront. `/domain-modeling` (reached through `/grill-with-docs`) creates them lazily, when a term or
a decision is actually resolved.

## File structure

Single-context repo — this one:

```
/
├── GLOSSARY.md          ← the vocabulary, one line per term
├── docs/
│   ├── adr/             ← system-wide decisions, one file per decision
│   ├── agents/          ← how work is tracked, and how to read this documentation
│   ├── ai/              ← provider and model reference
│   ├── coding-*.md      ← the terminal agent's own reference docs
│   └── manual-acceptance/ ← per-feature acceptance cases with source anchors
├── ai/  agent/  workflow/  internal/coding/  cmd/pips/
└── CODING_STANDARDS.md  ← the rules a review cites
```

## How deep docs relate to the glossary

`GLOSSARY.md` is the short form: a term and the one sentence that makes it unambiguous. `docs/` is the
long form: the behaviour, the wire format, the acceptance cases. When a long doc and the glossary
disagree, the glossary is the word and the long doc is the behaviour — fix the one that is wrong, in the
same change.

## ADR format

`docs/adr/NNNN-<slug>.md`, numbered from `0001` in order of the decision (not the date). Each one holds:
the status, the context that forced a choice, the decision, and the consequences a reader inherits —
including the ones that are unpleasant. A superseded ADR stays in place and points at its successor.
