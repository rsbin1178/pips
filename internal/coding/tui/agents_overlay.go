//nolint:wsl_v5 // Full-area agent routes keep their transition and rendering details adjacent.
package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/agent/team"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/subagent"
)

type childKind uint8

const (
	childSubagent childKind = iota
	childTeamWorker

	agentLibraryStatusAvailable = "available"
	genericSubagentLabel        = "child Agent"
)

// agentsRouteTab keeps the Agent definition catalog separate from durable
// child Runs. A definition is declarative metadata; a run is a persisted
// execution snapshot, so conflating them would make historical authority
// ambiguous. The definition tab is named "Agents" because that is what it
// lists; Runs is the run browser the same route also owns.
type agentsRouteTab uint8

const (
	agentsTabRuns agentsRouteTab = iota
	agentsTabAgents
)

// agentsRouteTabLine is the content line the tab row is drawn on, beneath the
// title line, and agentsRoutePreamble is the content line where the first item
// row starts. The pointer map measures both.
const (
	agentsRouteTabLine  = 1
	agentsRoutePreamble = 5
	agentsTabGap        = "  "
)

func agentsRouteTabs() []agentsRouteTab {
	return []agentsRouteTab{agentsTabRuns, agentsTabAgents}
}

func (tab agentsRouteTab) title() string {
	if tab == agentsTabAgents {
		return "Agents"
	}

	return "Runs"
}

type childSummary struct {
	kind           childKind
	subagent       subagent.Summary
	worker         coding.TeamAttemptView
	workerName     string
	taskTitle      string
	childSessionID string
	createdAt      time.Time
}

func subagentChildSummary(value subagent.Summary) childSummary {
	return childSummary{
		kind: childSubagent, subagent: value,
		childSessionID: value.ChildSessionID, createdAt: value.CreatedAt,
	}
}

func (value childSummary) live() bool {
	switch value.kind {
	case childSubagent:
		return value.subagent.State == subagent.StateCreated ||
			value.subagent.State == subagent.StateRunning
	case childTeamWorker:
		return teamAttemptActivityVisible(value.worker)
	default:
		return false
	}
}

func (value childSummary) searchableText() string {
	if value.kind == childSubagent {
		return strings.Join([]string{
			value.subagent.TaskPreview,
			subagentDisplayName(value.subagent.Identity, value.subagent.Role),
			value.subagent.Identity.ID,
			string(value.subagent.State),
		}, " ")
	}

	return strings.Join([]string{
		value.workerName, value.taskTitle, string(value.worker.LifecycleState),
		string(value.worker.Activity), string(value.worker.ResourceState),
	}, " ")
}

func (value childSummary) sameIdentity(other childSummary) bool {
	if value.kind != other.kind {
		return false
	}
	if value.kind == childSubagent {
		return value.childSessionID != "" && value.childSessionID == other.childSessionID
	}

	return value.worker.Target == other.worker.Target
}

type subagentToolEnvelope struct {
	Schema         string `json:"schema"`
	ChildSessionID string `json:"child_session_id"`
	AgentID        string `json:"agent_id"`
}

type agentsRouteDataMsg struct {
	generation uint64
	agents     []subagent.Summary
	teamViews  []coding.TeamView
	library    coding.AgentLibrary
	err        error
}

type subagentRouteDataMsg struct {
	detail         subagent.Detail
	state          coding.State
	err            error
	hasDetail      bool
	hasState       bool
	background     bool
	generation     uint64
	childSessionID string
}

type subagentCancelResultMsg struct {
	generation     uint64
	childSessionID string
	err            error
}

//nolint:gocyclo // Exact envelopes and legacy ownership fallback share one bounded decoder.
func subagentChildSessionID(
	tool coding.ToolState,
	children []coding.SubagentState,
) string {
	text := visibleToolMessage(tool.Result)
	for offset := strings.IndexByte(text, '{'); offset >= 0; {
		var envelope subagentToolEnvelope

		decoder := json.NewDecoder(strings.NewReader(text[offset:]))
		if decoder.Decode(&envelope) == nil {
			switch envelope.Schema {
			case subagent.ResultSchema:
				if envelope.ChildSessionID != "" {
					return envelope.ChildSessionID
				}
			case subagent.SpawnResultSchema:
				if envelope.AgentID != "" {
					return envelope.AgentID
				}
			}
		}

		next := strings.IndexByte(text[offset+1:], '{')
		if next < 0 {
			break
		}

		offset += next + 1
	}

	for _, v := range slices.Backward(children) {
		if tool.Call.ID != "" && v.ParentToolCallID == tool.Call.ID {
			return v.ChildSessionID
		}
	}
	for _, v := range slices.Backward(children) {
		if v.ParentToolCallID == "" && v.ParentRunID == tool.RunID {
			return v.ChildSessionID
		}
	}

	return ""
}

func (m *Model) openAgentsRoute() tea.Cmd {
	return m.requestRouteOpen(routeOpenRequest{kind: routeAgents})
}

func (m *Model) activateAgentsRoute() tea.Cmd {
	activityWasVisible := m.activityClockVisible()
	m.routeSeq++
	m.route = routeState{kind: routeAgents, loading: true, generation: m.routeSeq}
	m.composer.Blur()
	generation := m.route.generation
	controller := m.controller
	ctx := m.ctx
	views, requests := m.teamViewsForChildSelector()

	load := func() tea.Msg {
		values, err := controller.ListSubagents(ctx)
		var library coding.AgentLibrary
		if loader, ok := controller.(interface {
			ListAgentProfiles(context.Context) (coding.AgentLibrary, error)
		}); ok {
			loaded, loadErr := loader.ListAgentProfiles(ctx)
			library = loaded
			err = errors.Join(err, loadErr)
		}
		for _, request := range requests {
			view, readErr := controller.ReadTeam(ctx, request)
			if readErr != nil {
				continue
			}
			views = append(views, view)
		}

		return agentsRouteDataMsg{
			generation: generation, agents: values, teamViews: views, library: library, err: err,
		}
	}

	return tea.Batch(load, m.startActivityClock(activityWasVisible))
}

func (m *Model) teamViewsForChildSelector() ([]coding.TeamView, []coding.TeamReadRequest) {
	m.ensureTeamProjection()
	views := make([]coding.TeamView, 0, len(m.teamProjection.views))
	requests := make([]coding.TeamReadRequest, 0, len(m.teamProjection.views))
	seen := make(map[team.ID]struct{}, len(m.teamProjection.views))
	for _, teamID := range m.teamProjection.viewOrder {
		view, ok := m.teamProjection.views[teamID]
		if !ok {
			continue
		}
		views = append(views, view.Clone())
		seen[teamID] = struct{}{}
		requests = append(requests, coding.TeamReadRequest{
			TeamID: teamID, AfterRevision: view.ChangeCursor,
			AfterControlRevision: view.ControlCursor,
		})
	}

	for _, value := range m.state.Teams {
		if value.TeamID == "" {
			continue
		}
		if _, exists := seen[value.TeamID]; exists {
			continue
		}
		seen[value.TeamID] = struct{}{}
		requests = append(requests, coding.TeamReadRequest{TeamID: value.TeamID})
	}

	return views, requests
}

