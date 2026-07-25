//nolint:wsl_v5 // Picker state transitions and row composition stay locally visible.
package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/modelcatalog"
	"github.com/rsbin/pips/internal/coding/session"
)

type sessionPickerDataMsg struct {
	generation uint64
	sessions   []session.Metadata
	err        error
}

func newSessionPickerState(previousInput string, theme colorTheme, noColor bool) routeState {
	return routeState{
		kind:          routeSessions,
		search:        newRouteSearch(theme, noColor),
		previousInput: previousInput,
		openedAt:      time.Now(),
	}
}

func (m *Model) openSessionPicker(previousInput string) tea.Cmd {
	return m.requestRouteOpen(routeOpenRequest{
		kind: routeSessions, previousInput: previousInput,
	})
}

func (m *Model) activateSessionPicker(previousInput string) tea.Cmd {
	m.routeSeq++
	m.route = newSessionPickerState(previousInput, m.theme, m.options.NoColor)
	m.route.generation = m.routeSeq
	m.route.loading = true
	m.composer.Reset()
	m.setLayout()
	generation := m.route.generation

	load := func() tea.Msg {
		values, err := m.controller.ListSessions(m.ctx)

		return sessionPickerDataMsg{
			generation: generation,
			sessions:   values,
			err:        err,
		}
	}

	return tea.Batch(m.route.search.Focus(), load)
}

func (m *Model) closeSessionPicker(restoreInput bool) tea.Cmd {
	m.dismissSessionPicker(restoreInput)

	return tea.Sequence(m.commitStableTimeline(), m.composer.Focus())
}

func (m *Model) dismissSessionPicker(restoreInput bool) {
	previousInput := m.route.previousInput
	m.route = routeState{}
	if restoreInput {
		m.composer.SetValue(previousInput)
	} else {
		m.composer.Reset()
	}
	m.setLayout()
}

func (m *Model) updateSessionPickerKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.route.kind != routeSessions || m.route.controlling {
		return m, nil
	}

	key := message.String()
	if key == keyEscape || key == keyCtrlC {
		return m, m.closeSessionPicker(true)
	}

	values := m.filteredSessionPickerValues()
	switch key {
	case "up", "ctrl+p":
		m.route.cursor = wrapIndex(m.route.cursor-1, len(values))
	case keyDown, keyTab, "ctrl+n":
		m.route.cursor = wrapIndex(m.route.cursor+1, len(values))
	case keyEnter:
		if len(values) == 0 || m.route.loading || m.state.Phase != coding.PhaseIdle {
			return m, nil
		}

		return m, m.runControl(
			operationResume,
			values[m.route.cursor].ID,
			modelcatalog.Selection{},
		)
	default:
		before := m.route.search.Value()
		var command tea.Cmd
		m.route.search, command = m.route.search.Update(message)
		if m.route.search.Value() != before {
			m.route.cursor = 0
			m.route.err = nil
		}

		return m, command
	}

	return m, nil
}

func (m *Model) filteredSessionPickerValues() []session.Metadata {
	query := strings.ToLower(strings.TrimSpace(m.route.search.Value()))
	filtered := make([]session.Metadata, 0, len(m.route.sessions))
	for _, value := range m.route.sessions {
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

	return m.searchableRouteView(content, searchX, searchY)
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
	search, searchX, searchInnerY := m.routeSearchBox(width)
	prefix := []string{invocation, "", separator, "", title, "", search}
	listPadding := true
	if height < 18 {
		prefix = []string{invocation, separator, title, search}
		listPadding = false
	}
	searchY := lipgloss.Height(lipgloss.JoinVertical(
		lipgloss.Left,
		prefix[:len(prefix)-1]...,
	)) + searchInnerY
	footer := sessionPickerFooter(width)
	if !m.options.NoColor {
		footer = lipgloss.NewStyle().Foreground(paletteFor(m.theme).muted).Render(footer)
	}
	footer = ansi.Truncate(footer, width, "…")

	paddingHeight := 3
	if !listPadding {
		paddingHeight = 0
	}
	fixedHeight := lipgloss.Height(lipgloss.JoinVertical(lipgloss.Left, prefix...)) + paddingHeight
	available := max(1, height-fixedHeight)
	list := m.sessionPickerList(available)
	parts := make([]string, 0, len(prefix)+4)
	parts = append(parts, prefix...)
	if listPadding {
		parts = append(parts, "", list, "", footer)
	} else {
		parts = append(parts, list, footer)
	}
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

func (m *Model) sessionPickerList(maximum int) string {
	if m.route.controlling {
		return m.styleSessionPickerNotice("Resuming session…", false)
	}
	if m.route.loading {
		return m.styleSessionPickerNotice("Loading sessions…", false)
	}
	if m.route.err != nil {
		return m.styleSessionPickerNotice("Error: "+safeError(m.route.err), true)
	}

	values := m.filteredSessionPickerValues()
	if len(values) == 0 {
		return m.styleSessionPickerNotice("No matching sessions.", false)
	}

	rows := make([]string, len(values))
	heights := make([]int, len(values))
	for index, value := range values {
		rows[index] = m.renderSessionPickerRow(value, index == m.route.cursor)
		heights[index] = lipgloss.Height(rows[index]) + 1
	}
	start, end := selectionWindowByHeight(heights, m.route.cursor, maximum)
	visible := strings.Join(rows[start:end], "\n\n")

	return truncateHeight(visible, maximum)
}

func (m *Model) renderSessionPickerRow(value session.Metadata, selected bool) string {
	width := max(1, m.width)
	contentWidth := max(1, width-inputPromptWidth)
	title, identityLines := sessionPickerIdentity(value)
	titleLines := strings.Split(lipgloss.Wrap(title, contentWidth, ""), "\n")
	query := m.route.search.Value()
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
	parts := []string{relativeSessionTime(value.CreatedAt, m.route.openedAt)}
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
