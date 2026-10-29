# Release notes

One file per release, named after the tag: `docs/releases/<tag>.md` is the body of the GitHub release
`<tag>`. `.github/workflows/release.yml` reads it, appends the compare link, and publishes it; if the
file is missing the workflow warns and falls back to GitHub's generated notes.

## Updating a published note

The workflow publishes; it cannot republish. `gh release create` fails once the release exists, so a
correction to an already-published body goes through `gh release edit <tag> --notes-file <file>`, whose
file is this one's content plus the `**Full Changelog**` line the workflow would have appended — the
command replaces the whole body. Change the file and the body together, or the repository and the release
page disagree. Editing a body touches no artifact: the archives and `SHA256SUMS` are published artifacts,
and the notes are not part of them.

## Source material

Start from what merged since the previous tag: `git log --oneline <previous-tag>..HEAD`, the pull requests
it names, and the spec and tickets the change was built from (`.scratch/<feature>/`). Settle three things for each change before writing
a line about it — what a reader can now do, or no longer has to do; which surface changed; and whether
they have to act. A change that answers none of the three is internal churn, and belongs in the commit
rather than here.

## Convention

- **Structure.** The version as the page heading, then the language switch, then a one-paragraph
  summary of the release. `## Install` with the one-liners. Then the categories in the order Added,
  Changed, Fixed, Performance, Verification, omitting any that would be empty. The Chinese section
  repeats the same structure after `---`.
- **Language order.** English first, then a `---` rule and `## 简体中文`. English is what a reader sees
  by default. The English section opens with a link to the Chinese one, and the Chinese section links
  back to the English heading's anchor.
- **Entries.** One or two lines each, in the past or present tense, describing observable behaviour, and
  leading with what the reader gets rather than the mechanism that produced it. Name the flag, method, or
  command a reader would use, and name the surface when the change is not the whole product: the TUI,
  `pips exec`, `pips config`, a provider adapter, an installer, or one platform. Screaming-case categories
  are the convention; do not invent new ones.
- **Plain language.** No ticket, pull-request or issue numbers, no internal codenames, no identifier a
  reader cannot type. The compare link and the pull requests already carry that history.
- **Leave out** internal churn (tests, lint cleanups, refactors, dependency chores) and design
  rationale. Those live in the commits and pull requests. A release note answers "what changed for me",
  not "why is it built this way".
- **Action required.** A reader who has to do something has to see it without reading the whole note.
  Keep the entry in its category and open it with `**Action required:**`, then say what to do and what
  happens if they do not. A behaviour change, a new default, a removed flag, a platform limit, a
  deprecation and a removal are all changed behaviour: they belong under Changed with that marker, not in
  a category of their own.
- **Screenshots.** The notes render on the release page, so a screenshot belongs where the change is
  visual — a new panel, a changed layout — and nowhere else. Reference a committed asset by its
  repository path (`docs/assets/<name>.png`); the tapes under `.vhs/` regenerate the ones they own.
- **Verification.** Three or four terse lines: what ran, and any measurement that back the claims. This
  is where numbers belong. Say what was not verified rather than implying coverage.
- **No compare link in the file.** The workflow appends `Full Changelog`, so it cannot go stale.

## Rewrites

The same change as the commit states it, and as a note states it. The left side of each arrow is the raw
material, not a draft:

- `perf(tui): bound the live-tail CPU on long reasoning streams` → A long reasoning stream no longer
  spends a core on its own draft and on the live tail.
- `fix: composer did not scroll a draft taller than its box` → A draft taller than the box now scrolls
  with the wheel, in both directions, instead of only downwards.
- `feat(config): per-model stream idle bound` → `stream_idle_timeout_seconds` bounds the silence between
  reads on a streaming response: a stream that stops delivering for that long is replayed instead of
  hanging.

## Tone

State facts. No framing ("the largest change in the release"), no adjectives selling the release, no
before-and-after narrative in an entry, no rhetorical closers. If a sentence would look out of place in
the repository's own docs, it does not belong here.

## Skeleton

````markdown
# vX.Y.Z

**English** · [简体中文](#简体中文)

<one paragraph: what this release changes for someone using it>

## Install

macOS and Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/rsbin1178/pips/main/install.sh | sh
```

Windows (PowerShell 5.1 or newer):

```powershell
irm https://raw.githubusercontent.com/rsbin1178/pips/main/install.ps1 | iex
```

<one sentence on what the installers verify, and the environment variables a reader may need>

## Added

- <flag, command, or method, and what it does>

## Changed

- <new default or changed behaviour, and what it means>
- **Action required:** <what a reader must do, and what happens if they do not>

## Fixed

- <the failure as the reader saw it, and what happens now>

## Performance

- <what got cheaper>

## Verification

- <what ran>
- <the measurement behind any bound or limit in this release>

---

## 简体中文

**简体中文** · [English](#vxyz)

<一段话：这次发布对使用者意味着什么>

## 安装

macOS 与 Linux：

```sh
curl -fsSL https://raw.githubusercontent.com/rsbin1178/pips/main/install.sh | sh
```

Windows（PowerShell 5.1 及以上）：

```powershell
irm https://raw.githubusercontent.com/rsbin1178/pips/main/install.ps1 | iex
```

## 新增

- <flag / 命令 / 方法，以及它的作用>

## 变更

- <新的默认行为或改动，以及对使用者的影响>
- **需要操作：** <使用者必须做什么，以及不做的后果>

## 修复

- <使用者原本看到的现象，以及现在的行为>

## 性能

- <变快的那部分>

## 验证

- <跑了什么>
- <界值或限制背后的测量>
````
