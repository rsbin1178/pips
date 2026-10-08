# Release notes

One file per release, named after the tag: `docs/releases/v0.1.7.md` is the body of the GitHub release
`v0.1.7`. `.github/workflows/release.yml` reads it, appends the compare link, and publishes it; if the
file is missing the workflow warns and falls back to GitHub's generated notes.

## Convention

- English first, then a `---` rule and a `## 简体中文` section. English is what a reader sees by
  default, and the English section opens with a link to the Chinese one.
- Write prose only. The workflow appends the `Full Changelog` compare link, so the file does not
  repeat it, and there is nothing to update if the previous tag is not what you expected.
- Write plainly. State what changed and what it costs. No "the largest change in the release", no
  adjectives selling the release, no rhetorical framing of a fix. If a sentence would look out of place
  in the repository's own docs, it does not belong here.
- Group by what a user gets, not by commit type. Say what changed, what it means for them, and how it
  was verified; leave internal refactors out unless they change behaviour.
- Be honest about limits: a fix that covers one platform, a bound that is a ceiling rather than a
  deadline, a check that was reasoned rather than run. A release note is not marketing copy.
- Verification belongs in the notes when it is what makes the release trustworthy: CI suites that ran,
  measurements that back a bound, and anything that could not be checked.

## Skeleton

```markdown
# pips vX.Y.Z

**English** · [简体中文](#简体中文)

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/rsbin1178/pips/main/install.sh | sh
```

Windows (PowerShell):

```powershell
irm https://raw.githubusercontent.com/rsbin1178/pips/main/install.ps1 | iex
```

## <area>

- <what changed, and what it means>

## Verification

- <what ran, and what it covered>

---

## 简体中文

**English** · [English](#pips-vxyz)

## 安装

```sh
curl -fsSL https://raw.githubusercontent.com/rsbin1178/pips/main/install.sh | sh
```

Windows（PowerShell）：

```powershell
irm https://raw.githubusercontent.com/rsbin1178/pips/main/install.ps1 | iex
```

## <领域>

- <改了什​​么，对使用者意味着什么>

## 验证

- <跑了什么，覆盖了什么>
```
