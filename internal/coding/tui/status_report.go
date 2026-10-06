//nolint:wsl_v5 // Panel pages, their rows, and the shared formatters stay adjacent.
package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/config"
	codingmcp "github.com/rsbin1178/pips/internal/coding/mcp"
	"github.com/rsbin1178/pips/internal/coding/session"
)

// statusPanelTab identifies one page of the status panel, in draw order.
type statusPanelTab uint8

const (
	statusTabStatus statusPanelTab = iota
	statusTabConfig
	statusTabUsage
	statusTabStats
	statusPanelTabCount
)

// statusPanelTabTitles are the tab labels the panel draws.
var statusPanelTabTitles = [...]string{"Status", "Config", "Usage", "Stats"}

func (tab statusPanelTab) title() string {
	if int(tab) >= len(statusPanelTabTitles) {
		return ""
	}

	return statusPanelTabTitles[tab]
}

// step returns the tab delta notches away, wrapping at both ends.
func (tab statusPanelTab) step(delta int) statusPanelTab {
	count := int(statusPanelTabCount)

	return statusPanelTab(((int(tab)+delta)%count + count) % count)
}

// statusLineKind distinguishes the rows one page is made of.
type statusLineKind uint8

const (
	statusLineBlank statusLineKind = iota
	statusLineHeading
	statusLineField
	statusLinePair
	statusLineText
)

// statusHot marks a page line a pointer can address, so the panel's hit map can
// find it without matching rendered text.
type statusHot uint8

const (
	statusHotNone statusHot = iota
	statusHotRange
)

// statusPageLine is one line of a panel page. A field carries a label and a
// value, a pair carries two of them side by side, and text carries pre-rendered
// content such as the activity grid.
type statusPageLine struct {
	kind   statusLineKind
	hot    statusHot
	label  string
	value  string
	label2 string
	value2 string
	text   string
}

func statusHeading(text string) statusPageLine {
	return statusPageLine{kind: statusLineHeading, text: text}
}
func statusField(label, value string) statusPageLine {
	return statusPageLine{kind: statusLineField, label: label, value: value}
}
func statusPair(label, value, label2, value2 string) statusPageLine {
	return statusPageLine{
		kind: statusLinePair, label: label, value: value, label2: label2, value2: value2,
	}
}
func statusText(text string) statusPageLine { return statusPageLine{kind: statusLineText, text: text} }
func statusBlank() statusPageLine           { return statusPageLine{} }

// Layout constants for a panel page.
const (
	statusLabelIndent    = "  "
	statusLabelColumnMax = 22
	statusPairGap        = 3
)

// statusPageLines composes one page. Pages read live state, so a theme switch or
// a control result is reflected on the next frame without an invalidation step.
func (m *Model) statusPageLines(tab statusPanelTab) []statusPageLine {
	switch tab {
	case statusTabConfig:
		return m.statusConfigLines()
	case statusTabUsage:
		return m.statusUsageLines()
	case statusTabStats:
		return m.statusStatsLines()
	default:
		return m.statusOverviewLines()
	}
}

