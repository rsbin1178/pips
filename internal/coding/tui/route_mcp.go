//nolint:wsl_v5 // Route transitions and compact row rendering stay locally visible.
package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/internal/coding"
	codingmcp "github.com/rsbin1178/pips/internal/coding/mcp"
)

// mcpRoutePollInterval bounds how often the /mcp route re-reads connection
// state while at least one server is still connecting. Polling stops as soon
// as the snapshot settles or the route closes.
const mcpRoutePollInterval = 500 * time.Millisecond

const mcpRouteCompactToolNames = 3

type mcpRouteDataMsg struct {
	generation uint64
	snapshot   coding.MCPSnapshot
	err        error
}

type mcpRoutePollMsg struct {
	generation uint64
}

func (m *Model) openMCPRoute() tea.Cmd {
	previous := m.composer.Snapshot()

	return m.openMCPRouteSnapshot(previous)
}

func (m *Model) openMCPRouteSnapshot(previous composerSnapshot) tea.Cmd {
	return m.requestRouteOpen(routeOpenRequest{
		kind: routeMCP, previousInput: previous.display,
		previousComposer:    previous.clone(),
		hasPreviousComposer: true,
	})
}

func (m *Model) activateMCPRouteSnapshot(previous composerSnapshot) tea.Cmd {
	activityWasVisible := m.activityClockVisible()
	m.routeSeq++
	m.route = routeState{
		kind:                routeMCP,
		generation:          m.routeSeq,
		loading:             true,
		search:              newRouteSearch(m.theme, m.options.NoColor),
		previousInput:       previous.display,
		previousComposer:    previous.clone(),
		hasPreviousComposer: true,
	}
	m.composer.Reset()
	m.setLayout()

	return tea.Batch(
		m.route.search.Focus(), m.loadMCPRoute(),
		m.startActivityClock(activityWasVisible),
	)
}

// loadMCPRoute reads the current connection snapshot for this route
// generation. Stale results from a closed or reopened route are ignored.
func (m *Model) loadMCPRoute() tea.Cmd {
	generation := m.route.generation

	return func() tea.Msg {
		snapshot, err := m.controller.MCP(m.ctx)

		return mcpRouteDataMsg{generation: generation, snapshot: snapshot.Clone(), err: err}
	}
}

func (m *Model) applyMCPRouteData(message mcpRouteDataMsg) tea.Cmd {
	if m.route.kind != routeMCP || message.generation != m.route.generation {
		return nil
	}
	m.route.loading = false
	m.route.refreshing = false
	m.route.err = message.err
	if message.err == nil {
		m.route.mcp = message.snapshot
	}
	m.route.cursor = clampIndex(m.route.cursor, len(m.filteredMCPRouteValues()))
	if message.err != nil || message.snapshot.Settled {
		return nil
	}

	generation := m.route.generation

	return tea.Tick(mcpRoutePollInterval, func(time.Time) tea.Msg {
		return mcpRoutePollMsg{generation: generation}
	})
}

func (m *Model) applyMCPRoutePoll(message mcpRoutePollMsg) tea.Cmd {
	if m.route.kind != routeMCP || message.generation != m.route.generation ||
		m.route.loading || m.route.refreshing {
		return nil
	}
	m.route.refreshing = true

	return m.loadMCPRoute()
}

func clampIndex(value, length int) int {
	if length <= 0 {
		return 0
	}

	return min(max(0, value), length-1)
}

func (m *Model) updateMCPRouteKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.route.kind != routeMCP {
		return m, nil
	}

	key := message.String()
	if key == keyEscape || key == keyCtrlC {
		return m, m.closeRouteToParent()
	}

	values := m.filteredMCPRouteValues()
	switch key {
	case "up", "ctrl+p":
		m.route.cursor = wrapIndex(m.route.cursor-1, len(values))
	case keyDown, keyTab, "ctrl+n":
		m.route.cursor = wrapIndex(m.route.cursor+1, len(values))
	case "ctrl+d":
		m.route.showDetails = !m.route.showDetails
	default:
		before := m.route.search.Value()
		var command tea.Cmd
		m.route.search, command = m.route.search.Update(message)
		if m.route.search.Value() != before {
			m.route.cursor = 0
		}

		return m, command
	}

	return m, nil
}

func (m *Model) filteredMCPRouteValues() []codingmcp.ServerStatus {
	query := strings.ToLower(strings.TrimSpace(m.route.search.Value()))
	filtered := make([]codingmcp.ServerStatus, 0, len(m.route.mcp.Servers))
	for _, server := range m.route.mcp.Servers {
		searchable := strings.ToLower(strings.Join(append([]string{
			server.ID,
			string(server.Scope),
			string(server.Transport),
			string(server.Visibility),
			string(server.State),
			server.Code,
		}, server.Tools...), " "))
		if query == "" || strings.Contains(searchable, query) {
			filtered = append(filtered, server)
		}
	}

	return filtered
}

