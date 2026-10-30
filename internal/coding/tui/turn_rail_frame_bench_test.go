package tui

import (
	"strconv"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// railBenchModel builds a ready fullscreen viewport over a synthetic conversation
// of user turns, so every frame carries the turn rail. It does not need the frozen
// probe fixture, which is why these cases run everywhere.
func railBenchModel(tb testing.TB, userTurns int) *Model {
	tb.Helper()

	return benchModel(tb, turnRailConversation(userTurns), 80, 24)
}

// BenchmarkTurnRailFrame measures one painted frame with the turn rail on it. The
// rail's work should follow the viewport's rows, not the conversation's turns, so
// the two cases are expected to report the same allocations per frame.
func BenchmarkTurnRailFrame(b *testing.B) {
	for _, userTurns := range []int{200, 2000} {
		b.Run(strconv.Itoa(userTurns)+"_turns", func(b *testing.B) {
			model := railBenchModel(b, userTurns)
			model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			model.rerenderTranscript(true)
			_ = model.View()

			b.ReportAllocs()

			for b.Loop() {
				_ = model.View()
			}
		})
	}
}

// TestTurnRailFrameCostDoesNotTrackTheTurns is the rail's share of the bound
// TestTranscriptFrameCostDoesNotTrackTheConversation applies to the conversation:
// a frame that paints the rail costs for the visible rows, so ten times the turns
// may not add a per-turn allocation to it. The bound watches allocations, which is
// where per-turn work shows first; a scan of the whole conversation that allocated
// nothing would evade it, which is why the drawing loop stays bounded by the window
// it paints.
//
//nolint:paralleltest // The benchmark harness measures the whole process.
func TestTurnRailFrameCostDoesNotTrackTheTurns(t *testing.T) {
	results := make([]testing.BenchmarkResult, 0, 2)

	for _, userTurns := range []int{200, 2000} {
		model := railBenchModel(t, userTurns)
		model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
		model.rerenderTranscript(true)
		_ = model.View()

		require.NotEmpty(t, markedRows(model), "the fixture paints a rail")

		//nolint:thelper // The closure paints frames; it has no assertion to attribute.
		results = append(results, testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()

			for b.Loop() {
				_ = model.View()
			}
		}))
	}

	small, large := results[0], results[1]
	assert.Less(t, large.AllocedBytesPerOp(), small.AllocedBytesPerOp()+4096,
		"ten times the turns may not add a per-turn allocation to the frame: %v vs %v", large, small)
	assert.Less(t, large.AllocsPerOp(), small.AllocsPerOp()+16,
		"nor a per-turn allocation count")
}
