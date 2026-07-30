//nolint:wsl_v5 // Picker state transitions keep Composer edits and layout updates adjacent.
package tui

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/internal/coding/attachment"
)

type filePickerDataMsg struct {
	generation uint64
	snapshot   attachment.Snapshot
	err        error
}

func (m *Model) openFilePickerAfterAt() tea.Cmd {
	generation, ok := m.openInlinePicker('@', pickerFile)
	if !ok {
		return nil
	}

	return func() tea.Msg {
		snapshot, err := m.controller.ListWorkspaceFiles(m.ctx)

		return filePickerDataMsg{
			generation: generation,
			snapshot:   snapshot.Clone(),
			err:        err,
		}
	}
}

func (m *Model) canOpenFilePickerAtCursor() bool {
	value := m.composer.Value()
	cursor := composerCursorByte(value, m.composer.Line(), m.composer.Column())
	if cursor <= 0 || cursor > len(value) || value[cursor-1] != '@' {
		return false
	}
	if cursor == 1 {
		return true
	}

	previous, _ := utf8.DecodeLastRuneInString(value[:cursor-1])

	return unicode.IsSpace(previous)
}

func (m *Model) updateFilePickerKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.picker.kind != pickerFile || m.picker.controlling {
		return m, nil
	}

	filtered := m.filteredFiles()
	switch message.String() {
	case keyEscape, keyCtrlC:
		return m, m.restoreFilePicker()
	case "up":
		m.picker.cursor = wrapIndex(m.picker.cursor-1, len(filtered))
	case keyDown, keyTab:
		m.picker.cursor = wrapIndex(m.picker.cursor+1, len(filtered))
	case keyCtrlU:
		m.picker.query = ""
		m.picker.cursor = 0
		m.picker.err = nil
		m.syncInlinePickerToken("@")
	case keyBackspace:
		if m.picker.query == "" {
			return m, m.restoreFilePicker()
		}

		m.picker.query = trimLastRune(m.picker.query)
		m.picker.cursor = 0
		m.picker.err = nil
		m.syncInlinePickerToken("@" + m.picker.query)
	case keyEnter:
		return m.selectFilePicker(filtered)
	default:
		return m.updateFilePickerText(message)
	}

	m.setLayout()

	return m, nil
}

func (m *Model) selectFilePicker(
	filtered []attachment.Summary,
) (tea.Model, tea.Cmd) {
	if len(filtered) == 0 {
		return m, nil
	}

	selected := filtered[m.picker.cursor]
	if err := m.composer.InsertFile(
		m.picker.tokenStart,
		m.picker.tokenEnd,
		selected.Reference(),
	); err != nil {
		m.picker.err = err

		return m, nil
	}

	m.composer.InsertString(" ")
	m.picker = pickerState{}
	m.setLayout()

	return m, m.composer.Focus()
}

func (m *Model) updateFilePickerText(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	text := message.Key().Text
	if text == "" {
		return m, nil
	}
	if !validFileQueryText(text) {
		m.picker = pickerState{}
		var command tea.Cmd
		m.composer, command = m.composer.Update(message)
		m.setLayout()

		return m, command
	}

	m.picker.query += text
	m.picker.cursor = 0
	m.picker.err = nil
	m.syncInlinePickerToken("@" + m.picker.query)
	m.setLayout()

	return m, nil
}

func (m *Model) restoreFilePicker() tea.Cmd {
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

type rankedFile struct {
	summary attachment.Summary
	rank    int
}

func (m *Model) filteredFiles() []attachment.Summary {
	query := strings.ToLower(strings.TrimSpace(m.picker.query))
	ranked := make([]rankedFile, 0, len(m.picker.files))
	for _, summary := range m.picker.files {
		rank, matched := rankFile(summary.Path, query)
		if matched {
			ranked = append(ranked, rankedFile{summary: summary, rank: rank})
		}
	}
	sort.SliceStable(ranked, func(left, right int) bool {
		if ranked[left].rank != ranked[right].rank {
			return ranked[left].rank < ranked[right].rank
		}

		return ranked[left].summary.Path < ranked[right].summary.Path
	})

	filtered := make([]attachment.Summary, len(ranked))
	for index := range ranked {
		filtered[index] = ranked[index].summary
	}

	return filtered
}

func rankFile(name, query string) (int, bool) {
	if query == "" {
		return 0, true
	}

	lowerPath := strings.ToLower(name)
	base := strings.ToLower(path.Base(name))
	switch {
	case strings.HasPrefix(base, query):
		return 0, true
	case strings.Contains(base, query):
		return 1, true
	case strings.Contains(lowerPath, query):
		return 2, true
	case orderedSubsequence(lowerPath, query):
		return 3, true
	default:
		return 0, false
	}
}

func orderedSubsequence(value, query string) bool {
	remaining := []rune(query)
	for _, character := range value {
		if len(remaining) > 0 && character == remaining[0] {
			remaining = remaining[1:]
		}
	}

	return len(remaining) == 0
}

func (m *Model) filePickerView(maxHeight int) string {
	if m.picker.kind != pickerFile {
		return ""
	}

	footer := m.filePickerFooter()
	if maxHeight <= 1 {
		return m.styleCommandPickerFooter(footer)
	}
	maxHeight--

	filtered := m.filteredFiles()
	if len(filtered) == 0 {
		empty := "No matching Workspace files."
		if m.picker.loading {
			empty = "Loading Workspace files…"
		}

		return truncateHeight(empty+"\n"+m.styleCommandPickerFooter(footer), maxHeight+1)
	}

	rows := make([]string, len(filtered))
	heights := make([]int, len(filtered))
	for index, summary := range filtered {
		rows[index] = m.renderFilePickerRow(summary, index == m.picker.cursor)
		heights[index] = lipgloss.Height(rows[index])
	}
	start, end := selectionWindowByHeight(heights, m.picker.cursor, max(1, maxHeight))
	visible := truncateHeight(strings.Join(rows[start:end], "\n"), max(1, maxHeight))

	return visible + "\n" + m.styleCommandPickerFooter(footer)
}

func (m *Model) renderFilePickerRow(summary attachment.Summary, selected bool) string {
	marker := strings.Repeat(" ", inputPromptWidth)
	if selected {
		marker = "› "
	}

	kind := "text"
	if summary.Kind == attachment.KindImage {
		kind = "image"
	}
	line := fmt.Sprintf("%s@%s · %s · %d B", marker, summary.Path, kind, summary.Size)
	line = ansi.Truncate(line, max(1, m.width), "…")
	if !m.options.NoColor && selected {
		line = lipgloss.NewStyle().Foreground(paletteFor(m.theme).session).Render(line)
	}

	return line
}

func (m *Model) filePickerFooter() string {
	switch {
	case m.picker.err != nil:
		return "Error: " + safeError(m.picker.err)
	case m.picker.loading:
		return "Loading Workspace files…"
	case m.picker.truncated:
		return "Limited results · ↑/↓ select · Enter attach · Esc cancel"
	default:
		return "↑/↓ select · type to filter · Enter attach · Esc cancel"
	}
}

func validFileQueryText(value string) bool {
	if value == "" || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}

	return true
}