// statusOverviewLines is the default page: who this session is, which model
// answers, what the runtime may do, and which build is running.
func (m *Model) statusOverviewLines() []statusPageLine {
	modelState := m.controller.Model()
	modeState := m.controller.Mode()
	permissions := m.permissionState()
	lines := []statusPageLine{statusHeading("Session")}
	if build := statusBuildText(m.options.Build); build != "" {
		lines = append(lines, statusField("Version", build))
	}
	lines = append(lines,
		statusField("Workspace", m.options.Workspace),
		statusField("Session", m.statusSessionLabel()),
		statusField("Session kind", m.statusSessionKind()),
		statusField("Mode", string(modeState.Current)+modeStatusSuffix(modeState)),
	)
	lines = append(lines, statusBlank(), statusHeading("Model"))
	lines = append(lines,
		statusField("Model", modelState.Resolved.Ref.String()),
		statusField("Name", modelDisplayName(m.state.Provider, m.state.ModelID)),
	)
	lines = append(lines,
		statusField("Variant", valueOrDefault(modelState.Resolved.Variant)),
		statusField("Reasoning", reasoningOrDefault(modelState.Resolved.ReasoningLevel)),
		statusField("Protocol", statusValueOrUnknown(strings.TrimSpace(string(modelState.Resolved.Protocol)))),
		statusField("Endpoint", statusValueOrUnknown(resolvedEndpointText(modelState.Resolved.Endpoint))),
		statusField("Context window", statusTokenLimit(modelState.Resolved.Limits.ContextWindow)),
		statusField("Request output", optionalInt(modelState.Resolved.Options.MaxOutputTokens)),
		statusField("MCP servers", m.statusMCPSummary()),
	)
	lines = append(lines, statusBlank(), statusHeading("Permissions"))
	lines = append(lines,
		statusField("Sandbox", permissionModeText(permissions.SandboxProfile.Filesystem.Effective)),
		statusField("Approval", permissionApprovalText(permissions.ApprovalPolicy.Effective)),
		statusField("Network", permissionStatusNetworkText(
			permissions.SandboxProfile.Network.Effective,
			permissions.SandboxProfile.NetworkEnforced,
		)),
	)
	lines = append(lines, statusBlank(), statusHeading("Runtime"))
	lines = append(lines,
		statusField("Phase", phaseText(m.state.Phase)),
		statusField("Repository", m.repositoryStatusText()),
	)
	lines = append(lines, m.statusRuntimeRows()...)
	lines = append(lines, m.statusIntegrationRows()...)

	return lines
}

// statusIntegrationRows lists the integration notices the runtime reported,
// keeping the human title and message rather than the component/code pair.
func (m *Model) statusIntegrationRows() []statusPageLine {
	listed := visibleDiagnostics(m.state.Diagnostics)
	if len(listed) == 0 {
		return nil
	}
	lines := []statusPageLine{statusBlank(), statusHeading("Integrations")}
	for _, diagnostic := range listed {
		value := diagnosticTitle(diagnostic)
		if message := diagnosticBody(diagnostic); message != "" {
			value += ": " + message
		}
		lines = append(lines, statusText(statusLabelIndent+value))
	}

	return lines
}

// statusRuntimeRows are the rows a running runtime may or may not have to report.
func (m *Model) statusRuntimeRows() []statusPageLine {
	configState := m.controller.Config()
	modeState := m.controller.Mode()
	modelState := m.controller.Model()
	rows := []statusPageLine{
		statusField("Compaction", compactionStatusText(configState.Compaction)),
		statusField("Tool search", statusOnOff(configState.ToolSearch)),
	}
	if modelState.Overridden || modeState.Overridden {
		overrides := make([]string, 0, 2)
		if modelState.Overridden {
			overrides = append(overrides, "model")
		}
		if modeState.Overridden {
			overrides = append(overrides, "mode")
		}
		rows = append(rows, statusField("Overrides", strings.Join(overrides, ", ")))
	}
	if kind := strings.TrimSpace(string(m.state.Approval.Kind)); kind != "" {
		rows = append(rows, statusField("Pending approval", humanizeStatusCode(kind)))
	}
	if m.controller.Detached() {
		rows = append(rows, statusField("Detached", "the runtime is no longer attached to this session"))
	}
	if failure := m.state.LastError; failure != nil {
		rows = append(rows, statusField("Last error", humanizeStatusCode(failure.Code)))
	}

	return rows
}

