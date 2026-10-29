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
		if index == 0 && model.frameHit.turnIndicator {
			// The first row carries the walk-back arrow's overlay until it retires;
			// that cell is its business, not the rail's.
			continue
		}

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

			got := turnRailWindow(test.start, test.active, test.userTurns, test.capacity)
			assert.Equal(t, test.want, got)
			assert.Equal(t, got,
				turnRailWindow(got, test.active, test.userTurns, test.capacity),
				"a settled window must not move on the next frame")
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
		if index == 0 && model.frameHit.turnIndicator {
			// The first row carries the walk-back arrow's overlay until it retires;
			// that cell is its business, not the rail's.
			continue
		}

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
	starts := model.transcript.userTurns()
	capacity := railCapacity(model)
	require.Positive(t, capacity)
	active := capacity + 2
	require.Greater(t, len(starts), active+2, "the fixture needs user turns below the active one")

	model.transcriptScroll.scrollTo(starts[active])

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

	assert.Zero(t, model.transcript.userTurnScans,
		"a frame whose records did not change re-uses the cached user turn list")
	assert.NotEmpty(t, markedRows(model), "the rail is still painted")
}
