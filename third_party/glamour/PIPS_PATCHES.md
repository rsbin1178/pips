# In-tree Glamour fork

This directory is `charm.land/glamour/v2` at the commit `go.mod` previously
pinned through `github.com/rsbin1178/glamour/v2`
(`v2.0.2-0.20261006162314-63d07721665c`), carried in tree so a local patch is
reviewable, buildable offline and independent of the global module cache.

`go.mod` replaces the module with this directory:

```
replace charm.land/glamour/v2 => ./third_party/glamour
```

## Patch: linear fenced/indented code accumulation

`ansi/elements.go` built a fenced or indented code block with

```go
s := ""
for ... { s += string(line.Value(source)) }
```

which re-copies the whole body on every line. For a 128 KiB block that is
~1.9 GB of cumulative allocation per render and ~350 ms of CPU on an M5. The
patch accumulates through `strings.Builder`, which appends in place.

Rendered output is unchanged: `Builder.String()` returns the same bytes the
concatenation produced. `TestLiveFenceOutputIsStable` (TUI) pins output
fingerprints across sizes, fence shapes and colour modes.

Measured on the TUI row path (`BenchmarkLiveTailRender`, serial, medians of 5):

| block | before | after |
| --- | ---: | ---: |
| 8 KiB open fence | 17.2 ms / 23.3 MB | 16.9 ms / 16.5 MB |
| 32 KiB open fence | 71.4 ms / 168.5 MB | 65.9 ms / 65.4 MB |
| 128 KiB open fence | 349.9 ms / 1901.7 MB | 262.0 ms / 260.9 MB |

## Updating

Re-copy the upstream module over this directory, re-apply the two `strings.Builder`
edits in `ansi/elements.go`, and re-run the TUI Markdown tests.