//nolint:gocyclo // The keyboard map is kept explicit for the full-width route.
func (m *Model) updateAgentsRouteKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if key := message.String(); key == keyEscape || key == keyCtrlT || key == keyCtrlC {
		return m, m.closeRouteToParent()
	}
	if m.route.loading {
		return m, nil
	}
	if key := message.String(); key == "ctrl+a" {
		m.selectAgentsRouteTab(agentsTabAgents)

		return m, nil
	} else if key == "ctrl+r" {
		m.selectAgentsRouteTab(agentsTabRuns)

		return m, nil
	}
	if m.route.agentsTab == agentsTabAgents {
		return m.updateAgentLibraryRouteKey(message)
	}

	values := m.filteredChildren()

	if m.updateRouteScrollKey(message) {
		return m, nil
	}

	switch message.String() {
	case "up", "k":
		m.route.cursor = wrapIndex(m.route.cursor-1, len(values))
	case keyDown, "j", keyTab:
		m.route.cursor = wrapIndex(m.route.cursor+1, len(values))
	case keyCtrlU:
		m.route.query = ""
		m.route.cursor = 0
	case keyBackspace:
		m.route.query = trimLastRune(m.route.query)
		m.route.cursor = 0
	case keyEnter:
		if len(values) == 0 {
			return m, nil
		}

		return m, m.openChildRoute(values[m.route.cursor])
	case "c":
		if len(values) == 0 || m.route.controlling {
			return m, nil
		}
		if m.actionContext() != contextIdle {
			m.route.err = errRouteActionNeedsIdle

			return m, nil
		}

		return m, m.cancelChild(values[m.route.cursor])
	default:
		if text := message.Key().Text; text != "" {
			m.route.query += text
			m.route.cursor = 0
		}
	}

	return m, nil
}

func (m *Model) updateAgentLibraryRouteKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	values := m.filteredAgentLibrary()
	if m.updateRouteScrollKey(message) {
		return m, nil
	}

	switch message.String() {
	case "up", "k":
		m.route.cursor = wrapIndex(m.route.cursor-1, len(values))
	case keyDown, "j", keyTab:
		m.route.cursor = wrapIndex(m.route.cursor+1, len(values))
	case keyCtrlU:
		m.route.query = ""
		m.route.cursor = 0
	case keyBackspace:
		m.route.query = trimLastRune(m.route.query)
		m.route.cursor = 0
	case keyEnter:
		if len(values) == 0 {
			return m, nil
		}
		// Arming a direct Agent rewrites what the next submission means, so it
		// waits for the same idle boundary the runtime changes do.
		if m.actionContext() != contextIdle {
			m.route.err = errRouteActionNeedsIdle

			return m, nil
		}

		return m, m.selectDirectAgent(values[m.route.cursor])
	default:
		if text := message.Key().Text; text != "" {
			m.route.query += text
			m.route.cursor = 0
		}
	}

	return m, nil
}

// updateRouteScrollKey applies the shared full-area scrolling keys to a route
// that renders through fitScrollableContent. It reports whether the key was a
// scrolling key, so routes can keep their selection keys separate from their
// viewport keys.
func (m *Model) updateRouteScrollKey(message tea.KeyPressMsg) bool {
	rows := m.routeScrollRows()
	maximum := max(0, m.routeScrollLineCount()-rows)

	switch message.String() {
	case "pgup":
		m.route.offset = max(0, m.route.offset-rows)
	case "pgdown":
		m.route.offset = min(maximum, m.route.offset+rows)
	case "shift+up":
		m.route.offset = max(0, m.route.offset-1)
	case "shift+down":
		m.route.offset = min(maximum, m.route.offset+1)
	case "home":
		m.route.offset = 0
	case "end":
		m.route.offset = maximum
	default:
		return false
	}

	return true
}

// routeScrollRows reports the rows a scrollable route body may occupy.
func (m *Model) routeScrollRows() int {
	return max(1, m.height-layoutStatusRows)
}

// routeScrollLineCount reports how many rows the current route content occupies.
func (m *Model) routeScrollLineCount() int {
	return lipgloss.Height(m.agentsRouteContent())
}

func (m *Model) filteredChildren() []childSummary {
	query := strings.ToLower(strings.TrimSpace(m.route.query))

	values := make([]childSummary, 0, len(m.route.children))
	for _, value := range m.route.children {
		if query == "" || strings.Contains(strings.ToLower(value.searchableText()), query) {
			values = append(values, value)
		}
	}
	sort.SliceStable(values, func(left, right int) bool {
		leftRunning := values[left].live()
		rightRunning := values[right].live()
		if leftRunning != rightRunning {
			return leftRunning
		}

		return values[left].createdAt.After(values[right].createdAt)
	})

	return values
}

func (m *Model) filteredAgentLibrary() []coding.AgentLibraryEntry {
	query := strings.ToLower(strings.TrimSpace(m.route.query))
	values := make([]coding.AgentLibraryEntry, 0, len(m.route.agentLibrary))
	for _, value := range m.route.agentLibrary {
		searchable := strings.Join([]string{
			value.ID, value.Name, value.Description, value.Kind, value.Scope,
			value.Source, value.Status, value.Unavailable,
			strings.Join(value.DeclaredTools, " "), strings.Join(value.RequiredTools, " "),
		}, " ")
		if query == "" || strings.Contains(strings.ToLower(searchable), query) {
			values = append(values, value)
		}
	}
	sort.SliceStable(values, func(left, right int) bool {
		if values[left].Available != values[right].Available {
			return values[left].Available
		}

		return values[left].ID < values[right].ID
	})

	return values
}

// agentsRouteTabBar draws the Runs/Agents switch beneath the title line, with the
// active tab emphasized. Under NO_COLOR the active tab is bracketed so the
// selection survives without styling, and the bar is fitted to the width.
func (m *Model) agentsRouteTabBar(width int) string {
	return ansi.Truncate(
		strings.Join(m.agentsRouteTabTitles(), agentsTabGap), max(1, width), "…",
	)
}

