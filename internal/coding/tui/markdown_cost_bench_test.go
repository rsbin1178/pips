package tui

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// benchmarkOpenFenceBody builds a streamed fenced code block that has not been
// closed yet. It is the worst live shape: no boundary can be frozen, so every
// frame parses, highlights and wraps the whole body.
func benchmarkOpenFenceBody(bytes int) string {
	line := "value := compute(alpha, beta)\n"
	return "```go\n" + strings.Repeat(line, bytes/len(line)+1)[:bytes]
}

// benchmarkFencedCodeBody is the same block, closed, which is what a settled
// record renders.
func benchmarkFencedCodeBody(bytes int) string {
	return benchmarkOpenFenceBody(bytes) + "```\n"
}

// benchmarkLiveListBody is a long bullet list. A list may still be re-read as
// loose or continued while it grows, so its rows cannot be frozen either.
func benchmarkLiveListBody(bytes int) string {
	line := "- a list item with ordinary words in it\n"
	return strings.Repeat(line, bytes/len(line)+1)[:bytes]
}

// benchmarkLiveTableBody is a long table, whose column widths depend on every
// row.
func benchmarkLiveTableBody(bytes int) string {
	var body strings.Builder
	body.WriteString("| column one | column two |\n| --- | --- |\n")
	row := "| a value | another value |\n"
	body.WriteString(strings.Repeat(row, (bytes-body.Len())/len(row)+1))

	return body.String()[:bytes]
}

// liveFrameShapes names the live bodies the frame cost is measured on. Each one
// can only be rendered whole while it streams.
func liveFrameShapes(bytes int) map[string]string {
	return map[string]string{
		"open_fence": benchmarkOpenFenceBody(bytes),
		"list":       benchmarkLiveListBody(bytes),
		"table":      benchmarkLiveTableBody(bytes),
		"fence":      benchmarkFencedCodeBody(bytes),
	}
}

// shapes in a fixed order so two runs compare the same work.
var liveFrameShapeOrder = []string{"open_fence", "list", "table", "fence"}

// grownLiveShape extends one shape's body by a single streamed line.
func grownLiveShape(shape, body string, index int) string {
	switch shape {
	case "list":
		return body + fmt.Sprintf("- frame %07d\n", index)
	case "table":
		return body + fmt.Sprintf("| frame %07d | x |\n", index)
	default:
		return body + fmt.Sprintf("// frame %07d\n", index)
	}
}

// BenchmarkLiveTailRender measures one live render of each expensive shape
// through the managed store's row path. The frame reuse is dropped first, so
// every iteration pays the whole-document render — the cost the event loop used
// to run synchronously and the cost the background renderer pays instead.
func BenchmarkLiveTailRender(b *testing.B) {
	for _, size := range []int{8 << 10, 32 << 10, 128 << 10} {
		for _, shape := range liveFrameShapeOrder {
			body := liveFrameShapes(size)[shape]

			b.Run(fmt.Sprintf("%s/bytes=%d", shape, size), func(b *testing.B) {
				renderer := newMarkdownRenderer(128)

				b.ReportAllocs()

				for index := 0; b.Loop(); index++ {
					delete(renderer.liveFrames, "draft")

					rows, err := renderer.renderLiveRows(
						"draft", grownLiveShape(shape, body, index), 100, 2, themeDark, false, true,
					)
					if err != nil {
						b.Fatal(err)
					}

					if rows.count() == 0 {
						b.Fatal("no rows")
					}
				}
			})
		}
	}
}

