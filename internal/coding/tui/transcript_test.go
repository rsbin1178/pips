//nolint:wsl_v5 // Region cases and their assertions stay adjacent.
package tui

import (
	"context"
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

func rows(count int) []string {
	values := make([]string, count)
	for index := range values {
		values[index] = fmt.Sprintf("row-%d", index)
	}

	return values
}

func TestScrollRegionFollowsUntilTheReaderScrolls(t *testing.T) {
	t.Parallel()

	region := newScrollRegion()
	region.setHeight(3)
	region.setRows(rows(10))
	assert.True(t, region.follow, "a fresh region follows the tail")
	assert.Equal(t, "row-7\nrow-8\nrow-9", region.visible())

	// A user scroll pauses following, and new output must not move the window.
	region.scrollBy(-4)
	assert.False(t, region.follow)
	before := region.visible()
	region.setRows(rows(14))
	assert.Equal(t, before, region.visible(), "new output must not pull a scrolled reader")

	// Returning to the latest message restores following.
	region.gotoBottom()
	assert.True(t, region.follow)
	assert.Equal(t, "row-11\nrow-12\nrow-13", region.visible())

	region.setRows(rows(15))
	assert.Equal(t, "row-12\nrow-13\nrow-14", region.visible())
}

func TestScrollRegionShorterThanWindowHasNoScrollingState(t *testing.T) {
	t.Parallel()

	region := newScrollRegion()
	region.setHeight(10)
	region.setRows(rows(3))
	assert.True(t, region.follow)
	assert.Equal(t, "row-0\nrow-1\nrow-2", region.visible())
	assert.Equal(t, 0, region.maxOffset())
	assert.True(t, region.atBottom())
}

func TestScrollRegionClampsWhenContentDisappears(t *testing.T) {
	t.Parallel()

	region := newScrollRegion()
	region.setHeight(4)
	region.setRows(rows(20))
	region.scrollBy(-16)
	require.Equal(t, 0, region.offset)

	region.setRows(rows(2))
	assert.Equal(t, 0, region.offset)
	assert.True(t, region.follow, "content that fits returns the region to the tail")
	assert.Equal(t, "row-0\nrow-1", region.visible())
}

func TestScrollRegionHeightChangeKeepsFollowingPinned(t *testing.T) {
	t.Parallel()

	region := newScrollRegion()
	region.setHeight(4)
	region.setRows(rows(20))
	assert.Equal(t, "row-16\nrow-17\nrow-18\nrow-19", region.visible())

	region.setHeight(2)
	assert.Equal(t, "row-18\nrow-19", region.visible(), "a shrink keeps the newest rows visible")

	region.setHeight(6)
	assert.Equal(t, "row-14\nrow-15\nrow-16\nrow-17\nrow-18\nrow-19", region.visible())
}

// TestTranscriptStoreReusesSettledRecords is the AC7 guard: a streaming delta
// must re-render the growing record, not the whole conversation.
func TestTranscriptStoreReusesSettledRecords(t *testing.T) {
	t.Parallel()

	state := readyState()
	for _, marker := range []string{"TURN-1", "TURN-2", "TURN-3", "TURN-4"} {
		state.Transcript = append(state.Transcript, ai.UserText(marker))
	}
	model := readyModelWithController(t, stubController{state: state}, true)
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	// Inline mode commits stable history to the terminal, so the managed region
	// only sees the uncommitted tail. Reset the projection cursor so this test
	// observes the store holding the whole conversation, which is what the
	// fullscreen region will do.
	model.resetScrollback()
	model.rerenderTranscript(true)

	settled := model.transcript.rowCount()
	require.Positive(t, settled)
	model.transcript.resetRenders()

	// A streaming delta changes only the draft record.
	model.state.Draft = []coding.MessageDelta{{Kind: ai.StreamTextDelta, Text: "streaming…"}}
	model.rerenderTranscript(false)

	assert.LessOrEqual(t, model.transcript.renders, 1,
		"only the growing record may be re-rendered, got %d", model.transcript.renders)
	assert.GreaterOrEqual(t, model.transcript.rowCount(), settled)
}

// TestTranscriptStoreInvalidatesOnWidthChange asserts a reflow re-renders the
// cached records instead of reusing rows rendered at another width.
func TestTranscriptStoreInvalidatesOnWidthChange(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Transcript = append(state.Transcript, ai.UserText(strings.Repeat("wide ", 40)))
	model := readyModelWithController(t, stubController{state: state}, true)
	model.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	model.resetScrollback()
	model.rerenderTranscript(true)
	wide := model.transcript.rowCount()
	model.transcript.resetRenders()

	model.Update(tea.WindowSizeMsg{Width: 40, Height: 24})
	model.rerenderTranscript(false)

	assert.Positive(t, model.transcript.renders, "a width change must re-render")
	assert.Greater(t, model.transcript.rowCount(), wide, "narrower rows wrap into more rows")
}

// TestTranscriptAnchorSurvivesResizeAndGrowth asserts the reading position is
// held by record identity, not by row index.
func TestTranscriptAnchorSurvivesResizeAndGrowth(t *testing.T) {
	t.Parallel()

	state := readyState()
	for index := range 20 {
		state.Transcript = append(state.Transcript, ai.UserText(fmt.Sprintf("MARKER-%d", index)))
	}
	model := readyModelWithController(t, stubController{state: state}, true)
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model.resetScrollback()
	model.rerenderTranscript(true)

	region := &model.transcriptScroll
	region.setHeight(6)
	region.scrollBy(-8)
	captured, anchorRow := model.transcript.captureAnchor(region.offset)
	require.NotEmpty(t, captured.recordID, "the reading position must name a record")

	// New output arrives while the reader is scrolled up.
	model.state.Transcript = append(model.state.Transcript, ai.UserText("MARKER-NEW"))
	model.rerenderTranscript(false)

	resolved := model.transcript.resolveAnchor(captured)
	require.GreaterOrEqual(t, resolved, 0, "the anchored record must still exist")
	assert.Equal(t, anchorRow, resolved, "the anchored record keeps its row for content above it")

	// A width change reflows; the anchor still resolves to the same record.
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 24})
	model.rerenderTranscript(false)
	resolved = model.transcript.resolveAnchor(captured)
	require.GreaterOrEqual(t, resolved, 0)
	assert.Equal(t, captured.recordID, model.transcript.recordIDAt(resolved))
}