func (m *Model) agentsRouteTabTitles() []string {
	tabs := agentsRouteTabs()
	titles := make([]string, 0, len(tabs))
	for _, tab := range tabs {
		title := tab.title()
		switch {
		case tab != m.route.agentsTab:
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

// selectAgentsRouteTab switches the view, resetting the cursor like the tab keys.
func (m *Model) selectAgentsRouteTab(tab agentsRouteTab) {
	m.route.agentsTab = tab
	m.route.cursor = 0
}

func (m *Model) agentsRouteContent() string {
	content, _ := m.agentsRouteContentWithRows()

	return content
}

// agentsRouteContentWithRows renders the route and reports each addressable row's
// painted line count, so the pointer map measures the same rows the frame paints.
func (m *Model) agentsRouteContentWithRows() (string, []int) {
	title := "Agents · Runs"
	if m.route.agentsTab == agentsTabAgents {
		title = "Agents · Definitions"
	}
	lines := []string{
		title, m.agentsRouteTabBar(max(1, m.width)), "", "Search: " + m.route.query, "",
	}
	if m.route.loading {
		return strings.Join(append(lines, m.activityNotice("Loading…")), "\n"), nil
	}
	if m.route.agentsTab == agentsTabAgents {
		return m.agentLibraryRouteContent(lines)
	}

	values := m.filteredChildren()
	if len(values) == 0 {
		lines = append(lines, "No child Agents in this session.")
	}
	heights := make([]int, 0, len(values))
	for index, value := range values {
		marker := "  "
		if index == m.route.cursor {
			marker = "› "
		}
		rows := m.renderChildSummary(value, marker)
		lines = append(lines, rows...)
		heights = append(heights, len(rows))
	}

	if m.route.controlling {
		label := "Cancelling subagent…"
		if m.route.controllingChildSet &&
			m.route.controllingChildKind == childTeamWorker {
			label = "Interrupting Team Worker…"
		}
		lines = append(lines, "", m.activityNotice(label))
	}
	lines = append(lines, "", wheelHintNavigation+" · type to search · Enter inspect · c interrupt · Ctrl+A Agents · Ctrl+R Runs · Ctrl+T/Esc close")

	return strings.Join(lines, "\n"), heights
}

func (m *Model) agentLibraryRouteContent(lines []string) (string, []int) {
	values := m.filteredAgentLibrary()
	if len(values) == 0 {
		lines = append(lines, "No Agent definitions are available in this workspace.")
	}
	heights := make([]int, 0, len(values))
	for index, value := range values {
		marker := "  "
		if index == m.route.cursor {
			marker = "› "
		}
		rows := renderAgentLibrarySummary(value, marker)
		lines = append(lines, rows...)
		heights = append(heights, len(rows))
	}
	lines = append(lines,
		"",
		"Declared capabilities are intersected with active session authority at launch.",
		"↑/↓ choose · "+wheelHintNavigation+" · Enter run · type to search · Ctrl+A Agents · Ctrl+R Runs · Ctrl+T/Esc close",
	)

	return strings.Join(lines, "\n"), heights
}

func renderAgentLibrarySummary(value coding.AgentLibraryEntry, marker string) []string {
	name := safeDetailText(value.Name)
	if name == "" {
		name = value.ID
	}
	status := agentLibraryStatusAvailable
	if !value.Available {
		status = value.Unavailable
		if status == "" {
			status = value.Status
		}
	}
	provenance := strings.Join([]string{value.Kind, value.Scope, value.Source}, " · ")
	tools := "none"
	if len(value.DeclaredTools) > 0 {
		tools = strings.Join(value.DeclaredTools, ", ")
	}
	lines := []string{
		fmt.Sprintf("%s%s", marker, truncateText(name, 96)),
		fmt.Sprintf("  %s · %s", safeDetailText(status), safeDetailText(provenance)),
		"  declared tools: " + safeDetailText(truncateText(tools, 180)),
	}
	if description := safeDetailText(value.Description); description != "" {
		lines = append(lines, "  "+truncateText(description, 180))
	}
	if len(value.RequiredTools) > 0 {
		lines = append(lines, "  required: "+safeDetailText(truncateText(strings.Join(value.RequiredTools, ", "), 180)))
	}
	if len(value.Diagnostics) > 0 {
		// The message explains what happened; the code is only a fallback.
		diagnostic := value.Diagnostics[0]
		message := safeDetailText(strings.TrimSpace(diagnostic.Message))
		if message == "" {
			message = humanizeStatusCode(diagnostic.Code)
		}
		lines = append(lines, "  diagnostic: "+truncateText(message, 180))
	}

	return lines
}

func (m *Model) renderChildSummary(value childSummary, marker string) []string {
	activity := ""
	if value.live() {
		activity = m.activity.Frame() + " "
	}
	if value.kind == childTeamWorker {
		title := value.taskTitle
		if title == "" {
			title = genericTeamTaskLabel
		}
		worker := value.workerName
		if worker == "" {
			worker = genericTeamWorkerLabel
		}
		state := value.worker.LifecycleState
		if state == "" {
			state = coding.TeamLifecycleStatus(value.worker.ResourceState)
		}

		return []string{
			fmt.Sprintf("%s%s", marker, title),
			fmt.Sprintf("  %sTeam Worker · %s · %s · %s", activity, worker, state,
				formatInteractionDuration(value.worker.DurationMillis)),
		}
	}

	preview := value.subagent.TaskPreview
	if strings.TrimSpace(preview) == "" {
		preview = "(no task preview)"
	}

	return []string{
		fmt.Sprintf("%s%s", marker, preview),
		fmt.Sprintf("  %s%s subagent · %s · %s · %s", activity,
			subagentDisplayName(value.subagent.Identity, value.subagent.Role), value.subagent.State,
			relativeTime(value.subagent.CreatedAt),
			formatInteractionDuration(value.subagent.Duration.Milliseconds())),
	}
}

func subagentDisplayName(identity subagent.AgentIdentity, role subagent.Role) string {
	if role != "" {
		return string(role)
	}
	if name := safeDetailText(identity.Name); name != "" {
		return truncateText(name, 96)
	}
	if identity.ID != "" {
		return identity.ID
	}

	return genericSubagentLabel
}

func (m *Model) updateSubagentRouteSummary(event coding.Event) {
	if m.route.kind != routeAgents && m.route.kind != routeChild {
		return
	}
	lifecycle, ok := event.Payload.(coding.SubagentLifecycle)
	if !ok || lifecycle.ChildSessionID == "" {
		return
	}

	index := -1
	for candidate := range m.route.children {
		if m.route.children[candidate].kind == childSubagent &&
			m.route.children[candidate].childSessionID == lifecycle.ChildSessionID {
			index = candidate

			break
		}
	}
	if index < 0 {
		m.route.children = append(m.route.children, subagentChildSummary(subagent.Summary{
			ChildSessionID: lifecycle.ChildSessionID,
			CreatedAt:      event.Time,
		}))
		index = len(m.route.children) - 1
	}

	value := &m.route.children[index].subagent
	value.Ownership = subagent.Ownership{
		ParentSessionID:     m.state.SessionID,
		ParentInteractionID: lifecycle.ParentInteractionID,
		ParentRunID:         lifecycle.ParentRunID, ParentToolCallID: lifecycle.ParentToolCallID,
		RootInteractionID: lifecycle.RootInteractionID,
	}
	value.Delivery = lifecycle.Delivery
	value.Identity = lifecycle.Identity
	value.Role = lifecycle.Role
	value.State = lifecycle.State
	value.TaskPreview = lifecycle.TaskPreview
	value.Model = lifecycle.Model
	value.Duration = time.Duration(lifecycle.DurationMillis) * time.Millisecond
	value.Turns = lifecycle.Turns
	value.ToolCalls = lifecycle.ToolCalls
	value.Usage = ai.Usage{
		InputTokens: lifecycle.Usage.InputTokens, OutputTokens: lifecycle.Usage.OutputTokens,
		ReasoningTokens:   lifecycle.Usage.ReasoningTokens,
		CachedInputTokens: lifecycle.Usage.CachedInputTokens,
		CacheWriteTokens:  lifecycle.Usage.CacheWriteTokens,
	}
	value.Code = lifecycle.Code

	if m.route.kind == routeChild && m.route.childKind == childSubagent &&
		m.route.childSessionID == lifecycle.ChildSessionID &&
		m.route.detail != nil {
		m.route.detail.Summary = *value
	}
}

func childSummaries(agents []subagent.Summary, views []coding.TeamView) []childSummary {
	values := make([]childSummary, 0, len(agents))
	for _, value := range agents {
		values = append(values, subagentChildSummary(value))
	}
	for _, view := range views {
		values = mergeTeamWorkerChildren(values, view)
	}

	return values
}

func (m *Model) latestTeamWorkerChild() (childSummary, bool) {
	m.ensureTeamProjection()
	var latest childSummary
	found := false
	for _, teamID := range m.teamProjection.viewOrder {
		view, ok := m.teamProjection.views[teamID]
		if !ok {
			continue
		}
		for _, child := range mergeTeamWorkerChildren(nil, view) {
			if !found || !child.createdAt.Before(latest.createdAt) {
				latest = child
				found = true
			}
		}
	}

	return latest, found
}

func mergeTeamWorkerChildren(values []childSummary, view coding.TeamView) []childSummary {
	for _, attempt := range view.Attempts {
		if attempt.Target.TeamID == "" || attempt.Target.MemberID == "" ||
			attempt.Target.TaskID == "" || attempt.Target.AttemptID == "" ||
			attempt.Target.OwnerGeneration == 0 || attempt.ChildSessionID == "" {
			continue
		}
		value := childSummary{
			kind: childTeamWorker, worker: attempt,
			workerName:     teamWorkerMemberLabel(view, attempt.Target.MemberID),
			taskTitle:      teamWorkerTaskLabel(view, attempt.Target.TaskID),
			childSessionID: attempt.ChildSessionID,
			createdAt:      attempt.StartedAt,
		}
		found := false
		for index := range values {
			if sameChildSelectorIdentity(values[index], value) {
				values[index] = value
				found = true

				break
			}
		}
		if !found {
			values = append(values, value)
		}
	}

	return values
}

func sameChildSelectorIdentity(left, right childSummary) bool {
	if left.kind != right.kind {
		return false
	}
	if left.kind == childSubagent {
		return left.childSessionID != "" && left.childSessionID == right.childSessionID
	}

	return left.worker.Target.TeamID == right.worker.Target.TeamID &&
		left.worker.Target.MemberID == right.worker.Target.MemberID &&
		left.worker.Target.TaskID == right.worker.Target.TaskID &&
		left.worker.Target.AttemptID == right.worker.Target.AttemptID
}

func teamWorkerMemberLabel(view coding.TeamView, memberID team.MemberID) string {
	for _, member := range view.Members {
		if member.ID == memberID {
			return boundedTeamLabel(member.Name)
		}
	}

	return genericTeamWorkerLabel
}

func teamWorkerTaskLabel(view coding.TeamView, taskID team.TaskID) string {
	for _, task := range view.Tasks {
		if task.ID == taskID {
			return boundedTeamLabel(task.Title)
		}
	}

	return genericTeamTaskLabel
}

//nolint:gocyclo,nestif // Terminal result/error enrichment remains adjacent to ordinary timeline projection.
func (m *Model) subagentRouteContent(state coding.State, detail *subagent.Detail) string {
	// A child projection never renders reasoning text: the spec keeps child
	// thinking out of every parent-visible surface.
	blocks := withoutThinkingBlocks(projectTimeline(state))
	if detail != nil {
		if detail.Summary.State == subagent.StateSucceeded && detail.Result != nil {
			for index, block := range slices.Backward(blocks) {
				if block.kind != blockAssistant {
					continue
				}

				blocks[index].body = strings.TrimSpace(strings.Join(
					renderSubagentResult(detail.Result), "\n",
				))
				blocks[index].rendered = true

				break
			}
		} else if isTerminalSubagent(detail.Summary.State) && state.LastError == nil {
			body := humanizeStatusCode(detail.Summary.Code)
			if body == "" {
				body = subagentOutcomeText(detail.Summary)
			}
			blocks = append(blocks, timelineBlock{
				kind: blockError, title: subagentActivityLabel(
					detail.Summary.Role,
					detail.Summary.State,
				),
				body: body, position: len(state.Transcript),
			})
		}
		if marker, ok := subagentCompletionMarker(*detail); ok {
			blocks = append(blocks, marker)
		}
	}

	// The page renders through the conversation's own options, so its rows are the
	// rows the conversation would show for the same blocks: a Tool result collapses
	// to its one-line row here exactly as it does there.
	flow := m.renderTimelineBlocksWithOptions(blocks, timelineRenderOptions{})
	if flow != "" {
		return flow
	}
	if detail == nil {
		return m.activityNotice("Loading subagent activity…")
	}

	value := detail.Summary
	if value.State == subagent.StateCreated || value.State == subagent.StateRunning {
		return m.activityNotice(subagentPhaseLabel(detail.Activity.Phase))
	}

	return subagentOutcomeText(value)
}

func subagentCompletionMarker(detail subagent.Detail) (timelineBlock, bool) {
	var outcome coding.InteractionOutcome

	switch detail.Summary.State {
	case subagent.StateSucceeded:
		outcome = coding.InteractionSucceeded
	case subagent.StateFailed:
		outcome = coding.InteractionFailed
	case subagent.StateCanceled, subagent.StateInterrupted:
		outcome = coding.InteractionCanceled
	case subagent.StateCreated, subagent.StateRunning:
		return timelineBlock{}, false
	}

	return projectCompletionMarker(completionMarker{
		interactionID:  detail.Summary.ChildSessionID,
		afterMessages:  len(detail.Transcript),
		outcome:        outcome,
		durationMillis: detail.Summary.Duration.Milliseconds(),
		model:          detail.Summary.Model,
	})
}

func subagentPhaseLabel(phase subagent.ActivityPhase) string {
	switch phase {
	case subagent.ActivityPhaseStarting:
		return "Starting…"
	case subagent.ActivityPhaseThinking:
		return "Thinking…"
	case subagent.ActivityPhaseWorking:
		return "Working…"
	case subagent.ActivityPhaseFinalizing:
		return "Validating result…"
	default:
		return "Current activity unavailable."
	}
}

func subagentOutcomeText(value subagent.Summary) string {
	label := "Unavailable"

	switch value.State {
	case subagent.StateFailed:
		label = labelFailed
	case subagent.StateCanceled:
		label = "Canceled"
	case subagent.StateInterrupted:
		label = "Interrupted"
	case subagent.StateSucceeded:
		label = "Succeeded"
	case subagent.StateCreated, subagent.StateRunning:
	}

	if value.Code != "" {
		label += ": " + humanizeStatusCode(value.Code)
	}

	return label
}

func renderSubagentResult(value any) []string {
	switch result := value.(type) {
	case string:
		return renderTextResult(result)
	case subagent.ExploreResult:
		return renderExploreResult(result)
	case subagent.PlanResult:
		return renderPlanResult(result)
	case subagent.ReviewResult:
		return renderReviewResult(result)
	default:
		return []string{"  Validated result is unavailable."}
	}
}

// renderTextResult renders the default free-text builtin result, which is the
// child's final assistant text rather than a typed object.
func renderTextResult(value string) []string {
	trimmed := strings.TrimSpace(sanitizeToolText(value))
	if trimmed == "" {
		return []string{"  Validated result is unavailable."}
	}

	lines := strings.Split(trimmed, "\n")
	rendered := make([]string, 0, len(lines))
	for _, line := range lines {
		rendered = append(rendered, "  "+strings.TrimRight(line, " \t"))
	}

	return rendered
}

func renderExploreResult(result subagent.ExploreResult) []string {
	lines := []string{"  " + safeDetailText(result.Summary)}
	if len(result.Evidence) > 0 {
		lines = append(lines, "", "  Evidence")

		for _, evidence := range result.Evidence {
			location := fmt.Sprintf(
				"%s:%d-%d", safeWorkspaceToolPath(evidence.Path),
				evidence.StartLine, evidence.EndLine,
			)
			lines = append(lines, "    • "+location+" — "+safeDetailText(evidence.Claim))
		}
	}

	if len(result.Unknowns) > 0 {
		lines = append(lines, "", "  Unknowns")
		lines = append(lines, detailBulletLines(result.Unknowns, "    • ")...)
	}

	return lines
}

func renderPlanResult(result subagent.PlanResult) []string {
	lines := []string{"  " + safeDetailText(result.Summary)}
	if len(result.Steps) > 0 {
		lines = append(lines, "", "  Steps")
		for index, step := range result.Steps {
			lines = append(lines, fmt.Sprintf("    %d. %s", index+1, safeDetailText(step.Title)))
			if len(step.Files) > 0 {
				files := make([]string, 0, len(step.Files))
				for _, file := range step.Files {
					files = append(files, safeWorkspaceToolPath(file))
				}

				lines = append(lines, "       Files: "+strings.Join(files, ", "))
			}

			if rationale := safeDetailText(step.Rationale); rationale != "" {
				lines = append(lines, "       "+rationale)
			}
		}
	}

	lines = appendDetailList(lines, "Assumptions", result.Assumptions)
	lines = appendDetailList(lines, "Risks", result.Risks)
	lines = appendDetailList(lines, "Verification", result.Verification)

	return lines
}

func renderReviewResult(result subagent.ReviewResult) []string {
	lines := []string{"  " + safeDetailText(result.Summary)}
	if len(result.Findings) > 0 {
		lines = append(lines, "", "  Findings")

		for _, finding := range result.Findings {
			location := safeWorkspaceToolPath(finding.Path)
			if finding.Line > 0 {
				location += fmt.Sprintf(":%d", finding.Line)
			}

			lines = append(lines, fmt.Sprintf(
				"    • [%s] %s · %s", safeDetailText(finding.Severity),
				safeDetailText(finding.Title), location,
			))
			lines = append(lines, "      "+safeDetailText(finding.Evidence))
			lines = append(lines, "      Recommendation: "+safeDetailText(finding.Recommendation))
		}
	}

	lines = appendDetailList(lines, "Residual risks", result.ResidualRisks)

	return lines
}

func appendDetailList(lines []string, title string, values []string) []string {
	if len(values) == 0 {
		return lines
	}

	lines = append(lines, "", "  "+title)

	return append(lines, detailBulletLines(values, "    • ")...)
}

func detailBulletLines(values []string, prefix string) []string {
	lines := make([]string, 0, len(values))
	for _, value := range values {
		lines = append(lines, prefix+safeDetailText(value))
	}

	return lines
}

func safeDetailText(value string) string {
	return strings.TrimSpace(sanitizeToolText(value))
}

func relativeTime(value time.Time) string {
	if value.IsZero() {
		return "unknown time"
	}

	duration := time.Since(value)
	if duration < time.Minute {
		return labelJustNow
	}

	if duration < time.Hour {
		return fmt.Sprintf("%dm ago", int(duration.Minutes()))
	}

	if duration < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(duration.Hours()))
	}

	return fmt.Sprintf("%dd ago", int(duration.Hours()/24))
}

func (m *Model) agentsRouteView() tea.View {
	m.route.listHits = routeListHit{}
	m.route.tabHits = routeTabHits{}
	content, heights := m.agentsRouteContentWithRows()
	if m.route.err != nil {
		content += "\n\nError: " + safeError(m.route.err)
	}

	width := max(1, m.width)
	height := max(1, m.height)
	window, windowStart := fitScrollableContentWindow(content, width, height, m.route.offset)
	// The painted window counts content lines only: the trailing indicator row is
	// not one, so it must not answer as a row or as the tab bar.
	painted := paintedContentRows(content, height)
	m.recordRouteListHit(routeListHit{
		painted: true, frameTop: 0, windowStart: windowStart, windowRows: painted,
		firstLine: agentsRoutePreamble, first: 0, heights: heights, gap: 0,
	})
	m.route.tabHits = m.agentsRouteTabHitMap(windowStart, painted, width)
	if !m.options.NoColor {
		window = lipgloss.NewStyle().Foreground(paletteFor(m.theme).workspace).Render(window)
	}

	return m.presentationView(window)
}

// agentsRouteTabHitMap records the tab titles the frame paints, clamped to the
// width so a clipped title only answers where it is visible.
func (m *Model) agentsRouteTabHitMap(windowStart, windowRows, width int) routeTabHits {
	hits := routeTabHits{
		kind: routeAgents, painted: true, columns: max(1, width),
		line: agentsRouteTabLine, windowStart: windowStart, windowRows: windowRows,
	}
	titles := m.agentsRouteTabTitles()
	column := 0
	for index, tab := range agentsRouteTabs() {
		span := ansi.StringWidth(titles[index])
		if column < width {
			hits.tabs = append(hits.tabs, routeTabHit{
				start: column, end: min(column+span, width), tab: tab,
			})
		}
		column += span + ansi.StringWidth(agentsTabGap)
	}

	return hits
}

func (m *Model) updateSubagentRouteKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := message.String()

	// Ctrl+Q is the one chord the child view never owns: it bubbles so the
	// conversation's own quit handling decides.
	if key == keyCtrlQ {
		return m, tea.Quit
	}

	// The read-only transcript view owns a different key set from the Team
	// Worker route, which keeps an editable composer.
	if m.route.childKind != childTeamWorker {
		if model, command, handled := m.updateChildTranscriptKey(key); handled {
			return model, command
		}
	}

	switch key {
	case keyCtrlT:
		return m, m.closeRouteToParent()
	case keyCtrlC:
		// Only the Team Worker route reaches this; the transcript view answered
		// above, and a Worker interrupts with Ctrl+X.
		return m, m.closeRouteToParent()
	case keyEscape:
		return m.updateSubagentRouteEscapeKey()
	}
	if m.route.childKind == childTeamWorker {
		return m.updateTeamWorkerComposerKey(message)
	}
	if key == "c" {
		return m, m.cancelRouteChild()
	}
	m.scrollSubagentRoute(key)

	return m, nil
}

