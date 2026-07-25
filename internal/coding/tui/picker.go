//nolint:wsl_v5 // Model-picker state transitions and its rows are easier to audit together.
package tui

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/modelcatalog"
)

type pickerKind uint8

const defaultSelectionLabel = "default"

const (
	pickerNone pickerKind = iota
	pickerCommand
	pickerModel
	pickerSkill
)

type pickerState struct {
	kind          pickerKind
	cursor        int
	query         string
	err           error
	loading       bool
	controlling   bool
	models        []modelcatalog.Entry
	skills        []coding.SkillSummary
	diagnostics   int
	selection     modelcatalog.Selection
	previousInput string
	previousLine  int
	previousCol   int
	tokenStart    int
	tokenEnd      int
	generation    uint64
}

func (m *Model) openModelPicker() {
	state := m.controller.Model()
	m.picker = pickerState{
		kind:      pickerModel,
		models:    m.controller.Models(),
		selection: state.Selection,
	}
	for index, entry := range m.filteredPickerModels() {
		if entry.Ref == state.Resolved.Ref {
			m.picker.cursor = index
			break
		}
	}
}

func (m *Model) closePicker() tea.Cmd {
	m.picker = pickerState{}

	return m.composer.Focus()
}

//nolint:gocyclo // The picker owns the complete set of model-selection terminal bindings.
func (m *Model) updatePickerKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch m.picker.kind {
	case pickerCommand:
		return m.updateCommandPickerKey(message)
	case pickerModel:
	case pickerSkill:
		return m.updateSkillPickerKey(message)
	default:
		return m, nil
	}
	if m.picker.controlling {
		return m, nil
	}

	if key := message.String(); key == keyEscape || key == keyCtrlC {
		return m, m.closePicker()
	}

	filtered := m.filteredPickerModels()
	switch message.String() {
	case "up":
		m.picker.cursor = wrapIndex(m.picker.cursor-1, len(filtered))
	case keyDown, keyTab:
		m.picker.cursor = wrapIndex(m.picker.cursor+1, len(filtered))
	case "v":
		m.cyclePickerVariant(1)
	case "V":
		m.cyclePickerVariant(-1)
	case "r":
		m.cyclePickerReasoning(1)
	case "R":
		m.cyclePickerReasoning(-1)
	case keyCtrlU:
		m.picker.query = ""
		m.picker.cursor = 0
	case keyBackspace:
		m.picker.query = trimLastRune(m.picker.query)
		m.picker.cursor = 0
	case keyEnter:
		if len(filtered) == 0 {
			return m, nil
		}
		m.selectPickerModel(filtered[m.picker.cursor])

		return m, m.runControl(operationModel, "", m.picker.selection)
	default:
		if text := message.Key().Text; text != "" {
			m.picker.query += text
			m.picker.cursor = 0
		}
	}

	return m, nil
}

func (m *Model) filteredPickerModels() []modelcatalog.Entry {
	query := strings.ToLower(strings.TrimSpace(m.picker.query))
	filtered := make([]modelcatalog.Entry, 0, len(m.picker.models))
	for _, entry := range m.picker.models {
		if query == "" || strings.Contains(strings.ToLower(entry.Ref.String()), query) {
			filtered = append(filtered, entry)
		}
	}

	return filtered
}

func (m *Model) pickerModelEntry() (modelcatalog.Entry, bool) {
	filtered := m.filteredPickerModels()
	if len(filtered) == 0 {
		return modelcatalog.Entry{}, false
	}

	return filtered[m.picker.cursor], true
}

func (m *Model) selectPickerModel(entry modelcatalog.Entry) {
	if m.picker.selection.Ref != entry.Ref {
		m.picker.selection = modelcatalog.Selection{Ref: entry.Ref}
	}
}

func (m *Model) cyclePickerVariant(direction int) {
	entry, ok := m.pickerModelEntry()
	if !ok {
		return
	}
	m.selectPickerModel(entry)
	variants := append([]string{""}, entry.Variants...)
	index := slices.Index(variants, m.picker.selection.Variant)
	m.picker.selection.Variant = variants[wrapIndex(index+direction, len(variants))]
}

func (m *Model) cyclePickerReasoning(direction int) {
	entry, ok := m.pickerModelEntry()
	if !ok {
		return
	}
	m.selectPickerModel(entry)
	levels := append([]config.ReasoningLevel{""}, entry.ReasoningLevels...)
	current := config.ReasoningLevel("")
	if m.picker.selection.ReasoningOverride != nil {
		current = *m.picker.selection.ReasoningOverride
	}
	next := levels[wrapIndex(slices.Index(levels, current)+direction, len(levels))]
	if next == "" {
		m.picker.selection.ReasoningOverride = nil

		return
	}
	m.picker.selection.ReasoningOverride = new(next)
}

func (m *Model) pickerView(maxHeight int) string {
	if m.picker.kind != pickerModel {
		return ""
	}

	lines := []string{"Model · current process only · filter: " + m.picker.query}
	values := m.filteredPickerModels()
	for index, entry := range values {
		prefix := "  "
		if index == m.picker.cursor {
			prefix = "› "
		}
		line := prefix + entry.Ref.String()
		if !m.options.NoColor && index == m.picker.cursor {
			line = lipgloss.NewStyle().Foreground(paletteFor(m.theme).session).Render(line)
		}
		lines = append(lines, line)
	}
	if len(values) == 0 {
		lines = append(lines, "No matching models.")
	} else if entry, ok := m.pickerModelEntry(); ok {
		variant := defaultSelectionLabel
		reasoning := defaultSelectionLabel
		if m.picker.selection.Ref == entry.Ref {
			variant = valueOrDefault(m.picker.selection.Variant)
			reasoning = reasoningOrDefault(m.picker.selection.ReasoningOverride)
		}
		lines = append(lines, "Variant: "+variant+" · Reasoning: "+reasoning)
	}
	if m.picker.loading {
		lines = append(lines, "Working…")
	}
	if m.picker.err != nil {
		lines = append(lines, "Error: "+safeError(m.picker.err))
	}
	lines = append(lines, "↑/↓ model · type search · v/V variant · r/R reasoning · Enter apply · Esc cancel")

	for index := range lines {
		lines[index] = ansi.Truncate(lines[index], max(1, m.width), "…")
	}

	return truncateHeight(strings.Join(lines, "\n"), max(1, maxHeight))
}

func valueOrDefault(value string) string {
	if value == "" {
		return defaultSelectionLabel
	}

	return value
}

func reasoningOrDefault(value *config.ReasoningLevel) string {
	if value == nil || *value == "" {
		return defaultSelectionLabel
	}

	return string(*value)
}
