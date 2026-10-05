//nolint:wsl_v5 // Mode transitions and their paging bookkeeping stay adjacent.
package tui

import (
	"fmt"
	"io"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/internal/coding/config"
)

// The in-session transcript escape hatch. The fullscreen viewport owns the
// conversation, so the terminal's own wheel, selection and search have nothing
// to read. Transcript mode leaves the alternate screen, appends the rendered
// conversation to the terminal's main buffer page by page, and then offers the
// fullscreen view back. The terminal owns scrolling while the mode is active:
// the app deliberately captures neither the mouse nor the scroll keys there.

// transcriptPageRows bounds one write handed to the renderer. insertAbove already
// splits a payload by the physical rows available above the frame; the bound
// exists so a long conversation yields between pages and a Quit is honoured
// without waiting for the whole document.
const transcriptPageRows = 200

// transcriptModeState is the state of the in-session escape hatch.
type transcriptModeState struct {
	active bool
	// sequence invalidates page commands when the mode is left or re-entered, so
	// a stale page can never outlive its transaction.
	sequence uint64
	// page is the next page index to write; wrote is how many rows are handed
	// over already, for the progress hint.
	page  int
	wrote int
	// rows is the snapshot taken on entry. Taking it once keeps the printed
	// document stable even while a streaming reply keeps growing.
	rows []string
	// The saved reading position, restored when the fullscreen view returns.
	follow    bool
	hasAnchor bool
	anchor    anchor
	offset    int
}

// transcriptPageMsg requests the next page of the transcript write.
type transcriptPageMsg struct {
	sequence uint64
}

// toggleTranscriptMode enters the escape hatch, or leaves it when already active.
func (m *Model) toggleTranscriptMode() tea.Cmd {
	if m.transcriptMode.active {
		m.leaveTranscriptMode()

		return nil
	}

	return m.enterTranscriptMode()
}

// enterTranscriptMode snapshots the conversation and starts writing it to the
// terminal. Inline mode already handed history to the terminal, so entering the
// mode there would print the same rows twice.
func (m *Model) enterTranscriptMode() tea.Cmd {
	if !m.fullscreen() {
		return nil
	}

	anchor, row := m.transcript.captureAnchor(m.transcriptScroll.offset)
	m.transcriptMode = transcriptModeState{
		active:    true,
		sequence:  m.transcriptMode.sequence + 1,
		rows:      m.transcript.flatten(),
		follow:    m.transcriptScroll.follow,
		hasAnchor: row >= 0,
		anchor:    anchor,
		offset:    m.transcriptScroll.offset,
	}

	return m.transcriptPageCmd()
}

// leaveTranscriptMode abandons any pending page and returns to the fullscreen
// viewport at the reading position the user left.
func (m *Model) leaveTranscriptMode() {
	if !m.transcriptMode.active {
		return
	}

	mode := m.transcriptMode
	if mode.wrote > 0 {
		// The main buffer now holds the conversation, so the exit handoff must
		// not append the same rows a second time.
		m.transcriptHanded = true
	}
	m.transcriptMode = transcriptModeState{sequence: mode.sequence + 1}

	switch {
	case mode.follow:
		m.transcriptScroll.gotoBottom()
	case mode.hasAnchor:
		if row := m.transcript.resolveAnchor(mode.anchor); row >= 0 {
			m.transcriptScroll.scrollTo(row)

			break
		}

		m.transcriptScroll.scrollTo(mode.offset)
	default:
		m.transcriptScroll.scrollTo(mode.offset)
	}

	m.setLayout()
}

// transcriptPageCmd writes the next page and schedules the page after it. The
// loop skips pages that render as nothing so a run of blank rows cannot stall
// the transaction.
func (m *Model) transcriptPageCmd() tea.Cmd {
	if !m.transcriptMode.active {
		return nil
	}

	for m.transcriptMode.page*transcriptPageRows < len(m.transcriptMode.rows) {
		start := m.transcriptMode.page * transcriptPageRows
		m.transcriptMode.page++
		end := min(len(m.transcriptMode.rows), start+transcriptPageRows)
		m.transcriptMode.wrote = end
		body := strings.Join(m.transcriptMode.rows[start:end], "\n")
		if body == "" {
			continue
		}

		sequence := m.transcriptMode.sequence

		return tea.Sequence(
			tea.Println(body),
			func() tea.Msg { return transcriptPageMsg{sequence: sequence} },
		)
	}

	return nil
}