// updateChildTranscriptKey drives the read-only transcript view's own keys: it
// closes through the one close function, and Ctrl+C interrupts the child, which
// is the only turn that surface can reach.
func (m *Model) updateChildTranscriptKey(key string) (tea.Model, tea.Cmd, bool) {
	switch key {
	case "q":
		return m, m.closeRouteToParent(), true
	case keyCtrlC:
		return m, m.cancelRouteChild(), true
	}

	return m, nil, false
}

// updateSubagentRouteEscapeKey closes the child route to the conversation it
// belongs to, which is the one move the page's own footer advertises. A Worker
// clears its draft first: Escape is also the composer's own cancel there, and
// the route is the only place that draft lives.
func (m *Model) updateSubagentRouteEscapeKey() (tea.Model, tea.Cmd) {
	if m.route.childKind == childTeamWorker && m.composer.Value() != "" {
		m.composer.Reset()
		m.setLayout()

		return m, nil
	}

	return m, m.closeRouteToParent()
}

// scrollSubagentRoute applies the child transcript's scrolling keys to the page's
// reading region, so the keys and the wheel share the region's follow rule.
func (m *Model) scrollSubagentRoute(key string) {
	visible := max(1, m.subagentTranscriptBodyHeight())
	region := &m.childScroll

	switch key {
	case "up", "k":
		region.scrollBy(-1)
	case keyDown, "j":
		region.scrollBy(1)
	case keyPageUp:
		region.scrollBy(-visible)
	case keyPageDown:
		region.scrollBy(visible)
	case keyHome:
		region.gotoTop()
	case keyEnd:
		region.gotoBottom()
	}
}

func (m *Model) updateTeamWorkerComposerKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.route.controlling {
		return m, nil
	}

	key := message.String()
	if command, handled := m.updateTeamWorkerScrollKey(key); handled {
		return m, command
	}
	if command, handled := m.updateTeamWorkerActionKey(key); handled {
		return m, command
	}

	switch key {
	case "up":
		if m.composer.AtFirstVisualRow() && m.composer.HistoryUp() {
			m.setLayout()

			return m, nil
		}
	case keyDown:
		if m.composer.AtLastVisualRow() && m.composer.HistoryDown() {
			m.setLayout()

			return m, nil
		}
	}

	var command tea.Cmd
	m.composer, command = m.composer.Update(message)
	m.setLayout()

	return m, command
}

func (m *Model) updateTeamWorkerActionKey(key string) (tea.Cmd, bool) {
	switch key {
	case keyCtrlV:
		return m.readClipboardImage(), true
	case "ctrl+x":
		if m.composer.Value() == "" {
			return m.interruptTeamWorker(m.route.childSummary.worker.Target), true
		}
	case keyEnter:
		if strings.TrimSpace(m.composer.Value()) == "" {
			return nil, true
		}

		return m.messageTeamWorker(
			m.route.childSummary.worker.Target,
			m.composer.Snapshot(),
		), true
	case keyCtrlJ, keyShiftEnter:
		m.composer.InsertString("\n")
		m.setLayout()

		return nil, true
	}

	return nil, false
}

