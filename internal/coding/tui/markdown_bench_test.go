package tui

import (
	"fmt"
	"strings"
	"testing"
)

func benchmarkThinkingText(bytes int) string {
	line := "正在分析状态更新与界面渲染。**Important**: preserve completed results and avoid repeated work.\n\n"
	return strings.Repeat(line, bytes/len(line)+1)[:bytes]
}

// BenchmarkThinkingStreamRender measures one streamed delta of a live Thinking
// block: the body keeps its size and its tail changes, so every frame re-wraps
// the whole thought. This is the per-delta cost that used to spike the CPU.
func BenchmarkThinkingStreamRender(b *testing.B) {
	for _, size := range []int{1024, 8 << 10, 32 << 10} {
		b.Run(fmt.Sprintf("bytes=%d", size), func(b *testing.B) {
			base := benchmarkThinkingText(size)

			b.ReportAllocs()

			renderer := newMarkdownRenderer(128)

			for index := 0; b.Loop(); index++ {
				block := timelineBlock{
					kind: blockThinking,
					id:   draftThinkingID,
					body: fmt.Sprintf("%s%07d", base[:size-7], index),
				}
				if renderThinkingBlock(block, renderer, 100, themeDark, false) == "" {
					b.Fatal("empty rendering")
				}
			}
		})
	}
}

// benchmarkLiveMarkdownBody builds a streaming answer of the given size as
// ordinary paragraphs, the shape a real assistant message has.
func benchmarkLiveMarkdownBody(bytes int) string {
	paragraph := "A streamed answer paragraph with ordinary words and a **bold** run.\n\n"
	return strings.Repeat(paragraph, bytes/len(paragraph)+1)[:bytes]
}

// BenchmarkLiveMarkdownFrame measures one frame's render of a growing live
// answer. The body is one paragraph longer each iteration, so a whole-document
// render would grow with it; the frozen prefix keeps the frame's cost tied to
// the tail.
func BenchmarkLiveMarkdownFrame(b *testing.B) {
	for _, size := range []int{4 << 10, 32 << 10, 128 << 10} {
		b.Run(fmt.Sprintf("bytes=%d", size), func(b *testing.B) {
			base := benchmarkLiveMarkdownBody(size)
			renderer := newMarkdownRenderer(128)

			b.ReportAllocs()

			for index := 0; b.Loop(); index++ {
				body := base + fmt.Sprintf("frame %07d keeps streaming.\n\n", index)
				if _, err := renderer.renderLive("draft", body, 100, themeDark, false); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// benchmarkThinkingBody builds a reasoning body of at least the given size as
// complete paragraphs, the shape a streamed thought has.
func benchmarkThinkingBody(bytes int) string {
	line := "正在分析状态更新与界面渲染。Weighing the next step and its constraints.\n\n"
	return strings.Repeat(line, bytes/len(line)+1)
}

// BenchmarkLiveThinkingFrame measures one frame's render of a growing Thinking
// section. The body gains a paragraph each iteration and ends mid-line, the shape
// a streamed thought has, so rendering the whole thought per frame would grow
// with it; the frozen rows keep the frame's cost tied to the tail.
func BenchmarkLiveThinkingFrame(b *testing.B) {
	for _, size := range []int{4 << 10, 32 << 10, 128 << 10} {
		b.Run(fmt.Sprintf("bytes=%d", size), func(b *testing.B) {
			base := benchmarkThinkingBody(size)
			renderer := newMarkdownRenderer(128)

			b.ReportAllocs()

			for index := 0; b.Loop(); index++ {
				body := base + fmt.Sprintf("frame %07d keeps reasoning onward", index)
				if renderThinkingBlock(liveThinkingBlock(body), renderer, 100, themeDark, false) == "" {
					b.Fatal("empty rendering")
				}
			}
		})
	}
}

// BenchmarkLiveMarkdownRowsFrame measures one frame of the managed store's row
// path for a growing live answer. Unlike BenchmarkLiveMarkdownFrame it does not
// join the frozen prefix and the tail into one string, so the frame's allocation
// is the row headers it hands the store.
func BenchmarkLiveMarkdownRowsFrame(b *testing.B) {
	for _, size := range []int{4 << 10, 32 << 10, 128 << 10} {
		b.Run(fmt.Sprintf("bytes=%d", size), func(b *testing.B) {
			base := benchmarkLiveMarkdownBody(size)
			renderer := newMarkdownRenderer(128)

			b.ReportAllocs()

			for index := 0; b.Loop(); index++ {
				body := base + fmt.Sprintf("frame %07d keeps streaming.\n\n", index)
				rows, err := renderer.renderLiveRows("draft", body, 100, themeDark, false)
				if err != nil {
					b.Fatal(err)
				}
				if len(rows) == 0 {
					b.Fatal("no rows")
				}
			}
		})
	}
}