// transcriptModePending reports whether more pages of the snapshot remain to be
// written.
func (m *Model) transcriptModePending() bool {
	return m.transcriptMode.active &&
		m.transcriptMode.page*transcriptPageRows < len(m.transcriptMode.rows)
}

// updateTranscriptPage continues a paged write. A page whose sequence no longer
// matches belongs to a transaction the user has already left.
func (m *Model) updateTranscriptPage(message transcriptPageMsg) tea.Cmd {
	if !m.transcriptMode.active || message.sequence != m.transcriptMode.sequence {
		return nil
	}

	return m.transcriptPageCmd()
}

// updateTranscriptModeKey consumes every key while the escape hatch is open. The
// terminal owns scrolling, so the app only has to offer the way back and a quit.
func (m *Model) updateTranscriptModeKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch message.String() {
	case keyCtrlO, keyEscape:
		m.leaveTranscriptMode()
	case keyCtrlC:
		return m.cancelOrExit(m.actionContext())
	}

	return m, nil
}

// transcriptModeView is the compact frame kept at the bottom of the main buffer
// while the conversation is printed above it. It never uses the alternate screen
// or mouse capture, which is what gives the terminal its native scrolling back.
func (m *Model) transcriptModeView() tea.View {
	view := m.presentationView(clampFrame(m.transcriptModeHint(), m.height))
	view.Cursor = nil

	return view
}

// transcriptModeHint describes the transaction and the way out. It also reports
// pending input, because a prompt raised while the user reads would otherwise be
// invisible until they return.
func (m *Model) transcriptModeHint() string {
	total := len(m.transcriptMode.rows)
	hint := ""
	switch {
	case m.transcriptMode.page*transcriptPageRows < total:
		hint = fmt.Sprintf(
			"Writing transcript · %d%% · Esc returns",
			m.transcriptMode.wrote*100/max(1, total),
		)
	case total == 0:
		hint = "Transcript · no history yet · Esc returns"
	default:
		hint = fmt.Sprintf("Transcript · %d rows · Esc returns · Ctrl+C quits", total)
	}

	// The way out is written before the optional suffix so a narrow terminal
	// truncates the suffix, never the escape.
	if m.prompt.kind != promptNone {
		hint += " · input waiting"
	}

	return ansi.Truncate(hint, max(1, m.width), "…")
}

// transcriptExitRows returns the rows to hand to the terminal after a normal
// exit, or nil when the exit should leave the main screen alone.
//
// A fullscreen session owns the conversation and the alternate screen is
// discarded on exit, so the rows have to be written out or they are lost.
// Inline mode already wrote them, and a session that ended while the escape
// hatch was open has them in the main buffer already.
func (m *Model) transcriptExitRows() []string {
	if m.transcriptMode.active || m.transcriptHanded {
		return nil
	}
	if m.presentationSnapshot.screen != ScreenFullscreen {
		return nil
	}
	if m.presentationSnapshot.exit != config.ExitOutputTranscript {
		return nil
	}

	return m.transcript.flatten()
}

// writeExitTranscript appends the conversation to the main screen after the
// terminal has been restored. It writes in bounded pages rather than building the
// whole document, so a long session does not materialise twice.
func writeExitTranscript(output io.Writer, rows []string) error {
	if output == nil || len(rows) == 0 {
		return nil
	}

	for start := 0; start < len(rows); start += transcriptPageRows {
		end := min(len(rows), start+transcriptPageRows)
		page := strings.Join(rows[start:end], "\n") + "\n"
		written, err := io.WriteString(output, page)
		if err != nil {
			return err
		}
		if written != len(page) {
			return io.ErrShortWrite
		}
	}

	return nil
}
