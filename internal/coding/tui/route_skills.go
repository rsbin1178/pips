//nolint:wsl_v5 // Route transitions and compact row rendering stay locally visible.
package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/internal/coding"
)

type skillsRouteDataMsg struct {
	generation uint64
	snapshot   coding.SkillSnapshot
	err        error
}

type skillToggleResultMsg struct {
	generation uint64
	id         coding.SkillID
	enabled    bool
	err        error
}

func (m *Model) openSkillsRoute(previousInput string) tea.Cmd {
	return m.openSkillsRouteSnapshot(plainComposerSnapshot(previousInput))
}

func (m *Model) openSkillsRouteSnapshot(previous composerSnapshot) tea.Cmd {
	return m.requestRouteOpen(routeOpenRequest{
		kind: routeSkills, previousInput: previous.display,
		previousComposer:    previous.clone(),
		hasPreviousComposer: true,
	})
}

func (m *Model) activateSkillsRoute(previousInput string) tea.Cmd {
	return m.activateSkillsRouteSnapshot(plainComposerSnapshot(previousInput))
}

func (m *Model) activateSkillsRouteSnapshot(previous composerSnapshot) tea.Cmd {
	activityWasVisible := m.activityClockVisible()
	m.routeSeq++
	m.route = routeState{
		kind:                routeSkills,
		generation:          m.routeSeq,
		loading:             true,
		search:              newRouteSearch(m.theme, m.options.NoColor),
		previousInput:       previous.display,
		previousComposer:    previous.clone(),
		hasPreviousComposer: true,
	}
	m.composer.Reset()
	m.setLayout()
	generation := m.route.generation

	load := func() tea.Msg {
		snapshot, err := m.controller.Skills(m.ctx)

		return skillsRouteDataMsg{
			generation: generation, snapshot: snapshot.Clone(), err: err,
		}
	}

	return tea.Batch(
		m.route.search.Focus(), load,
		m.startActivityClock(activityWasVisible),
	)
}