func (m *Model) updateTeamWorkerScrollKey(key string) (tea.Cmd, bool) {
	visible := max(1, m.height-5)
	maximum := m.subagentRouteMaximumOffset()
	switch key {
	case keyPageUp:
		m.route.offset = max(0, m.route.offset-visible)
	case keyPageDown:
		m.route.offset = min(maximum, m.route.offset+visible)
	case keyHome:
		m.route.offset = 0
	case keyEnd:
		m.route.offset = maximum
	default:
		return nil, false
	}

	return nil, true
}

func (m *Model) cancelChild(value childSummary) tea.Cmd {
	m.route.controllingChildKind = value.kind
	m.route.controllingChildSet = true
	if value.kind == childTeamWorker {
		return m.interruptTeamWorker(value.worker.Target)
	}

	return m.cancelSubagent(value.childSessionID)
}

// cancelRouteChild interrupts the child the open route is showing. The route
// owns its child session id, so the interrupt does not depend on a summary
// surviving the way in.
func (m *Model) cancelRouteChild() tea.Cmd {
	if m.route.controlling || m.route.childSessionID == "" {
		return nil
	}
	m.route.controllingChildKind = childSubagent
	m.route.controllingChildSet = true

	return m.cancelSubagent(m.route.childSessionID)
}

func (m *Model) cancelSubagent(childSessionID string) tea.Cmd {
	activityWasVisible := m.activityClockVisible()
	m.route.controlling = true
	generation := m.route.generation

	cancel := func() tea.Msg {
		return subagentCancelResultMsg{
			generation: generation, childSessionID: childSessionID,
			err: m.controller.CancelSubagent(m.ctx, childSessionID),
		}
	}

	return tea.Batch(cancel, m.startActivityClock(activityWasVisible))
}

func (m *Model) subagentRouteView() tea.View {
	if m.route.childKind == childTeamWorker {
		return m.teamWorkerRouteView()
	}

	return m.subagentTranscriptRouteView()
}

// subagentRouteBody renders whichever region the open child route shows: its
// live activity placeholder, its own transcript, or the failure that replaced it.
func (m *Model) subagentRouteBody() string {
	body := "Child activity unavailable."
	switch {
	case m.route.controlling:
		label := "Cancelling subagent…"
		if m.route.childKind == childTeamWorker {
			label = "Applying Worker control…"
		}
		body = m.activityNotice(label)
	case m.route.childState == nil && (m.route.loading || m.route.refreshing):
		body = m.activityNotice("Loading child activity…")
	case m.route.childState != nil:
		if m.route.childKind == childTeamWorker {
			body = m.teamWorkerRouteContent(*m.route.childState, m.route.childSummary)
		} else {
			body = m.subagentRouteContent(*m.route.childState, m.route.detail)
		}
	}

	if m.route.err != nil {
		body += "\n\nError: " + m.safeChildRouteError(m.route.err)
	}
	if m.route.refreshErr != nil {
		body += "\n\nRefresh: " + m.safeChildRouteError(m.route.refreshErr)
	}

	return body
}

// teamWorkerRouteView keeps the Worker route's editable composer: a Worker
// accepts messages, so it needs the input box and its caret.
func (m *Model) teamWorkerRouteView() tea.View {
	width := max(1, m.width)
	height := max(1, m.height)

	body := m.subagentRouteBody()
	footer := m.subagentRouteFooter(width)
	bodyHeight := max(0, height-lipgloss.Height(footer))

	// The child route is the only place a subagent's results are readable, so
	// its body wraps instead of losing the right-hand end of long lines.
	body = renderSubagentRouteBody(body, width, bodyHeight, m.route.offset)
	if padding := bodyHeight - lipgloss.Height(body); padding > 0 {
		body += strings.Repeat("\n", padding)
	}

	view := m.presentationView(lipgloss.JoinVertical(lipgloss.Left, body, footer))
	if !m.route.controlling {
		view.Cursor = m.composer.Cursor()
		if view.Cursor != nil {
			cursorX, cursorY := m.composerBoxCursorOffset()
			view.Cursor.X += cursorX
			view.Cursor.Y += bodyHeight + 1 + cursorY
		}
	}

	return view
}

// subagentTranscriptRouteView draws the read-only child transcript as a
// full-width page in the same shape a Tool detail page uses: the child's own
// blocks through the conversation's renderer, then the pinned separator and the
// row that names the child and the keys this page answers. The page keeps no
// composer and no frame, so it reads as the conversation it belongs to rather
// than as a window floating over it, and its region follows the child's newest
// row until the reader scrolls away from the tail.
func (m *Model) subagentTranscriptRouteView() tea.View {
	width := max(1, m.width)
	height := max(1, m.height)

	status := m.subagentRouteStatusLine()
	footer := m.sessionPickerSeparator(width) + "\n" + status
	bodyHeight := m.subagentTranscriptBodyHeight()

	// A terminal too short for both keeps the footer: the hint explains the keys,
	// while the body is recoverable by resizing.
	content := status
	if height > 2 {
		content = padHeight(m.childTranscriptWindow(bodyHeight), bodyHeight) + "\n" + footer
	}

	return m.presentationView(truncateHeight(content, height))
}

// subagentTranscriptBodyHeight reserves the page's pinned footer — the
// separator row and the status row — so the transcript never pushes it off the
// frame.
func (m *Model) subagentTranscriptBodyHeight() int {
	return max(0, m.height-2)
}

// childTranscriptWindow installs the child page's wrapped rows into its reading
// region and returns the window it shows. Installing the rows is what makes the
// page follow: the region pins itself to the newest row whenever the content grows
// while the reader holds the tail, and leaves a reader who scrolled away where
// they were.
func (m *Model) childTranscriptWindow(height int) string {
	region := &m.childScroll
	region.setRows(wrapPlainLines(m.subagentRouteBody(), max(1, m.width)))
	region.setHeight(height)

	return region.visible()
}

