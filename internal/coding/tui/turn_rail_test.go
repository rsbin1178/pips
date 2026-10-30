package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// turnRailConversation builds a conversation of user turns, each followed by a
// short answer, so the rail has exactly one mark per user turn.
func turnRailConversation(userTurns int) coding.State {
	state := readyState()
	messages := make([]ai.Message, 0, userTurns*2)

	for index := range userTurns {
		messages = append(
			messages,
			ai.UserText("TURN-"+itoa(index+1)),
			ai.AssistantText("answer "+itoa(index+1)),
		)
	}

	state.Transcript = messages

	return state
}

// turnRailModel opens a painted fullscreen viewport over the fixture.
func turnRailModel(t *testing.T, userTurns, width, height int, noColor bool) *Model {
	t.Helper()

	model := fullscreenModel(t, stubController{state: turnRailConversation(userTurns)}, noColor)
	model.Update(tea.WindowSizeMsg{Width: width, Height: height})
	model.rerenderTranscript(true)
	_ = model.View()

	return model
}

// railCells returns the raw cell painted in the frame's rail column, one entry
// per transcript row, styling included. The band's row count keeps the read
// honest: the rail is drawn on the transcript band and on nothing else.
func railCells(model *Model) []string {
	rows := strings.Split(model.View().Content, "\n")
	rows = rows[:min(len(rows), max(0, model.frameHit.transcript))]
	cells := make([]string, 0, len(rows))

	for _, row := range rows {
		cells = append(cells, ansi.Cut(row, model.railColumn(), model.railColumn()+1))
	}

	return cells
}

// railGlyphs returns the rail column without styling, as the reader sees it.
func railGlyphs(model *Model) []string {
	cells := railCells(model)
	for index := range cells {
		cells[index] = ansi.Strip(cells[index])
	}

	return cells
}

// markedRows reports the rows the rail painted a mark on, in frame order.
func markedRows(model *Model) []int {
	glyphs := railGlyphs(model)
	rows := make([]int, 0, len(glyphs))

	for index, glyph := range glyphs {
		if glyph != "" && glyph != " " {
			rows = append(rows, index)
		}
	}

	return rows
}

// railMarks reports how many rail marks the frame paints, in either colour mode.
func railMarks(model *Model) int {
	glyphs := railGlyphs(model)

	return countGlyph(glyphs, turnRailRest) + countGlyph(glyphs, turnRailActive) +
		countGlyph(glyphs, turnRailRestNoColor) + countGlyph(glyphs, turnRailActiveNoColor)
}

// countGlyph counts one mark glyph in a painted column.
func countGlyph(cells []string, glyph string) int {
	count := 0

	for _, cell := range cells {
		if cell == glyph {
			count++
		}
	}

	return count
}

// indexOfGlyph reports the first row holding a glyph, or -1.
func indexOfGlyph(cells []string, glyph string) int {
	for index, cell := range cells {
		if cell == glyph {
			return index
		}
	}

	return -1
}

// railCapacity reports how many marks the rail holds at the painted geometry.
func railCapacity(model *Model) int {
	return turnRailCapacity(model.transcriptScroll.height, model.turnRail.markPitch())
}

// assertBandUnpainted proves a hidden rail leaves the transcript band alone: the
// frame paints the store's rows as they were rendered, with no padding and no
// mark added to the margin.
func assertBandUnpainted(t *testing.T, model *Model) {
	t.Helper()

	frame := strings.Split(model.View().Content, "\n")
	band := min(model.frameHit.transcript, len(frame))
	rows := model.transcript.rowsIn(model.transcriptScroll.offset, model.transcriptScroll.offset+band)
	// A short conversation is padded to the band, so the store may offer fewer
	// rows than the band: those padding rows carry nothing to compare.
	require.NotEmpty(t, rows)
	require.LessOrEqual(t, len(rows), band)

	for index, row := range rows {
		assert.Equal(t, strings.TrimRight(ansi.Strip(row), " "),
			strings.TrimRight(ansi.Strip(frame[index]), " "),
			"a hidden rail leaves row %d exactly as it was", index)
	}
}

func TestTurnRailPaintsOneMarkPerUserTurnAtTheFixedPitch(t *testing.T) {
	t.Parallel()

	model := turnRailModel(t, 3, 80, 30, true)
	glyphs := railGlyphs(model)

	require.Equal(t, 3, railMarks(model), "one mark per user turn: %q", glyphs)
	assert.Equal(t, 1, countGlyph(glyphs, turnRailActiveNoColor),
		"exactly one mark is the current user turn: %q", glyphs)

	rows := markedRows(model)
	assert.Equal(t, []int{0, 2, 4}, rows,
		"marks sit one row apart with a blank row between, from the window's top")

	frame := strings.Split(model.View().Content, "\n")
	for _, row := range rows {
		assert.Equal(t, model.width, ansi.StringWidth(frame[row]),
			"a mark occupies the frame's last cell and never a second one")
	}
}

