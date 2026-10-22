# Dependency patches

## Glamour

`go.mod` replaces `charm.land/glamour/v2` with the public
[rsbin1178/glamour fork](https://github.com/rsbin1178/glamour), pinned to
`v2.0.2-0.20261022123127-1aa8efd0868c` on
[`pips/linear-code-block-accumulation`](https://github.com/rsbin1178/glamour/tree/pips/linear-code-block-accumulation).
The immutable commit is
[`1aa8efd0868ce1b77ae0a00afaaa16750b2eb1d1`](https://github.com/rsbin1178/glamour/commit/1aa8efd0868ce1b77ae0a00afaaa16750b2eb1d1).

This commit builds on pips' previous fork pin, `63d07721665c`, preserving its
existing table row-border support and dependency versions. It replaces repeated
string concatenation with `strings.Builder` when extracting fenced and indented
code. The bytes passed to highlighting remain unchanged, including empty blocks,
UTF-8, tabs, CRLF and nested blocks. The fork includes equivalence tests
and an extraction benchmark. No local dependency copy or module-cache patch is
required.

Upstream contribution:
[issue #648](https://github.com/charmbracelet/glamour/issues/648) and
[pull request #649](https://github.com/charmbracelet/glamour/pull/649). The PR
contains only the accumulation fix, its equivalence tests and benchmark; the
integration branch additionally retains pips' earlier fork changes.

### Updating

Keep the integration branch based on the previously pinned fork until all of
its patches are available upstream. Publish and test a fork commit before
changing the replacement to its immutable Go pseudo-version; commit the matching
`go.sum` checksums. Re-run the fork tests, pips TUI tests and CLI build, and run
`go mod verify`. Remove the replacement only after the selected upstream release
contains both the accumulation fix and the existing fork behaviour pips uses.

For local experiments, use a separate Go workspace or temporary replacement;
never commit a developer-specific path or modify the global module cache.
