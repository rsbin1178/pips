//nolint:wsl_v5 // Search cases keep each fixture next to its assertions.
package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// searchModel builds a fullscreen model whose conversation carries numbered
// markers, with the managed projection reset so every marker is in the store.
func searchModel(t *testing.T, noColor bool, markers int) *Model {
	t.Helper()

	state := readyState()
	for index := range markers {
		state.Transcript = append(
			state.Transcript,
			ai.AssistantText(fmt.Sprintf("MARKER-%02d", index)),
		)
	}

	model := fullscreenModel(t, stubController{state: state}, noColor)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 16})
	model.resetScrollback()
	model.rerenderTranscript(true)

	return model
}

func typeQuery(t *testing.T, model *Model, query string) {
	t.Helper()

	for _, character := range query {
		model.Update(key(string(character)))
	}
}

func previousMatchKey() tea.KeyPressMsg {
	return tea.KeyPressMsg{Text: keyShiftEnter, Code: tea.KeyEnter, Mod: tea.ModShift}
}

func TestSearchJumpsToAndHighlightsAMatch(t *testing.T) {
	t.Parallel()

	model := searchModel(t, false, 40)
	model.Update(key("ctrl+f"))
	require.True(t, model.search.active)

	assert.Contains(t, ansi.Strip(model.View().Content), routeSearchPrompt,
		"the find box takes the composer's band")

	typeQuery(t, model, "MARKER-01")
	require.Len(t, model.search.matches, 1, "only the second marker matches")
	assert.Contains(t, ansi.Strip(model.View().Content), "find · 1 matches")

	model.Update(key("enter"))
	assert.False(t, model.transcriptScroll.follow, "a jump pauses following")
	assert.Contains(t, ansi.Strip(model.View().Content), "find · 1/1")

	frame := model.View().Content
	row := model.search.matches[model.search.index]
	assert.Contains(t, ansi.Strip(frame), "MARKER-01")
	assert.LessOrEqual(t, row-model.transcriptScroll.offset, searchContextRows)
	assert.Less(t, model.transcriptScroll.offset, row, "context stays visible above the match")
	assert.Contains(t, frame, "\x1b[1;7m", "the current match is highlighted")
	assert.LessOrEqual(t, frameHeight(frame), 16, "the highlighted frame still fits the window")
}

func TestSearchWithoutMatchesStillTypes(t *testing.T) {
	t.Parallel()

	model := searchModel(t, true, 3)
	model.Update(key("ctrl+f"))
	typeQuery(t, model, "ABSENT")

	assert.Empty(t, model.search.matches)
	assert.Contains(t, ansi.Strip(model.View().Content), "find · no match")

	following := model.transcriptScroll.follow
	model.Update(key("enter"))
	assert.Equal(t, following, model.transcriptScroll.follow, "nothing to jump to")
}

func TestSearchStepsAndWrapsAtBothEnds(t *testing.T) {
	t.Parallel()

	model := searchModel(t, true, 5)
	model.Update(key("ctrl+f"))
	typeQuery(t, model, "MARKER")
	require.Len(t, model.search.matches, 5)

	// The first step starts from the reader's window, not from the end of the list.
	model.Update(key("enter"))
	require.NotNil(t, model.search.anchor)
	require.Positive(t, model.search.matches[model.search.index]+1)

	// Wrap around at both ends.
	model.search.index = len(model.search.matches) - 1
	model.Update(key("enter"))
	assert.Equal(t, 0, model.search.index, "stepping past the last match wraps to the first")

	model.Update(previousMatchKey())
	assert.Equal(t, len(model.search.matches)-1, model.search.index,
		"stepping back past the first match wraps to the last")
}

func TestSearchStartsFromTheReaderPosition(t *testing.T) {
	t.Parallel()

	model := searchModel(t, true, 8)
	model.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	model.Update(key("ctrl+f"))
	typeQuery(t, model, "MARKER")

	model.Update(key("enter"))
	assert.Equal(t, 0, model.search.index, "a reader at the top starts at the oldest match")
	assert.Contains(t, ansi.Strip(model.View().Content), "MARKER-00")
}

func TestSearchKeepsItsMatchAcrossNewOutput(t *testing.T) {
	t.Parallel()

	model := searchModel(t, true, 6)
	model.Update(key("ctrl+f"))
	typeQuery(t, model, "MARKER-02")
	model.Update(key("enter"))
	require.Equal(t, 0, model.search.index)

	before := ansi.Strip(model.View().Content)
	anchor := model.search.anchor

	model.state.Transcript = append(model.state.Transcript, ai.AssistantText("LATE-ENTRY"))
	model.rerenderTranscript(false)

	assert.Equal(t, anchor, model.search.anchor, "the match is kept by identity")
	assert.Equal(t, 0, model.search.index)
	assert.Contains(t, ansi.Strip(model.View().Content), "MARKER-02")
	assert.Equal(t, before, ansi.Strip(model.View().Content),
		"new output must not move the reader away from the match")
}

