//nolint:wsl_v5 // Panel state, key handling and rendering stay locally visible.
package tui

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The Help panel answers the two questions a reader has about the keyboard: which
// keys exist, and which commands exist. It borrows the status panel's chrome, page
// lines and footer, so the two panels read as one surface and a key that moves shows
// up in one place.
type helpTab uint8

const (
	helpTabShortcuts helpTab = iota
	helpTabCommands
	helpTabCount
)

func (t helpTab) title() string {
	if t == helpTabCommands {
		return "Commands"
	}

	return "Shortcuts"
}

func (t helpTab) step(delta int) helpTab {
	count := int(helpTabCount)

	return helpTab(((int(t)+delta)%count + count) % count)
}

// openHelp opens the panel, the way /status opens its own: a full-area route that
// owns the screen until it closes.
func (m *Model) openHelp() tea.Cmd {
	return m.openHelpRoute()
}

// openHelpRoute opens the panel over the ready view.
func (m *Model) openHelpRoute() tea.Cmd {
	previous := m.composer.Snapshot()

	return m.requestRouteOpen(routeOpenRequest{
		kind:                routeHelp,
		previousInput:       previous.display,
		previousComposer:    previous.clone(),
		hasPreviousComposer: true,
	})
}

func (m *Model) activateHelpRoute(previous composerSnapshot) tea.Cmd {
	m.routeSeq++
	m.route = routeState{
		kind:                routeHelp,
		generation:          m.routeSeq,
		helpTab:             helpTabShortcuts,
		previousInput:       previous.display,
		previousComposer:    previous.clone(),
		hasPreviousComposer: true,
	}
	m.composer.Blur()
	m.setLayout()

	return nil
}

func (m *Model) updateHelpRouteKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch message.String() {
	case keyEscape, keyCtrlC:
		return m, m.closeRouteToParent()
	case keyTab, keyRight, "l":
		return m, m.selectHelpTab(m.route.helpTab.step(1))
	case keyShiftTab, keyLeft, "h":
		return m, m.selectHelpTab(m.route.helpTab.step(-1))
	}

	return m, m.scrollHelpPage(message)
}

func (m *Model) selectHelpTab(tab helpTab) tea.Cmd {
	m.route.helpTab = tab
	m.route.offset = 0
	m.setLayout()

	return nil
}

// scrollHelpPage applies one scrolling key to the open page.
func (m *Model) scrollHelpPage(message tea.KeyPressMsg) tea.Cmd {
	visible := m.helpRouteBodyHeight()
	maximum := m.helpRouteMaximumOffset()
	switch message.String() {
	case "up", "k":
		m.route.offset = max(0, m.route.offset-1)
	case keyDown, "j":
		m.route.offset = min(maximum, m.route.offset+1)
	case keyPageUp:
		m.route.offset = max(0, m.route.offset-visible)
	case keyPageDown:
		m.route.offset = min(maximum, m.route.offset+visible)
	case keyHome:
		m.route.offset = 0
	case keyEnd:
		m.route.offset = maximum
	}
	m.route.offset = min(max(0, m.route.offset), maximum)

	return nil
}

// scrollHelpRoute moves the panel by one wheel notch.
func (m *Model) scrollHelpRoute(lines int) {
	m.route.offset = min(max(0, m.route.offset+lines), m.helpRouteMaximumOffset())
}

func (m *Model) helpRouteView() tea.View {
	width := max(1, m.width)
	height := max(1, m.height)
	chrome := m.helpRouteChrome(width)
	chromeRows := statusChromeRows(chrome)
	bodyHeight := m.helpRouteBodyHeight()
	lines := m.helpRoutePageLines(m.route.helpTab)
	labelWidth := m.statusLabelColumn(lines)
	pairColumn := m.statusPairColumn(lines, labelWidth)
	parts := make([]string, 0, chromeRows+bodyHeight+2)
	parts = append(parts, chrome...)
	parts = append(parts, m.statusRouteBody(width, lines, labelWidth, pairColumn, bodyHeight)...)
	parts = append(parts, m.sessionPickerSeparator(width), m.helpRouteFooterText(width))

	// Through presentationView, like every other full-area route: a raw
	// tea.NewView leaves view.AltScreen false, which takes the panel off the
	// alternate screen and paints it into the terminal's scrollback, where it
	// outlives the session.
	return m.presentationView(truncateHeight(strings.Join(parts, "\n"), height))
}

// helpRouteChrome is the pinned header: the invocation, the separator and the tab
// bar, laid out exactly like the status panel's.
func (m *Model) helpRouteChrome(width int) []string {
	return []string{
		renderUserMessage("/help", width, m.theme, m.options.NoColor),
		m.sessionPickerSeparator(width),
		m.helpRouteTabBar(width),
		"",
	}
}