// subagentPageStatusCandidates orders the child page's footer row from the most
// to the least informative: the child's own metadata, then the keys the page
// answers. Every step down the ladder shortens a key hint before it drops a
// metadata part, and the last step drops the metadata entirely, so a narrow
// terminal loses the model and the clock rather than the identity, the state, or
// the way out.
func (m *Model) subagentPageStatusCandidates() []string {
	hintSets := m.subagentPageHintSets()
	parts := m.subagentPageStatusParts()
	separator := "  ·  "

	candidates := make([]string, 0, len(hintSets)*(len(parts)+1))
	for trim := len(parts); trim >= 0; trim-- {
		head := strings.Join(parts[:trim], separator)
		for _, hints := range hintSets {
			hint := strings.Join(hints, separator)
			switch {
			case head == "":
				candidates = append(candidates, hint)
			case hint == "":
				candidates = append(candidates, head)
			default:
				candidates = append(candidates, head+separator+hint)
			}
		}
	}

	return candidates
}

// subagentPageStatusParts is the child page's own metadata, most to least
// protected: the identity, its state, the task it was handed, the model it runs,
// its live activity, and its elapsed time.
func (m *Model) subagentPageStatusParts() []string {
	summary := m.subagentRouteSummaryState()
	parts := []string{
		m.subagentRouteGlyph(summary.State) + " " + subagentPageIdentity(summary),
		subagentStateLabel(summary.State),
	}
	if description := oneLineSubagentTask(summary.TaskPreview); description != "" {
		parts = append(parts, quotedSubagentDescription(description, 48))
	}
	if model := safeDetailText(summary.Model); model != "" {
		parts = append(parts, model)
	}
	if activity := m.childRouteActivityLabel(); activity != "" {
		parts = append(parts, activity)
	}
	if summary.Duration > 0 || isTerminalSubagent(summary.State) {
		parts = append(parts, formatInteractionDuration(summary.Duration.Milliseconds()))
	}

	return parts
}

// subagentPageIdentity names the child on its own page: the resolved role or
// identity, then the kind. A child with no identity yet already reads "child
// Agent", so the kind is not repeated after it.
func subagentPageIdentity(summary subagent.Summary) string {
	name := subagentDisplayName(summary.Identity, summary.Role)
	if name == genericSubagentLabel {
		return name
	}

	return name + " subagent"
}

// subagentPageHintSets lists the child page's key hints, widest first. An
// interrupt hint only appears while the child can still be interrupted.
func (m *Model) subagentPageHintSets() [][]string {
	terminal := isTerminalSubagent(m.subagentRouteSummaryState().State)
	hints := func(values ...string) []string {
		if terminal {
			values = slices.DeleteFunc(slices.Clone(values), func(value string) bool {
				return value == subagentHintInterrupt
			})
		}

		return values
	}

	return [][]string{
		hints(detailHintScrollFull, subagentHintClose, subagentHintInterrupt),
		hints("↑/↓ scroll", subagentHintClose, subagentHintInterrupt),
		hints(subagentHintClose, subagentHintInterrupt),
		hints(subagentHintClose),
	}
}

// subagentRouteGlyph is the child page's state mark: the animated spinner while
// the child runs, and a settled mark once it stops.
func (m *Model) subagentRouteGlyph(state subagent.State) string {
	switch state {
	case subagent.StateCreated, subagent.StateRunning:
		return m.activity.Frame()
	case subagent.StateSucceeded:
		return toolActivityGlyph(toolStateSucceeded)
	case subagent.StateFailed:
		return toolActivityGlyph(toolStateFailed)
	case subagent.StateCanceled, subagent.StateInterrupted:
		return toolActivityGlyph(toolStateInterrupted)
	}

	return m.activity.Frame()
}

// childRouteActivityLabel is the open child's own live activity, derived through
// the same activity model every other surface uses. A settled child reports none.
func (m *Model) childRouteActivityLabel() string {
	if m.route.childState == nil || isTerminalSubagent(m.subagentRouteSummaryState().State) {
		return ""
	}
	status, visible := resolveActivity(activityContext{state: *m.route.childState})
	if !visible {
		return ""
	}
	if status.detail == "" {
		return status.label
	}

	return status.label + " " + status.detail
}

// subagentRouteStatusLine is the child route's pinned status row. The Team
// Worker route keeps its own two-row form — its composer and keys are a
// different vocabulary — while the read-only child page uses the single
// candidate row a Tool detail page uses.
func (m *Model) subagentRouteStatusLine() string {
	if m.route.childKind != childTeamWorker {
		return m.subagentPageStatusLine()
	}

	values := m.teamWorkerRouteStatusValues()
	values = append(values, "Enter message", "Ctrl+J newline", "PgUp/PgDn scroll", "Ctrl+X interrupt", "Esc Lead")

	value := strings.Join(values, "  ·  ")
	if !m.options.NoColor {
		value = lipgloss.NewStyle().Foreground(paletteFor(m.theme).muted).Render(value)
	}

	return ansi.Truncate(value, max(1, m.width), "…")
}

// subagentPageStatusLine renders the child page's footer row: the widest
// candidate that fits, then the muted tone every footer on this surface uses.
func (m *Model) subagentPageStatusLine() string {
	candidates := m.subagentPageStatusCandidates()
	value := candidates[len(candidates)-1]
	for _, candidate := range candidates {
		if ansi.StringWidth(candidate) <= m.width {
			value = candidate

			break
		}
	}

	value = ansi.Truncate(value, max(1, m.width), "…")
	if m.options.NoColor {
		return value
	}

	return lipgloss.NewStyle().Foreground(paletteFor(m.theme).muted).Render(value)
}

