//nolint:wsl_v5 // Picker state transitions and row composition stay locally visible.
package tui

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/modelcatalog"
)

type commandDescriptor struct {
	name        string
	description string
	idleOnly    bool
	arguments   bool
}

const (
	commandPermissions              = "permissions"
	commandStatus                   = "status"
	commandTeam                     = "team"
	commandTheme                    = "theme"
	maximumCommandArgumentTailBytes = 16 << 10
)

var commands = []commandDescriptor{
	{name: "new", description: "start a new session", idleOnly: true},
	{name: "resume", description: "resume a workspace session", idleOnly: true},
	{name: "plan", description: "enter read-only Plan Mode", idleOnly: true},
	{name: "mode", description: "switch Agent or Plan operating mode", idleOnly: true},
	{name: "agents", description: "inspect read-only specialist runs", idleOnly: true},
	{name: commandTeam, description: "propose or inspect a coding Team", idleOnly: true, arguments: true},
	{name: "skills", description: "enable or disable project Skills", idleOnly: true},
	{name: "model", description: "switch the process-local model", idleOnly: true},
	{name: commandPermissions, description: "change process-local execution permissions", idleOnly: true},
	{name: "statusline", description: "configure status-line fields", idleOnly: true},
	{name: commandTheme, description: "choose the TUI color theme", idleOnly: true},
	{name: "tree", description: "navigate the current session tree", idleOnly: true},
	{name: "fork", description: "fork a node into a new session", idleOnly: true},
	{name: "compact", description: "preview and compact older context", idleOnly: true},
	{name: "review", description: "review workspace changes in Plan Mode", idleOnly: true},
	{name: "reload", description: "reload resources and integrations", idleOnly: true},
	{name: commandStatus, description: "show runtime status"},
	{name: string(actionHelp), description: "show keyboard help"},
	{name: "quit", description: "exit Pips"},
}

func (m *Model) openCommandPicker() {
	if m.picker.kind == pickerCommand {
		return
	}

	previous := m.composer.Snapshot()
	m.picker = pickerState{
		kind:             pickerCommand,
		previousInput:    previous.display,
		previousComposer: previous,
	}
	m.syncCommandInput()
	m.setLayout()
}