func (m *Model) updateSkillsRouteKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.route.kind != routeSkills {
		return m, nil
	}

	key := message.String()
	if key == keyEscape || key == keyCtrlC {
		previousInput := m.route.previousInput
		previousComposer := m.route.previousComposer
		m.route = routeState{}
		if err := m.composer.Restore(previousComposer); err != nil {
			m.composer.SetValue(previousInput)
			m.streamErr = err
		}
		m.setLayout()

		return m, tea.Sequence(m.commitStableTimeline(), m.composer.Focus())
	}
	if m.route.controlling {
		return m, nil
	}

	values := m.filteredSkillsRouteValues()
	switch key {
	case "up", "ctrl+p":
		m.route.cursor = wrapIndex(m.route.cursor-1, len(values))
	case keyDown, keyTab, "ctrl+n":
		m.route.cursor = wrapIndex(m.route.cursor+1, len(values))
	case "ctrl+d":
		m.route.showDetails = !m.route.showDetails
	case keyEnter, " ":
		if len(values) == 0 || m.route.loading || m.state.Phase != coding.PhaseIdle {
			return m, nil
		}

		selected := values[m.route.cursor]
		enabled := !selected.Enabled
		activityWasVisible := m.activityClockVisible()
		m.route.controlling = true
		m.route.err = nil
		generation := m.route.generation

		toggle := func() tea.Msg {
			err := m.controller.SetSkillEnabled(m.ctx, selected.ID, enabled)

			return skillToggleResultMsg{
				generation: generation, id: selected.ID, enabled: enabled, err: err,
			}
		}

		return m, tea.Batch(toggle, m.startActivityClock(activityWasVisible))
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

func (m *Model) filteredSkillsRouteValues() []coding.SkillSummary {
	query := strings.ToLower(strings.TrimSpace(m.route.search.Value()))
	filtered := make([]coding.SkillSummary, 0, len(m.route.skills))
	for _, skill := range m.route.skills {
		state := "disabled"
		if skill.Enabled {
			state = "enabled"
		}
		searchable := strings.ToLower(strings.Join([]string{
			skill.Name,
			skill.Description,
			string(skill.Source),
			skillSourceLabel(skill.Source),
			state,
		}, " "))
		if query == "" || strings.Contains(searchable, query) {
			filtered = append(filtered, skill)
		}
	}

	return filtered
}

func (m *Model) skillsRouteView() tea.View {
	content, searchX, searchY := m.skillsRouteContent()

	return m.searchableRouteView(content, searchX, searchY)
}

func (m *Model) skillsRouteContent() (string, int, int) {
	width := max(1, m.width)
	height := max(1, m.height)
	invocation := renderUserMessage("/skills", width, m.theme, m.options.NoColor)
	separator := m.sessionPickerSeparator(width)
	title := "Project Skills"
	if !m.options.NoColor {
		title = lipgloss.NewStyle().Bold(true).Foreground(
			paletteFor(m.theme).session,
		).Render(title)
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
	footer := m.skillsRouteFooter(width)
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
	list := m.skillsRouteList(available)
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

func (m *Model) skillsRouteList(maximum int) string {
	if m.route.loading {
		return m.styleSessionPickerNotice(m.activityNotice("Loading Skills…"), false)
	}
	if m.route.err != nil && len(m.route.skills) == 0 {
		return m.styleSessionPickerNotice("Error: "+safeError(m.route.err), true)
	}
	errorNotice := ""
	if m.route.err != nil {
		errorNotice = m.styleSessionPickerNotice("Error: "+safeError(m.route.err), true)
		maximum = max(1, maximum-lipgloss.Height(errorNotice)-1)
	}

	values := m.filteredSkillsRouteValues()
	if len(values) == 0 {
		return m.styleSessionPickerNotice("No matching Skills.", false)
	}

	detail := ""
	if m.route.showDetails {
		detail = m.skillsDiagnosticDetail(max(1, maximum/3))
		if detail != "" {
			maximum = max(1, maximum-lipgloss.Height(detail)-1)
		}
	}
	rows := make([]string, len(values))
	heights := make([]int, len(values))
	for index, skill := range values {
		rows[index] = m.renderSkillsRouteRow(skill, index == m.route.cursor)
		heights[index] = lipgloss.Height(rows[index]) + 1
	}
	start, end := selectionWindowByHeight(heights, m.route.cursor, maximum)
	visible := truncateHeight(strings.Join(rows[start:end], "\n\n"), maximum)
	if errorNotice != "" {
		visible = errorNotice + "\n" + visible
	}
	if detail != "" {
		visible += "\n" + detail
	}

	return visible
}

func (m *Model) renderSkillsRouteRow(skill coding.SkillSummary, selected bool) string {
	marker := "  "
	if selected {
		marker = "› "
	}
	checkbox := "[ ]"
	if skill.Enabled {
		checkbox = "[x]"
	}
	label := marker + checkbox + " " + skill.Name
	contentWidth := max(1, m.width-2)
	if !selected {
		compact := label + " · " + skillSourceLabel(skill.Source)

		return m.styleSkillsRouteLines(
			[]string{ansi.Truncate(compact, contentWidth, "…")},
			false,
		)
	}

	details := skill.Description + " · " + skillSourceLabel(skill.Source)
	if skill.ResourceCount > 0 {
		details += fmt.Sprintf(" · %d resources", skill.ResourceCount)
	}
	lines := []string{ansi.Truncate(label, contentWidth, "…")}
	for line := range strings.SplitSeq(lipgloss.Wrap(details, max(1, contentWidth-6), ""), "\n") {
		lines = append(lines, "      "+line)
	}

	return m.styleSkillsRouteLines(lines, true)
}

func (m *Model) styleSkillsRouteLines(lines []string, selected bool) string {
	if m.options.NoColor {
		return strings.Join(lines, "\n")
	}

	palette := paletteFor(m.theme)
	queryStyle := lipgloss.NewStyle().Bold(true).Foreground(palette.model)
	for index := range lines {
		lines[index] = highlightCommandMatch(lines[index], m.route.search.Value(), queryStyle)
		switch {
		case selected:
			lines[index] = lipgloss.NewStyle().Bold(index == 0).Foreground(
				palette.session,
			).Render(lines[index])
		case index > 0:
			lines[index] = lipgloss.NewStyle().Foreground(palette.muted).Render(lines[index])
		}
	}

	return strings.Join(lines, "\n")
}

func (m *Model) skillsDiagnosticDetail(maximum int) string {
	if len(m.route.diagnostics) == 0 {
		return m.styleSessionPickerNotice("Diagnostics: none.", false)
	}

	lines := []string{"Diagnostics"}
	for _, diagnostic := range m.route.diagnostics {
		line := diagnostic.Code
		if diagnostic.Resource != "" {
			line += " · " + diagnostic.Resource
		}
		if diagnostic.Message != "" {
			line += " — " + diagnostic.Message
		}
		lines = append(lines, "  "+line)
	}

	return truncateHeight(strings.Join(lines, "\n"), maximum)
}

func (m *Model) skillsRouteFooter(width int) string {
	if m.route.controlling {
		return m.activityNotice("Saving project Skill settings…")
	}
	if width < 50 {
		return "↑/↓ · Space toggle · Ctrl+D details · Esc"
	}

	return "↑/↓ select · Space/Enter toggle · Ctrl+D diagnostics · type to search · Esc cancel"
}