func TestTurnRailSupportsAPitchOfOneMarkPerRow(t *testing.T) {
	t.Parallel()

	model := turnRailModel(t, 3, 80, 30, true)
	model.turnRail.pitch = 1
	model.rerenderTranscript(false)
	_ = model.View()

	assert.Equal(t, []int{0, 1, 2}, markedRows(model),
		"a pitch of one puts a mark on every row it can afford")
	assert.Equal(t, 3, railMarks(model), "the pitch changes spacing, not how many user turns are marked")
}

func TestTurnRailFollowsTheReadingPosition(t *testing.T) {
	t.Parallel()

	model := turnRailModel(t, 6, 80, 30, true)
	rows := markedRows(model)
	require.NotEmpty(t, rows)
	assert.Equal(t, rows[len(rows)-1], indexOfGlyph(railGlyphs(model), turnRailActiveNoColor),
		"following the newest rows makes the last user turn current")
}

func TestTurnRailHighlightsTheUserTurnAtTheTopOfTheWindow(t *testing.T) {
	t.Parallel()

	model := turnRailModel(t, 12, 80, 30, true)
	model.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	require.Zero(t, model.transcriptScroll.offset, "the fixture scrolled to the beginning")
	require.False(t, model.transcriptScroll.follow, "the fixture is long enough to leave the tail")

	glyphs := railGlyphs(model)
	assert.Equal(t, 0, indexOfGlyph(glyphs, turnRailActiveNoColor),
		"the user turn the window opens on is the current one: %q", glyphs)
	assert.Equal(t, 1, countGlyph(glyphs, turnRailActiveNoColor),
		"only one mark is current: %q", glyphs)
}

func TestTurnRailScrollsItsWindowInsteadOfMergingMarks(t *testing.T) {
	t.Parallel()

	long := turnRailModel(t, 30, 80, 30, true)
	capacity := railCapacity(long)
	require.GreaterOrEqual(t, capacity, 2)
	require.Less(t, capacity, 30, "the fixture needs more user turns than the rail holds")

	assert.Len(t, markedRows(long), capacity,
		"every slot holds one mark: the window scrolls rather than compressing")

	// The same geometry with exactly as many user turns as slots shows every one.
	exact := turnRailModel(t, capacity, 80, 30, true)
	require.Equal(t, capacity, railCapacity(exact))
	assert.Len(t, markedRows(exact), capacity, "a conversation that fits shows all of it")

	// Scrolling away from the tail moves the highlight without changing how many
	// marks the rail shows.
	long.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	assert.Len(t, markedRows(long), capacity, "the window keeps its size while it scrolls")
	assert.Equal(t, 0, indexOfGlyph(railGlyphs(long), turnRailActiveNoColor),
		"the rail follows the reading position back to the top")
}

func TestTurnRailWindowKeepsTheActiveMarkInItsComfortBand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		start     int
		active    int
		userTurns int
		capacity  int
		want      int
	}{
		{name: "fits", start: 0, active: 2, userTurns: 3, capacity: 6, want: 0},
		{name: "exactly fits", start: 0, active: 5, userTurns: 6, capacity: 6, want: 0},
		{name: "active in the middle does not move", start: 0, active: 2, userTurns: 12, capacity: 6, want: 0},
		{name: "active near the top stays put", start: 0, active: 0, userTurns: 12, capacity: 6, want: 0},
		{name: "active past the bottom band scrolls down", start: 0, active: 5, userTurns: 12, capacity: 6, want: 2},
		{name: "window clamps at the last window", start: 0, active: 11, userTurns: 12, capacity: 6, want: 6},
		{name: "window keeps the reader's position", start: 4, active: 6, userTurns: 12, capacity: 6, want: 4},
		{name: "a mark from the top edge scrolls up", start: 4, active: 5, userTurns: 12, capacity: 6, want: 3},
		{name: "window ahead of the active scrolls up", start: 8, active: 2, userTurns: 12, capacity: 6, want: 0},
		{name: "a stale start clamps to the last window", start: 99, active: 9, userTurns: 12, capacity: 6, want: 6},
		{name: "a small window keeps a one-mark band", start: 0, active: 9, userTurns: 12, capacity: 4, want: 7},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := turnRailWindowWithin(
				test.start, test.active, test.userTurns, test.capacity, turnRailComfort,
			)
			assert.Equal(t, test.want, got)
			assert.Equal(t, got,
				turnRailWindowWithin(got, test.active, test.userTurns, test.capacity, turnRailComfort),
				"a settled window must not move on the next frame")
		})
	}
}