func (m *Model) mcpRouteView() tea.View {
	m.route.listHits = routeListHit{}
	content, searchX, searchY := m.mcpRouteContent()

	return m.searchableRouteView(content, searchX, searchY)
}

func (m *Model) mcpRouteContent() (string, int, int) {
	width := max(1, m.width)
	height := max(1, m.height)
	invocation := renderUserMessage("/mcp", width, m.theme, m.options.NoColor)
	separator := m.sessionPickerSeparator(width)
	title := "MCP Servers"
	if !m.options.NoColor {
		title = lipgloss.NewStyle().Bold(true).Foreground(
			paletteFor(m.theme).session,
		).Render(title)
	}
	// The summary counts the loaded snapshot only; while the first load is in
	// flight or failed without data, the list notice already explains the
	// state and a "no servers" summary would contradict it.
	summary := ""
	if !m.route.loading && (m.route.err == nil || len(m.route.mcp.Servers) > 0) {
		summary = m.styleSessionPickerNotice(mcpRouteSummary(m.route.mcp), false)
	}
	search, searchX, searchInnerY := m.routeSearchBox(width)
	prefix := []string{invocation, "", separator, "", title, summary, "", search}
	listPadding := true
	if height < 18 {
		prefix = []string{invocation, separator, title, summary, search}
		listPadding = false
	}
	searchY := lipgloss.Height(lipgloss.JoinVertical(
		lipgloss.Left,
		prefix[:len(prefix)-1]...,
	)) + searchInnerY
	footer := m.mcpRouteFooter(width)
	if !m.options.NoColor {
		footer = lipgloss.NewStyle().Foreground(paletteFor(m.theme).muted).Render(footer)
	}
	footer = ansi.Truncate(footer, width, "…")

	// Reserve the footer line (plus the two blank lines around a padded list)
	// so a short terminal truncates rows, not the key help.
	paddingHeight := 3
	if !listPadding {
		paddingHeight = 1
	}
	prefixHeight := lipgloss.Height(lipgloss.JoinVertical(lipgloss.Left, prefix...))
	available := max(1, height-(prefixHeight+paddingHeight))
	listTop := prefixHeight
	if listPadding {
		listTop++
	}
	list := m.mcpRouteList(available, listTop)
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

func mcpRouteSummary(snapshot coding.MCPSnapshot) string {
	if len(snapshot.Servers) == 0 {
		return "No MCP servers are configured."
	}

	counts := make(map[codingmcp.ServerState]int, 5)
	for _, server := range snapshot.Servers {
		counts[server.State]++
	}
	parts := make([]string, 0, 5)
	for _, state := range []codingmcp.ServerState{
		codingmcp.ServerStateConnected,
		codingmcp.ServerStateConnecting,
		codingmcp.ServerStateFailed,
		codingmcp.ServerStatePending,
		codingmcp.ServerStateDisabled,
	} {
		if counts[state] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[state], state))
		}
	}

	return strings.Join(parts, " · ")
}

func (m *Model) mcpRouteList(maximum, frameTop int) string {
	if m.route.loading {
		return m.styleSessionPickerNotice(m.activityNotice("Loading MCP servers…"), false)
	}
	if m.route.err != nil && len(m.route.mcp.Servers) == 0 {
		return m.styleSessionPickerNotice("Error: "+safeError(m.route.err), true)
	}
	noticeLines := 0
	errorNotice := ""
	if m.route.err != nil {
		errorNotice = m.styleSessionPickerNotice("Error: "+safeError(m.route.err), true)
		noticeLines = lipgloss.Height(errorNotice) + 1
		maximum = max(1, maximum-noticeLines)
	}

	values := m.filteredMCPRouteValues()
	if len(values) == 0 {
		if len(m.route.mcp.Servers) == 0 {
			return m.styleSessionPickerNotice("Configure servers in mcp.json to see them here.", false)
		}

		return m.styleSessionPickerNotice("No matching MCP servers.", false)
	}

	cursor := clampIndex(m.route.cursor, len(values))
	detail := ""
	if m.route.showDetails {
		detail = m.mcpServerDetail(values[cursor], max(1, maximum/2))
		if detail != "" {
			maximum = max(1, maximum-lipgloss.Height(detail)-1)
		}
	}
	rows := make([]string, len(values))
	heights := make([]int, len(values))
	for index, server := range values {
		rows[index] = m.renderMCPRouteRow(server, index == cursor)
		heights[index] = lipgloss.Height(rows[index])
	}
	start, end := selectionWindowByHeight(selectionHeights(heights, 1), cursor, maximum)
	visible := truncateHeight(strings.Join(rows[start:end], "\n\n"), maximum)
	m.recordRouteListHit(routeListHit{
		painted: true, frameTop: frameTop + noticeLines, windowStart: 0,
		windowRows: lipgloss.Height(visible), firstLine: 0, first: start,
		heights: heights, gap: 1,
	})
	if errorNotice != "" {
		visible = errorNotice + "\n" + visible
	}
	if detail != "" {
		visible += "\n" + detail
	}

	return visible
}