// statusConfigLines lists the effective settings this process runs with. Values
// are the same human-facing words the pickers show: configuration paths,
// environment names, sources and raw enum names never appear here.
func (m *Model) statusConfigLines() []statusPageLine {
	configState := m.controller.Config()
	modelState := m.controller.Model()
	permissions := m.permissionState()
	tui := configState.TUI
	lines := []statusPageLine{statusHeading("Execution")}
	lines = append(lines,
		statusField("Sandbox", permissionModeText(permissions.SandboxProfile.Filesystem.Effective)),
		statusField("Approval", permissionApprovalText(permissions.ApprovalPolicy.Effective)),
		statusField("Network", permissionStatusNetworkText(
			permissions.SandboxProfile.Network.Effective,
			permissions.SandboxProfile.NetworkEnforced,
		)),
		statusField("Operating mode", string(configState.Mode)),
	)
	lines = append(lines, statusBlank(), statusHeading("Model"))
	lines = append(lines,
		statusField("Model", configState.Model.String()),
		statusField("Variant", statusValueOrDefault(configState.Variant)),
		statusField("Reasoning", reasoningOrDefault(configState.Reasoning)),
		statusField("Context window", statusTokenLimit(modelState.Resolved.Limits.ContextWindow)),
		statusField("Request output", optionalInt(modelState.Resolved.Options.MaxOutputTokens)),
	)
	lines = append(lines, statusBlank(), statusHeading("Runtime"))
	lines = append(lines,
		statusField("Compaction", compactionStatusText(configState.Compaction)),
		statusField("Tool search", statusOnOff(configState.ToolSearch)),
		statusField("Dynamic subagents", statusOnOff(configState.DynamicSubagents)),
	)
	lines = append(lines, statusBlank(), statusHeading("Interface"))
	lines = append(lines,
		statusField("Theme", valueOrDefault(m.themeSelection)),
		statusField("Screen", m.statusScreenLabel()),
		statusField("Alt screen", statusValueOrDefault(tui.AltScreen)),
		statusField("Mouse", statusOnOff(m.mouseReportingEnabled())),
		statusField("Status line", m.statusLineSummary()),
		statusField("Exit output", statusValueOrDefault(tui.ExitOutput)),
		statusField("Thinking blocks", statusOnOff(tui.ShowThinkingBlocks)),
	)

	return lines
}

// statusUsageLines reports this session's local usage. pips tracks the last
// completed interaction rather than a lifetime total, and prices nothing, so the
// page states what it measured instead of an estimate.
func (m *Model) statusUsageLines() []statusPageLine {
	lines := []statusPageLine{statusHeading("Session")}
	lines = append(lines,
		statusField("Context", m.contextUseText()),
		statusField("Cache", m.cacheUseText()),
		statusField("Last turn", m.statusLastTurnUsage()),
		statusField("Workspace changes", m.statusWorkspaceChangesText()),
		statusField("Started", m.statusSessionStartedText()),
	)
	if models := m.modelUsage.models(); len(models) > 0 {
		lines = append(lines, statusBlank(), statusHeading("By model"))
		for _, entry := range models {
			lines = append(lines, statusField(entry.name, entry.text()))
		}
		lines = append(lines, statusText(
			statusLabelIndent+"Counted by this process; a resumed session starts over.",
		))
	}
	lines = append(lines, m.statusUsageProjectionLines()...)
	if m.route.statusDataErr != nil {
		lines = append(lines, statusBlank(), statusText(
			"Session history unavailable: "+safeError(m.route.statusDataErr),
		))
	}

	return lines
}

// statusUsageProjectionLines is the Usage page's on-disk source: the per-model
// totals this Session's projection holds. A Session with no usable projection
// reads as unknown rather than an estimate from the transcript.
func (m *Model) statusUsageProjectionLines() []statusPageLine {
	view := m.usageProjection
	lines := []statusPageLine{statusBlank(), statusHeading("This session on disk")}
	switch {
	case view.loading:
		lines = append(lines, statusText(
			statusLabelIndent+"Reading the session's projection…",
		))
	case !view.loaded || !view.usable:
		lines = append(lines, statusText(
			statusLabelIndent+"History unknown: no usable projection for this session.",
		))
	default:
		for _, ref := range view.models() {
			lines = append(lines, statusField(ref, usageTotalsText(view.totals[ref])))
		}
		lines = append(lines, statusText(statusLabelIndent+m.usageProjectionRangeText(view)))
	}
	if view.err != nil {
		lines = append(lines, statusText(
			statusLabelIndent+"Last write failed: "+safeError(view.err),
		))
	}

	return lines
}

// usageProjectionRangeText names what the projection covers. The file keeps only
// the last interaction apart from the earlier totals, so the range is stated by
// interaction rather than by a turn count the state does not carry.
func (m *Model) usageProjectionRangeText(view usageProjectionView) string {
	if view.incomplete {
		return "History incomplete: turns before the last rewrite are not included."
	}
	if view.lastInteraction != "" {
		return "Covers this session's turns through " + view.lastInteraction + "."
	}

	return "No completed turn has been projected yet."
}