func TestTurnRailWindowKeepsAFocusedMarkMerelyVisible(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		start     int
		active    int
		userTurns int
		capacity  int
		want      int
	}{
		{name: "a mark inside the window keeps it", start: 4, active: 5, userTurns: 12, capacity: 6, want: 4},
		{name: "a mark at the last slot keeps it", start: 4, active: 9, userTurns: 12, capacity: 6, want: 4},
		{name: "a mark past the window brings it along", start: 4, active: 10, userTurns: 12, capacity: 6, want: 5},
		{name: "a mark before the window brings it back", start: 4, active: 3, userTurns: 12, capacity: 6, want: 3},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := turnRailWindowWithin(test.start, test.active, test.userTurns, test.capacity, 0)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestTurnRailCapacityHoldsOneMarkPerPitchSlot(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 8, turnRailCapacity(8, 1), "one mark per row")
	assert.Equal(t, 4, turnRailCapacity(8, 2), "a mark row and a blank row")
	assert.Equal(t, 4, turnRailCapacity(9, 2), "a partial slot does not hold a mark")
	assert.Equal(t, 0, turnRailCapacity(0, 2))
	assert.Equal(t, 0, turnRailCapacity(8, 0))
}

func TestTurnRailNeedsTheManagedViewport(t *testing.T) {
	t.Parallel()

	t.Run("inline mode hands scrolling to the terminal", func(t *testing.T) {
		t.Parallel()

		model := readyModelWithController(t, stubController{state: turnRailConversation(6)}, true)
		model.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
		model.rerenderTranscript(true)

		// Inline mode hands the settled history to the terminal and keeps only the
		// mutable tail, so there is no store band to compare against here; the
		// rail's gate is the property under test.
		assert.Zero(t, railMarks(model),
			"a frame the terminal scrolls has no managed viewport to report")
	})

	t.Run("the escape hatch gives the screen to the terminal", func(t *testing.T) {
		t.Parallel()

		model := turnRailModel(t, 6, 80, 30, true)
		model.Update(ctrlO())
		require.True(t, model.transcriptMode.active)

		assert.Zero(t, railMarks(model))
	})
}

func TestTurnRailNeedsRoomAndMoreThanOneUserTurn(t *testing.T) {
	t.Parallel()

	t.Run("a single user turn says nothing", func(t *testing.T) {
		t.Parallel()

		model := turnRailModel(t, 1, 80, 30, true)
		assert.Zero(t, railMarks(model), "a single mark would say nothing")
		assertBandUnpainted(t, model)
	})

	t.Run("a frame with no margin keeps its width", func(t *testing.T) {
		t.Parallel()

		model := turnRailModel(t, 6, composerBoxMinWidth-1, 30, true)
		assert.Zero(t, railMarks(model), "a frame with no margin keeps its cells")
		assertBandUnpainted(t, model)
	})

	t.Run("a short viewport keeps its rows", func(t *testing.T) {
		t.Parallel()

		model := turnRailModel(t, 6, 80, 12, true)
		require.Less(t, model.transcriptScroll.height, turnRailMinRows,
			"the fixture has to be shorter than the rail's minimum")

		assert.Zero(t, railMarks(model), "a short band keeps its rows")
		assertBandUnpainted(t, model)
	})
}

func TestTurnRailLeavesTheTextColumnAlone(t *testing.T) {
	t.Parallel()

	model := turnRailModel(t, 6, 80, 30, false)
	frame := strings.Split(model.View().Content, "\n")
	band := min(model.frameHit.transcript, len(frame))
	rows := model.transcript.rowsIn(model.transcriptScroll.offset,
		model.transcriptScroll.offset+band)
	require.Len(t, rows, band)

	for index, row := range rows {
		assert.Equal(t,
			strings.TrimRight(ansi.Strip(row), " "),
			strings.TrimRight(ansi.Strip(ansi.Truncate(frame[index], model.railColumn(), "")), " "),
			"row %d keeps its text cells", index)
	}
}

