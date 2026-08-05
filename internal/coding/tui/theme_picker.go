//nolint:wsl_v5 // Theme picker transitions and rendering stay adjacent.
package tui

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/internal/coding/config"
)

type themeSavedMsg struct {
	generation uint64
	selection  string
	err        error
}

var (
	errThemeSelectionInvalid     = errors.New("theme selection is invalid")
	errThemeSelectionUnavailable = errors.New("theme selection is unavailable")
)

func (m *Model) openThemePicker() {
	registry := loadThemeRegistry(m.options.ThemeDirectory)
	options := registry.Options()
	requested := config.NormalizeThemeSelection(m.themeSelection)
	selection := requested
	diagnostics := registry.DisplayDiagnostics()
	for _, diagnostic := range m.themeDiagnostics {
		if diagnostic.category == themeDiagnosticSelection {
			diagnostics = appendThemeDiagnostic(diagnostics, diagnostic)
		}
	}
	if selection != config.ThemeAuto {
		if _, ok := registry.Resolve(selection); !ok {
			selection = config.ThemeAuto
			diagnostics = appendThemeDiagnostic(diagnostics, themeDiagnostic{category: themeDiagnosticSelection, count: 1})
		}
	}
	cursor := 0
	for index, option := range options {
		if option.id == selection {
			cursor = index
			break
		}
	}
	m.pickerSeq++
	m.picker = pickerState{
		kind:             pickerTheme,
		cursor:           cursor,
		generation:       m.pickerSeq,
		themes:           options,
		themeRegistry:    registry,
		themeDiagnostics: diagnostics,
		themeSelection:   selection,
	}
	m.setLayout()
}

func (m *Model) updateThemePickerKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.picker.kind != pickerTheme || m.picker.controlling {
		return m, nil
	}

	options := m.picker.themes
	if len(options) == 0 {
		return m, nil
	}
	switch message.String() {
	case keyEscape, keyCtrlC:
		return m, m.closePicker()
	case "up", "k":
		m.picker.cursor = wrapIndex(m.picker.cursor-1, len(options))
	case keyDown, "j", keyTab:
		m.picker.cursor = wrapIndex(m.picker.cursor+1, len(options))
	case keyEnter:
		return m, m.saveThemePicker()
	}

	m.setLayout()

	return m, nil
}

func (m *Model) saveThemePicker() tea.Cmd {
	if m.options.SaveTheme == nil {
		m.picker.err = errThemePersistenceUnavailable

		return nil
	}
	if m.picker.cursor < 0 || m.picker.cursor >= len(m.picker.themes) {
		m.picker.err = errThemeSelectionInvalid

		return nil
	}
	option := m.picker.themes[m.picker.cursor]
	selection := option.id
	if option.automatic {
		selection = config.ThemeAuto
	}
	if _, ok := resolvedTheme(m.picker.themeRegistry, selection, m.themeIsDark); !ok {
		m.picker.err = fmt.Errorf("%w: %q", errThemeSelectionUnavailable, selection)

		return nil
	}

	generation := m.picker.generation
	m.picker.loading = true
	m.picker.controlling = true
	m.picker.err = nil
	activityWasVisible := m.activityClockVisible()

	return tea.Batch(func() tea.Msg {
		err := m.options.SaveTheme(m.ctx, selection)

		return themeSavedMsg{generation: generation, selection: selection, err: err}
	}, m.startActivityClock(activityWasVisible))
}