// TestTranscriptStoreKeepsLiveRecordIdentity asserts the streaming record keeps
// one identity across the promotion to settled history, which is what prevents a
// duplicate or a jump at the handoff.
func TestTranscriptStoreKeepsLiveRecordIdentity(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Interaction = coding.InteractionState{ID: "interaction-1", Active: true}
	state.Phase = coding.PhaseRunning
	state.Draft = []coding.MessageDelta{{Kind: ai.StreamTextDelta, Text: "partial answer"}}
	model := readyModelWithController(t, stubController{state: state}, true)
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model.resetScrollback()
	model.rerenderTranscript(true)

	live := 0
	for _, record := range model.transcript.records {
		if record.live {
			live++
		}
	}
	assert.Equal(t, 1, live, "exactly one record may be live")

	before := model.transcript.rowCount()
	// The draft settles into an assistant message carrying the same candidate.
	model.state.Draft = nil
	model.state.MessageCandidates = append(model.state.MessageCandidates, model.state.DraftCandidate)
	model.state.Transcript = append(model.state.Transcript, ai.AssistantText("partial answer"))
	model.rerenderTranscript(false)

	assert.GreaterOrEqual(t, model.transcript.rowCount(), before)
	for _, record := range model.transcript.records {
		assert.False(t, record.live, "no record stays live after settlement")
	}
}

