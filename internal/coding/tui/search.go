//nolint:wsl_v5 // Search state transitions stay adjacent to their row math.
package tui

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// executeFindCommand opens the find box from the command picker. Inline mode has
// no managed viewport to search, so it says so instead of opening a box over a
// tail the terminal already owns.
func (m *Model) executeFindCommand(arguments string) (tea.Model, tea.Cmd) {
	if !m.fullscreen() {
		m.picker.err = errors.New(
			"the terminal owns the conversation in inline mode; use its own search",
		)

		return m, nil
	}

	previous := m.picker.previousComposer
	m.closeCommandPicker(false)
	m.restoreCommandComposer(previous)
	m.openSearch(strings.TrimSpace(arguments))

	return m, nil
}

// searchContextRows is how many rows stay visible above the current match, so a
// jump never lands on a row whose meaning is off screen.
const searchContextRows = 2

// searchState is the find box over the managed viewport: the query, the rows that
// match it, and the match the reader is on. The box takes the composer's band
// while it is open, so the frame height does not change when it appears.
type searchState struct {
	active bool
	input  textinput.Model
	// matches are flattened transcript rows containing the query, ascending.
	matches []int
	// scanned is the store revision matches were computed from, so an unchanged
	// frame does not rescan the conversation.
	scanned uint64
	// index is the reader's position in matches, or -1 before the first jump.
	index int
	// anchor keeps the current match across a resize, a reflow or a prepended
	// history page, exactly like the reading position itself.
	anchor anchor
}

// openSearch starts the find box with an optional initial query. A non-empty
// query jumps to its first match immediately, so `/find foo` behaves like typing
// the query and pressing Enter.
func (m *Model) openSearch(query string) {
	m.search = searchState{
		active: true,
		input:  newSearchInput(m.theme, m.options.NoColor),
		index:  -1,
	}
	m.search.input.SetValue(query)
	m.search.input.CursorEnd()
	m.refreshSearch()
	if m.searchQuery() != "" {
		m.stepMatch(1)
	}
	m.setLayout()
}

// searchOwnsKeys reports whether the find box is the surface the ready view's keys
// reach. A route, prompt, picker, inline Team route or focused Team panel that
// takes the keyboard over hides the box rather than showing a query nothing can
// type into; the state survives, so the box comes back when they are done.
func (m *Model) searchOwnsKeys() bool {
	return m.search.active && m.route.kind == routeNone &&
		m.prompt.kind == promptNone && m.picker.kind == pickerNone &&
		!m.teamRouteIsInline() && !m.teamPanel.isFocused
}

// closeSearch leaves the reading position where the search put it: the reader
// asked to look at a match, so closing must not move them again.
func (m *Model) closeSearch() {
	m.search.active = false
	m.search.matches = nil
	m.search.index = -1
	m.search.anchor = anchor{}
	m.search.input.Blur()
	m.setLayout()
}

func newSearchInput(theme colorTheme, noColor bool) textinput.Model {
	input := textinput.New()
	input.Prompt = routeSearchPrompt
	// The placeholder is the only place the keys are taught at the moment they are
	// needed: it shows while the query is empty, in the band the box already takes.
	input.Placeholder = "Find in conversation… (Enter next · Esc closes)"
	input.SetVirtualCursor(false)
	input.SetStyles(sessionSearchStyles(theme, noColor))
	input.Focus()

	return input
}

// searchQuery is the normalized query every match comparison uses.
func (m *Model) searchQuery() string {
	return strings.ToLower(strings.TrimSpace(m.search.input.Value()))
}

// refreshSearch recomputes the matches for the current query and re-resolves the
// current match by identity, so a reflow or a prepended page cannot leave the
// counter pointing at a different row. It never scrolls: the reader moves only
// when they ask to.
func (m *Model) refreshSearch() {
	if !m.search.active {
		return
	}

	m.search.matches = m.transcript.searchRows(m.searchQuery())
	m.search.scanned = m.transcript.revision

	if len(m.search.matches) == 0 {
		m.search.index = -1
		m.search.anchor = anchor{}

		return
	}
	if m.search.anchor.recordID == "" {
		m.search.index = -1

		return
	}

	row := m.transcript.resolveAnchor(m.search.anchor)
	if row < 0 {
		m.search.index = -1
		m.search.anchor = anchor{}

		return
	}

	m.search.index = nearestMatch(m.search.matches, row)
}

// stepMatch moves the reader to the next or previous match, wrapping at both
// ends. The first step starts from the reader's position rather than from the end
// of the list, so a reader halfway down the conversation does not land at the top.
func (m *Model) stepMatch(delta int) {
	if len(m.search.matches) == 0 {
		return
	}

	if m.search.index < 0 {
		if delta < 0 {
			m.selectMatch(previousMatch(m.search.matches, m.transcriptScroll.offset))

			return
		}

		m.selectMatch(nearestMatch(m.search.matches, m.transcriptScroll.offset))

		return
	}

	m.selectMatch(wrapIndex(m.search.index+delta, len(m.search.matches)))
}

func (m *Model) selectMatch(index int) {
	if index < 0 || index >= len(m.search.matches) {
		return
	}

	row := m.search.matches[index]
	m.search.index = index
	m.search.anchor, _ = m.transcript.captureAnchor(row)
	m.transcriptScroll.scrollTo(max(0, row-searchContextRows))
}

