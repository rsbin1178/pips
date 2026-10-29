package tui

import (
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The turn rail is the managed viewport's position indicator: one mark per user
// turn, drawn down the right margin's outermost column. It answers what a
// scrollbar cannot: how many user turns the conversation has, and where the
// reader sits among them.
//
// The rail is drawn into cells that are blank today. Conversation rows render at
// width-2*inset inside a two-cell inset, so the rail column costs the text column
// nothing: wrapping is unchanged and timelineInset is untouched. Rows are padded
// to the rail column before the glyph is written, so a blank or short row still
// has a cell to replace.
//
// A mark is a user turn — the same unit rowOfPreviousUserTurn walks — and never
// stands for two. When the user turns do not all fit, the window scrolls instead
// of compressing, so a click can always be exact. That policy is transplanted
// from DeepSeek Harness's web TurnNavigator; only the policy, never the pixels.
const (
	// turnRailRest is a user turn the reader is not on, and turnRailActive the
	// one at the reading position. Both pairs differ in shape as well as colour,
	// so the current mark survives NO_COLOR.
	turnRailRest          = "▁"
	turnRailActive        = "▊"
	turnRailRestNoColor   = "·"
	turnRailActiveNoColor = "█"
	// turnRailPitch is how many rows one mark occupies by default: the mark row
	// plus a blank row, which reads as a dashed ladder instead of a continuous
	// line. A pitch of 1 — one mark per row — is the other defensible value; see
	// [turnRailState.markPitch].
	turnRailPitch = 2
	// turnRailMinRows is the shortest transcript band that still affords a rail.
	// Below it the rail would spend more rows on navigation than on conversation.
	turnRailMinRows = 8
	// turnRailComfort is how many marks from either end the active mark may sit
	// before the window scrolls back over it. The reference rail scrolls the
	// active mark into view only when it would leave an equally padded band, which
	// is why the window moves as little as possible rather than recentring.
	turnRailComfort = 2
)

// turnRailState is the rail's window between frames: the first user turn index it
// shows. It is deliberately not part of the transcript store, because the rail
// scrolls independently of the reading position it reports.
type turnRailState struct {
	start int
	// pitch is how many rows one mark occupies. Zero selects turnRailPitch, which
	// is the shipped frame; the geometry keeps the spacing a parameter because a
	// mark per row and a mark every other row read very differently.
	pitch int
	// marks is the scratch the frame's pointer map is recorded into, so a frame
	// does not allocate for the rail. The map itself belongs to the frame's.
	marks []turnRailHit
}

// markPitch reports the row stride one mark occupies.
func (s turnRailState) markPitch() int {
	if s.pitch <= 0 {
		return turnRailPitch
	}

	return s.pitch
}

// turnRailHit is one painted mark: the frame row it sits on, the user turn it
// stands for, and that turn's first transcript row as the frame was painted. The
// row travels with the mark rather than being looked up at click time, so a click
// lands where the mark said it would even if the store changed underneath it.
type turnRailHit struct {
	row      int
	turn     int
	startRow int
}

// turnRailHits is the rail's pointer map, recorded from the frame that painted
// the marks rather than re-decided at click time: every painted mark answers for
// its own user turn, in the cell the frame actually used.
type turnRailHits struct {
	painted bool
	column  int
	marks   []turnRailHit
}

// hitAt resolves a frame row to the mark the rail painted there. A row the rail
// did not paint — the blank row between two marks, or below the last one —
// answers ok false.
func (hits turnRailHits) hitAt(row int) (turnRailHit, bool) {
	if !hits.painted {
		return turnRailHit{}, false
	}

	for _, mark := range hits.marks {
		if mark.row == row {
			return mark, true
		}
	}

	return turnRailHit{}, false
}

// turnRailShown reports whether this frame has the cells and the rows for a rail.
// Only a managed viewport with a margin has cells to spare. The caller drops a
// conversation with fewer than two user turns once the turn list is known: a
// single mark is a position with no alternative, and the list is not worth
// collecting to learn that.
func (m *Model) turnRailShown(rows int) bool {
	return m.fullscreen() && m.readyViewOwnsPointer() &&
		m.width >= composerBoxMinWidth && rows >= turnRailMinRows
}

// turnRailCapacity reports how many marks a band of rows holds at one pitch.
func turnRailCapacity(rows, pitch int) int {
	if rows <= 0 || pitch <= 0 {
		return 0
	}

	return rows / pitch
}

// turnRailWindow returns the first user turn a rail window shows. The active mark
// is kept inside a comfort band measured from each end; only when it would leave
// that band does the window move, and then only far enough to restore the margin.
// A mark in the middle of the window therefore never drags the rail, and a
// settled window is a fixed point of this function.
func turnRailWindow(start, active, userTurns, capacity int) int {
	if capacity <= 0 || userTurns <= capacity {
		return 0
	}

	start = min(max(0, start), userTurns-capacity)
	comfort := min(turnRailComfort, (capacity-1)/2)

	if offset := active - start; offset < comfort {
		start = active - comfort
	} else if offset > capacity-1-comfort {
		start = active - (capacity - 1 - comfort)
	}

	return min(max(0, start), userTurns-capacity)
}

// turnRailActiveIndex reports which user turn contains the viewport's top row.
// While the reader follows the newest row the last user turn is current, so the
// tail reads as a position rather than as an absence.
func (m *Model) turnRailActiveIndex(starts []int) int {
	last := len(starts) - 1
	if last < 0 {
		return 0
	}

	if m.transcriptScroll.follow {
		return last
	}

	top := m.transcriptScroll.offset
	turn := sort.Search(len(starts), func(index int) bool { return starts[index] > top }) - 1

	return min(max(0, turn), last)
}

// turnRailFrame is the rail one frame paints: the user turns it can show, the
// window it shows them from, and the active one among them.
type turnRailFrame struct {
	starts   []int
	start    int
	active   int
	capacity int
	pitch    int
}

// frameRail resolves the rail for a frame with bandRows transcript rows, scrolling
// the window so the active mark stays inside its comfort band. ok is false when
// this frame affords no rail.
func (m *Model) frameRail(bandRows int) (turnRailFrame, bool) {
	if bandRows <= 0 || !m.turnRailShown(bandRows) {
		return turnRailFrame{}, false
	}

	starts := m.transcript.userTurns()
	if len(starts) < 2 {
		return turnRailFrame{}, false
	}

	pitch := m.turnRail.markPitch()
	capacity := turnRailCapacity(bandRows, pitch)

	if capacity < 2 {
		return turnRailFrame{}, false
	}

	active := m.turnRailActiveIndex(starts)
	m.turnRail.start = turnRailWindow(m.turnRail.start, active, len(starts), capacity)

	return turnRailFrame{
		starts: starts, start: m.turnRail.start, active: active, capacity: capacity, pitch: pitch,
	}, true
}

// isFaded reports whether one mark sits at a scrollable end of the window and is
// therefore dimmed one step further. The current mark never fades.
func (rail turnRailFrame) isFaded(turn int) bool {
	if turn == rail.active {
		return false
	}

	if turn == rail.start && rail.start > 0 {
		return true
	}

	return turn == rail.start+rail.capacity-1 && rail.start+rail.capacity < len(rail.starts)
}

// drawTurnRail paints the rail into the right margin of a composed frame and
// hands back that frame's own pointer map. bandRows is how many transcript rows
// the frame shows: the rail decorates those and nothing else, so a click on the
// composer or the status line never reaches it.
//
// A hidden frame is returned untouched, so losing the rail restores the frame
// exactly and costs nothing: the frame is only split, and the user turn list only
// consulted, once the cheap gates have passed.
func (m *Model) drawTurnRail(frame string, bandRows int) (string, turnRailHits) {
	rail, ok := m.frameRail(bandRows)
	if !ok || frame == "" {
		return frame, turnRailHits{}
	}

	rows := strings.Split(frame, "\n")
	if bandRows > len(rows) {
		bandRows = len(rows)
	}

	column := m.railColumn()
	marks := m.turnRail.marks[:0]

	for slot := range rail.capacity {
		turn := rail.start + slot
		row := slot * rail.pitch

		if turn >= len(rail.starts) || row >= bandRows {
			break
		}

		glyph := m.turnRailGlyph(turn == rail.active, rail.isFaded(turn))
		rows[row] = paintRailCell(rows[row], glyph, column)
		marks = append(marks, turnRailHit{row: row, turn: turn, startRow: rail.starts[turn]})
	}

	m.turnRail.marks = marks

	return strings.Join(rows, "\n"), turnRailHits{painted: true, column: column, marks: marks}
}

// turnRailMouse answers a left click in the rail column from the pointer map the
// last painted frame recorded. A click on a mark jumps to that user turn; a click
// on a blank row inside the transcript band is swallowed, because the column is
// the rail's and not the entry underneath it. Everything else — another button,
// another column, a row outside the band, or a frame that painted no rail —
// belongs to whatever the pointer found there.
func (m *Model) turnRailMouse(message tea.MouseMsg) (tea.Cmd, bool) {
	hits := m.frameHit.turnRail
	if !hits.painted || !m.readyViewOwnsPointer() {
		return nil, false
	}

	click, ok := leftClick(message)
	if !ok || click.X != hits.column || click.Y < 0 || click.Y >= m.frameHit.transcript {
		return nil, false
	}

	mark, ok := hits.hitAt(click.Y)
	if !ok {
		return nil, true
	}

	return m.scrollToRow(mark.startRow), true
}

// turnRailGlyph renders one mark. A faded mark is one step dimmer than the rest
// state, which is the terminal's stand-in for the scrollable-end fade band.
func (m *Model) turnRailGlyph(active, faded bool) string {
	if m.options.NoColor {
		if active {
			return turnRailActiveNoColor
		}

		return turnRailRestNoColor
	}

	glyph, colour := turnRailRest, paletteFor(m.theme).separator
	if active {
		glyph, colour = turnRailActive, paletteFor(m.theme).session
	}

	style := lipgloss.NewStyle().Foreground(colour)
	if faded {
		style = style.Faint(true)
	}

	return style.Render(glyph)
}

// railColumn reports the frame column the rail occupies: the outermost cell of
// the right margin.
func (m *Model) railColumn() int {
	return max(0, m.width-1)
}

// paintRailCell replaces one cell of a row with a glyph, padding the row to the
// column first so a blank or short row still has a cell to replace.
func paintRailCell(row, cell string, column int) string {
	if width := ansi.StringWidth(row); width < column {
		row += strings.Repeat(" ", column-width)
	}

	return ansi.Truncate(row, column, "") + cell + ansi.TruncateLeft(row, column+1, "")
}
