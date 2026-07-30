//nolint:wsl_v5 // Picker state transitions keep Composer edits and layout updates adjacent.
package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/internal/coding"
)

type skillPickerDataMsg struct {
	generation uint64
	snapshot   coding.SkillSnapshot
	err        error
}

func (m *Model) openSkillPickerForInput(input string) tea.Cmd {
	m.composer.SetValue(input)
	m.composer.CursorEnd()
	m.composer.InsertString("$")

	return m.openSkillPickerAfterDollar()
}

func (m *Model) openSkillPickerAfterDollar() tea.Cmd {
	value := m.composer.Value()
	cursor := composerCursorByte(value, m.composer.Line(), m.composer.Column())
	if cursor <= 0 || cursor > len(value) || value[cursor-1] != '$' {
		return nil
	}

	start := cursor - 1
	previous := value[:start] + value[cursor:]
	previousLine, previousColumn := composerPositionAtByte(previous, start)
	previousComposer := m.composer.Snapshot()
	previousComposer.display = previous
	previousComposer.position = composerPosition{
		line: previousLine, column: previousColumn,
	}

	m.pickerSeq++
	m.picker = pickerState{
		kind:             pickerSkill,
		loading:          true,
		previousInput:    previous,
		previousComposer: previousComposer,
		previousLine:     previousLine,
		previousCol:      previousColumn,
		tokenStart:       start,
		tokenEnd:         cursor,
		generation:       m.pickerSeq,
	}
	m.setLayout()
	generation := m.picker.generation

	return func() tea.Msg {
		snapshot, err := m.controller.Skills(m.ctx)

		return skillPickerDataMsg{generation: generation, snapshot: snapshot.Clone(), err: err}
	}
}

func (m *Model) canOpenSkillPickerAtCursor() bool {
	value := m.composer.Value()
	cursor := composerCursorByte(value, m.composer.Line(), m.composer.Column())
	if cursor <= 0 || cursor > len(value) || value[cursor-1] != '$' {
		return false
	}

	return cursor == 1 || !skillTokenByte(value[cursor-2])
}

func (m *Model) updateSkillPickerKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.picker.kind != pickerSkill || m.picker.controlling {
		return m, nil
	}

	filtered := m.filteredSkills()
	switch message.String() {
	case keyEscape, keyCtrlC:
		return m, m.restoreSkillPicker()
	case "up":
		m.picker.cursor = wrapIndex(m.picker.cursor-1, len(filtered))
	case keyDown, keyTab:
		m.picker.cursor = wrapIndex(m.picker.cursor+1, len(filtered))
	case keyCtrlU:
		m.picker.query = ""
		m.picker.cursor = 0
		m.picker.err = nil
		m.syncSkillToken("$")
	case keyBackspace:
		if m.picker.query == "" {
			return m, m.restoreSkillPicker()
		}

		m.picker.query = trimLastRune(m.picker.query)
		m.picker.cursor = 0
		m.picker.err = nil
		m.syncSkillToken("$" + m.picker.query)
	case keyEnter:
		if len(filtered) == 0 {
			return m, nil
		}

		m.syncSkillToken("$" + filtered[m.picker.cursor].Name + " ")
		m.picker = pickerState{}
		m.setLayout()

		return m, m.composer.Focus()
	default:
		text := message.Key().Text
		if text == "" {
			return m, nil
		}
		if !validSkillQueryText(text) {
			m.picker = pickerState{}
			var command tea.Cmd
			m.composer, command = m.composer.Update(message)
			m.setLayout()

			return m, command
		}

		m.picker.query += text
		m.picker.cursor = 0
		m.picker.err = nil
		m.syncSkillToken("$" + m.picker.query)
	}

	m.setLayout()

	return m, nil
}

func (m *Model) restoreSkillPicker() tea.Cmd {
	previous := m.picker.previousInput
	previousComposer := m.picker.previousComposer
	line := m.picker.previousLine
	column := m.picker.previousCol
	m.picker = pickerState{}
	if err := m.composer.Restore(previousComposer); err != nil {
		m.composer.SetValue(previous)
		setComposerPosition(&m.composer, line, column)
		m.streamErr = err
	}
	m.setLayout()

	return m.composer.Focus()
}

func (m *Model) syncSkillToken(replacement string) {
	value := m.composer.Value()
	start := min(max(0, m.picker.tokenStart), len(value))
	end := min(max(start, m.picker.tokenEnd), len(value))
	updated := value[:start] + replacement + value[end:]
	m.picker.tokenEnd = start + len(replacement)
	if err := m.composer.setDisplayPreservingElements(updated); err != nil {
		m.picker.err = err

		return
	}
	line, column := composerPositionAtByte(updated, m.picker.tokenEnd)
	setComposerPosition(&m.composer, line, column)
}

func (m *Model) filteredSkills() []coding.SkillSummary {
	query := strings.ToLower(strings.TrimSpace(m.picker.query))
	filtered := make([]coding.SkillSummary, 0, len(m.picker.skills))
	for _, skill := range m.picker.skills {
		if !skill.Enabled || !skill.UserInvocable {
			continue
		}

		searchable := strings.ToLower(skill.Name + " " + skill.Description + " " + string(skill.Source))
		if query == "" || strings.Contains(searchable, query) {
			filtered = append(filtered, skill)
		}
	}

	return filtered
}

