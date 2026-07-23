//nolint:wsl_v5 // Picker state transitions and row composition stay locally visible.
package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/modelcatalog"
	"github.com/rsbin/pips/internal/coding/session"
)

const sessionPickerSearchPrompt = "⌕ "

type sessionPickerState struct {
	open          bool
	generation    uint64
	search        textinput.Model
	cursor        int
	sessions      []session.Metadata
	previousInput string
	openedAt      time.Time
	err           error
	loading       bool
	controlling   bool
}

type sessionPickerDataMsg struct {
	generation uint64
	sessions   []session.Metadata
	err        error
}

func newSessionPickerState(previousInput string, theme colorTheme, noColor bool) sessionPickerState {
	search := textinput.New()
	search.Prompt = sessionPickerSearchPrompt
	search.Placeholder = "Search…"
	search.SetVirtualCursor(false)
	search.SetStyles(sessionSearchStyles(theme, noColor))
	search.Focus()

	return sessionPickerState{
		open:          true,
		search:        search,
		previousInput: previousInput,
		openedAt:      time.Now(),
	}
}

func (m *Model) openSessionPicker(previousInput string) tea.Cmd {
	m.sessionPickerSeq++
	m.sessionPicker = newSessionPickerState(previousInput, m.theme, m.options.NoColor)
	m.sessionPicker.generation = m.sessionPickerSeq
	m.sessionPicker.loading = true
	m.composer.Reset()
	m.setLayout()
	generation := m.sessionPicker.generation

	load := func() tea.Msg {
		values, err := m.controller.ListSessions(m.ctx)

		return sessionPickerDataMsg{
			generation: generation,
			sessions:   values,
			err:        err,
		}
	}

	return tea.Batch(m.sessionPicker.search.Focus(), load)
}

func (m *Model) closeSessionPicker(restoreInput bool) {
	previousInput := m.sessionPicker.previousInput
	m.sessionPicker = sessionPickerState{}
	if restoreInput {
		m.composer.SetValue(previousInput)
	} else {
		m.composer.Reset()
	}
	m.composer.Focus()
	m.setLayout()
}

func (m *Model) updateSessionPickerKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.sessionPicker.controlling {
		return m, nil
	}

	key := message.String()
	if key == keyEscape || key == keyCtrlC {
		m.closeSessionPicker(true)

		return m, nil
	}

	values := m.filteredSessionPickerValues()
	switch key {
	case "up", "ctrl+p":
		m.sessionPicker.cursor = wrapIndex(m.sessionPicker.cursor-1, len(values))
	case keyDown, keyTab, "ctrl+n":
		m.sessionPicker.cursor = wrapIndex(m.sessionPicker.cursor+1, len(values))
	case keyEnter:
		if len(values) == 0 || m.sessionPicker.loading || m.state.Phase != coding.PhaseIdle {
			return m, nil
		}

		return m, m.runControl(
			operationResume,
			values[m.sessionPicker.cursor].ID,
			modelcatalog.Selection{},
		)
	default:
		before := m.sessionPicker.search.Value()
		var command tea.Cmd
		m.sessionPicker.search, command = m.sessionPicker.search.Update(message)
		if m.sessionPicker.search.Value() != before {
			m.sessionPicker.cursor = 0
			m.sessionPicker.err = nil
		}

		return m, command
	}

	return m, nil
}

func (m *Model) filteredSessionPickerValues() []session.Metadata {
	query := strings.ToLower(strings.TrimSpace(m.sessionPicker.search.Value()))
	filtered := make([]session.Metadata, 0, len(m.sessionPicker.sessions))
	for _, value := range m.sessionPicker.sessions {
		created := strings.ToLower(value.CreatedAt.Local().Format(time.DateTime))
		matches := query == "" ||
			strings.Contains(strings.ToLower(value.ID), query) ||
			strings.Contains(created, query) ||
			strings.Contains(strings.ToLower(value.ParentSessionID), query) ||
			strings.Contains(strings.ToLower(value.Name), query) ||
			strings.Contains(strings.ToLower(value.Preview), query) ||
			strings.Contains(strings.ToLower(value.CurrentLeafID), query)
		if matches {
			filtered = append(filtered, value)
		}
	}

	return filtered
}