func TestTurnRailDistinguishesTheCurrentUserTurnByShape(t *testing.T) {
	t.Parallel()

	model := turnRailModel(t, 3, 80, 30, true)
	glyphs := railGlyphs(model)
	assert.Equal(t, 1, countGlyph(glyphs, turnRailActiveNoColor))
	assert.Equal(t, 2, countGlyph(glyphs, turnRailRestNoColor))

	for row := range strings.SplitSeq(model.View().Content, "\n") {
		cell := ansi.Cut(row, model.railColumn(), model.railColumn()+1)
		assert.NotContains(t, cell, "\x1b", "NO_COLOR paints no styling")
	}
}

func TestTurnRailColoursTheMarks(t *testing.T) {
	t.Parallel()

	model := turnRailModel(t, 3, 80, 30, false)
	cells := railCells(model)
	rows := markedRows(model)
	require.Len(t, rows, 3)

	palette := paletteFor(model.theme)
	rest := lipgloss.NewStyle().Foreground(palette.separator).Render(turnRailRest)
	active := lipgloss.NewStyle().Foreground(palette.session).Render(turnRailActive)

	// A cell carries the row's own styling around the mark, so the assertion is
	// that the mark's rendering is the one inside it — not that the cell equals it.
	assert.Contains(t, cells[rows[0]], rest, "a rest mark uses the dim border colour")
	assert.Contains(t, cells[rows[1]], rest)
	assert.Contains(t, cells[rows[2]], active,
		"the current mark uses the primary label colour")
	assert.NotContains(t, cells[rows[2]], rest,
		"the current mark is not painted like the rest")
}

func TestTurnRailDimsTheScrollableEnd(t *testing.T) {
	t.Parallel()

	model := turnRailModel(t, 30, 80, 30, false)
	turns := model.transcript.userTurns()
	capacity := railCapacity(model)
	require.Positive(t, capacity)
	active := capacity + 2
	require.Greater(t, len(turns), active+2, "the fixture needs user turns below the active one")

	model.transcriptScroll.scrollTo(turns[active].start)

	cells := railCells(model)
	require.Positive(t, model.turnRail.start, "the window had to scroll to follow the active mark")
	rows := markedRows(model)
	require.Greater(t, len(rows), 4)

	palette := paletteFor(model.theme)
	rest := lipgloss.NewStyle().Foreground(palette.separator).Render(turnRailRest)
	faded := lipgloss.NewStyle().Foreground(palette.separator).Faint(true).Render(turnRailRest)
	activeCell := lipgloss.NewStyle().Foreground(palette.session).Render(turnRailActive)
	fadedActive := lipgloss.NewStyle().Foreground(palette.session).Faint(true).Render(turnRailActive)

	assert.Contains(t, cells[rows[0]], faded, "the end that can scroll back is dimmed one step")
	assert.Contains(t, cells[rows[len(rows)-1]], faded, "the end that can scroll on is dimmed one step")
	assert.Contains(t, cells[rows[1]], rest, "a mark inside the band keeps the rest colour")
	assert.NotContains(t, cells[rows[1]], "\x1b[2;", "a mark inside the band is not dimmed")

	glyphs := railGlyphs(model)
	require.Equal(t, 1, countGlyph(glyphs, turnRailActive))
	activeRow := indexOfGlyph(glyphs, turnRailActive)
	assert.Contains(t, cells[activeRow], activeCell,
		"the current mark keeps the primary label colour")
	assert.NotContains(t, cells[activeRow], fadedActive,
		"the current mark is never dimmed, whichever end the window sits at")
}

func TestDragSelectionOverTheRailCopiesProseOnly(t *testing.T) {
	t.Parallel()

	model := turnRailModel(t, 6, 60, 30, false)
	calls := &[]TextSaveRequest{}
	model.options.SaveText = captureSaver(calls)
	require.NotEmpty(t, markedRows(model), "the rail is on the frame")

	// The drag starts at the frame's left edge and ends at its right edge, so the
	// rail column is inside the cells the reader addressed.
	last := model.frameHit.transcript - 1
	model.handleMouse(tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseLeft})
	model.handleMouse(tea.MouseMotionMsg{X: model.width, Y: last, Button: tea.MouseLeft})
	command := model.handleMouse(tea.MouseReleaseMsg{X: model.width, Y: last, Button: tea.MouseLeft})
	require.NotNil(t, command)

	texts := clipboardTexts(runCommandTree(t, command))
	require.NotEmpty(t, texts)
	copied := strings.Join(texts, "\n")

	assert.Contains(t, copied, "TURN-1", "the drag copies the prose it addressed")
	assert.NotContains(t, copied, turnRailRest)
	assert.NotContains(t, copied, turnRailActive)

	for line := range strings.SplitSeq(copied, "\n") {
		assert.Equal(t, strings.TrimRight(line, " "), line,
			"the padding that carries the rail is not text the reader addressed")
	}
}

