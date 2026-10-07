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

			for index := 0; b.Loop(); index++ {
				block := timelineBlock{
					kind: blockThinking,
					id:   draftThinkingID,
					body: fmt.Sprintf("%s%07d", base[:size-7], index),
				}
				if renderThinkingBlock(block, 100, themeDark, false) == "" {
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