// matchRows returns the flattened rows containing the query, ascending. Matching
// is case-insensitive over the row's visible text, which is what the terminal's
// own search does and what a reader can see.
func matchRows(rows []string, query string) []int {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return nil
	}

	matches := make([]int, 0, 8)
	for index, row := range rows {
		if rowMatches(row, query) {
			matches = append(matches, index)
		}
	}

	return matches
}

// nearestMatch returns the index of the first match at or after row, wrapping to
// the first match when row is past the last one.
func nearestMatch(matches []int, row int) int {
	for index, match := range matches {
		if match >= row {
			return index
		}
	}

	return 0
}

// previousMatch returns the index of the last match at or before row, wrapping to
// the last match when row is above the first one.
func previousMatch(matches []int, row int) int {
	for index, match := range slices.Backward(matches) {
		if match <= row {
			return index
		}
	}

	return len(matches) - 1
}

// matchColumns locates the query inside a rendered row and reports the visual
// columns it occupies, so the highlight is spliced by display width rather than
// by byte or rune offset.
func matchColumns(row, query string) (int, int, bool) {
	if query == "" {
		return 0, 0, false
	}

	plain := ansi.Strip(row)
	index := strings.Index(strings.ToLower(plain), query)
	if index < 0 {
		return 0, 0, false
	}

	start := ansi.StringWidth(plain[:index])

	return start, start + ansi.StringWidth(plain[index:index+len(query)]), true
}

// highlightColumns wraps a visual column range in style, preserving the escape
// sequences around it so the row's own styling survives the splice.
func highlightColumns(value string, start, end int, style lipgloss.Style) string {
	if end <= start {
		return value
	}

	return ansi.Cut(value, 0, start) +
		style.Render(ansi.Cut(value, start, end)) +
		ansi.Cut(value, end, ansi.StringWidth(value))
}

// highlightSearchWindow marks every visible match, emphasizing the current one.
// It never changes a row's display width, so the frame geometry the hit map and
// the cursor were measured against stays valid.
func (m *Model) highlightSearchWindow(visible string) string {
	if visible == "" || len(m.search.matches) == 0 || m.options.NoColor {
		return visible
	}

	query := m.searchQuery()
	if query == "" {
		return visible
	}

	rows := strings.Split(visible, "\n")
	first := m.transcriptScroll.offset

	for offset := range rows {
		row := first + offset
		start, end, ok := matchColumns(rows[offset], query)
		if !ok {
			continue
		}

		style := searchMatchStyle
		if m.search.index >= 0 && m.search.matches[m.search.index] == row {
			style = searchCurrentMatchStyle
		}

		rows[offset] = highlightColumns(rows[offset], start, end, style)
	}

	return strings.Join(rows, "\n")
}

var (
	// searchMatchStyle marks a match without depending on the theme's palette, and
	// reads correctly over both colored and plain rows.
	searchMatchStyle        = lipgloss.NewStyle().Reverse(true)
	searchCurrentMatchStyle = lipgloss.NewStyle().Reverse(true).Bold(true)
)

// searchStatusText reports the find box's state on the status line, which is where
// a reader looks while the composer's band is showing the query.
func (m *Model) searchStatusText() string {
	if m.searchQuery() == "" {
		return "find · type to search"
	}
	if len(m.search.matches) == 0 {
		return "find · no match"
	}
	if m.search.index < 0 {
		return fmt.Sprintf("find · %d matches", len(m.search.matches))
	}

	return fmt.Sprintf("find · %d/%d", m.search.index+1, len(m.search.matches))
}

// updateSearchKey keeps every key the find box owns in one place. It is a reading
// surface: the query editor keeps its own editing keys, the viewport keeps the
// scrolling keys, and the composer never sees either.
func (m *Model) updateSearchKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch message.String() {
	case keyEscape:
		m.closeSearch()

		return m, nil
	case keyCtrlC:
		return m.cancelOrExit(m.actionContext())
	case keyEnter:
		m.stepMatch(1)

		return m, nil
	case keyShiftEnter:
		m.stepMatch(-1)

		return m, nil
	case "up":
		m.transcriptScroll.scrollBy(-1)

		return m, nil
	case keyDown:
		m.transcriptScroll.scrollBy(1)

		return m, nil
	case keyPageUp, keyPageDown:
		if command, handled := m.updateTranscriptScrollKey(message); handled {
			return m, command
		}

		return m, nil
	}

	previous := m.search.input.Value()

	var command tea.Cmd

	m.search.input, command = m.search.input.Update(message)
	if m.search.input.Value() != previous {
		m.refreshSearch()
	}
	m.setLayout()

	return m, command
}

// updateSearchPaste feeds a paste into the query box, because the composer is not
// on screen while the find box is open.
func (m *Model) updateSearchPaste(message tea.PasteMsg) tea.Cmd {
	previous := m.search.input.Value()

	var command tea.Cmd

	m.search.input, command = m.search.input.Update(message)
	if m.search.input.Value() != previous {
		m.refreshSearch()
	}
	m.setLayout()

	return command
}
