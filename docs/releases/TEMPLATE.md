# Release notes

One file per release, named after the tag: `docs/releases/v0.1.7.md` is the body of the GitHub release
`v0.1.7`. `.github/workflows/release.yml` reads it, appends the compare link, and publishes it; if the
file is missing the workflow warns and falls back to GitHub's generated notes.

## Convention

- **Structure.** The version as the page heading, then the language switch, then a one-paragraph
  summary of the release. `## Install` with the one-liners. Then the categories in the order Added,
  Changed, Fixed, Performance, Verification, omitting any that would be empty. The Chinese section
  repeats the same structure after `---`.
- **Language order.** English first, then a `---` rule and `## 简体中文`. English is what a reader sees
  by default. The English section opens with a link to the Chinese one, and the Chinese section links
  back to the English heading's anchor.
- **Entries.** One or two lines each, in the past or present tense, describing observable behaviour.
  Name the flag, method, or command a reader would use. Screaming-case categories are the convention;
  do not invent new ones.
- **Leave out** internal churn (tests, lint cleanups, refactors, dependency chores) and design
  rationale. Those live in the commits and pull requests. A release note answers "what changed for me",
  not "why is it built this way".
- **Call out** anything a reader must act on: a behaviour change (a new default, a changed message), a
  removed flag, a platform limit. Put it under Changed and say what it means for them.
- **Verification.** Three or four terse lines: what ran, and any measurement that back the claims. This
  is where numbers belong. Say what was not verified rather than implying coverage.
- **No compare link in the file.** The workflow appends `Full Changelog`, so it cannot go stale.

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

## 修复

- <使用者原本看到的现象，以及现在的行为>

## 性能

- <变快的那部分>

## 验证

- <跑了什么>
- <界值或限制背后的测量>
````