func (m *Model) renderMCPRouteRow(server codingmcp.ServerStatus, selected bool) string {
	marker := "  "
	if selected {
		marker = "› "
	}
	label := marker + mcpStateGlyph(server.State) + " " + server.ID
	contentWidth := max(1, m.width-2)
	compact := label + " · " + string(server.State)
	if server.State == codingmcp.ServerStateConnected {
		compact += " · " + mcpToolCountLabel(len(server.Tools))
	}
	compact += " · " + mcpScopeLabel(server) + " · " + string(server.Transport)
	if !selected {
		return m.styleMCPRouteLines(
			[]string{ansi.Truncate(compact, contentWidth, "…")},
			server.State,
			false,
		)
	}

	lines := []string{ansi.Truncate(compact, contentWidth, "…")}
	details := []string{
		mcpScopeLabel(server) + " · " + string(server.Transport) + " · " + mcpVisibilityLabel(server.Visibility),
		mcpStateDetail(server, time.Now()),
	}
	if names := mcpCompactToolNames(server.Tools); names != "" {
		details = append(details, names)
	}
	for _, detail := range details {
		for line := range strings.SplitSeq(lipgloss.Wrap(detail, max(1, contentWidth-6), ""), "\n") {
			lines = append(lines, "      "+line)
		}
	}

	return m.styleMCPRouteLines(lines, server.State, true)
}

// mcpScopeLabel names who owns the server: the project, the user, or the app.
func mcpScopeLabel(server codingmcp.ServerStatus) string {
	switch string(server.Scope) {
	case "project":
		return "project"
	case "user":
		return "user"
	case "agent_private", "private":
		return "agent-private"
	case "":
		return "unscoped"
	default:
		return humanizeStatusCode(string(server.Scope))
	}
}

func mcpVisibilityLabel(visibility codingmcp.Visibility) string {
	switch visibility {
	case codingmcp.VisibilityAmbient, "":
		return "shared with the session"
	case codingmcp.VisibilityAgentPrivate:
		return "agent-private"
	default:
		return humanizeStatusCode(string(visibility))
	}
}

func mcpStateGlyph(state codingmcp.ServerState) string {
	switch state {
	case codingmcp.ServerStateConnected:
		return "●"
	case codingmcp.ServerStateConnecting:
		return "◌"
	case codingmcp.ServerStateFailed:
		return "✗"
	case codingmcp.ServerStatePending, codingmcp.ServerStateDisabled:
		return "○"
	default:
		return "·"
	}
}

func mcpToolCountLabel(count int) string {
	if count == 1 {
		return "1 tool"
	}

	return fmt.Sprintf("%d tools", count)
}

func mcpStateDetail(server codingmcp.ServerStatus, now time.Time) string {
	switch server.State {
	case codingmcp.ServerStateConnected:
		return "connected in " + mcpDuration(server.SettledAt.Sub(server.StartedAt))
	case codingmcp.ServerStateConnecting:
		return "connecting for " + mcpDuration(now.Sub(server.StartedAt))
	case codingmcp.ServerStateFailed:
		// The actionable part is the message (it carries the fix hint), so it
		// leads; the stage names where it failed and the code is omitted when a
		// message already explains the failure.
		detail := "failed while " + mcpStageLabel(server.Stage)
		if message := strings.TrimSpace(server.Message); message != "" {
			return detail + ": " + message
		}
		if server.Code != "" {
			return detail + ": " + humanizeStatusCode(server.Code)
		}

		return detail
	case codingmcp.ServerStatePending:
		return "pending · awaiting project approval"
	case codingmcp.ServerStateDisabled:
		return "disabled · denied by project permissions"
	default:
		return string(server.State)
	}
}

func mcpStageLabel(stage string) string {
	switch stage {
	case "connect":
		return "connecting"
	case "configuration":
		return "loading configuration"
	case "initialize":
		return "initializing"
	case "tools":
		return "listing tools"
	case "":
		return "starting"
	default:
		return humanizeStatusCode(stage)
	}
}

