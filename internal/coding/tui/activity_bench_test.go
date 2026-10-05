package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
)

func benchmarkCompletedTools(count, bytes int) []coding.ToolState {
	line := "Output line: checked source file and completed local validation.\n"
	body := strings.Repeat(line, bytes/len(line)+1)[:bytes]

	tools := make([]coding.ToolState, count)
	for i := range tools {
		id := fmt.Sprintf("probe-%d", i)
		tools[i] = coding.ToolState{
			Call:   coding.ToolCall{ID: id, Name: "shell", Arguments: ai.JSON(`{"command":"go test ./..."}`)},
			Status: coding.ToolStatusCompleted,
			Result: codingToolResultFor(id, "shell", body),
		}
	}

	return tools
}

func BenchmarkRunningToolActivities(b *testing.B) {
	for _, count := range []int{0, 10, 50} {
		b.Run(fmt.Sprintf("completed=%d/output=32KiB", count), func(b *testing.B) {
			tools := benchmarkCompletedTools(count, 32<<10)

			b.ReportAllocs()

			for b.Loop() {
				if got := runningToolActivities(tools); len(got) != 0 {
					b.Fatal("completed tools reported running")
				}
			}
		})
	}
}