func TestSearchCloseRestoresTheComposerAndItsDraft(t *testing.T) {
	t.Parallel()

	model := searchModel(t, true, 3)
	model.composer.SetValue("draft to keep")
	model.Update(key("ctrl+f"))
	typeQuery(t, model, "MARKER")

	assert.NotContains(t, ansi.Strip(model.View().Content), "draft to keep",
		"the find box takes the composer's band")

	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.False(t, model.search.active)
	assert.Equal(t, "draft to keep", model.composer.Value())
	assert.Contains(t, ansi.Strip(model.View().Content), "draft to keep")
}

func TestSearchBackspaceRecomputesMatches(t *testing.T) {
	t.Parallel()

	model := searchModel(t, true, 4)
	model.Update(key("ctrl+f"))
	typeQuery(t, model, "MARKER-01")
	require.Len(t, model.search.matches, 1)

	model.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	assert.Equal(t, "marker-0", model.searchQuery())
	assert.Len(t, model.search.matches, 4, "the wider query matches every marker")
}

func TestSearchOwnsClicksAndKeepsTheWheel(t *testing.T) {
	t.Parallel()

	model := searchModel(t, true, 6)
	model.Update(key("ctrl+f"))
	typeQuery(t, model, "MARKER-0")
	model.Update(key("enter"))

	offset := model.transcriptScroll.offset
	_, command := model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	assert.Nil(t, command)
	assert.Greater(t, model.transcriptScroll.offset, offset, "the wheel still reads the viewport")

	// A click would open a route that hides the box, so it is dropped while the
	// box is open.
	_, command = model.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 5, Y: 1})
	assert.Nil(t, command)
	assert.Equal(t, routeNone, model.route.kind)

	// Escape hands the pointer back: the same click now opens the addressed entry.
	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.False(t, model.search.active)
}

func TestSearchIsUnavailableInline(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(key("ctrl+f"))
	assert.False(t, model.search.active)
	assert.Contains(t, strings.Join(model.statusExtras(), "\n"), "terminal owns")

	model.openCommandPicker()
	model.picker.query = commandFind
	model.syncCommandInput()

	_, command := model.Update(key("enter"))
	assert.Nil(t, command)
	require.Error(t, model.picker.err)
	assert.Contains(t, model.picker.err.Error(), "inline mode")
}

func TestFindCommandOpensTheBoxWithAQuery(t *testing.T) {
	t.Parallel()

	model := searchModel(t, true, 6)
	model.openCommandPicker()
	model.picker.query = "find MARKER-03"
	model.syncCommandInput()

	_, command := model.Update(key("enter"))
	assert.Nil(t, command)
	require.True(t, model.search.active)
	assert.Equal(t, "MARKER-03", model.search.input.Value())
	assert.Equal(t, 0, model.search.index, "a seeded query jumps immediately")
	assert.Contains(t, ansi.Strip(model.View().Content), "MARKER-03")
}

func TestMatchRowsAndColumnsUseVisibleText(t *testing.T) {
	t.Parallel()

	rows := []string{
		"plain line",
		"\x1b[31mCOLOURED\x1b[0m match here",
		"wide 世界 line",
		"",
	}

	assert.Equal(t, []int{1}, matchRows(rows, "coloured"), "matching is case-insensitive")
	assert.Equal(t, []int{1}, matchRows(rows, "match"), "escape sequences are not searched")
	assert.Equal(t, []int{2}, matchRows(rows, "世界"))
	assert.Empty(t, matchRows(rows, "   "))
	assert.Empty(t, matchRows(nil, "anything"))

	start, end, ok := matchColumns("\x1b[31mCOLOURED\x1b[0m match", "coloured")
	require.True(t, ok)
	assert.Equal(t, 0, start)
	assert.Equal(t, 8, end, "the escape sequence is not counted as columns")

	start, end, ok = matchColumns("wide 世界 line", "世界")
	require.True(t, ok)
	assert.Equal(t, 5, start)
	assert.Equal(t, 9, end, "wide characters count as two columns")

	_, _, ok = matchColumns("plain", "absent")
	assert.False(t, ok)

	assert.Equal(t, "ab\x1b[7mcd\x1b[mef", highlightColumns("abcdef", 2, 4, searchMatchStyle))
	assert.Equal(t, "abcdef", highlightColumns("abcdef", 2, 2, searchMatchStyle))
}