//nolint:gocyclo // The picker keeps row, diagnostic, status, and footer budgets explicit.
func (m *Model) themePickerView(maxHeight int) string {
	if m.picker.kind != pickerTheme {
		return ""
	}

	maxHeight = max(1, maxHeight)
	header := "Theme · saved in active config.toml"
	rows := make([]string, len(m.picker.themes))
	heights := make([]int, len(rows))
	for index, option := range m.picker.themes {
		marker := "  "
		if index == m.picker.cursor {
			marker = "› "
		}
		current := ""
		if option.id == m.picker.themeSelection {
			current = " · current"
		}
		background := string(option.background)
		if option.automatic {
			background = "adaptive"
		}
		line := marker + option.id + " · " + option.name + " · " + background +
			" · " + string(option.source) + current
		if !m.options.NoColor && index == m.picker.cursor {
			line = lipgloss.NewStyle().Foreground(paletteFor(m.theme).session).Render(line)
		}
		rows[index] = ansi.Truncate(line, max(1, m.width), "…")
		heights[index] = max(1, lipgloss.Height(rows[index]))
	}

	diagnosticLines := make([]string, 0, len(m.picker.themeDiagnostics))
	for _, diagnostic := range m.picker.themeDiagnostics {
		diagnosticLines = append(diagnosticLines, "Notice: "+diagnostic.message())
	}
	statusLines := make([]string, 0, 2)
	if m.picker.loading {
		statusLines = append(statusLines, m.activityNotice("Saving…"))
	}
	if m.picker.err != nil {
		statusLines = append(statusLines, "Error: "+safeThemeError(m.picker.err))
	}
	footer := "↑/↓ choose · Enter apply · Esc cancel"

	// Keep the selected row, diagnostics, status, and footer as separate
	// height-budgeted regions. Rendering every row and truncating the complete
	// result can hide both the cursor and the footer on short terminals.
	if len(diagnosticLines) > 1 {
		diagnosticLines = []string{"Notice: " + joinThemeDiagnostics(m.picker.themeDiagnostics)}
	}
	trailing := append(slices.Clone(diagnosticLines), statusLines...)
	trailing = append(trailing, footer)
	includeHeader := len(trailing)+2 <= maxHeight
	if !includeHeader && len(trailing)+1 > maxHeight && len(statusLines) > 0 {
		compact := append(slices.Clone(diagnosticLines), statusLines...)
		compact = append(compact, footer)
		trailing = []string{strings.Join(compact[:len(compact)-1], " · "), footer}
	}
	if len(trailing)+1 > maxHeight {
		trailing = []string{strings.Join(trailing, " · ")}
	}

	rowBudget := maxHeight - len(trailing)
	if includeHeader {
		rowBudget--
	}
	rowBudget = max(0, rowBudget)
	start, end := 0, 0
	if rowBudget > 0 {
		start, end = selectionWindowByHeight(heights, m.picker.cursor, rowBudget)
	}

	lines := make([]string, 0, 1+end-start+len(trailing))
	if includeHeader {
		lines = append(lines, ansi.Truncate(header, max(1, m.width), "…"))
	}
	if start < end {
		lines = append(lines, rows[start:end]...)
	}
	lines = append(lines, trailing...)

	for index := range lines {
		lines[index] = ansi.Truncate(lines[index], max(1, m.width), "…")
	}

	return truncateHeight(strings.Join(lines, "\n"), maxHeight)
}

func joinThemeDiagnostics(diagnostics []themeDiagnostic) string {
	messages := make([]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		messages = append(messages, diagnostic.message())
	}

	return strings.Join(messages, " · ")
}

func safeThemeError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errThemePersistenceUnavailable):
		return "theme persistence is unavailable"
	case errors.Is(err, errThemeSelectionInvalid):
		return "theme selection is invalid"
	case errors.Is(err, errThemeSelectionUnavailable):
		return "selected theme is no longer available"
	case errors.Is(err, config.ErrConflict):
		return "theme configuration changed; try again"
	case errors.Is(err, config.ErrUnsafe):
		return "theme configuration is not safe to update"
	case errors.Is(err, config.ErrThemeEditUnsupported):
		return "theme configuration shape is unsupported"
	case errors.Is(err, config.ErrThemeDurability):
		return "theme saved; disk durability is uncertain"
	case errors.Is(err, config.ErrDecode):
		return "theme configuration is invalid"
	case errors.Is(err, config.ErrFile):
		return "theme configuration cannot be read or written"
	case errors.Is(err, config.ErrInvalid):
		return "theme selection is invalid"
	default:
		return "theme could not be saved"
	}
}
