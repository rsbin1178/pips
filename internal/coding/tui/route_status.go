//nolint:wsl_v5 // Panel state, key handling and rendering stay locally visible.
package tui

import (
	"context"
	"errors"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/session"
)

// statusSearchPlaceholder names what the Config page's search box filters.
const statusSearchPlaceholder = "Search settings…"

// Panel chrome gaps: the tab bar and the Stats range row both lay their titles out
// as one string, so the pointer map measures the same separators.
const (
	statusTabGap    = "  "
	statusWindowGap = " · "
)

// statusTabBarRow is the chrome row the tab bar is drawn on: the invocation, the
// separator above it, then the tabs.
const statusTabBarRow = 2

// statusPanelHit is one addressable column range on a panel row.
type statusPanelHit struct {
	start  int
	end    int
	tab    statusPanelTab
	window statusStatsRange
}

// statusPanelRect is an addressable row and column block.
type statusPanelRect struct {
	top    int
	bottom int
	left   int
	right  int
}

func (rect statusPanelRect) contains(row, column int) bool {
	return row >= rect.top && row < rect.bottom && column >= rect.left && column < rect.right
}

// statusPanelHits records what a pointer may address in the panel frame the
// renderer painted last: the tab titles, the Config page's search box, and the
// Stats page's range row. Resolving against the painted frame is what keeps a click
// from addressing a row that scrolled or reflowed since the reader saw it.
type statusPanelHits struct {
	painted   bool
	tabRow    int
	tabs      []statusPanelHit
	search    statusPanelRect
	hasSearch bool
	rangeRow  int
	windows   []statusPanelHit
}

func (hits statusPanelHits) tabAt(column int) (statusPanelTab, bool) {
	for _, hit := range hits.tabs {
		if column >= hit.start && column < hit.end {
			return hit.tab, true
		}
	}

	return statusTabStatus, false
}

func (hits statusPanelHits) windowAt(column int) (statusStatsRange, bool) {
	for _, hit := range hits.windows {
		if column >= hit.start && column < hit.end {
			return hit.window, true
		}
	}

	return statusStatsAllTime, false
}

// statusPanelHitMap records the addressable regions of the frame about to be
// painted.
func (m *Model) statusPanelHitMap(
	width, chromeRows, bodyHeight int,
	lines []statusPageLine,
) statusPanelHits {
	hits := statusPanelHits{painted: true, tabRow: statusTabBarRow}
	titles := m.statusRouteTabTitles()
	column := ansi.StringWidth(statusLabelIndent)
	for tab := range statusPanelTabCount {
		span := ansi.StringWidth(titles[tab])
		hits.tabs = append(hits.tabs, statusPanelHit{start: column, end: column + span, tab: tab})
		column += span + ansi.StringWidth(statusTabGap)
	}
	if m.route.statusTab == statusTabConfig {
		hits.hasSearch = true
		hits.search = statusPanelRect{
			top: statusTabBarRow + 1, bottom: chromeRows, left: 0, right: width,
		}
	}
	for index, line := range lines {
		if line.hot != statusHotRange {
			continue
		}
		row := chromeRows + index - m.route.offset
		if row < chromeRows || row >= chromeRows+bodyHeight {
			continue
		}
		hits.rangeRow = row
		column = ansi.StringWidth(statusLabelIndent)
		labels := m.statusStatsWindowLabels()
		for window := range statusStatsRangeCount {
			span := ansi.StringWidth(labels[window])
			hits.windows = append(hits.windows, statusPanelHit{
				start: column, end: column + span, window: window,
			})
			column += span + ansi.StringWidth(statusWindowGap)
		}

		break
	}

	return hits
}