func TestTurnRailReusesTheUserTurnListAcrossFrames(t *testing.T) {
	t.Parallel()

	model := turnRailModel(t, 12, 80, 30, true)
	require.NotEmpty(t, markedRows(model), "the rail is on the frame")

	// A rerender at the same geometry and theme rebuilds the records but not what
	// the frame describes, so the rail must not walk them again.
	model.transcript.resetRenders()
	model.rerenderTranscript(false)
	_ = model.View()

	assert.Zero(t, model.transcript.turnRebuilds,
		"a frame whose records did not change re-uses the cached user turn list")
	assert.NotEmpty(t, markedRows(model), "the rail is still painted")
}

// railClick delivers a left click in the rail column on one frame row.
func railClick(model *Model, row int) tea.Cmd {
	return model.handleMouse(tea.MouseClickMsg{X: model.railColumn(), Y: row, Button: tea.MouseLeft})
}

// markRowForTurn reports the frame row the pointer map says one user turn's mark
// sits on.
func markRowForTurn(model *Model, turn int) (int, bool) {
	for _, mark := range model.frameHit.turnRail.marks {
		if mark.turn == turn {
			return mark.row, true
		}
	}

	return 0, false
}

func TestTurnRailClickJumpsToTheUserTurn(t *testing.T) {
	t.Parallel()

	model := turnRailModel(t, 30, 80, 30, true)
	turns := model.transcript.userTurns()
	capacity := railCapacity(model)
	require.Greater(t, capacity, 2, "the fixture needs a scrolled rail window")

	rows := markedRows(model)
	require.Len(t, rows, capacity)

	// Following the tail shows the last window, so the first visible mark is the
	// window's first user turn.
	clicked := len(turns) - capacity

	require.NotNil(t, railClick(model, rows[0]))

	assert.Equal(t, turns[clicked].start, model.transcriptScroll.offset,
		"one click puts that user turn's first row at the top")
	assert.False(t, model.transcriptScroll.follow, "a jump leaves the tail")

	_ = model.View()
	top, _, _ := strings.Cut(ansi.Strip(model.View().Content), "\n")
	assert.Contains(t, top, "TURN-"+itoa(clicked+1),
		"the frame opens on the prompt the mark stands for")
	activeRow, ok := markRowForTurn(model, clicked)
	require.True(t, ok, "the clicked user turn still has a mark")
	assert.Equal(t, turnRailActiveNoColor, railGlyphs(model)[activeRow],
		"the clicked user turn is the current mark")
}

func TestTurnRailClickLandsEachEndInOneClick(t *testing.T) {
	t.Parallel()

	model := turnRailModel(t, 30, 80, 30, true)
	turns := model.transcript.userTurns()
	capacity := railCapacity(model)
	rows := markedRows(model)
	require.Greater(t, len(rows), 2)

	// Following the tail already puts the newest turn's first row at the top, so
	// that click cannot move the viewport. What it must do is answer as the rail —
	// not as a selection — and leave the newest turn current.
	newest := len(turns) - 1
	require.NotNil(t, railClick(model, rows[len(rows)-1]))
	assert.False(t, model.selection.visible, "the rail answered the click, not the transcript")
	assert.Equal(t, min(turns[newest].start, model.transcriptScroll.maxOffset()),
		model.transcriptScroll.offset, "the newest mark lands on the newest user turn")
	assert.True(t, model.transcriptScroll.atBottom(),
		"and the viewport is at the tail, where that turn is visible")

	_ = model.View()
	glyphs := railGlyphs(model)
	assert.Equal(t, turnRailActiveNoColor, glyphs[rows[len(rows)-1]],
		"the newest mark stays the current one")

	// The first visible mark is the oldest turn of the window; one click reaches it
	// even though the rail's own window has scrolled.
	rows = markedRows(model)
	require.Len(t, rows, capacity)
	require.NotNil(t, railClick(model, rows[0]))
	assert.Equal(t, turns[len(turns)-capacity].start, model.transcriptScroll.offset,
		"the first visible mark lands on its own user turn")
	assert.False(t, model.transcriptScroll.follow, "and the jump leaves the tail")
}