func (m *Model) closeCommandPicker(restoreInput bool) {
	previousInput := m.picker.previousInput
	previousComposer := m.picker.previousComposer

	m.picker = pickerState{}
	if restoreInput {
		if err := m.composer.Restore(previousComposer); err != nil {
			m.composer.SetValue(previousInput)
			m.streamErr = err
		}
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
			candidate := m.picker.query + text
			_, arguments := splitCommandQuery(candidate)
			if len(arguments) > maximumCommandArgumentTailBytes {
				m.picker.err = errors.New("command argument is too long")

				return m, nil
			}
			m.picker.query = candidate
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

	_, arguments := splitCommandQuery(m.picker.query)
	if arguments != "" && !command.arguments {
		m.picker.err = fmt.Errorf("/%s does not accept an argument", command.name)

		return m, nil
	}
	if len(arguments) > maximumCommandArgumentTailBytes {
		m.picker.err = errors.New("command argument is too long")

		return m, nil
	}

	m.picker.query = command.name
	m.picker.err = nil
	m.syncCommandInput()

	switch command.name {
	case "plan":
		return m, m.runModeControl(coding.ModePlan)
	case "mode":
		previous := m.picker.previousComposer
		m.closeCommandPicker(false)
		m.restoreCommandComposer(previous)
		m.openModePicker()

		return m, nil
	case "new":
		return m, m.runControl(operationNew, "", modelcatalog.Selection{})
	case "resume":
		previous := m.picker.previousComposer
		m.closeCommandPicker(false)

		return m, m.openSessionPickerSnapshot(previous)
	case "model":
		previous := m.picker.previousComposer
		m.closeCommandPicker(false)
		m.restoreCommandComposer(previous)
		m.openModelPicker()

		return m, nil
	case commandPermissions:
		previous := m.picker.previousComposer
		m.closeCommandPicker(false)
		m.restoreCommandComposer(previous)
		m.openPermissionsPicker()

		return m, nil
	case "statusline":
		previous := m.picker.previousComposer
		m.closeCommandPicker(false)
		m.restoreCommandComposer(previous)
		m.openStatusLinePicker()

		return m, nil
	case commandTheme:
		return m.openThemeCommand()
	case "agents":
		previous := m.picker.previousComposer
		m.closeCommandPicker(false)
		m.restoreCommandComposer(previous)

		return m, m.openAgentsRoute()
	case commandTeam:
		previous := m.picker.previousComposer
		m.closeCommandPicker(false)
		m.restoreCommandComposer(previous)

		return m, m.openTeamRoute(arguments)
	case "skills":
		previous := m.picker.previousComposer
		m.closeCommandPicker(false)

		return m, m.openSkillsRouteSnapshot(previous)
	case "tree":
		previous := m.picker.previousComposer
		m.closeCommandPicker(false)
		m.restoreCommandComposer(previous)

		return m, m.openTreeRoute(false)
	case "fork":
		previous := m.picker.previousComposer
		m.closeCommandPicker(false)
		m.restoreCommandComposer(previous)

		return m, m.openTreeRoute(true)
	case "compact":
		previous := m.picker.previousComposer
		m.closeCommandPicker(false)
		m.restoreCommandComposer(previous)

		return m, m.openCompactPrompt()
	case "review":
		if m.controller.Mode().Current != coding.ModePlan {
			m.picker.err = errors.New("/review is available only in Plan Mode")

			return m, nil
		}
		previous := m.picker.previousComposer
		m.closeCommandPicker(false)
		m.restoreCommandComposer(previous)

		return m, m.startStream(func(ctx context.Context) iter.Seq2[coding.Event, error] {
			return m.controller.Prompt(ctx, ai.UserText(
				"Review the current workspace changes. Identify correctness, security, and test risks. Do not modify files.",
			))
		})
	case "reload":
		return m, m.runControl(operationReload, "", modelcatalog.Selection{})
	case commandStatus:
		previous := m.picker.previousComposer
		m.closeCommandPicker(false)
		m.restoreCommandComposer(previous)

		return m, m.printStatus()
	case string(actionHelp):
		previous := m.picker.previousComposer
		m.closeCommandPicker(false)
		m.restoreCommandComposer(previous)

		return m, m.printHelp()
	case "quit":
		m.closeCommandPicker(false)

		return m, tea.Quit
	default:
		return m, nil
	}
}

func (m *Model) openThemeCommand() (tea.Model, tea.Cmd) {
	previous := m.picker.previousComposer
	m.closeCommandPicker(false)
	m.restoreCommandComposer(previous)
	m.openThemePicker()

	return m, nil
}

func (m *Model) restoreCommandComposer(snapshot composerSnapshot) {
	if err := m.composer.Restore(snapshot); err != nil {
		m.streamErr = err
	}
}

func (m *Model) filteredCommands() []commandDescriptor {
	query, _ := splitCommandQuery(m.picker.query)
	query = strings.ToLower(query)

	filtered := make([]commandDescriptor, 0, len(commands))
	for _, command := range commands {
		if query == "" || strings.Contains(command.name, query) ||
			strings.Contains(command.description, query) {
			filtered = append(filtered, command)
		}
	}

	return filtered
}

func splitCommandQuery(value string) (string, string) {
	value = strings.TrimLeftFunc(value, unicode.IsSpace)
	separator := strings.IndexFunc(value, unicode.IsSpace)
	if separator < 0 {
		return value, ""
	}

	return value[:separator], strings.TrimSpace(value[separator:])
}

func (m *Model) commandPickerView(maxHeight int) string {
	if m.picker.kind != pickerCommand {
		return ""
	}

	footer := ""
	if m.picker.loading {
		footer = m.activityNotice("Working…")
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
		query, _ := splitCommandQuery(m.picker.query)
		for index := range lines {
			lines[index] = highlightCommandMatch(lines[index], query, matchStyle)
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