// usageTotalsText renders one model's projected totals the way the in-process rows read.
func usageTotalsText(usage coding.TokenUsage) string {
	return fmt.Sprintf(
		"%s input · %s output · %s cache read · %s cache write",
		tokenCountText(usage.InputTokens),
		tokenCountText(usage.OutputTokens),
		tokenCountText(usage.CachedInputTokens),
		tokenCountText(usage.CacheWriteTokens),
	)
}

// statusStatsLines is the local activity page: the year-long grid, the range the
// numbers describe, and those numbers.
func (m *Model) statusStatsLines() []statusPageLine {
	lines := []statusPageLine{statusHeading("Activity")}
	// The store read is asynchronous, so the page says it is reading rather than
	// reporting an empty store as if it had looked.
	if m.route.statusDataLoading {
		return append(lines, statusText(
			statusLabelIndent+m.activityNotice("Reading the session store…"),
		))
	}
	width := max(1, m.statusPageWidth())
	activity := newSessionActivity(m.route.statusSessions.Sessions)
	now := m.statusNow()
	if activity.total == 0 {
		lines = append(lines, statusText(statusLabelIndent+"No sessions recorded yet."))
	} else {
		for _, row := range m.statusHeatmapRows(activity, now, width) {
			lines = append(lines, statusText(row))
		}
		lines = append(lines,
			statusBlank(),
			statusText(statusLabelIndent+m.statusHeatmapLegend()),
			statusPageLine{
				kind: statusLineText, hot: statusHotRange,
				text: statusLabelIndent + m.statusStatsRangeRow(),
			},
		)
	}
	summary := activity.summary(m.route.statusRange, now)
	lines = append(lines, statusBlank(), statusHeading("Totals"))
	lines = append(lines,
		statusPair(
			"Sessions", strconv.Itoa(summary.sessions),
			"Active days", m.statusActiveDaysText(summary),
		),
		statusPair(
			"Current streak", statusDaysText(summary.currentStreak),
			"Longest streak", statusDaysText(summary.longestStreak),
		),
		statusPair(
			"Most active day", m.statusMostActiveText(summary),
			"First session", statusValueOrDefault(summary.firstDay),
		),
	)
	if unreadable := m.route.statusSessions.Unreadable; unreadable > 0 {
		lines = append(lines, statusBlank(), statusText(
			statusLabelIndent+fmt.Sprintf(
				"%d sessions could not be read and are not counted.",
				unreadable,
			),
		))
	}
	if m.route.statusDataErr != nil {
		lines = append(lines, statusBlank(), statusText(
			"Session history unavailable: "+safeError(m.route.statusDataErr),
		))
	}

	return lines
}

// statusStatsRangeRow shows the three windows the numbers can describe, with the
// active one highlighted.
func (m *Model) statusStatsRangeRow() string {
	return strings.Join(m.statusStatsWindowLabels(), statusWindowGap)
}

// statusStatsWindowLabels renders the range labels, emphasizing the active one. The
// pointer map measures these exact strings, so a click lands on the label it saw.
func (m *Model) statusStatsWindowLabels() []string {
	labels := make([]string, 0, int(statusStatsRangeCount))
	for value := range statusStatsRangeCount {
		label := value.title()
		if value == m.route.statusRange {
			if m.options.NoColor {
				label = "[" + label + "]"
			} else {
				label = lipgloss.NewStyle().Bold(true).Foreground(paletteFor(m.theme).active).Render(label)
			}
		}
		labels = append(labels, label)
	}

	return labels
}

// statusActiveDaysText pairs the active days with the window they were counted in.
func (m *Model) statusActiveDaysText(summary sessionStatsSummary) string {
	return fmt.Sprintf("%d/%d", summary.activeDays, max(1, summary.windowDays))
}

// statusMostActiveText reports the busiest day of the range with its count.
func (m *Model) statusMostActiveText(summary sessionStatsSummary) string {
	if summary.mostActiveDay == "" {
		return "none"
	}

	return fmt.Sprintf("%s (%d)", summary.mostActiveDay, summary.mostActiveCount)
}

// statusDaysText renders a streak length.
func statusDaysText(days int) string {
	if days <= 0 {
		return "none"
	}
	if days == 1 {
		return "1 day"
	}

	return fmt.Sprintf("%d days", days)
}