func TestTurnRailHitMapMatchesThePaintedMarks(t *testing.T) {
	t.Parallel()

	model := turnRailModel(t, 30, 80, 30, true)
	capacity := railCapacity(model)
	turns := model.transcript.userTurns()
	rows := markedRows(model)
	require.Len(t, rows, capacity)

	for slot, row := range rows {
		mark, ok := model.frameHit.turnRail.hitAt(row)
		require.True(t, ok, "row %d holds a mark", row)
		assert.Equal(t, len(turns)-capacity+slot, mark.turn,
			"mark %d stands for its own user turn", slot)
		assert.Equal(t, turns[mark.turn].start, mark.startRow,
			"and carries that turn's first row as the frame painted it")
	}

	_, ok := model.frameHit.turnRail.hitAt(rows[0] + 1)
	assert.False(t, ok, "the blank row between two marks answers nothing")

	// The window scrolls to the beginning: the map has to follow it rather than
	// keep answering the window it was built for.
	model.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	_ = model.View()

	rows = markedRows(model)
	require.Len(t, rows, capacity)

	for slot, row := range rows {
		mark, ok := model.frameHit.turnRail.hitAt(row)
		require.True(t, ok)
		assert.Equal(t, slot, mark.turn, "from the beginning the window starts at turn zero")
	}
}

func TestTurnRailClickOnABlankRowDoesNothing(t *testing.T) {
	t.Parallel()

	model := turnRailModel(t, 30, 80, 30, true)
	rows := markedRows(model)
	require.NotEmpty(t, rows)
	blank := rows[0] + 1
	require.Less(t, blank, model.frameHit.transcript, "the fixture needs a blank rail row")

	_, painted := model.frameHit.turnRail.hitAt(blank)
	require.False(t, painted, "the row between two marks carries no mark")

	before := model.transcriptScroll.offset
	assert.Nil(t, railClick(model, blank), "a blank rail row answers nothing")
	assert.Equal(t, before, model.transcriptScroll.offset)
	assert.False(t, model.selection.visible, "and it does not start a selection")
}

func TestTurnRailClickIsNotATranscriptClick(t *testing.T) {
	t.Parallel()

	model := turnRailModel(t, 12, 80, 30, true)
	rows := markedRows(model)
	require.NotEmpty(t, rows)

	press := model.handleMouse(tea.MouseClickMsg{X: model.railColumn(), Y: rows[0], Button: tea.MouseLeft})
	require.NotNil(t, press)
	assert.False(t, model.selection.visible, "the press is the rail's, not a selection")
	assert.False(t, model.selection.dragging)

	release := model.handleMouse(tea.MouseReleaseMsg{X: model.railColumn(), Y: rows[0], Button: tea.MouseLeft})
	assert.Nil(t, release, "the release has no selection to finish")
	assert.False(t, model.selection.visible, "the rail owns its column, not the transcript")
	assert.Equal(t, routeNone, model.route.kind, "and no entry under it was activated")
}

func TestTurnRailClickNeedsAPaintedRail(t *testing.T) {
	t.Parallel()

	t.Run("inline mode never paints one", func(t *testing.T) {
		t.Parallel()

		model := readyModelWithController(t, stubController{state: turnRailConversation(12)}, true)
		model.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
		model.rerenderTranscript(true)
		_ = model.View()
		require.True(t, model.frameHit.painted, "the fixture has to paint a frame")
		assert.False(t, model.turnRailShown(turnRailMinRows),
			"inline mode has no managed viewport to raise a rail in")

		before := model.transcriptScroll.offset
		assert.Nil(t, railClick(model, 0), "there is no rail to answer")
		assert.Equal(t, before, model.transcriptScroll.offset)
		assert.False(t, model.selection.visible)
	})

	t.Run("the escape hatch answers none", func(t *testing.T) {
		t.Parallel()

		model := turnRailModel(t, 30, 80, 30, true)
		model.Update(ctrlO())
		require.True(t, model.transcriptMode.active)

		before := model.transcriptScroll.offset
		assert.Nil(t, railClick(model, 0), "the terminal owns the screen and its clicks")
		assert.Equal(t, before, model.transcriptScroll.offset)
	})

	t.Run("a narrow frame paints none", func(t *testing.T) {
		t.Parallel()

		model := turnRailModel(t, 30, 80, 30, true)
		require.True(t, model.frameHit.turnRail.painted)

		model.Update(tea.WindowSizeMsg{Width: composerBoxMinWidth - 1, Height: 30})
		_ = model.View()
		require.False(t, model.frameHit.turnRail.painted, "the narrow frame paints no rail")

		before := model.transcriptScroll.offset
		railClick(model, 0)
		assert.Equal(t, before, model.transcriptScroll.offset, "no rail jump is answered")
		assert.True(t, model.selection.visible,
			"the margin is the transcript's again, so the click selects")
	})
}