// subagentRouteFooter is the pinned footer of the Team Worker route: the
// separator, the Worker's composer, and its identity status line.
func (m *Model) subagentRouteFooter(width int) string {
	separator := strings.Repeat("-", max(1, width))
	if !m.options.NoColor {
		separator = lipgloss.NewStyle().Foreground(
			paletteFor(m.theme).separator,
		).Render(strings.Repeat("─", max(1, width)))
	}
	parts := []string{separator}
	if m.route.childKind == childTeamWorker {
		parts = append(parts, m.composerBox())
	}
	parts = append(parts, m.subagentRouteStatusLine())

	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// subagentRouteSummaryState reports whether the open child has finished, so the
// interrupt hint only appears while it can act.
func (m *Model) subagentRouteSummaryState() subagent.Summary {
	if m.route.detail == nil {
		return subagent.Summary{}
	}

	return m.route.detail.Summary
}

// The child page's key hints. The page has no close control of its own — it is
// the conversation's own container, so Escape is how it leaves.
const (
	subagentHintInterrupt = "Ctrl+C interrupt"
	subagentHintClose     = "Esc close"
)

func subagentStateLabel(state subagent.State) string {
	switch state {
	case subagent.StateCreated:
		return "Created"
	case subagent.StateRunning:
		return "Running"
	case subagent.StateSucceeded:
		return "Succeeded"
	case subagent.StateFailed:
		return labelFailed
	case subagent.StateCanceled:
		return "Canceled"
	case subagent.StateInterrupted:
		return "Interrupted"
	default:
		return humanizeStatusCode(string(state))
	}
}

func (m *Model) teamWorkerRouteStatusValues() []string {
	worker := m.route.childSummary.workerName
	if worker == "" {
		worker = genericTeamWorkerLabel
	}
	values := []string{"pips", worker + " · Team Worker"}
	if m.route.childState != nil {
		state := m.route.childState
		if state.Provider != "" || state.ModelID != "" {
			values = append(values, fmt.Sprintf("%s/%s", state.Provider, state.ModelID))
		}
	}
	status := m.route.childSummary.worker.LifecycleState
	if status == "" {
		status = coding.TeamLifecycleStatus(m.route.childSummary.worker.ResourceState)
	}

	return append(values, string(status))
}

func (m *Model) openSubagentRoute(childSessionID string) tea.Cmd {
	child := subagentChildSummary(subagent.Summary{ChildSessionID: childSessionID})
	for _, candidate := range m.route.children {
		if candidate.kind == childSubagent && candidate.childSessionID == childSessionID {
			child = candidate

			break
		}
	}

	return m.openChildRoute(child)
}

func (m *Model) openChildRoute(child childSummary) tea.Cmd {
	previous := m.route

	return m.requestRouteOpen(newChildRouteRequest(previous, child))
}

func (m *Model) activateChildRoute(request routeOpenRequest) tea.Cmd {
	if request.child.kind == childTeamWorker {
		return m.activateTeamWorkerRoute(request)
	}

	return m.activateSubagentRoute(request)
}

func (m *Model) activateSubagentRoute(request routeOpenRequest) tea.Cmd {
	activityWasVisible := m.activityClockVisible()
	m.routeSeq++
	m.route = routeState{
		kind: routeChild, loading: true, generation: m.routeSeq,
		childKind: childSubagent, childSummary: request.child,
		childSessionID: request.childSessionID, children: request.children,
		query: request.query, cursor: request.cursor,
	}
	// Opening a page lands on the child's newest row, which is where a reader who
	// just asked to see a running child is looking.
	m.childScroll.gotoBottom()
	if child, ok := m.childStates[request.childSessionID]; ok {
		state := child.Clone()
		m.route.childState = &state
	}
	m.composer.Blur()
	generation := m.route.generation

	load := func() tea.Msg {
		state, stateErr := m.controller.InspectSubagentState(m.ctx, request.childSessionID)
		detail, detailErr := m.controller.InspectSubagent(m.ctx, request.childSessionID)

		return subagentRouteDataMsg{
			detail: detail, hasDetail: detailErr == nil,
			state: state, hasState: stateErr == nil,
			err:        errors.Join(stateErr, detailErr),
			generation: generation, childSessionID: request.childSessionID,
		}
	}

	return tea.Batch(load, m.startActivityClock(activityWasVisible))
}

func (m *Model) invalidateAgentDetail(item streamItem) tea.Cmd {
	if item.err != nil || m.route.kind != routeSubagent ||
		m.route.childSessionID == "" {
		return nil
	}

	lifecycle, ok := item.event.Payload.(coding.SubagentLifecycle)
	if !ok || lifecycle.ChildSessionID != m.route.childSessionID ||
		!isTerminalSubagent(lifecycle.State) {
		return nil
	}

	return m.refreshSubagentRoute()
}

func (m *Model) refreshSubagentRoute() tea.Cmd {
	if m.route.kind != routeSubagent || m.route.childSessionID == "" {
		return nil
	}

	if m.route.loading || m.route.refreshing {
		m.route.refreshPending = true

		return nil
	}

	activityWasVisible := m.activityClockVisible()
	m.route.refreshing = true
	generation := m.route.generation
	childSessionID := m.route.childSessionID

	load := func() tea.Msg {
		state, stateErr := m.controller.InspectSubagentState(m.ctx, childSessionID)
		detail, detailErr := m.controller.InspectSubagent(m.ctx, childSessionID)

		return subagentRouteDataMsg{
			detail: detail, hasDetail: detailErr == nil,
			state: state, hasState: stateErr == nil,
			err:        errors.Join(stateErr, detailErr),
			background: true, generation: generation, childSessionID: childSessionID,
		}
	}

	return tea.Batch(load, m.startActivityClock(activityWasVisible))
}

// applySubagentRouteRefresh applies a fresh read to the open child route. The
// read-only page reads through its scroll region, which re-anchors when the fresh
// rows are installed, so it needs nothing here. The Team Worker page keeps the
// private offset its composer shares the screen with, so that offset is clamped
// against the new height.
//
//nolint:nestif // Refresh preserves scroll anchoring while applying two independently available views.
func (m *Model) applySubagentRouteRefresh(message subagentRouteDataMsg) (tea.Model, tea.Cmd) {
	m.route.refreshing = false
	if message.err != nil && !message.hasState && !message.hasDetail {
		m.route.refreshErr = message.err
	} else if message.hasState || message.hasDetail {
		worker := m.route.childKind == childTeamWorker
		wasAtBottom := worker && m.route.offset >= m.subagentRouteMaximumOffset()
		previousOffset := m.route.offset
		if message.hasState {
			state := message.state
			m.route.childState = &state
			m.adoptChildState(message.childSessionID, state)
		}
		if message.hasDetail {
			detail := message.detail
			m.route.detail = &detail
		}
		m.route.refreshErr = nil

		if worker {
			maximum := m.subagentRouteMaximumOffset()
			if wasAtBottom {
				m.route.offset = maximum
			} else {
				m.route.offset = min(previousOffset, maximum)
			}
		}
	}

	if !m.route.refreshPending {
		return m, nil
	}

	m.route.refreshPending = false

	return m, m.refreshSubagentRoute()
}

// subagentRouteMaximumOffset is the Team Worker page's last scroll position. The
// read-only child page moves through its scroll region instead.
func (m *Model) subagentRouteMaximumOffset() int {
	content, visible := m.subagentRouteScrollContent()

	return max(0, len(content)-visible)
}

// subagentRouteScrollContent returns the Team Worker page's wrapped body rows and
// the number that fit, so its scrolling counts physical rows rather than logical
// lines.
func (m *Model) subagentRouteScrollContent() ([]string, int) {
	if m.route.childState == nil {
		return nil, 1
	}

	visible := max(1, m.height-lipgloss.Height(m.subagentRouteFooter(m.width)))
	content := m.teamWorkerRouteContent(*m.route.childState, m.route.childSummary)

	return wrapPlainLines(content, max(1, m.width)), visible
}

// renderSubagentRouteBody wraps each body row to the page width with a hanging
// indent, then windows the requested offset. It serves the Team Worker page, whose
// window follows the private offset its composer shares the screen with.
func renderSubagentRouteBody(body string, width, height, offset int) string {
	rows := wrapPlainLines(body, max(1, width))
	if len(rows) == 0 {
		return ""
	}

	start := min(max(0, offset), max(0, len(rows)-height))
	end := min(len(rows), start+height)

	return strings.Join(rows[start:end], "\n")
}

// wrapPlainLines wraps plain text rows to width, keeping each row's leading
// indentation on every physical line. The wrap width excludes the indent so a
// continuation line cannot push past the frame.
func wrapPlainLines(body string, width int) []string {
	rows := make([]string, 0, 16)
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimRight(line, " ")
		if line == "" || ansi.StringWidth(line) <= width {
			rows = append(rows, line)

			continue
		}

		leading := line[:len(line)-len(strings.TrimLeft(line, " "))]
		wrapped := ansi.Wrap(
			strings.TrimLeft(line, " "),
			max(1, width-ansi.StringWidth(leading)),
			"",
		)
		for part := range strings.SplitSeq(wrapped, "\n") {
			rows = append(rows, leading+part)
		}
	}

	return rows
}
