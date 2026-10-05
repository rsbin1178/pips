//nolint:wsl_v5 // Each residency assertion stays next to the window it describes.
package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// markerState builds a fullscreen conversation of n one-row entries, each naming
// itself so a test can tell which record a window or a match landed on.
func markerState(n int) coding.State {
	state := readyState()
	for index := range n {
		state.Transcript = append(state.Transcript, ai.UserText(fmt.Sprintf("MARKER-%03d", index)))
	}

	return state
}

// bulkState builds a conversation of the given message count with a full-sized
// assistant body on every other message, which is the shape the memory
// measurements use.
func bulkState(messages int) coding.State {
	state := readyState()
	body := strings.Repeat("lorem ipsum dolor sit amet ", 40)
	state.Transcript = make([]ai.Message, 0, messages)

	for index := range messages {
		if index%2 == 0 {
			state.Transcript = append(state.Transcript, ai.UserText(fmt.Sprintf("question %d", index)))

			continue
		}

		state.Transcript = append(state.Transcript, ai.Assistant(ai.Text(body)))
	}

	return state
}

// transcriptWindowModel opens a sized fullscreen model over a conversation long
// enough that most of it sits outside the reading window.
func transcriptWindowModel(t *testing.T, markers int) *Model {
	t.Helper()

	model := fullscreenModel(t, stubController{state: markerState(markers)}, true)
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model.rerenderTranscript(true)
	_ = model.View()

	return model
}

// residentRowBound is the row count a window may keep: the window, its margin on
// both sides, and one whole record at each end (records are released whole).
func residentRowBound(model *Model) int {
	largest := 0

	for _, record := range model.transcript.records {
		largest = max(largest, record.height())
	}

	return model.transcriptScroll.height + 2*transcriptWindowMargin + 2*largest
}

// TestTranscriptKeepsOnlyTheReadingWindowResident asserts the store holds the
// rows the window touches and releases the rest, so residency is bounded by the
// window rather than by the conversation length.
func TestTranscriptKeepsOnlyTheReadingWindowResident(t *testing.T) {
	t.Parallel()

	model := transcriptWindowModel(t, 400)
	store := &model.transcript

	total := store.rowCount()
	resident := store.residentRowCount()
	require.Positive(t, model.transcriptScroll.height)
	require.Greater(t, total, 4*resident,
		"the conversation must be much longer than what stays resident")

	assert.LessOrEqual(t, resident, residentRowBound(model),
		"resident rows stay inside the window plus its margin")
	assert.LessOrEqual(t, store.residentRowBytes(), 16*1024,
		"the rendered rows the window keeps are bounded by a small constant")

	// The oldest records are far above the window, so their rows are gone while
	// their identity and row count remain.
	assert.False(t, store.records[0].loaded, "a record above the window holds no rows")
	assert.Positive(t, store.records[0].height(), "its row count is still known")
	assert.Positive(t, store.evictions, "the window released something")

	// Residency does not depend on how long the conversation is.
	longer := transcriptWindowModel(t, 800)
	assert.LessOrEqual(t, longer.transcript.residentRowCount(), residentRowBound(longer))
	assert.Less(t, longer.transcript.residentRowCount(), 4*resident,
		"a conversation twice as long does not hold four times the rows")
}

// TestTranscriptRendersReleasedRowsAgainOnDemand asserts that scrolling back to
// a released record reproduces exactly the rows it had before.
func TestTranscriptRendersReleasedRowsAgainOnDemand(t *testing.T) {
	t.Parallel()

	model := transcriptWindowModel(t, 120)
	store := &model.transcript

	// The full document, read once, is the reference rendering.
	document := store.flatten()
	require.NotEmpty(t, document)
	require.False(t, store.records[0].loaded, "reading the document does not pin it")

	region := &model.transcriptScroll
	region.setHeight(8)
	region.gotoTop()

	assert.Equal(t,
		strings.Join(document[:8], "\n"),
		region.visible(),
		"a released record renders to the same rows it had")
	assert.True(t, store.records[0].loaded, "the window rendered the record it needs")
}

// TestTranscriptSearchReachesReleasedRecords asserts the search corpus survives
// eviction: a released record is read and rendered again to answer a query, and
// the window goes back to its bound afterwards.
func TestTranscriptSearchReachesReleasedRecords(t *testing.T) {
	t.Parallel()

	model := transcriptWindowModel(t, 120)
	store := &model.transcript
	require.False(t, store.records[0].loaded, "the first record is outside the window")

	model.openSearch("MARKER-000")
	require.Len(t, model.search.matches, 1, "a released record is still searchable")

	row := model.search.matches[0]
	assert.Contains(t, ansi.Strip(store.rowsIn(row, row+1)[0]), "MARKER-000",
		"the match names the row that holds the marker")

	model.selectMatch(0)
	assert.Contains(t, ansi.Strip(model.View().Content), "MARKER-000")
	assert.LessOrEqual(t, store.residentRowCount(), residentRowBound(model))
}

// TestSearchMatchesTheRowsAThinkingBlockRenders asserts the find box sees the
// rendered rows: a Thinking block renders every reasoning row, so all of them
// are searchable.
func TestSearchMatchesTheRowsAThinkingBlockRenders(t *testing.T) {
	t.Parallel()

	const tail = "the very last thought line"

	model := fullscreenModel(t, stubController{state: thinkingState(tail)}, true)
	model.rerenderTranscript(true)

	model.openSearch("step one")
	assert.Len(t, model.search.matches, 1, "the first reasoning row is searchable")

	model.openSearch(tail)
	assert.Len(t, model.search.matches, 1, "a later reasoning row stays searchable")
}

// TestTranscriptFrameCostDoesNotTrackTheConversation asserts a drawn frame reads
// the window rather than the conversation: the same frame work for four times the
// messages, and no settled record re-rendered.
//
//nolint:paralleltest // AllocsPerRun must not run beside another test.
func TestTranscriptFrameCostDoesNotTrackTheConversation(t *testing.T) {
	allocs := make([]float64, 0, 3)

	for _, messages := range []int{400, 1600, 4096} {
		model := fullscreenModel(t, stubController{state: bulkState(messages)}, true)
		model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
		model.rerenderTranscript(true)
		_ = model.View()

		model.transcript.resetRenders()
		_ = model.View()
		assert.Zero(t, model.transcript.renders,
			"drawing a settled conversation re-renders no record")

		allocs = append(allocs, testing.AllocsPerRun(5, func() { _ = model.View() }))
	}

	assert.Less(t, allocs[2], 2*allocs[0],
		"a frame allocates for the window, not for the conversation: %v", allocs)
}
