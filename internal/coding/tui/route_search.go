package tui

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const routeSearchPrompt = "⌕ "

const (
	routeSearchOuterInset      = 4
	routeSearchHorizontalFrame = 4
)

func newRouteSearch(theme colorTheme, noColor bool) textinput.Model {
	search := textinput.New()
	search.Prompt = routeSearchPrompt
	search.Placeholder = "Search…"
	search.SetVirtualCursor(false)
	search.SetStyles(sessionSearchStyles(theme, noColor))
	search.Focus()

	return search
}

func (m *Model) routeSearchBox(width int) (string, int, int) {
	width = max(1, width)
	if width < 8 {
		return routeSearchLine(m.route.search.View(), width), 0, 0
	}

	boxWidth := max(1, width-routeSearchOuterInset)
	contentWidth := max(1, boxWidth-routeSearchHorizontalFrame)
	content := routeSearchLine(m.route.search.View(), contentWidth)

	style := lipgloss.NewStyle().
		Width(boxWidth).
		Padding(0, 1).
		Border(lipgloss.RoundedBorder(), true)
	if m.options.NoColor {
		style = style.Border(lipgloss.NormalBorder(), true)
	} else {
		style = style.BorderForeground(paletteFor(m.theme).separator)
	}

	return style.Render(content), 2, 1
}

func routeSearchLine(view string, width int) string {
	line, _, _ := strings.Cut(strings.ReplaceAll(view, "\r", ""), "\n")

	return ansi.Truncate(line, max(1, width), "")
}

func routeSearchInputWidth(width int) int {
	width = max(1, width)
	if width < 8 {
		return max(1, width-ansi.StringWidth(routeSearchPrompt)-1)
	}

	boxWidth := max(1, width-routeSearchOuterInset)
	contentWidth := max(1, boxWidth-routeSearchHorizontalFrame)

	return max(1, contentWidth-ansi.StringWidth(routeSearchPrompt)-1)
}

func (m *Model) searchableRouteView(content string, searchX, searchY int) tea.View {
	view := tea.NewView(content)
	view.AltScreen = false
	view.MouseMode = tea.MouseModeNone
	view.WindowTitle = appTitle

	view.Cursor = m.route.search.Cursor()
	if view.Cursor != nil {
		view.Cursor.X += searchX

		view.Cursor.Y += searchY
		if view.Cursor.X >= max(1, m.width) || view.Cursor.Y >= max(1, m.height) {
			view.Cursor = nil
		}
	}

	return view
}