// statusRouteMouse resolves one pointer event against the panel frame the reader
// saw. Only a left press on a painted control acts, and motion and drag are
// ignored: the panel has nothing to drag.
func (m *Model) statusRouteMouse(message tea.MouseMsg) tea.Cmd {
	click, ok := message.(tea.MouseClickMsg)
	if !ok || click.Button != tea.MouseLeft {
		return nil
	}
	hits := m.route.statusHits
	if !hits.painted {
		return nil
	}
	row, column := click.Y, click.X
	if row == hits.tabRow {
		if tab, ok := hits.tabAt(column); ok {
			return m.selectStatusTab(tab)
		}
	}
	if hits.search.contains(row, column) {
		m.route.search.Focus()
		m.setLayout()

		return nil
	}
	if row == hits.rangeRow {
		if window, ok := hits.windowAt(column); ok && window != m.route.statusRange {
			m.route.statusRange = window
			m.setLayout()
		}
	}

	return nil
}

// statusRouteDataMsg carries the panel's history and integration snapshot. The
// panel reads header-level Session metadata and the published MCP generation, so
// it stays available while a turn runs.
type statusRouteDataMsg struct {
	generation  uint64
	sessions    session.MetadataListing
	projections map[string]usageProjectionSnapshot
	mcp         coding.MCPSnapshot
	err         error
}

// openStatusRoute reads the runtime status into its own full-area panel. It is
// read-only, so a running turn keeps streaming into the deferred parent
// projection while the panel is on screen.
func (m *Model) openStatusRoute() tea.Cmd {
	previous := m.composer.Snapshot()

	return m.requestRouteOpen(routeOpenRequest{
		kind: routeStatus, previousInput: previous.display,
		previousComposer:    previous.clone(),
		hasPreviousComposer: true,
	})
}

func (m *Model) activateStatusRoute(previous composerSnapshot) tea.Cmd {
	activityWasVisible := m.activityClockVisible()
	// The runtime reads the workspace only while idle, so a panel opened over a
	// running turn starts without the repository summary instead of failing. The
	// Session store is only read once a page that needs it is opened.
	idle := m.actionContext() == contextIdle
	m.routeSeq++
	m.route = routeState{
		kind:                routeStatus,
		generation:          m.routeSeq,
		loading:             idle,
		statusTab:           statusTabStatus,
		search:              newRouteSearch(m.theme, m.options.NoColor),
		previousInput:       previous.display,
		previousComposer:    previous.clone(),
		hasPreviousComposer: true,
	}
	m.route.search.Placeholder = statusSearchPlaceholder
	m.route.search.Blur()
	m.composer.Reset()
	m.setLayout()
	if !idle {
		return m.startActivityClock(activityWasVisible)
	}

	return tea.Batch(m.loadWorkspaceStatus(), m.startActivityClock(activityWasVisible))
}

// loadStatusRouteData reads the local Session store and the published MCP
// generation. Neither needs an idle runtime. Every listed Session's usage
// projection is read in the same pass, so the Stats page reduces the store
// without a per-frame read. The Session-store read uses the route's own context,
// so closing the panel or starting a replacement read stops it instead of
// letting it finish for a result nobody reads.
func (m *Model) loadStatusRouteData(ctx context.Context) tea.Cmd {
	generation := m.route.generation

	return func() tea.Msg {
		sessions, sessionErr := m.controller.ListAllSessions(ctx)
		snapshot, mcpErr := m.controller.MCP(m.ctx)

		return statusRouteDataMsg{
			generation:  generation,
			sessions:    sessions,
			projections: m.readStatusProjections(sessions),
			mcp:         snapshot.Clone(),
			err:         errors.Join(sessionErr, mcpErr),
		}
	}
}

