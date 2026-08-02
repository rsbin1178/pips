package tui

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/internal/coding/statusline"
)

type statusLineSavedMsg struct {
	generation uint64
	items      []statusline.Item
	err        error
}

func (m *Model) openStatusLinePicker() {
	ordered, err := statusline.OrderedInventory(m.statusLineItems)
	if err != nil {
		m.streamErr = err

		return
	}

	enabled := make(map[statusline.Item]bool, len(m.statusLineItems))
	for _, item := range m.statusLineItems {
		enabled[item] = true
	}
	m.pickerSeq++
	m.picker = pickerState{
		kind: pickerStatusLine, generation: m.pickerSeq,
		statusItems: ordered, statusEnabled: enabled,
	}
	m.setLayout()
}

func (m *Model) updateStatusLinePickerKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.picker.kind != pickerStatusLine || m.picker.controlling {
		return m, nil
	}

	switch message.String() {
	case keyEscape, keyCtrlC:
		return m, m.closePicker()
	case "up", "k":
		m.picker.cursor = wrapIndex(m.picker.cursor-1, len(m.picker.statusItems))
	case keyDown, "j", keyTab:
		m.picker.cursor = wrapIndex(m.picker.cursor+1, len(m.picker.statusItems))
	case keyLeft:
		m.moveStatusLinePicker(-1)
	case keyRight:
		m.moveStatusLinePicker(1)
	case " ", "space":
		if item, ok := m.statusLinePickerItem(); ok {
			m.picker.statusEnabled[item] = !m.picker.statusEnabled[item]
			m.picker.err = nil
		}
	case keyEnter:
		return m, m.saveStatusLinePicker()
	}

	return m, nil
}

func (m *Model) moveStatusLinePicker(delta int) {
	if len(m.picker.statusItems) < 2 {
		return
	}

	current := m.picker.cursor
	next := min(max(0, current+delta), len(m.picker.statusItems)-1)
	if current == next {
		return
	}
	m.picker.statusItems[current], m.picker.statusItems[next] =
		m.picker.statusItems[next], m.picker.statusItems[current]
	m.picker.cursor = next
}

func (m *Model) statusLinePickerItem() (statusline.Item, bool) {
	if m.picker.cursor < 0 || m.picker.cursor >= len(m.picker.statusItems) {
		return "", false
	}

	return m.picker.statusItems[m.picker.cursor], true
}

func (m *Model) selectedStatusLineItems() []statusline.Item {
	items := make([]statusline.Item, 0, len(m.picker.statusItems))
	for _, item := range m.picker.statusItems {
		if m.picker.statusEnabled[item] {
			items = append(items, item)
		}
	}

	return items
}

func (m *Model) saveStatusLinePicker() tea.Cmd {
	if m.options.SaveStatusLine == nil {
		m.picker.err = errStatusLinePersistenceUnavailable

		return nil
	}

	items := m.selectedStatusLineItems()
	generation := m.picker.generation
	m.picker.loading = true
	m.picker.controlling = true
	m.picker.err = nil

	return func() tea.Msg {
		err := m.options.SaveStatusLine(m.ctx, slices.Clone(items))

		return statusLineSavedMsg{generation: generation, items: items, err: err}
	}
}

func (m *Model) statusLinePickerView(maxHeight int) string {
	if m.picker.kind != pickerStatusLine {
		return ""
	}

	preview := ansi.Strip(m.statusLineWithItems(m.selectedStatusLineItems()))
	if preview == "" {
		preview = "(empty)"
	}
	lines := []string{"Status line · saved for future sessions", "Preview: " + preview}
	for index, item := range m.picker.statusItems {
		cursor := "  "
		if index == m.picker.cursor {
			cursor = "› "
		}
		check := "[ ]"
		if m.picker.statusEnabled[item] {
			check = "[x]"
		}
		description, _ := statusline.Description(item)
		line := cursor + check + " " + string(item) + " · " + description
		if !m.options.NoColor && index == m.picker.cursor {
			line = lipgloss.NewStyle().Foreground(paletteFor(m.theme).session).Render(line)
		}
		lines = append(lines, line)
	}
	if m.picker.loading {
		lines = append(lines, "Saving…")
	}
	if m.picker.err != nil {
		lines = append(lines, "Error: "+safeError(m.picker.err))
	}
	lines = append(lines, "↑/↓ choose · Space toggle · ←/→ reorder · Enter save · Esc cancel")
	for index := range lines {
		lines[index] = ansi.Truncate(lines[index], max(1, m.width), "…")
	}

	return truncateHeight(strings.Join(lines, "\n"), max(1, maxHeight))
}