func mcpDuration(value time.Duration) string {
	if value < 0 {
		value = 0
	}
	if value < time.Second {
		return fmt.Sprintf("%dms", value.Milliseconds())
	}

	return value.Round(100 * time.Millisecond).String()
}

func mcpCompactToolNames(tools []string) string {
	if len(tools) == 0 {
		return ""
	}
	shown := tools
	if len(shown) > mcpRouteCompactToolNames {
		shown = shown[:mcpRouteCompactToolNames]
	}
	label := "tools: " + strings.Join(shown, ", ")
	if remaining := len(tools) - len(shown); remaining > 0 {
		label += fmt.Sprintf(" (+%d more · Ctrl+D)", remaining)
	}

	return label
}

func (m *Model) styleMCPRouteLines(lines []string, state codingmcp.ServerState, selected bool) string {
	if m.options.NoColor {
		return strings.Join(lines, "\n")
	}

	palette := paletteFor(m.theme)
	queryStyle := lipgloss.NewStyle().Bold(true).Foreground(palette.model)
	stateColor := palette.muted
	switch state {
	case codingmcp.ServerStateConnected:
		stateColor = palette.idle
	case codingmcp.ServerStateConnecting:
		stateColor = palette.warning
	case codingmcp.ServerStateFailed:
		stateColor = palette.error
	case codingmcp.ServerStatePending, codingmcp.ServerStateDisabled:
		stateColor = palette.muted
	}
	for index := range lines {
		lines[index] = highlightCommandMatch(lines[index], m.route.search.Value(), queryStyle)
		switch {
		case selected && index == 0:
			lines[index] = lipgloss.NewStyle().Bold(true).Foreground(palette.session).Render(lines[index])
		case selected:
			lines[index] = lipgloss.NewStyle().Foreground(palette.session).Render(lines[index])
		default:
			lines[index] = lipgloss.NewStyle().Foreground(stateColor).Render(lines[index])
		}
	}

	return strings.Join(lines, "\n")
}

// mcpServerDetail is the Ctrl+D pane: the complete tool-name list of the
// mcpFailureSummary names where a server failed without repeating a code the
// message already explains.
func mcpFailureSummary(server codingmcp.ServerStatus) string {
	summary := "failed while " + mcpStageLabel(server.Stage)
	if strings.TrimSpace(server.Message) == "" && server.Code != "" {
		summary += " · " + humanizeStatusCode(server.Code)
	}

	return summary
}

// wrapDetailText wraps one detail paragraph with a two-space hanging indent.
func wrapDetailText(value string, width int) []string {
	wrapped := ansi.Wrap(sanitizeToolText(value), max(1, width), "")
	rows := make([]string, 0, 4)
	for part := range strings.SplitSeq(wrapped, "\n") {
		rows = append(rows, "  "+part)
	}

	return rows
}

// selected server, or its failure diagnostic. It never renders definitions,
// endpoints, commands, or environment values.
func (m *Model) mcpServerDetail(server codingmcp.ServerStatus, maximum int) string {
	lines := []string{"Details · " + server.ID}
	switch server.State {
	case codingmcp.ServerStateConnected:
		if len(server.Tools) == 0 {
			lines = append(lines, "  No tools advertised.")
		}
		for _, tool := range server.Tools {
			lines = append(lines, "  "+tool)
		}
	case codingmcp.ServerStateFailed:
		lines = append(lines, "  "+mcpFailureSummary(server))
		if message := strings.TrimSpace(server.Message); message != "" {
			lines = append(lines, wrapDetailText(server.Message, max(1, m.width-4))...)
		}
	case codingmcp.ServerStateConnecting, codingmcp.ServerStatePending, codingmcp.ServerStateDisabled:
		lines = append(lines, "  "+mcpStateDetail(server, time.Now()))
	default:
		lines = append(lines, "  "+string(server.State))
	}
	width := max(1, m.width)
	for index := range lines {
		lines[index] = ansi.Truncate(lines[index], width, "…")
	}
	detail := truncateHeight(strings.Join(lines, "\n"), maximum)
	if m.options.NoColor {
		return detail
	}

	return lipgloss.NewStyle().Foreground(paletteFor(m.theme).muted).Render(detail)
}

func (m *Model) mcpRouteFooter(width int) string {
	connecting := !m.route.loading && m.route.err == nil && !m.route.mcp.Settled
	if width < 50 {
		if connecting {
			return "↑/↓ · Ctrl+D details · Esc · connecting…"
		}

		return "↑/↓ · Ctrl+D details · Esc"
	}
	if connecting {
		return "↑/↓ select · Ctrl+D details · type to search · Esc close · connecting…"
	}

	return "↑/↓ select · Ctrl+D details · type to search · Esc close"
}