func (m *Model) helpRouteTabBar(width int) string {
	return ansi.Truncate(
		statusLabelIndent+strings.Join(m.helpRouteTabTitles(), statusTabGap),
		max(1, width), "…",
	)
}

// helpRouteTabTitles renders one title per page, emphasizing the active one. Under
// NO_COLOR the active page is bracketed, so the selection survives without styling.
func (m *Model) helpRouteTabTitles() []string {
	titles := make([]string, 0, int(helpTabCount))
	for tab := range helpTabCount {
		title := tab.title()
		switch {
		case tab != m.route.helpTab:
		case m.options.NoColor:
			title = "[" + title + "]"
		default:
			title = lipgloss.NewStyle().Bold(true).Foreground(
				paletteFor(m.theme).session,
			).Render(title)
		}
		titles = append(titles, title)
	}

	return titles
}

func (m *Model) helpRouteFooterText(width int) string {
	hint := "</>/tab to switch · ↑/↓ " + wheelHintNavigation + " · Esc to close"

	return ansi.Truncate(hint, max(1, width), "…")
}

func (m *Model) helpRouteBodyHeight() int {
	return m.statusRouteBodyHeight(statusChromeRows(m.helpRouteChrome(max(1, m.width))))
}

func (m *Model) helpRouteMaximumOffset() int {
	return max(0, len(m.helpRoutePageLines(m.route.helpTab))-m.helpRouteBodyHeight())
}

// helpRoutePageLines composes one page of the panel.
func (m *Model) helpRoutePageLines(tab helpTab) []statusPageLine {
	if tab == helpTabCommands {
		return helpCommandLines()
	}

	return m.helpShortcutLines()
}

// helpShortcutLines lists the keys the ready view answers to, grouped by when it
// answers them. The binding table is the single source, so a key that moves appears
// here without a second list to keep in step.
func (m *Model) helpShortcutLines() []statusPageLine {
	lines := []statusPageLine{statusHeading("Always")}
	for _, binding := range defaultActions {
		if !slices.Contains(binding.Contexts, contextIdle) {
			continue
		}
		lines = append(lines, statusField(strings.Join(binding.Keys, " / "), binding.Label))
	}

	lines = append(lines, statusBlank(), statusHeading("While a turn runs"))
	for _, binding := range defaultActions {
		if slices.Contains(binding.Contexts, contextIdle) ||
			!slices.Contains(binding.Contexts, contextRunning) {
			continue
		}
		lines = append(lines, statusField(strings.Join(binding.Keys, " / "), binding.Label))
	}

	return append(lines,
		statusBlank(),
		statusHeading("The draft"),
		statusField("wheel over the draft", "scroll a draft taller than its box"),
		statusField("home / end", "start / end of the line"),
		statusBlank(),
		statusHeading("Reading the conversation"),
		statusField("pgup / pgdown", "scroll the conversation"),
		statusField("ctrl+f", "search the conversation"),
		statusField("ctrl+o", "hand the transcript to the terminal"),
		statusField("history", m.conversationOwnerHint()),
		statusField("mouse", m.mouseCaptureHint()),
	)
}

// conversationOwnerHint names the surface that owns the conversation, because that
// ownership differs between the managed viewport and the inline compatibility mode
// and this panel is the only place a reader can find it.
func (m *Model) conversationOwnerHint() string {
	if !m.fullscreen() {
		return "the terminal owns it: use its wheel or scrollback keys"
	}

	return "this session owns it: the wheel and PgUp/PgDn scroll it"
}

// mouseCaptureHint states the modifier that gives bare drag-select back while the
// mouse is captured, because capturing it is what takes that away.
func (m *Model) mouseCaptureHint() string {
	if !m.mouseReportingEnabled() {
		return "the capture is off, so dragging selects text"
	}

	return "hold Shift (Option in iTerm2, macOS Terminal and tmux on macOS) while dragging to select text"
}

// helpCommandLines lists the commands the Composer's picker offers, in the order the
// picker shows them.
func helpCommandLines() []statusPageLine {
	lines := []statusPageLine{
		statusHeading("Commands"),
		statusText(statusLabelIndent + "Type / in the draft to complete one."),
		statusBlank(),
	}
	for _, command := range commands {
		name := "/" + command.name
		if len(command.aliases) > 0 {
			name += " (" + strings.Join(command.aliases, ", ") + ")"
		}
		lines = append(lines, statusField(name, commandDescription(command)))
	}

	return lines
}

func commandDescription(command commandDescriptor) string {
	description := command.description
	if command.arguments {
		description += " · takes arguments"
	}
	if command.idleOnly {
		description += " · idle only"
	}

	return description
}