// BenchmarkLiveTailFrameReused measures the event-loop share of the same frames
// when the cooldown holds: the frame is served from the previous render and no
// content is parsed.
func BenchmarkLiveTailFrameReused(b *testing.B) {
	for _, size := range []int{32 << 10, 128 << 10} {
		for _, shape := range liveFrameShapeOrder {
			body := liveFrameShapes(size)[shape]

			b.Run(fmt.Sprintf("%s/bytes=%d", shape, size), func(b *testing.B) {
				renderer := newMarkdownRenderer(128)
				// The warm-up render is what the loop's own first iteration would
				// otherwise pay for; b.Loop resets the timer on its first iteration.
				if _, err := renderer.renderLiveRows("draft", body, 100, 2, themeDark, false, true); err != nil {
					b.Fatal(err)
				}

				for index := 0; b.Loop(); index++ {
					rows, err := renderer.renderLiveRows(
						"draft", grownLiveShape(shape, body, index), 100, 2, themeDark, false, true,
					)
					if err != nil {
						b.Fatal(err)
					}

					if rows.count() == 0 {
						b.Fatal("no rows")
					}
				}
			})
		}
	}
}

// BenchmarkLiveMutableSeam measures the event-loop cost of a growing expensive
// block after a freezable paragraph. The warm first render is outside the timer;
// the worker's separate whole-document cost belongs to BenchmarkLiveTailRender.
func BenchmarkLiveMutableSeam(b *testing.B) {
	for _, size := range []int{32 << 10, 128 << 10} {
		for _, shape := range []string{"open_fence", "list", "table"} {
			body := "Here is the implementation.\n\n" + liveFrameShapes(size)[shape]
			b.Run(fmt.Sprintf("%s/bytes=%d", shape, size), func(b *testing.B) {
				renderer := newMarkdownRenderer(4)
				if _, err := renderer.renderLiveRows("draft", body, 100, 2, themeDark, false, true); err != nil {
					b.Fatal(err)
				}

				b.ReportAllocs()
				for index := 0; b.Loop(); index++ {
					rows, err := renderer.renderLiveRows(
						"draft", grownLiveShape(shape, body, index), 100, 2, themeDark, false, true,
					)
					if err != nil {
						b.Fatal(err)
					}
					if rows.count() == 0 {
						b.Fatal("no rows")
					}
				}
			})
		}
	}
}

// BenchmarkSettledCodeBlock measures one render of a settled fenced code block,
// the shape a completed tool result or a finished answer has.
func BenchmarkSettledCodeBlock(b *testing.B) {
	for _, size := range []int{8 << 10, 32 << 10, 128 << 10} {
		b.Run(fmt.Sprintf("bytes=%d", size), func(b *testing.B) {
			body := benchmarkFencedCodeBody(size)

			b.ReportAllocs()

			for b.Loop() {
				renderer := newMarkdownRenderer(4)
				if _, err := renderer.render(body, 100, themeDark, false); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// windowRetainedHeap measures what a windowed record keeps alive after its
// source render is out of scope. The rows of one rendered document are substrings
// of it, so a 100-row window of a 10 MB render retains the whole render unless the
// window detaches its rows.
//
// alreadyDetached skips the copy, which is what the renderer does for a window
// whose rows it produced individually. Passing false exercises the window the
// transcript store takes of one rendered document.
func windowRetainedHeap(alreadyDetached bool) int64 {
	source := strings.Repeat(strings.Repeat("x", 100)+"\n", 100000)
	split := strings.Split(source, "\n")
	rows := rowSegments{frozen: split, detached: alreadyDetached}.window(99999, 100000)
	runtime.KeepAlive(source)
	source = ""
	split = nil

	runtime.GC()

	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	runtime.KeepAlive(rows)

	return int64(stats.HeapAlloc)
}

// TestRowWindowRetainedHeap pins the residency fix with a measurement rather than
// a tight number: the window that keeps one row of a 10 MB render must not retain
// that render.
func TestRowWindowRetainedHeap(t *testing.T) {
	if testing.Short() {
		t.Skip("heap measurement")
	}

	// Warm both shapes first so the comparison is between steady states.
	windowRetainedHeap(false)
	windowRetainedHeap(true)

	// A window that stands on the render's own rows: no copy is made, so the whole
	// 10 MB document stays alive behind the 100 retained bytes.
	aliasing := windowRetainedHeap(true)
	detached := windowRetainedHeap(false)
	t.Logf("window retained heap: aliasing=%d B detached=%d B", aliasing, detached)

	assert.Less(t, detached+1<<20, aliasing,
		"a detached window does not retain the render it was cut out of")
}