// statusLastTurnUsage reports the last completed interaction's tokens.
func (m *Model) statusLastTurnUsage() string {
	usage := m.state.Interaction.Usage
	if usage == (coding.TokenUsage{}) {
		return "no completed turn reported usage"
	}

	return fmt.Sprintf(
		"%s input · %s output · %s cache read · %s cache write",
		tokenCountText(usage.InputTokens),
		tokenCountText(usage.OutputTokens),
		tokenCountText(usage.CachedInputTokens),
		tokenCountText(usage.CacheWriteTokens),
	)
}

// statusWorkspaceChangesText reports the workspace diff pips has summarised. It is
// the current diff against the retained baseline, not a lifetime edit count.
func (m *Model) statusWorkspaceChangesText() string {
	changed := m.state.Changes
	if changed == nil || (changed.Files == 0 && len(changed.Entries) == 0) {
		return "none reported"
	}

	return fmt.Sprintf(
		"%d files · +%d −%d",
		max(changed.Files, len(changed.Entries)),
		changed.Additions,
		changed.Deletions,
	)
}

// statusSessionStartedText reports how long this session has been running, when
// the store has a header for it.
func (m *Model) statusSessionStartedText() string {
	if m.route.statusDataLoading {
		return "reading…"
	}
	if m.state.IsSessionProvisional() {
		return "this session is not durable yet"
	}
	for _, meta := range m.route.statusSessions.Sessions {
		if meta.ID != m.state.SessionID || meta.CreatedAt.IsZero() {
			continue
		}

		return formatSessionAge(m.statusNow().Sub(meta.CreatedAt))
	}

	return "unknown"
}

// statusSessionLabel names the durable session, or says the process has none yet.
func (m *Model) statusSessionLabel() string {
	if m.state.IsSessionProvisional() {
		return "new (not durable yet)"
	}
	if name := m.statusSessionName(); name != "" {
		return m.state.SessionID + " · " + name
	}

	return m.state.SessionID
}

// statusSessionName is the durable name this session was given, if any.
func (m *Model) statusSessionName() string {
	for _, meta := range m.route.statusSessions.Sessions {
		if meta.ID == m.state.SessionID {
			return strings.TrimSpace(meta.Name)
		}
	}

	return ""
}

// statusSessionKind names the transcript this process owns. The header listing is
// only read once the Usage or Stats page asks for it; until then the answer is the
// kind of every session this process can submit to, which is a conversation.
func (m *Model) statusSessionKind() string {
	for _, meta := range m.route.statusSessions.Sessions {
		if meta.ID != m.state.SessionID {
			continue
		}
		switch meta.Kind {
		case session.KindSubagent:
			return "subagent"
		case session.KindTeamWorker:
			return "team worker"
		}

		return "interactive"
	}

	return "interactive"
}

// statusMCPSummary counts the connected servers and points at the /mcp route.
func (m *Model) statusMCPSummary() string {
	if m.route.statusDataErr != nil {
		return "unavailable"
	}
	if m.route.statusDataLoading {
		return "reading…"
	}
	if len(m.route.statusMCP.Servers) == 0 {
		return "no servers"
	}
	counts := make(map[codingmcp.ServerState]int, 5)
	for _, server := range m.route.statusMCP.Servers {
		counts[server.State]++
	}
	parts := make([]string, 0, 3)
	for _, state := range []codingmcp.ServerState{
		codingmcp.ServerStateConnected,
		codingmcp.ServerStateConnecting,
		codingmcp.ServerStateFailed,
	} {
		if counts[state] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[state], state))
		}
	}
	parts = append(parts, "/mcp")

	return strings.Join(parts, " · ")
}

// statusScreenLabel names the layout this process draws.
func (m *Model) statusScreenLabel() string {
	if m.fullscreen() {
		return config.ScreenFullscreen
	}

	return config.ScreenInline
}

// statusOnOff renders a boolean setting as a word.
func statusOnOff(value bool) string {
	if value {
		return "on"
	}

	return "off"
}

// statusTokenLimit renders a token limit, or says it is unknown, so a large
// window reads at a glance.
func statusTokenLimit(value int) string {
	if value <= 0 {
		return "unknown"
	}

	return tokenCountText(value)
}