// TestTranscriptScrollKeysDriveTheReadyView asserts the conversation keys move
// the region from the main key handler and that reading is not disturbed by new
// output.
func TestTranscriptScrollKeysDriveTheReadyView(t *testing.T) {
	t.Parallel()

	state := readyState()
	for index := range 30 {
		state.Transcript = append(state.Transcript, ai.UserText("MARKER-"+fmt.Sprintf("%02d", index)))
	}
	model := readyModelWithController(t, stubController{state: state}, true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	model.resetScrollback()
	model.rerenderTranscript(true)
	require.True(t, model.transcriptScroll.follow)

	before := model.View().Content
	require.Contains(t, before, "MARKER-29", "the tail is visible while following")

	model.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	assert.False(t, model.transcriptScroll.follow, "scrolling up pauses following")
	scrolled := ansi.Strip(model.View().Content)
	assert.NotContains(t, scrolled, "MARKER-29", "the window moved away from the tail")

	// New output arrives while the reader is scrolled up: the window must not move.
	captured, anchorRow := model.transcript.captureAnchor(model.transcriptScroll.offset)
	model.state.Transcript = append(model.state.Transcript, ai.UserText("MARKER-NEW"))
	model.rerenderTranscript(false)
	assert.Equal(t, scrolled, ansi.Strip(model.View().Content),
		"new output must not change a paused reading position")
	assert.Equal(t, anchorRow, model.transcript.resolveAnchor(captured))

	// Returning to the latest message resumes following and shows the new row.
	model.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	model.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	model.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	assert.True(t, model.transcriptScroll.follow)
	assert.Contains(t, ansi.Strip(model.View().Content), "MARKER-NEW")
}

// TestFullscreenTranscriptOwnsTheWholeConversation is the AC3/AC11 core: in
// fullscreen mode the managed region renders every entry, the frame stays inside
// the window, and no native write is attempted (tea.Println is a no-op under the
// alternate screen).
func TestFullscreenTranscriptOwnsTheWholeConversation(t *testing.T) {
	t.Parallel()

	state := readyState()
	for index := range 25 {
		state.Transcript = append(state.Transcript, ai.UserText(fmt.Sprintf("ENTRY-%02d", index)))
	}
	controller := stubController{state: state}
	model := fullscreenModel(t, controller, true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	model.rerenderTranscript(true)

	view := model.View()
	assert.True(t, view.AltScreen, "fullscreen owns the alternate screen")
	assert.Equal(t, tea.MouseModeAllMotion, view.MouseMode)
	assert.LessOrEqual(t, frameHeight(view.Content), 20)

	content := ansi.Strip(view.Content)
	assert.Contains(t, content, "ENTRY-24", "the newest entry is visible while following")

	// The oldest entries are reachable by scrolling rather than by the terminal.
	model.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	model.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	scrolled := ansi.Strip(model.View().Content)
	assert.False(t, model.transcriptScroll.follow)
	assert.NotContains(t, scrolled, "ENTRY-24")

	// A stable commit in fullscreen must not queue a native write.
	model.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	command := model.commitStableTimeline()
	assert.Nil(t, command, "fullscreen must not dispatch a native scrollback write")
	assert.Empty(t, model.presentation.writes)
}

// TestFullscreenStartupPutsTheBannerInTheTranscript asserts the banner and any
// inspection output land in the managed region instead of a terminal history that
// does not exist under the alternate screen.
func TestFullscreenStartupPutsTheBannerInTheTranscript(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Transcript = []ai.Message{ai.UserText("STARTUP-HISTORY")}
	controller := stubController{state: state}
	model := newModel(t.Context(), Options{
		Workspace:       "/workspace",
		NoColor:         true,
		PinPresentation: true,
		Screen:          ScreenFullscreen,
		AltScreen:       AltScreenAlways,
		Bootstrap: func(context.Context, bool) (Controller, error) {
			return controller, nil
		},
	})
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	_, command := model.Update(bootstrapResult{controller: controller})
	// Startup still returns a command for focus/subscription, but no native write
	// may be queued and the banner must be managed content.
	assert.Empty(t, model.presentation.writes)
	require.NotNil(t, command)
	_ = command

	content := ansi.Strip(model.View().Content)
	assert.Contains(t, content, "✻ Pips", "the banner is a managed record")
	assert.Contains(t, content, "STARTUP-HISTORY")

	model.printInspection("Status", "Session: session-1")
	inspection := ansi.Strip(model.View().Content)
	assert.Contains(t, inspection, "Status")
	assert.Empty(t, model.presentation.writes)
}

// buildRevisionProbe builds a store holding one settled record and one live
// record with the given body.
func buildRevisionProbe(store *transcriptStore, live string) {
	store.build(40, themeDark, themeFingerprint(themeDark.fingerprint), false, newMarkdownRenderer(8),
		[]transcriptEntry{
			{id: "settled", block: timelineBlock{kind: blockAssistant, id: "settled", body: "settled paragraph"}},
			{id: "draft", live: true, block: timelineBlock{kind: blockDraft, id: "draft", body: live}},
		})
}

// TestStoreRevisionDoesNotReadTheLiveRows pins that a frame's revision is cheap:
// hashing the live record's growing text is left to the search scan that reads
// it, instead of running on every frame.
func TestStoreRevisionDoesNotReadTheLiveRows(t *testing.T) {
	t.Parallel()

	var store transcriptStore
	buildRevisionProbe(&store, strings.Repeat("streaming words ", 40))

	store.liveRowsHashed = 0
	store.updateRevision()

	assert.Zero(t, store.liveRowsHashed, "a frame's revision must not read the live text")

	store.fingerprint()
	assert.Positive(t, store.liveRowsHashed, "a search fingerprint must read the live text")
}

// TestStoreFingerprintSeesLiveTextAtTheSameHeight pins that the lazy fingerprint
// still catches a live record whose text changed without changing its height,
// which is the only reason a search has to re-read its rows.
func TestStoreFingerprintSeesLiveTextAtTheSameHeight(t *testing.T) {
	t.Parallel()

	var before, after transcriptStore
	buildRevisionProbe(&before, "aaaa bbbb")
	buildRevisionProbe(&after, "cccc dddd")

	require.Len(t, before.rowsIn(0, before.rowCount()), len(after.rowsIn(0, after.rowCount())),
		"the probe bodies must render to the same row count so the height cannot differ")
	require.Equal(t, before.revision, after.revision, "the height-only revision cannot tell them apart")
	assert.NotEqual(t, before.fingerprint(), after.fingerprint(),
		"a search must be told the live text changed")
}
