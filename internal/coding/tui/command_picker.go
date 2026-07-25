//nolint:wsl_v5 // Picker state transitions and row composition stay locally visible.
package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/internal/coding/modelcatalog"
)

type commandDescriptor struct {
	name        string
	description string
	idleOnly    bool
}

const (
	commandDiff   = "diff"
	commandStatus = "status"
)

var commands = []commandDescriptor{
	{name: "new", description: "start a new session", idleOnly: true},
	{name: "resume", description: "resume a workspace session", idleOnly: true},
	{name: "agents", description: "inspect read-only specialist runs", idleOnly: true},
	{name: "skills", description: "browse and select available Skills", idleOnly: true},
	{name: "model", description: "switch the process-local model", idleOnly: true},
	{name: "tree", description: "navigate the current session tree", idleOnly: true},
	{name: "fork", description: "fork a node into a new session", idleOnly: true},
	{name: "compact", description: "preview and compact older context", idleOnly: true},
	{name: commandDiff, description: "inspect workspace changes"},
	{name: "reload", description: "reload resources and integrations", idleOnly: true},
	{name: commandStatus, description: "show runtime status"},
	{name: string(actionHelp), description: "show keyboard help"},
	{name: "quit", description: "exit Pips"},
}

func (m *Model) openCommandPicker() {
	if m.picker.kind == pickerCommand {
		return
	}

	m.picker = pickerState{
		kind:          pickerCommand,
		previousInput: m.composer.Value(),
	}
	m.syncCommandInput()
	m.setLayout()
}

func (m *Model) closeCommandPicker(restoreInput bool) {
	previousInput := m.picker.previousInput

	m.picker = pickerState{}
	if restoreInput {
		m.composer.SetValue(previousInput)
	} else {
		m.composer.Reset()
	}

	m.setLayout()
}

func (m *Model) syncCommandInput() {
	m.composer.SetValue("/" + m.picker.query)
}

//nolint:gocyclo // Command-picker key handling keeps every terminal transition in one place.
func (m *Model) updateCommandPickerKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.picker.kind != pickerCommand || m.picker.controlling {
		return m, nil
	}

	key := message.String()
	if key == keyEscape || key == keyCtrlC {
		m.closeCommandPicker(true)

		return m, nil
	}

	filtered := m.filteredCommands()

	switch key {
	case "up":
		m.picker.cursor = wrapIndex(m.picker.cursor-1, len(filtered))
	case keyDown, keyTab:
		m.picker.cursor = wrapIndex(m.picker.cursor+1, len(filtered))
	case keyEnter:
		if len(filtered) == 0 {
			return m, nil
		}

		return m.executeCommand(filtered[m.picker.cursor])
	case keyCtrlU:
		m.picker.query = ""
		m.picker.cursor = 0
		m.picker.err = nil
		m.syncCommandInput()
	case keyBackspace:
		if m.picker.query == "" {
			m.closeCommandPicker(true)

			return m, nil
		}

		m.picker.query = trimLastRune(m.picker.query)
		m.picker.cursor = 0
		m.picker.err = nil
		m.syncCommandInput()
	default:
		text := message.Key().Text
		if text != "" && text != "/" {
			m.picker.query += text
			m.picker.cursor = 0
			m.picker.err = nil
			m.syncCommandInput()
		}
	}

	m.setLayout()

	return m, nil
}