// statusValueOrDefault drops an unset string rather than printing an empty field.
func statusValueOrDefault(value string) string {
	if strings.TrimSpace(value) == "" {
		return "default"
	}

	return value
}

// statusValueOrUnknown names an unset value the runtime does not default.
func statusValueOrUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}

	return value
}

// statusBuildText joins the build stamps the running binary reported.
func statusBuildText(build BuildStamp) string {
	parts := make([]string, 0, 3)
	for _, value := range []string{build.Version, build.Commit, build.Date} {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}

	return strings.Join(parts, " · ")
}

// statusLineSummary lists the enabled status-line fields, or says the line is
// hidden when the user saved an empty selection.
func (m *Model) statusLineSummary() string {
	if len(m.statusLineItems) == 0 {
		return "hidden"
	}
	names := make([]string, 0, len(m.statusLineItems))
	for _, item := range m.statusLineItems {
		names = append(names, string(item))
	}

	return strings.Join(names, " · ")
}

// contextUseText reports the current-context gauge the same way the status-line
// item does, with the token counts beside it.
func (m *Model) contextUseText() string {
	window := m.state.ContextWindow
	if window <= 0 {
		return "unknown window"
	}
	percent := min(100, max(0, int(float64(m.state.ContextTokens)/float64(window)*100)))

	return fmt.Sprintf(
		"%d%% · %s of %s tokens",
		percent,
		tokenCountText(m.state.ContextTokens),
		tokenCountText(window),
	)
}

// cacheUseText reports the share of the last turn's prompt the provider served
// from its cache.
func (m *Model) cacheUseText() string {
	percent, ok := promptCacheHitRate(m.state.Interaction.Usage)
	if !ok {
		return "no completed turn reported a prompt size"
	}

	return fmt.Sprintf("%d%% of the last turn's prompt", percent)
}

// repositoryStatusText reports the loaded repository summary, the read in flight,
// the failure that replaced it, or the read a running turn defers.
func (m *Model) repositoryStatusText() string {
	switch {
	case m.route.loading:
		return "reading…"
	case m.route.err != nil:
		return "unavailable (Git status: " + safeError(m.route.err) + ")"
	case m.worktreeSummary != "":
		return m.worktreeSummary
	case m.actionContext() != contextIdle:
		return "available once the current turn finishes"
	default:
		return "unknown"
	}
}

// formatSessionAge renders a wall-clock age the way the reference's duration
// fields do: coarse units, most significant first.
func formatSessionAge(age time.Duration) string {
	switch {
	case age <= 0:
		return "unknown"
	case age < time.Minute:
		return "less than a minute"
	case age < time.Hour:
		return fmt.Sprintf("%dm", int(age.Minutes()))
	case age < 48*time.Hour:
		return fmt.Sprintf("%dh %dm", int(age.Hours()), int(age.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %dh", int(age.Hours())/24, int(age.Hours())%24)
	}
}

// tokenCountText groups a token count so a large window reads at a glance.
func tokenCountText(value int) string {
	text := strconv.Itoa(max(0, value))
	if len(text) <= 3 {
		return text
	}
	var builder strings.Builder
	builder.Grow(len(text) + len(text)/3)
	for index, digit := range text {
		if index > 0 && (len(text)-index)%3 == 0 {
			builder.WriteByte(',')
		}
		builder.WriteRune(digit)
	}

	return builder.String()
}

// wrapStatusField lays one field out: the label occupies the shared column and a
// long value wraps under its own start.
func wrapStatusField(label, value string, labelWidth, width int) []string {
	head := statusLabelIndent + padCells(label, labelWidth+1)
	available := max(1, width-ansi.StringWidth(head))
	indent := strings.Repeat(" ", ansi.StringWidth(head))
	if ansi.StringWidth(value) <= available {
		return []string{head + value}
	}
	lines := strings.Split(ansi.Wrap(value, available, ""), "\n")
	rows := make([]string, 0, len(lines))
	for index, line := range lines {
		if index == 0 {
			rows = append(rows, head+line)

			continue
		}
		rows = append(rows, indent+line)
	}

	return rows
}