func (m *Model) skillPickerView(maxHeight int) string {
	if m.picker.kind != pickerSkill {
		return ""
	}

	footer := m.skillPickerFooter()
	if footer != "" {
		if maxHeight <= 1 {
			return m.styleCommandPickerFooter(footer)
		}
		maxHeight--
	}

	filtered := m.filteredSkills()
	if len(filtered) == 0 {
		empty := "No matching user-invocable Skills."
		if m.picker.loading {
			empty = "Loading Skills…"
		}
		if footer != "" && !m.picker.loading {
			empty += "\n" + m.styleCommandPickerFooter(footer)
		}

		return truncateHeight(empty, max(1, maxHeight+1))
	}

	rows := make([]string, len(filtered))
	heights := make([]int, len(filtered))
	for index, skill := range filtered {
		rows[index] = m.renderSkillPickerRow(skill, index == m.picker.cursor)
		heights[index] = lipgloss.Height(rows[index])
	}
	start, end := selectionWindowByHeight(heights, m.picker.cursor, max(1, maxHeight))
	visible := truncateHeight(strings.Join(rows[start:end], "\n"), max(1, maxHeight))
	if footer != "" {
		visible += "\n" + m.styleCommandPickerFooter(footer)
	}

	return visible
}

func (m *Model) renderSkillPickerRow(skill coding.SkillSummary, selected bool) string {
	marker := strings.Repeat(" ", inputPromptWidth)
	if selected {
		marker = "› "
	}

	label := "$" + skill.Name
	labelWidth := m.skillPickerLabelWidth()
	details := skill.Description + " · " + skillSourceLabel(skill.Source)
	if skill.ResourceCount > 0 {
		details += fmt.Sprintf(" · %d resources", skill.ResourceCount)
	}
	detailWidth := max(1, m.width-inputPromptWidth-labelWidth)
	detailLines := strings.Split(lipgloss.Wrap(details, detailWidth, ""), "\n")
	lines := make([]string, 0, len(detailLines))
	for index, line := range detailLines {
		prefix := strings.Repeat(" ", inputPromptWidth+labelWidth)
		if index == 0 {
			prefix = marker + label + strings.Repeat(" ", max(1, labelWidth-ansi.StringWidth(label)))
		}
		lines = append(lines, prefix+line)
	}

	if !m.options.NoColor {
		palette := paletteFor(m.theme)
		matchStyle := lipgloss.NewStyle().Bold(true).Foreground(palette.model)
		for index := range lines {
			lines[index] = highlightCommandMatch(lines[index], m.picker.query, matchStyle)
			if selected {
				lines[index] = lipgloss.NewStyle().Foreground(palette.session).Render(lines[index])
			}
		}
	}

	for index := range lines {
		lines[index] = ansi.Truncate(lines[index], max(1, m.width), "…")
	}

	return strings.Join(lines, "\n")
}

func (m *Model) skillPickerLabelWidth() int {
	maximum := 0
	for _, skill := range m.filteredSkills() {
		maximum = max(maximum, ansi.StringWidth("$"+skill.Name))
	}
	available := max(1, m.width-inputPromptWidth)
	upper := max(1, available/3)

	return min(max(16, maximum+2), upper)
}

func (m *Model) skillPickerFooter() string {
	switch {
	case m.picker.err != nil:
		return "Error: " + safeError(m.picker.err)
	case m.picker.loading:
		return "Loading Skills…"
	case m.picker.diagnostics > 0:
		return fmt.Sprintf(
			"↑/↓ select · type to filter · Enter insert · Esc cancel · %d diagnostics",
			m.picker.diagnostics,
		)
	default:
		return "↑/↓ select · type to filter · Enter insert · Esc cancel"
	}
}

func skillSourceLabel(source coding.SkillSource) string {
	switch source {
	case coding.SkillSourceProjectPips:
		return "project · pips"
	case coding.SkillSourceProjectAgents:
		return "project · agents"
	case coding.SkillSourceUserPips:
		return "user · pips"
	case coding.SkillSourceUserAgents:
		return "user · agents"
	case coding.SkillSourceExtension:
		return "extension"
	default:
		return "unknown source"
	}
}

func validSkillQueryText(value string) bool {
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || char == '-' {
			continue
		}

		return false
	}

	return value != ""
}

func skillTokenByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || value == '-' || value == '_'
}

func composerCursorByte(value string, line, column int) int {
	lines := strings.Split(value, "\n")
	line = min(max(0, line), len(lines)-1)
	offset := 0
	for index := range line {
		offset += len(lines[index]) + 1
	}
	runes := []rune(lines[line])
	column = min(max(0, column), len(runes))

	return offset + len(string(runes[:column]))
}

func composerPositionAtByte(value string, offset int) (int, int) {
	offset = min(max(0, offset), len(value))
	prefix := value[:offset]
	line := strings.Count(prefix, "\n")
	lastNewline := strings.LastIndex(prefix, "\n")
	columnText := prefix
	if lastNewline >= 0 {
		columnText = prefix[lastNewline+1:]
	}

	return line, len([]rune(columnText))
}

type composerPositioner interface {
	Line() int
	CursorUp()
	CursorDown()
	SetCursorColumn(int)
}

func setComposerPosition(composer composerPositioner, line, column int) {
	for composer.Line() > line {
		composer.CursorUp()
	}
	for composer.Line() < line {
		composer.CursorDown()
	}
	composer.SetCursorColumn(column)
}
