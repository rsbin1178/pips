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