// frameRowIndex reports the frame row containing a text, or -1.
func frameRowIndex(model *Model, text string) int {
	rows := strings.Split(ansi.Strip(model.View().Content), "\n")
	for index, row := range rows {
		if strings.Contains(row, text) {
			return index
		}
	}

	return -1
}

// findFrameRow returns the first frame row containing a text, as the reader sees
// it.
func findFrameRow(model *Model, text string) (string, bool) {
	for row := range strings.SplitSeq(ansi.Strip(model.View().Content), "\n") {
		if strings.Contains(row, text) {
			return row, true
		}
	}

	return "", false
}

func TestTurnRailStepKeysWalkTheUserTurns(t *testing.T) {
	t.Parallel()

	model := turnRailModel(t, 12, 80, 30, true)
	turns := model.transcript.userTurns()
	model.Update(tea.KeyPressMsg{Code: tea.KeyHome})

	model.Update(tea.KeyPressMsg{Text: "]"})
	assert.Equal(t, 1, model.turnRail.focus.turn, "the next turn is focused")
	assert.Equal(t, turns[1].start, model.transcriptScroll.offset,
		"and its first row comes to the top")

	model.Update(tea.KeyPressMsg{Text: "]"})
	assert.Equal(t, 2, model.turnRail.focus.turn)
	assert.Equal(t, turns[2].start, model.transcriptScroll.offset)

	model.Update(tea.KeyPressMsg{Text: "["})
	assert.Equal(t, 1, model.turnRail.focus.turn, "the previous key walks back")
	assert.Equal(t, turns[1].start, model.transcriptScroll.offset)

	assert.Empty(t, model.composer.Value(), "the step never typed into the draft")
	assert.True(t, model.composer.Focused(), "and the composer keeps the caret")
}

func TestTurnRailStepKeysTypeIntoADraft(t *testing.T) {
	t.Parallel()

	model := turnRailModel(t, 12, 80, 30, true)
	before := model.transcriptScroll.offset
	model.composer.InsertString("draft")

	model.Update(tea.KeyPressMsg{Text: "["})
	model.Update(tea.KeyPressMsg{Text: "]"})

	assert.Equal(t, "draft[]", model.composer.Value())
	assert.Equal(t, before, model.transcriptScroll.offset, "the keys belonged to the draft")
	assert.False(t, model.turnRail.focus.on)
}

func TestTurnRailStepKeysClampAtTheEnds(t *testing.T) {
	t.Parallel()

	model := turnRailModel(t, 12, 80, 30, true)
	turns := model.transcript.userTurns()

	model.Update(tea.KeyPressMsg{Code: tea.KeyHome})

	for range len(turns) + 2 {
		model.Update(tea.KeyPressMsg{Text: "["})
	}

	assert.Equal(t, 0, model.turnRail.focus.turn, "stepping past the first turn stops there")
	assert.Equal(t, turns[0].start, model.transcriptScroll.offset)

	for range len(turns) + 2 {
		model.Update(tea.KeyPressMsg{Text: "]"})
	}

	assert.Equal(t, len(turns)-1, model.turnRail.focus.turn, "stepping past the last turn stops there")
	assert.True(t, model.transcriptScroll.atBottom())
}

func TestTurnRailAltKeysStep(t *testing.T) {
	t.Parallel()

	model := turnRailModel(t, 12, 80, 30, true)
	turns := model.transcript.userTurns()
	last := len(turns) - 1

	model.Update(tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModAlt})
	assert.Equal(t, last-1, model.turnRail.focus.turn, "alt+up steps back")
	assert.Equal(t, min(turns[last-1].start, model.transcriptScroll.maxOffset()),
		model.transcriptScroll.offset, "and takes the viewport as far up as it goes")

	model.Update(tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModAlt})
	assert.Equal(t, last, model.turnRail.focus.turn, "alt+down steps on")
}

func TestTurnRailPreviewNamesTheFocusedTurn(t *testing.T) {
	t.Parallel()

	model := turnRailModel(t, 12, 80, 30, true)
	model.Update(tea.KeyPressMsg{Text: "["})
	_ = model.View()

	assert.Contains(t, ansi.Strip(model.View().Content), "turn 11/12 · TURN-11")

	// Scrolling some other way ends the focus, so the band stops naming a turn the
	// reader has moved away from.
	model.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	_ = model.View()

	assert.False(t, model.turnRail.focus.on)
	assert.NotContains(t, ansi.Strip(model.View().Content), "turn 11/12")
}

