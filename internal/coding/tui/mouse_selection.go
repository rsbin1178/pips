//nolint:wsl_v5 // Selection geometry and its bounds checks stay adjacent.
package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Capturing the mouse is what lets a wheel notch scroll the managed viewport, and
// it is also what stops the terminal from doing its own drag selection. So the
// viewport implements the gesture: press, drag and release copies the selected
// rows through the same clipboard path `/copy` uses. The terminal's own selection
// stays one key away — `Ctrl+R` releases the capture, the way the reference
// agent's scrollback toggle does — and `Shift` still bypasses the capture in the
// terminal itself.

// selectionPoint is one caret position in the transcript: a flattened row and a
// display column inside it.
type selectionPoint struct {
	row    int
	column int
}

// selectionState is the drag selection over the transcript region.
type selectionState struct {
	anchor selectionPoint
	focus  selectionPoint
	// dragging is true between the press and the release.
	dragging bool
	// visible is true while the selection is painted.
	visible bool
}

// selectionRedrawMsg asks for one more frame after a pointer gesture changed the
// selection. Mouse handling happens outside Update, and the renderer only
// repaints when a message comes back from the handler.
type selectionRedrawMsg struct{}

// selectionStyle paints the selected cells. It is the same reverse-video
// treatment the find box gives a match — both mean "this text is addressed" — and
// NO_COLOR drops both.
var selectionStyle = lipgloss.NewStyle().Reverse(true)

// redrawSelection returns the no-op command that makes the renderer paint the
// gesture's result.
func redrawSelection() tea.Cmd {
	return func() tea.Msg { return selectionRedrawMsg{} }
}

// handleSelectionMouse routes a pointer gesture that is not a wheel notch. A
// gesture it does not own returns no command, which is also what tells the
// renderer that nothing needs repainting.
func (m *Model) handleSelectionMouse(message tea.MouseMsg) tea.Cmd {
	switch message := message.(type) {
	case tea.MouseClickMsg:
		if message.Button != tea.MouseLeft {
			return nil
		}

		row, ok := m.entryRowAt(message.Y)
		if !ok {
			return nil
		}

		point := selectionPoint{row: row, column: max(0, message.X)}
		m.selection = selectionState{anchor: point, focus: point, dragging: true, visible: true}

		return redrawSelection()
	case tea.MouseMotionMsg:
		if !m.selection.dragging {
			return nil
		}

		m.selection.focus = selectionPoint{row: m.selectionRowAt(message.Y), column: max(0, message.X)}

		return redrawSelection()
	case tea.MouseReleaseMsg:
		if !m.selection.dragging || message.Button != tea.MouseLeft {
			return nil
		}

		return m.finishSelection()
	default:
		return nil
	}
}

// selectionRowAt converts a frame row into a transcript row for a drag, clamping
// a pointer that left the transcript band to its nearest row so a drag that runs
// off the top or bottom still extends the selection.
func (m *Model) selectionRowAt(frameRow int) int {
	if m.frameHit.transcript <= 0 {
		return m.transcriptScroll.offset
	}

	frameRow = min(max(frameRow, 0), m.frameHit.transcript-1)

	return m.transcriptScroll.offset + m.frameHit.topDropped + frameRow
}

// finishSelection ends a drag: a press and release on one cell is a click and
// activates the entry, anything longer is a selection and is copied.
func (m *Model) finishSelection() tea.Cmd {
	selection := m.selection
	m.selection.dragging = false

	if selection.anchor == selection.focus {
		m.selection = selectionState{}

		return m.activateEntry(selection.anchor.row)
	}

	text := m.selectionText()
	if strings.TrimSpace(text) == "" {
		m.selection = selectionState{}

		return redrawSelection()
	}

	return tea.Batch(m.saveText(TextKindCopy, "", text, true), redrawSelection())
}

// clearSelection drops the highlight, which is what Escape and a capture change
// do.
func (m *Model) clearSelection() {
	m.selection = selectionState{}
}

// orderedSelection returns the drag in reading order, so a selection made
// backwards copies the same text as one made forwards.
func (m *Model) orderedSelection() (selectionPoint, selectionPoint) {
	anchor, focus := m.selection.anchor, m.selection.focus
	if anchor.row > focus.row || (anchor.row == focus.row && anchor.column > focus.column) {
		return focus, anchor
	}

	return anchor, focus
}

// selectionSpan returns the display-cell range one transcript row contributes to
// the selection. Rows between the ends are selected whole; a row outside the
// selection contributes nothing.
func selectionSpan(row int, start, end selectionPoint, width int) (int, int) {
	switch {
	case row < start.row || row > end.row:
		return 0, 0
	case start.row == end.row:
		return min(start.column, width), min(end.column, width)
	case row == start.row:
		return min(start.column, width), width
	case row == end.row:
		return 0, min(end.column, width)
	default:
		return 0, width
	}
}

// selectionText extracts the selected rows as plain text, without styling or the
// padding the renderer adds to a full-width row.
//
// Rows keep their own line breaks, which is what the terminal's own selection
// produces: a paragraph that wrapped is copied as the lines it was painted as.
// The durable, reflow-free copies remain `/copy` and `/export`, and a reader who
// wants the terminal's own reflow can release the capture with Ctrl+R.
func (m *Model) selectionText() string {
	if !m.selection.visible {
		return ""
	}

	start, end := m.orderedSelection()

	rows := m.transcript.rowsIn(start.row, end.row+1)
	if len(rows) == 0 {
		return ""
	}

	lines := make([]string, 0, len(rows))
	for offset, row := range rows {
		line := ansi.Strip(row)

		left, right := selectionSpan(start.row+offset, start, end, ansi.StringWidth(line))
		if right > left {
			line = ansi.Cut(line, left, right)
		} else {
			line = ""
		}

		lines = append(lines, strings.TrimRight(line, " \t"))
	}

	// Trailing blank rows are the frame's padding, not text the reader addressed.
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// highlightSelectionWindow paints the selection over the window's rows. It never
// changes a row's display width, because the hit map and the cursor were measured
// against the frame that is about to be painted.
func (m *Model) highlightSelectionWindow(visible string) string {
	if visible == "" || !m.selection.visible || m.options.NoColor {
		return visible
	}

	start, end := m.orderedSelection()
	rows := strings.Split(visible, "\n")
	first := m.transcriptScroll.offset

	for offset := range rows {
		row := first + offset
		if row < start.row || row > end.row {
			continue
		}

		left, right := selectionSpan(row, start, end, ansi.StringWidth(rows[offset]))
		if right <= left {
			continue
		}

		rows[offset] = highlightColumns(rows[offset], left, right, selectionStyle)
	}

	return strings.Join(rows, "\n")
}

// toggleMouseCapture releases the terminal's mouse capture, or takes it back.
// While it is released the terminal does its own selection, copy and link
// handling; the viewport receives no wheel, click or drag until it is captured
// again. The choice is per session, like the reference agent's scrollback toggle,
// and `[tui] mouse = false` remains the persisted opt-out.
func (m *Model) toggleMouseCapture() tea.Cmd {
	m.mouseCaptureOff = !m.mouseCaptureOff
	m.clearSelection()
	m.setLayout()

	if m.mouseCaptureOff {
		return m.setStatusNotice("mouse released · Ctrl+R captures again · select and copy natively")
	}

	return m.setStatusNotice("mouse captured · wheel and click work again")
}