// deriveStatusRouteContext cancels the panel's previous Session-store read and
// returns a fresh child of the program context, so the route owns exactly one
// live read at a time.
func (m *Model) deriveStatusRouteContext() context.Context {
	if m.route.statusCancel != nil {
		m.route.statusCancel()
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.route.statusCancel = cancel

	return ctx
}

// readStatusProjections reads every listed Session's sidecar in the listing's
// off-loop command. A Session without a usable projection is remembered as such,
// so one bad file contributes nothing and never blanks the page.
func (m *Model) readStatusProjections(sessions session.MetadataListing) map[string]usageProjectionSnapshot {
	directory := strings.TrimSpace(m.options.SessionsDirectory)
	if directory == "" || len(sessions.Sessions) == 0 {
		return nil
	}

	projections := make(map[string]usageProjectionSnapshot, len(sessions.Sessions))
	for _, meta := range sessions.Sessions {
		projection, read := readUsageProjectionStatus(directory, meta.ID)
		projections[meta.ID] = usageProjectionSnapshot{projection: projection, read: read}
	}

	return projections
}

func (m *Model) applyStatusPanelData(message statusRouteDataMsg) {
	if m.route.kind != routeStatus || message.generation != m.route.generation {
		return
	}
	m.route.statusDataLoading = false
	m.route.statusDataErr = message.err
	m.route.statusSessions = message.sessions
	m.route.statusProjections = message.projections
	m.route.statusMCP = message.mcp
	m.setLayout()
}

// applyStatusRepositoryData folds the asynchronous repository read into the open
// panel. The rest of the page is composed from live state on demand, so only the
// summary, the failure and the layout land here.
func (m *Model) applyStatusRepositoryData(message workspaceStatusResultMsg) {
	if message.err == nil {
		m.worktreeSummary = compactWorktreeSummary(message.status)
	}
	m.route.loading = false
	m.route.err = message.err
	m.setLayout()
}

func (m *Model) updateStatusRouteKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.statusSearching() {
		return m.updateStatusSearchKey(message)
	}
	switch message.String() {
	case keyEscape, keyCtrlC:
		return m, m.closeRouteToParent()
	case keyTab, keyRight, "l":
		return m, m.selectStatusTab(m.route.statusTab.step(1))
	case "shift+tab", keyLeft, "h":
		return m, m.selectStatusTab(m.route.statusTab.step(-1))
	case "/":
		if m.route.statusTab == statusTabConfig {
			m.route.search.Focus()
			m.setLayout()
		}

		return m, nil
	case "r":
		return m, m.statusTabKey()
	case "R":
		return m, m.reloadStatusRouteData()
	}

	return m, m.scrollStatusPage(message)
}

// updateStatusSearchKey keeps tab switching and closing available while the
// search box has the keyboard, exactly like the reference footer advertises;
// Enter and Down return to the list.
func (m *Model) updateStatusSearchKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch message.String() {
	case keyEscape, keyCtrlC:
		return m, m.closeRouteToParent()
	case keyTab, keyRight:
		return m, m.selectStatusTab(m.route.statusTab.step(1))
	case "shift+tab", keyLeft:
		return m, m.selectStatusTab(m.route.statusTab.step(-1))
	case keyEnter, keyDown, "j":
		m.stopStatusSearch()

		return m, nil
	}
	before := m.route.search.Value()
	var command tea.Cmd
	m.route.search, command = m.route.search.Update(message)
	if m.route.search.Value() != before {
		m.route.offset = 0
	}

	return m, command
}

// statusSearching reports whether the Config page's search box owns the keyboard.
func (m *Model) statusSearching() bool {
	return m.route.kind == routeStatus && m.route.statusTab == statusTabConfig &&
		m.route.search.Focused()
}

func (m *Model) stopStatusSearch() {
	m.route.search.Blur()
	m.setLayout()
}

// selectStatusTab switches pages, resetting the shared scroll window and
// returning the read a page that reduces the Session store needs.
func (m *Model) selectStatusTab(tab statusPanelTab) tea.Cmd {
	m.stopStatusSearch()
	m.route.statusTab = tab
	m.route.offset = 0
	m.setLayout()

	return tea.Batch(m.requestStatusPanelData(), m.requestUsageProjection())
}

// requestUsageProjection reads this Session's sidecar when the Usage page is
// opened, so its second source is on disk rather than inferred from the tally.
func (m *Model) requestUsageProjection() tea.Cmd {
	if m.route.statusTab != statusTabUsage {
		return nil
	}

	return m.loadUsageProjection()
}