func TestTurnRailBandKeepsTheScrollHintWithoutFocus(t *testing.T) {
	t.Parallel()

	model := turnRailModel(t, 12, 80, 30, true)
	model.Update(tea.KeyPressMsg{Text: "["})
	require.True(t, model.turnRail.focus.on)

	// A scroll key that is not a rail step ends the focus; the band is the scroll
	// hint again, not a preview.
	model.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	_ = model.View()

	assert.False(t, model.turnRail.focus.on)
	assert.NotContains(t, ansi.Strip(model.View().Content), "turn ")
	_, hinted := findFrameRow(model, bandScrollIcon)
	assert.True(t, hinted, "the band is the return-to-newest hint again")
}

func TestTurnRailPreviewSharesTheBandWithTheCopyNotice(t *testing.T) {
	t.Parallel()

	model := turnRailModel(t, 12, 60, 30, true)
	model.Update(tea.KeyPressMsg{Text: "["})
	model.copiedNotice = "copied 3 lines"
	_ = model.View()

	row, ok := findFrameRow(model, "turn 11/12")
	require.True(t, ok, "the band names the focused turn")
	assert.Contains(t, row, "turn 11/12", "the preview keeps the left of the row")
	assert.Contains(t, row, "copied 3 lines", "the confirmation keeps the right edge")
}

func TestTurnRailBracketsTypeWhenThereIsNoRail(t *testing.T) {
	t.Parallel()

	model := turnRailModel(t, 1, 80, 30, true)
	require.Zero(t, railMarks(model), "a single prompt draws no rail")

	model.Update(tea.KeyPressMsg{Text: "["})
	model.Update(tea.KeyPressMsg{Text: "]"})

	assert.Equal(t, "[]", model.composer.Value(),
		"with nothing to step through, a bracket is an ordinary character")
	assert.False(t, model.turnRail.focus.on)
}

func TestTurnRailPreviewOwnsTheBandRow(t *testing.T) {
	t.Parallel()

	model := turnRailModel(t, 12, 80, 30, true)
	model.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	model.Update(tea.KeyPressMsg{Text: "]"})
	_ = model.View()

	require.True(t, model.turnRail.focus.on)
	require.NotEmpty(t, model.bandScrollHint(), "the fixture is scrolled away from the tail")
	assert.Equal(t, -1, model.frameHit.band,
		"the frame drew no arrow, so the band affords no return click")

	row := frameRowIndex(model, "turn 2/12")
	require.GreaterOrEqual(t, row, 0, "the band names the focused turn")

	before := model.transcriptScroll.offset
	assert.Nil(t, model.handleMouse(tea.MouseClickMsg{X: 10, Y: row, Button: tea.MouseLeft}))
	assert.Equal(t, before, model.transcriptScroll.offset, "the click did not return to the newest rows")
}

func TestTurnRailCopyNoticeWinsATightBand(t *testing.T) {
	t.Parallel()

	model := turnRailModel(t, 12, composerBoxMinWidth, 30, true)
	model.Update(tea.KeyPressMsg{Text: "["})
	model.copiedNotice = "copied 3 lines"
	_ = model.View()

	row, ok := findFrameRow(model, "copied 3 lines")
	require.True(t, ok, "the confirmation keeps the row")
	assert.NotContains(t, row, "turn 11/12", "the preview yields the cells it cannot have")
}

func TestTurnRailFocusHoldsTheWindow(t *testing.T) {
	t.Parallel()

	model := turnRailModel(t, 30, 80, 30, true)
	model.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	_ = model.View()

	capacity := railCapacity(model)
	require.Greater(t, capacity, 2)

	start := model.turnRail.start
	require.Zero(t, start, "the fixture's window is at the beginning")

	// Walk the focus to the window's last slot. The ordinary window keeps two
	// marks of margin and would slide here; a focus session must not.
	for range capacity - 1 {
		model.Update(tea.KeyPressMsg{Text: "]"})
	}

	_ = model.View()
	assert.Equal(t, start, model.turnRail.start,
		"a focused step must not slide the window under the reader")

	focused := model.turnRail.focus.turn
	row, ok := markRowForTurn(model, focused)
	require.True(t, ok, "the focused turn has a mark in the window")
	assert.Equal(t, turnRailActiveNoColor, railGlyphs(model)[row],
		"and that mark is the one the rail calls current")
}

func TestTurnRailKeysAreInTheHelpPanel(t *testing.T) {
	t.Parallel()

	model := turnRailModel(t, 12, 80, 30, true)
	page := pageLinesText(model.helpRoutePageLines(helpTabShortcuts))

	assert.Contains(t, page, "[ / ]")
	assert.Contains(t, page, "alt+up")
}