func (m *Model) sessionPickerView() tea.View {
	content, searchX, searchY := m.sessionPickerContent()
	view := tea.NewView(content)
	view.AltScreen = false
	view.MouseMode = tea.MouseModeNone
	view.WindowTitle = appTitle
	view.Cursor = m.sessionPicker.search.Cursor()
	if view.Cursor != nil {
		view.Cursor.X += searchX
		view.Cursor.Y += searchY
		if view.Cursor.X >= max(1, m.width) || view.Cursor.Y >= max(1, m.height) {
			view.Cursor = nil
		}
	}

	return view
}

func (m *Model) sessionPickerContent() (string, int, int) {
	width := max(1, m.width)
	height := max(1, m.height)
	invocation := renderUserMessage("/resume", width, m.theme, m.options.NoColor)
	separator := m.sessionPickerSeparator(width)
	title := "Resume session"
	if !m.options.NoColor {
		title = lipgloss.NewStyle().Bold(true).Foreground(paletteFor(m.theme).session).Render(title)
	}
	search := m.sessionPickerSearchBox(width)
	prefix := []string{invocation, "", separator, "", title, "", search}
	searchY := lipgloss.Height(lipgloss.JoinVertical(lipgloss.Left, prefix[:len(prefix)-1]...)) + 1
	searchX := 2
	footer := sessionPickerFooter(width)
	if !m.options.NoColor {
		footer = lipgloss.NewStyle().Foreground(paletteFor(m.theme).muted).Render(footer)
	}
	footer = ansi.Truncate(footer, width, "…")

	fixedHeight := lipgloss.Height(lipgloss.JoinVertical(lipgloss.Left, prefix...)) + 3
	available := max(1, height-fixedHeight)
	list := m.sessionPickerList(available)
	parts := make([]string, 0, len(prefix)+4)
	parts = append(parts, prefix...)
	parts = append(parts, "", list, "", footer)
	content := lipgloss.JoinVertical(lipgloss.Left, parts...)

	return truncateHeight(content, height), searchX, searchY
}

func sessionPickerFooter(width int) string {
	if width < 44 {
		return "↑/↓ · Enter · Esc cancel"
	}
	if width < 70 {
		return "↑/↓ select · Enter resume · Esc cancel"
	}

	return "↑/↓ select · Enter resume · type to search · Esc cancel"
}

func (m *Model) sessionPickerSeparator(width int) string {
	value := strings.Repeat("─", max(1, width))
	if m.options.NoColor {
		return strings.Repeat("-", max(1, width))
	}

	return lipgloss.NewStyle().Foreground(paletteFor(m.theme).session).Render(value)
}

func (m *Model) sessionPickerSearchBox(width int) string {
	if width < 5 {
		return ansi.Truncate(m.sessionPicker.search.View(), width, "…")
	}

	innerWidth := max(1, width-4)
	border := lipgloss.RoundedBorder()
	style := lipgloss.NewStyle().Width(innerWidth).Padding(0, 1).Border(border, true)
	if m.options.NoColor {
		style = style.Border(lipgloss.NormalBorder(), true)
	} else {
		style = style.BorderForeground(paletteFor(m.theme).separator)
	}

	return style.Render(m.sessionPicker.search.View())
}

func (m *Model) sessionPickerList(maximum int) string {
	if m.sessionPicker.controlling {
		return m.styleSessionPickerNotice("Resuming session…", false)
	}
	if m.sessionPicker.loading {
		return m.styleSessionPickerNotice("Loading sessions…", false)
	}
	if m.sessionPicker.err != nil {
		return m.styleSessionPickerNotice("Error: "+safeError(m.sessionPicker.err), true)
	}

	values := m.filteredSessionPickerValues()
	if len(values) == 0 {
		return m.styleSessionPickerNotice("No matching sessions.", false)
	}

	rows := make([]string, len(values))
	heights := make([]int, len(values))
	for index, value := range values {
		rows[index] = m.renderSessionPickerRow(value, index == m.sessionPicker.cursor)
		heights[index] = lipgloss.Height(rows[index]) + 1
	}
	start, end := selectionWindowByHeight(heights, m.sessionPicker.cursor, maximum)
	visible := strings.Join(rows[start:end], "\n\n")

	return truncateHeight(visible, maximum)
}