// requestStatusPanelData starts the Session-store read the first time a page that
// reduces it is opened, so a glance at the Status page reads nothing.
func (m *Model) requestStatusPanelData() tea.Cmd {
	if m.route.statusDataRequested ||
		(m.route.statusTab != statusTabUsage && m.route.statusTab != statusTabStats &&
			m.route.statusTab != statusTabModels) {
		return nil
	}
	m.route.statusDataRequested = true
	m.route.statusDataLoading = true
	m.setLayout()

	return m.loadStatusRouteData(m.deriveStatusRouteContext())
}

// statusTabKey applies the tab-specific key: the repository read on Status, and
// the time range on Stats, matching the reference panel's split.
func (m *Model) statusTabKey() tea.Cmd {
	switch m.route.statusTab {
	case statusTabStats, statusTabModels:
		m.route.statusRange = m.route.statusRange.next()

		return nil
	case statusTabUsage:
		return tea.Batch(m.reloadStatusRouteData(), m.reloadUsageProjection())
	case statusTabStatus:
		if m.route.loading || !m.statusRepositoryReadable() {
			return nil
		}
		m.route.loading = true
		m.route.err = nil
		m.setLayout()

		return m.loadWorkspaceStatus()
	default:
		return m.reloadStatusRouteData()
	}
}

// reloadStatusRouteData re-reads the page's loaded data, which is also the retry
// path when a read failed. A replacement read supersedes the previous one: the
// generation moves on, so a result that raced the reload is dropped, and the
// previous read is cancelled rather than left to finish unread.
func (m *Model) reloadStatusRouteData() tea.Cmd {
	m.routeSeq++
	m.route.generation = m.routeSeq
	m.route.statusDataRequested = true
	m.route.statusDataLoading = true
	m.route.statusDataErr = nil
	m.setLayout()

	return m.loadStatusRouteData(m.deriveStatusRouteContext())
}

