package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The turn indicator is the top counterpart of the reserved band. While there are
// rows above the window it draws a small up triangle in the transcript's first row;
// a left click puts the newest user entry above the window at its top, so repeated
// clicks walk back through the conversation one turn at a time. The band's own hint
// then offers the way forward again, so the two arrows bracket the reader.
//
// It is drawn into the first row rather than into a band of its own because there is
// no free row above the transcript: reserving one would take a row from the messages
// and, worse, would change the very window height that decides whether the indicator
// is needed, so showing it could hide it again on the next frame.
//
// The glyphs are solid on purpose: the hollow triangle is this UI's attention marker
// (an awaiting approval, a compacting context, the picker's polarity mark), so the
// scroll pair uses the filled pair to read as movement rather than as a warning.
const turnIndicatorIcon = "▲"

// turnIndicatorShown reports whether the up indicator belongs on this frame. It marks
// that the conversation continues above the window, so it stays for every turn the
// reader walks back through and only leaves once the window reaches the beginning of
// the conversation. Only the managed fullscreen viewport owns such a window: the
// inline layout hands history to the terminal and keeps only the tail, and the
// transcript escape hatch gives the screen to the terminal.
func (m *Model) turnIndicatorShown() bool {
	if !m.fullscreen() || m.transcriptMode.active || !m.readyViewOwnsPointer() {
		return false
	}

	return m.transcriptScroll.offset > 0
}

// turnIndicator reports the frame row the indicator is painted at and the columns it
// covers, or ok false while it is hidden.
func (m *Model) turnIndicator() (row, left, right int, ok bool) {
	if !m.turnIndicatorShown() {
		return 0, 0, 0, false
	}

	size := ansi.StringWidth(turnIndicatorIcon)
	left = max(0, (max(1, m.width)-size)/2)

	return 0, left, left + size, true
}

// drawTurnIndicator writes the indicator into the first row of a composed frame,
// centred, replacing the cells it covers. The row is padded to the frame width first,
// because the overlay needs a cell to replace even when the row it lands on is blank
// or short.
//
// shown is the decision the frame made, which the caller takes from the frame it
// composed: the indicator marks a row of the transcript, so a frame too short to show
// any transcript row draws none. Without that test the overlay would land on the
// reserved band's own row in a very short frame and cover the band's marker with its
// own.
func (m *Model) drawTurnIndicator(content string, shown bool) string {
	_, left, right, ok := m.turnIndicator()
	if !ok || !shown || content == "" {
		return content
	}

	newline := strings.Index(content, "\n")
	row, tail := content, ""

	if newline >= 0 {
		row, tail = content[:newline], content[newline:]
	}

	// The overlay needs a cell to replace, so a blank or short row is padded to the
	// span it covers first.
	if width := ansi.StringWidth(row); width < right {
		row += strings.Repeat(" ", right-width)
	}

	icon := turnIndicatorIcon
	if !m.options.NoColor {
		icon = lipgloss.NewStyle().Foreground(paletteFor(m.theme).muted).Render(icon)
	}

	return ansi.Truncate(row, left, "") + icon + ansi.TruncateLeft(row, right, "") + tail
}

// turnIndicatorMouse answers a left click on the indicator by walking back one turn.
// Any other button, and any column outside the indicator's own span, belongs to the
// transcript underneath it.
func (m *Model) turnIndicatorMouse(message tea.MouseMsg) (tea.Cmd, bool) {
	if !m.frameHit.turnIndicator {
		return nil, false
	}

	row, left, right, ok := m.turnIndicator()
	if !ok {
		return nil, false
	}

	click, isClick := message.(tea.MouseClickMsg)
	if !isClick || click.Button != tea.MouseLeft || click.Y != row ||
		click.X < left || click.X >= right {
		return nil, false
	}

	return m.jumpToPreviousTurn(), true
}

// jumpToPreviousTurn puts the newest user entry above the window at its top, so
// repeated clicks walk back through the conversation one turn at a time. A viewport
// whose remaining rows hold no user entry goes to the beginning of the conversation
// instead, which is the last place the indicator can take the reader.
func (m *Model) jumpToPreviousTurn() tea.Cmd {
	row, ok := m.transcript.rowOfPreviousUserTurn(m.transcriptScroll.offset)
	if !ok {
		row = 0
	}

	m.transcriptScroll.scrollTo(row)

	return redrawFrame()
}