func (m *Model) renderSessionPickerRow(value session.Metadata, selected bool) string {
	width := max(1, m.width)
	contentWidth := max(1, width-inputPromptWidth)
	title, identityLines := sessionPickerIdentity(value)
	titleLines := strings.Split(lipgloss.Wrap(title, contentWidth, ""), "\n")
	query := m.sessionPicker.search.Value()
	for index := range titleLines {
		prefix := strings.Repeat(" ", inputPromptWidth)
		if index == 0 && selected {
			prefix = "› "
		}
		titleLines[index] = prefix + titleLines[index]
	}

	lines := append([]string{}, titleLines...)
	for _, identity := range identityLines {
		for line := range strings.SplitSeq(lipgloss.Wrap(identity, contentWidth, ""), "\n") {
			lines = append(lines, strings.Repeat(" ", inputPromptWidth)+line)
		}
	}
	metadata := m.sessionPickerMetadata(value)
	for line := range strings.SplitSeq(lipgloss.Wrap(metadata, contentWidth, ""), "\n") {
		lines = append(lines, strings.Repeat(" ", inputPromptWidth)+line)
	}

	if m.options.NoColor {
		return strings.Join(lines, "\n")
	}

	palette := paletteFor(m.theme)
	matchStyle := lipgloss.NewStyle().Bold(true).Foreground(palette.model)
	for index := range lines {
		lines[index] = highlightCommandMatch(lines[index], query, matchStyle)
		if index < len(titleLines) && selected {
			lines[index] = lipgloss.NewStyle().Bold(true).Foreground(palette.session).Render(lines[index])
		} else if index >= len(titleLines) {
			lines[index] = lipgloss.NewStyle().Foreground(palette.muted).Render(lines[index])
		}
	}

	return strings.Join(lines, "\n")
}

func sessionPickerIdentity(value session.Metadata) (string, []string) {
	switch {
	case value.Preview != "" && value.Name != "":
		return value.Preview, []string{value.Name}
	case value.Preview != "":
		return value.Preview, nil
	case value.Name != "":
		return value.Name, nil
	default:
		return "Untitled session", []string{value.ID}
	}
}

func (m *Model) sessionPickerMetadata(value session.Metadata) string {
	parts := []string{relativeSessionTime(value.CreatedAt, m.sessionPicker.openedAt)}
	countSuffix := ""
	if value.Truncated {
		countSuffix = "+"
	}
	if value.NodeCount > 0 {
		parts = append(parts, pluralCount(value.NodeCount, countSuffix, "node"))
	}
	if value.BranchCount > 0 {
		parts = append(parts, pluralCount(value.BranchCount, countSuffix, "branch"))
	}
	if value.ParentSessionID != "" {
		lineage := "fork of " + shortDisplayID(value.ParentSessionID)
		if value.ParentEntryID != "" {
			lineage += " @ " + shortDisplayID(value.ParentEntryID)
		}
		parts = append(parts, lineage)
	}

	return strings.Join(parts, " · ")
}

func relativeSessionTime(createdAt, now time.Time) string {
	if createdAt.IsZero() {
		return "unknown time"
	}
	if now.IsZero() {
		now = time.Now()
	}
	duration := now.Sub(createdAt)
	if duration < time.Minute {
		return "just now"
	}
	if duration < time.Hour {
		return relativeCount(int(duration/time.Minute), "minute")
	}
	if duration < 24*time.Hour {
		return relativeCount(int(duration/time.Hour), "hour")
	}
	if duration < 7*24*time.Hour {
		return relativeCount(int(duration/(24*time.Hour)), "day")
	}

	return createdAt.Local().Format("Jan 2, 2006")
}

func relativeCount(value int, unit string) string {
	if value == 1 {
		return "1 " + unit + " ago"
	}

	return fmt.Sprintf("%d %ss ago", value, unit)
}

func pluralCount(value int, suffix, noun string) string {
	if value == 1 && suffix == "" {
		return "1 " + noun
	}

	plural := noun + "s"
	if noun == "branch" {
		plural = "branches"
	}

	return fmt.Sprintf("%d%s %s", value, suffix, plural)
}

func (m *Model) styleSessionPickerNotice(value string, failed bool) string {
	value = ansi.Truncate(value, max(1, m.width), "…")
	if m.options.NoColor {
		return value
	}

	color := paletteFor(m.theme).muted
	if failed {
		color = paletteFor(m.theme).error
	}

	return lipgloss.NewStyle().Foreground(color).Render(value)
}