//nolint:gocyclo // The closed command inventory is dispatched in one auditable switch.
func (m *Model) executeCommand(command commandDescriptor) (tea.Model, tea.Cmd) {
	if command.idleOnly && m.actionContext() != contextIdle {
		m.picker.err = fmt.Errorf("/%s is available only while idle", command.name)

		return m, nil
	}

	m.picker.query = command.name
	m.picker.err = nil
	m.syncCommandInput()

	switch command.name {
	case "new":
		return m, m.runControl(operationNew, "", modelcatalog.Selection{})
	case "resume":
		previousInput := m.picker.previousInput
		m.closeCommandPicker(false)

		return m, m.openSessionPicker(previousInput)
	case "model":
		m.closeCommandPicker(false)
		m.openModelPicker()

		return m, nil
	case "agents":
		m.closeCommandPicker(false)

		return m, m.openAgentsRoute()
	case "skills":
		previousInput := m.picker.previousInput
		m.closeCommandPicker(false)

		return m, m.openSkillPickerForInput(previousInput)
	case "tree":
		m.closeCommandPicker(false)

		return m, m.openTreeRoute(false)
	case "fork":
		m.closeCommandPicker(false)

		return m, m.openTreeRoute(true)
	case "compact":
		m.closeCommandPicker(false)

		return m, m.openCompactPrompt()
	case commandDiff:
		m.closeCommandPicker(false)

		return m, m.printDiff()
	case "reload":
		return m, m.runControl(operationReload, "", modelcatalog.Selection{})
	case commandStatus:
		m.closeCommandPicker(false)

		return m, m.printStatus()
	case string(actionHelp):
		m.closeCommandPicker(false)

		return m, m.printHelp()
	case "quit":
		m.closeCommandPicker(false)

		return m, tea.Quit
	default:
		return m, nil
	}
}

func (m *Model) filteredCommands() []commandDescriptor {
	query := strings.ToLower(strings.TrimSpace(m.picker.query))

	filtered := make([]commandDescriptor, 0, len(commands))
	for _, command := range commands {
		if query == "" || strings.Contains(command.name, query) ||
			strings.Contains(command.description, query) {
			filtered = append(filtered, command)
		}
	}

	return filtered
}

func (m *Model) commandPickerView(maxHeight int) string {
	if m.picker.kind != pickerCommand {
		return ""
	}

	footer := ""
	if m.picker.loading {
		footer = "Working…"
	} else if m.picker.err != nil {
		footer = "Error: " + safeError(m.picker.err)
	}

	if footer != "" {
		if maxHeight <= 1 {
			return m.styleCommandPickerFooter(footer)
		}

		maxHeight--
	}

	filtered := m.filteredCommands()
	if len(filtered) == 0 {
		return m.styleCommandPickerFooter("No matching commands.")
	}

	rows := make([]string, len(filtered))
	heights := make([]int, len(filtered))
	for index := range filtered {
		rows[index] = m.renderCommandPickerRow(
			filtered[index],
			index == m.picker.cursor,
		)
		heights[index] = lipgloss.Height(rows[index])
	}
	start, end := selectionWindowByHeight(heights, m.picker.cursor, max(1, maxHeight))
	visible := truncateHeight(strings.Join(rows[start:end], "\n"), max(1, maxHeight))

	if footer != "" {
		visible += "\n" + m.styleCommandPickerFooter(footer)
	}

	return visible
}

func (m *Model) renderCommandPickerRow(command commandDescriptor, selected bool) string {
	marker := strings.Repeat(" ", inputPromptWidth)
	if selected {
		marker = "› "
	}
	label := "/" + command.name
	description := command.description
	if command.idleOnly && m.actionContext() != contextIdle {
		description += " · idle only"
	}
	labelWidth := m.commandPickerLabelWidth()
	descriptionWidth := max(1, m.width-inputPromptWidth-labelWidth)
	descriptionLines := strings.Split(lipgloss.Wrap(description, descriptionWidth, ""), "\n")
	if len(descriptionLines) == 0 {
		descriptionLines = []string{""}
	}
	lines := make([]string, 0, len(descriptionLines))
	for index, line := range descriptionLines {
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

func (m *Model) commandPickerLabelWidth() int {
	maximum := 0
	for _, command := range m.filteredCommands() {
		maximum = max(maximum, ansi.StringWidth("/"+command.name))
	}
	available := max(1, m.width-inputPromptWidth)
	upper := max(1, available/3)

	return min(max(16, maximum+2), upper)
}

func (m *Model) styleCommandPickerFooter(value string) string {
	value = ansi.Truncate(value, max(1, m.width), "…")
	if m.options.NoColor {
		return value
	}

	color := paletteFor(m.theme).muted
	if m.picker.err != nil {
		color = paletteFor(m.theme).error
	}

	return lipgloss.NewStyle().Foreground(color).Render(value)
}

func highlightCommandMatch(value, query string, style lipgloss.Style) string {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return value
	}

	index := strings.Index(strings.ToLower(value), query)
	if index < 0 {
		return value
	}

	end := index + len(query)

	return value[:index] + style.Render(value[index:end]) + value[end:]
}