// TestSearchHighlightNeverChangesTheRowWidth is the frame-geometry guard: the hit
// map and the caret are measured against the composed frame, so a highlight that
// added or removed columns would silently break both.
func TestSearchHighlightNeverChangesTheRowWidth(t *testing.T) {
	t.Parallel()

	for _, row := range []string{
		"plain with match inside",
		"\x1b[1mbold\x1b[0m with \x1b[32mmatch\x1b[0m inside",
		"wide 世界 match 世界",
		"match",
	} {
		for _, style := range []lipgloss.Style{searchMatchStyle, searchCurrentMatchStyle} {
			start, end, ok := matchColumns(row, "match")
			require.True(t, ok, "row %q matches", row)

			highlighted := highlightColumns(row, start, end, style)
			assert.Equal(t, ansi.StringWidth(row), ansi.StringWidth(highlighted),
				"row %q kept its width", row)
			assert.Equal(t, ansi.Strip(row), ansi.Strip(highlighted),
				"row %q kept its text", row)
		}
	}
}

func TestSearchStatusTextReportsTheCounter(t *testing.T) {
	t.Parallel()

	model := searchModel(t, true, 4)
	model.Update(key("ctrl+f"))
	assert.Contains(t, model.searchStatusText(), "type to search")

	typeQuery(t, model, "MARKER")
	assert.Contains(t, model.searchStatusText(), "4 matches")

	model.Update(key("enter"))
	assert.Contains(t, model.searchStatusText(), "1/4")

	model.search.input.SetValue("ABSENT")
	model.refreshSearch()
	assert.Contains(t, model.searchStatusText(), "no match")
}

func TestSearchHighlightIsSkippedWithoutColor(t *testing.T) {
	t.Parallel()

	model := searchModel(t, true, 4)
	model.Update(key("ctrl+f"))
	typeQuery(t, model, "MARKER-01")
	model.Update(key("enter"))

	assert.NotContains(t, model.View().Content, "\x1b[7m",
		"NO_COLOR renders no highlight, so the frame carries no styling")
}

func TestSearchBindingIsAvailableWithoutConflicts(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateActions(defaultActions))

	for _, context := range []actionContext{contextIdle, contextRunning, contextPaused} {
		action, ok := resolveAction(defaultActions, context, keyCtrlF)
		require.True(t, ok, "Ctrl+F must resolve in %s", context)
		assert.Equal(t, actionSearch, action)
	}

	assert.Contains(t, renderActionHelp(defaultActions, contextIdle), keyCtrlF+" — find")
}

// TestSearchFrameStaysInsideTheWindow is the geometry guard for the band swap: the
// find box takes the composer's place at every window size, so the frame must keep
// fitting the terminal it is drawn into.
func TestSearchFrameStaysInsideTheWindow(t *testing.T) {
	t.Parallel()

	model := searchModel(t, true, 20)
	model.Update(key("ctrl+f"))
	typeQuery(t, model, "MARKER")

	for _, height := range []int{1, 2, 3, 4, 5, 6, 9, 10, 16, 24, 60} {
		for _, width := range []int{8, 20, 24, 40, 120} {
			model.Update(tea.WindowSizeMsg{Width: width, Height: height})

			frame := model.View().Content
			assert.LessOrEqual(t, frameHeight(frame), height,
				"frame at %dx%d: %q", width, height, frame)
		}
	}

	// The box survives a resize: the query and its counter are still on screen.
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 16})
	assert.Contains(t, ansi.Strip(model.View().Content), "MARKER")
	assert.Contains(t, model.searchStatusText(), "matches")
}

// TestSearchHidesWhileAnotherSurfaceOwnsInput pins the visibility rule: a surface
// that takes the keyboard also takes the band, and the box comes back with its
// query intact instead of showing a query nothing can type into.
func TestSearchHidesWhileAnotherSurfaceOwnsInput(t *testing.T) {
	t.Parallel()

	model := searchModel(t, true, 6)
	model.Update(key("ctrl+f"))
	typeQuery(t, model, "MARKER")
	require.True(t, model.searchOwnsKeys())

	model.openCommandPicker()
	assert.False(t, model.searchOwnsKeys(), "a picker takes the keyboard over")
	assert.NotContains(t, ansi.Strip(model.View().Content), routeSearchPrompt,
		"the find box is hidden while the picker owns the keys")

	model.closeCommandPicker(true)
	assert.True(t, model.searchOwnsKeys())
	assert.Contains(t, ansi.Strip(model.View().Content), routeSearchPrompt)
	assert.Equal(t, "MARKER", model.search.input.Value(), "the query survived")
	assert.Len(t, model.search.matches, 6)
}

// TestSearchBoxTeachesItsKeysWhileEmpty records where a first-time user learns the
// keys: the box teaches them in place, while the query is still empty.
func TestSearchBoxTeachesItsKeysWhileEmpty(t *testing.T) {
	t.Parallel()

	model := searchModel(t, true, 3)
	model.Update(key("ctrl+f"))

	assert.Contains(t, ansi.Strip(model.View().Content),
		"Find in conversation… (Enter next · Esc closes)")

	typeQuery(t, model, "MARKER")
	assert.NotContains(t, ansi.Strip(model.View().Content), "Find in conversation…")
	assert.Contains(t, ansi.Strip(model.View().Content), "MARKER")
}
