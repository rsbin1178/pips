//nolint:wsl_v5 // The fixture, its measurement, and the comparison note stay adjacent.
package tui

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/require"
)

// toolBulkState builds a conversation of messages with tools completed Tool
// calls. Half of the messages carry one call each and a matching durable
// result, which is the shape the managed viewport projects twice: once as the
// scan records and once as the cards it renders.
func toolBulkState(messages, tools int) coding.State {
	state := readyState()
	state.Interaction = coding.InteractionState{ID: "interaction-1", Active: false}
	state.Phase = coding.PhaseIdle
	state.Transcript = make(ai.Messages, 0, messages)
	state.Tools = make([]coding.ToolState, 0, tools)

	body := strings.Repeat("durable assistant prose for the projection fixture ", 4)
	resultBody := strings.Repeat("tool result body ", 10)

	// Each tool contributes one user/tool-call/result triple. The rest of the
	// conversation is ordinary text, so the message count stays exact.
	for tool := 0; tool < tools && len(state.Transcript)+3 <= messages; tool++ {
		callID := fmt.Sprintf("call-%04d", tool)
		args := ai.JSON(fmt.Sprintf(`{"path":"internal/file-%04d.go","pattern":"needle"}`, tool))
		result := ai.ToolMessage{Parts: []ai.ToolResultPart{{
			ToolCallID: callID, Name: "read",
			Content: []ai.Part{ai.TextPart{Text: resultBody}},
		}}}

		state.Transcript = append(state.Transcript,
			ai.UserText(fmt.Sprintf("read file %d", tool)),
			ai.Assistant(ai.ToolCallPart{ID: callID, Name: "read", Args: args}),
			result,
		)
		state.Tools = append(state.Tools, coding.ToolState{
			RunID: "run-1", Turn: 1,
			Call:   coding.ToolCall{ID: callID, Name: "read", Arguments: args},
			Status: coding.ToolStatusCompleted,
			Result: result,
		})
	}

	for index := len(state.Transcript); index < messages; index++ {
		if index%2 == 0 {
			state.Transcript = append(state.Transcript, ai.UserText(fmt.Sprintf("question %d", index)))

			continue
		}

		state.Transcript = append(state.Transcript, ai.AssistantText(body))
	}

	return state
}

func fullscreenProjectionModel(tb testing.TB, state coding.State) *Model {
	tb.Helper()

	model := newModel(tb.Context(), Options{
		Workspace: "/workspace", NoColor: true,
		PinPresentation: true, Screen: ScreenFullscreen, AltScreen: AltScreenAlways,
	})
	model.state = state
	model.lifecycle = lifecycleReady
	model.sizeReady = true
	model.width = 120
	model.height = 40
	model.subscription = &subscriptionBridge{}
	model.setLayout()
	model.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	model.rerenderTranscript(true)
	_ = model.View()

	return model
}

// BenchmarkFullscreenProjectionRetained measures the heap a painted fullscreen
// model keeps alive over a 6000-message conversation with 0/500/2000 tools.
func BenchmarkFullscreenProjectionRetained(b *testing.B) {
	for _, tools := range []int{0, 500, 2_000} {
		b.Run(fmt.Sprintf("tools=%d", tools), func(b *testing.B) {
			b.StopTimer()

			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)

			state := toolBulkState(6_000, tools)
			model := fullscreenProjectionModel(b, state)

			runtime.GC()
			runtime.ReadMemStats(&after)

			retained := float64(after.HeapAlloc) - float64(before.HeapAlloc)
			b.Logf("messages=%d tools=%d retained=%.0fKB activities=%d projectionEntries=%d scanRecords=%d blocks=%d records=%d",
				len(state.Transcript), len(state.Tools), retained/1024,
				len(model.toolActivities), len(model.toolProjection.entries),
				len(model.frameCache.toolScan.records), len(model.frameCache.entries),
				len(model.transcript.records))
			b.ReportMetric(retained, "retained-B")

			b.ResetTimer()

			for range b.N {
				runtime.KeepAlive(model)
			}
		})
	}
}

func TestFullscreenProjectionRetainsOneBlockPerRecord(t *testing.T) {
	t.Parallel()

	state := toolBulkState(600, 200)
	model := fullscreenProjectionModel(t, state)

	require.Len(t, model.transcript.records, len(model.frameCache.entries),
		"the store keeps one record per cached entry")
	require.Len(t, state.Tools, len(model.toolActivities))

	shared := 0

	for index := range model.frameCache.entries {
		entry := &model.frameCache.entries[index]
		require.NotNil(t, entry.block)
		require.Same(t, entry.block, model.transcript.records[index].block,
			"the store and the frame share one block value")
		if entry.block.kind != blockTool {
			continue
		}

		require.NotEmpty(t, entry.block.tools)
		record, ok := model.frameCache.toolScan.records[entry.block.tools[0].id]
		require.True(t, ok)
		require.Same(t, record.activity, entry.block.tools[0],
			"the card and the scan share one activity value")
		shared++
	}

	require.Equal(t, len(state.Tools), shared)
}

// BenchmarkTranscriptModeSnapshotRetained compares the old whole-document
// snapshot with the paged document the escape hatch now keeps.
func BenchmarkTranscriptModeSnapshotRetained(b *testing.B) {
	state := bulkState(6_000)
	model := fullscreenProjectionModel(b, state)

	b.Run("flattened_rows", func(b *testing.B) {
		b.StopTimer()
		runtime.GC()

		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		rows := model.transcript.flatten()
		runtime.GC()
		runtime.ReadMemStats(&after)

		retained := float64(after.HeapAlloc) - float64(before.HeapAlloc)
		b.Logf("rows=%d retained=%.0fKB", len(rows), retained/1024)
		b.ReportMetric(retained, "retained-B")
		b.ResetTimer()

		for range b.N {
			runtime.KeepAlive(rows)
		}
	})

	b.Run("paged_document", func(b *testing.B) {
		b.StopTimer()
		runtime.GC()

		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		document := model.transcript.snapshotDocument()
		runtime.GC()
		runtime.ReadMemStats(&after)

		retained := float64(after.HeapAlloc) - float64(before.HeapAlloc)
		b.Logf("records=%d retained=%.0fKB", len(document.store.records), retained/1024)
		b.ReportMetric(retained, "retained-B")
		b.ResetTimer()

		for range b.N {
			runtime.KeepAlive(document)
		}
	})
}

// BenchmarkFullscreenProjectionMessageScaling measures retained heap as the
// conversation grows, with one tool per three messages, at the 1k/4k/16k sizes
// the memory budget is defined against.
func BenchmarkFullscreenProjectionMessageScaling(b *testing.B) {
	for _, messages := range []int{1_000, 4_000, 16_000} {
		b.Run(fmt.Sprintf("messages=%d", messages), func(b *testing.B) {
			b.StopTimer()

			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)

			state := toolBulkState(messages, messages/3)
			model := fullscreenProjectionModel(b, state)

			runtime.GC()
			runtime.ReadMemStats(&after)

			retained := float64(after.HeapAlloc) - float64(before.HeapAlloc)
			b.Logf("messages=%d tools=%d retained=%.0fKB",
				len(state.Transcript), len(state.Tools), retained/1024)
			b.ReportMetric(retained, "retained-B")
			b.ReportMetric(retained/float64(len(state.Transcript)), "B/message")

			b.ResetTimer()

			for range b.N {
				runtime.KeepAlive(model)
			}
		})
	}
}
