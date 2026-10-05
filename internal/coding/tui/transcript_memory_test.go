//nolint:wsl_v5 // The measurement and its guard stay adjacent to the note that fixes the bound.
package tui

import (
	"runtime"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// retainedHeap reports the heap a value keeps alive, in bytes. Two collections
// around the measurement make it a retained size rather than a cumulative
// allocation.
func retainedHeap(build func() any) uint64 {
	runtime.GC()
	runtime.GC()

	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	value := build()

	runtime.GC()
	runtime.GC()

	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(value)

	return after.HeapAlloc - before.HeapAlloc
}

// TestTranscriptRetainedRowsStayBoundedAtTheTranscriptCap is the Phase 3
// regression guard. A fullscreen model over a conversation at the transcript cap
// used to retain the whole rendered conversation twice: once as the
// whole-conversation timeline string and once as the store's rows. Both are gone,
// so what is measured here is the record index plus the reading window.
//
// Measured on this machine (darwin/arm64, 80-column frame, 960-byte assistant
// bodies, 4096 messages):
//
//	messages   state    fullscreen   resident rows
//	     400    29KB         507KB             5KB
//	    1600   115KB         947KB             5KB
//	    4096   289KB        2041KB             5KB
//
// The guard is the absolute bound at the cap, with room for the runtime's own
// noise. It fails if the whole-conversation timeline string or the whole row set
// comes back, which measured 5374KB before this change.
func TestTranscriptRetainedRowsStayBoundedAtTheTranscriptCap(t *testing.T) {
	const messages = 4096

	state := bulkState(messages)
	stateOnly := retainedHeap(func() any {
		clone := state.Clone()

		return &clone
	})

	var model *Model

	fullscreen := retainedHeap(func() any {
		model = fullscreenModel(t, stubController{state: state}, true)
		model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
		model.rerenderTranscript(true)
		_ = model.View()

		return model
	})

	require.NotNil(t, model)

	t.Logf("messages=%d state=%dKB fullscreen=%dKB rows=%d resident=%d residentBytes=%dKB",
		len(state.Transcript), stateOnly/1024, fullscreen/1024,
		model.transcript.rowCount(), model.transcript.residentRowCount(),
		model.transcript.residentRowBytes()/1024)

	assert.Less(t, fullscreen, uint64(4*1024*1024),
		"the model retains the record index and one window, not the rendered conversation")
	assert.Less(t, model.transcript.residentRowBytes(), 16*1024,
		"the rendered rows are one window, whatever the conversation length")
	assert.Greater(t, model.transcript.rowCount(), 10*model.transcript.residentRowCount(),
		"the window is a small fraction of a capped conversation")
}

// TestTranscriptRowIndexCountsEveryRecordWithoutHoldingIt asserts the row index
// covers the conversation while the rows stay windowed.
func TestTranscriptRowIndexCountsEveryRecordWithoutHoldingIt(t *testing.T) {
	t.Parallel()

	short := transcriptWindowModel(t, 60)
	long := transcriptWindowModel(t, 300)

	assert.Greater(t, long.transcript.rowCount(), 4*short.transcript.rowCount())
	assert.LessOrEqual(t, long.transcript.residentRowCount(), residentRowBound(long))

	for _, record := range long.transcript.records {
		assert.NotEmpty(t, record.id)
		require.Positive(t, record.height(), "every record keeps its row count")
	}
}