// scrollStatusPage applies one scrolling key to the current page.
func (m *Model) scrollStatusPage(message tea.KeyPressMsg) tea.Cmd {
	chrome, _, _ := m.statusRouteChrome(max(1, m.width))
	visible := m.statusRouteBodyHeight(statusChromeRows(chrome))
	maximum := m.statusRouteMaximumOffset()
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

// scrollStatusRoute moves the panel by one wheel notch.
func (m *Model) scrollStatusRoute(lines int) {
	m.route.offset = min(
		max(0, m.route.offset+lines),
		m.statusRouteMaximumOffset(),
	)
}

// statusRepositoryReadable reports whether the runtime will answer a workspace
// read: it refuses one while a turn owns it, so the panel defers that row rather
// than showing a busy failure.
func (m *Model) statusRepositoryReadable() bool {
	return m.actionContext() == contextIdle
}

func (m *Model) statusRouteView() tea.View {
	width := max(1, m.width)
	height := max(1, m.height)
	// The page is composed once per frame: the body, the label column and the pair
	// column all read the same lines instead of rebuilding them.
	chrome, searchX, searchY := m.statusRouteChrome(width)
	chromeRows := statusChromeRows(chrome)
	bodyHeight := m.statusRouteBodyHeight(chromeRows)
	lines := m.statusVisiblePageLines()
	labelWidth := m.statusLabelColumn(lines)
	pairColumn := m.statusPairColumn(lines, labelWidth)
	parts := make([]string, 0, chromeRows+bodyHeight+2)
	parts = append(parts, chrome...)
	parts = append(parts, m.statusRouteBody(width, lines, labelWidth, pairColumn, bodyHeight)...)
	parts = append(parts, m.sessionPickerSeparator(width), m.statusRouteFooterText(width))
	m.route.statusHits = m.statusPanelHitMap(width, chromeRows, bodyHeight, lines)

	return m.searchableRouteView(truncateHeight(strings.Join(parts, "\n"), height), searchX, searchY)
}

// statusChromeRows counts the rows a chrome slice occupies: the Config page's
// search box is one element that draws three rows.
func statusChromeRows(chrome []string) int {
	rows := 0
	for _, entry := range chrome {
		rows += lipgloss.Height(entry)
	}

	return rows
}

// statusRouteChrome is the pinned header: the invocation that opened the panel,
// the tab bar, and — on the Config page — the search box.
func (m *Model) statusRouteChrome(width int) ([]string, int, int) {
	chrome := []string{
		renderUserMessage("/status", width, m.theme, m.options.NoColor),
		m.sessionPickerSeparator(width),
		m.statusRouteTabBar(width),
	}
	searchX, searchY := 0, 0
	if m.route.statusTab == statusTabConfig {
		box, boxX, boxY := m.routeSearchBox(width)
		searchX = boxX
		searchY = len(chrome) + boxY
		chrome = append(chrome, box)

		return chrome, searchX, searchY
	}
	chrome = append(chrome, "")

	return chrome, searchX, searchY
}

// statusRouteTabBar draws the pages with the active one highlighted. Under
// NO_COLOR the active page is bracketed so the selection survives without styling.
// The bar is fitted to the panel width like the chrome rows around it, so a narrow
// terminal clips the trailing pages instead of wrapping the header.
func (m *Model) statusRouteTabBar(width int) string {
	return ansi.Truncate(
		statusLabelIndent+strings.Join(m.statusRouteTabTitles(), statusTabGap),
		max(1, width), "…",
	)
}

// statusRouteTabTitles renders one title per page, emphasizing the active one. The
// pointer map measures these exact strings, so a click lands on the title it saw.
func (m *Model) statusRouteTabTitles() []string {
	titles := make([]string, 0, int(statusPanelTabCount))
	for tab := range statusPanelTabCount {
		title := tab.title()
		switch {
		case tab != m.route.statusTab:
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

// statusRouteBody renders the visible window of one composed page.
func (m *Model) statusRouteBody(
	width int,
	lines []statusPageLine,
	labelWidth, pairColumn, visible int,
) []string {
	m.route.offset = min(max(0, m.route.offset), max(0, len(lines)-visible))
	end := min(len(lines), m.route.offset+visible)
	rows := make([]string, 0, visible)
	for _, line := range lines[m.route.offset:end] {
		rows = append(rows, m.renderStatusPageLine(line, labelWidth, pairColumn, width)...)
	}
	for len(rows) < visible {
		rows = append(rows, "")
	}

	return rows
}

// statusRouteBodyHeight reserves the pinned chrome and footer, and never returns a
// height that would push them off screen.
func (m *Model) statusRouteBodyHeight(chromeHeight int) int {
	if fixed := chromeHeight + 2; m.height > fixed {
		return max(1, m.height-fixed)
	}

	return 1
}

// statusRouteMaximumOffset is the last scrollable offset of the current page.
func (m *Model) statusRouteMaximumOffset() int {
	chrome, _, _ := m.statusRouteChrome(max(1, m.width))

	return max(0, len(m.statusVisiblePageLines())-m.statusRouteBodyHeight(statusChromeRows(chrome)))
}

// statusVisiblePageLines is the current page plus the Config page's filter.
func (m *Model) statusVisiblePageLines() []statusPageLine {
	if m.route.statusTab != statusTabConfig {
		return m.statusPageLines(m.route.statusTab)
	}
	query := strings.ToLower(strings.TrimSpace(m.route.search.Value()))
	lines := m.statusPageLines(statusTabConfig)
	if query == "" {
		return lines
	}
	// A section heading survives only while one of its own rows matches, so the
	// filtered list never shows an empty group.
	filtered := make([]statusPageLine, 0, len(lines))
	pending := statusPageLine{}
	for _, line := range lines {
		switch line.kind {
		case statusLineHeading:
			pending = line
		case statusLineField:
			if !strings.Contains(strings.ToLower(line.label), query) &&
				!strings.Contains(strings.ToLower(line.value), query) {
				continue
			}
			if pending.kind == statusLineHeading {
				if len(filtered) > 0 {
					filtered = append(filtered, statusBlank())
				}
				filtered = append(filtered, pending)
				pending = statusPageLine{}
			}
			filtered = append(filtered, line)
		default:
			filtered = append(filtered, line)
		}
	}
	if len(filtered) == 0 {
		return []statusPageLine{statusText(
			statusLabelIndent + "No setting matches " + strings.TrimSpace(m.route.search.Value()) + ".",
		)}
	}

	return filtered
}

// statusLabelColumn is the width of the label column: the widest label on the
// page, capped so a long label cannot push every value off screen.
func (m *Model) statusLabelColumn(lines []statusPageLine) int {
	width := 0
	for _, line := range lines {
		if line.kind == statusLineField {
			width = max(width, ansi.StringWidth(line.label))
		}
		if line.kind == statusLinePair {
			width = max(width, ansi.StringWidth(line.label), ansi.StringWidth(line.label2))
		}
	}

	return min(width, statusLabelColumnMax)
}

// statusPairColumn is where the second column of a pair row starts.
func (m *Model) statusPairColumn(lines []statusPageLine, labelWidth int) int {
	width := 0
	for _, line := range lines {
		if line.kind != statusLinePair {
			continue
		}
		width = max(width, labelWidth+2+ansi.StringWidth(line.value))
	}

	return width + statusPairGap
}

// renderStatusPageLine lays one page line out for the terminal width.
func (m *Model) renderStatusPageLine(
	line statusPageLine,
	labelWidth, pairColumn, width int,
) []string {
	switch line.kind {
	case statusLineBlank:
		return []string{""}
	case statusLineHeading:
		text := statusLabelIndent + line.text
		if m.options.NoColor {
			return []string{text}
		}

		return []string{lipgloss.NewStyle().Bold(true).Foreground(
			paletteFor(m.theme).session,
		).Render(text)}
	case statusLineText:
		return []string{ansi.Truncate(line.text, max(1, width), "…")}
	case statusLinePair:
		return m.renderStatusPairLines(line, labelWidth, pairColumn, width)
	default:
		return wrapStatusField(line.label, line.value, labelWidth, width)
	}
}

// renderStatusPairLines lays out two label/value pairs on one line, wrapping the
// second pair onto its own line when the terminal is too narrow for both.
func (m *Model) renderStatusPairLines(
	line statusPageLine,
	labelWidth, pairColumn, width int,
) []string {
	left := statusLabelIndent + padCells(line.label, labelWidth+1) + line.value
	right := padCells(line.label2, labelWidth+1) + line.value2
	if ansi.StringWidth(left)+statusPairGap+ansi.StringWidth(right) > width {
		return []string{left, statusLabelIndent + right}
	}

	return []string{left + strings.Repeat(" ", max(1, pairColumn-ansi.StringWidth(left))) + right}
}

// statusRouteFooterText states the keys the current page answers to, in the same
// words the reference panel's footer uses.
func (m *Model) statusRouteFooterText(width int) string {
	if m.statusSearching() {
		return ansi.Truncate("</>/tab to switch · ↓ to return · Esc to close", max(1, width), "…")
	}
	hint := "</>/tab to switch · ↑/↓ " + wheelHintNavigation
	switch m.route.statusTab {
	case statusTabConfig:
		hint += " · / search"
	case statusTabStats, statusTabModels:
		hint += " · r range · R reload"
	case statusTabUsage:
		hint += " · R reload"
	default:
		hint += " · " + m.statusRepositoryHintKey()
	}
	hint += " · Esc to close"

	return ansi.Truncate(hint, max(1, width), "…")
}

// statusRepositoryHintKey advertises the repository retry only where the runtime
// will answer the read.
func (m *Model) statusRepositoryHintKey() string {
	if m.route.loading || !m.statusRepositoryReadable() {
		return "R reload"
	}

	return "r re-read repository · R reload"
}

// statusPageWidth is the width a page composes its own content for.
func (m *Model) statusPageWidth() int {
	return max(1, m.width)
}

// statusNow is the clock a page reads, so its output stays deterministic in tests.
func (m *Model) statusNow() time.Time {
	return time.Now()
}
