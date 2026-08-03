//nolint:wsl_v5 // Full-area agent routes keep their transition and rendering details adjacent.
package tui

import (
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
	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/subagent"
	"github.com/rsbin/pips/internal/coding/teamstate"
)

type childKind uint8

const (
	childSubagent childKind = iota
	childTeamWorker
)

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
		return value.worker.DomainState == team.AttemptStatusRunning &&
			value.worker.ResourceState == teamstate.AttemptRunning
	default:
		return false
	}
}

func (value childSummary) searchableText() string {
	if value.kind == childSubagent {
		return strings.Join([]string{
			value.subagent.TaskPreview, string(value.subagent.Role),
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
	m.routeSeq++
	m.route = routeState{kind: routeAgents, loading: true, generation: m.routeSeq}
	m.composer.Blur()
	generation := m.route.generation
	controller := m.controller
	ctx := m.ctx
	views, requests := m.teamViewsForChildSelector()

	return func() tea.Msg {
		values, err := controller.ListSubagents(ctx)
		for _, request := range requests {
			view, readErr := controller.ReadTeam(ctx, request)
			if readErr != nil {
				continue
			}
			views = append(views, view)
		}

		return agentsRouteDataMsg{
			generation: generation, agents: values, teamViews: views, err: err,
		}
	}
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

	values := m.filteredChildren()

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

		return m, m.cancelChild(values[m.route.cursor])
	default:
		if text := message.Key().Text; text != "" {
			m.route.query += text
			m.route.cursor = 0
		}
	}

	return m, nil
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

func (m *Model) agentsRouteContent() string {
	lines := []string{"Agents", "", "Search: " + m.route.query, ""}
	if m.route.loading {
		return strings.Join(append(lines, "Loading…"), "\n")
	}

	values := m.filteredChildren()
	if len(values) == 0 {
		lines = append(lines, "No child Agents in this session.")
	}

	for index, value := range values {
		marker := "  "
		if index == m.route.cursor {
			marker = "› "
		}

		lines = append(lines, renderChildSummary(value, marker)...)
	}

	lines = append(lines, "", "↑/↓ choose · type to search · Enter inspect · c interrupt · Ctrl+T/Esc close")

	return strings.Join(lines, "\n")
}

func renderChildSummary(value childSummary, marker string) []string {
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
			fmt.Sprintf("  Team Worker · %s · %s · %s", worker, state,
				formatInteractionDuration(value.worker.DurationMillis)),
		}
	}

	preview := value.subagent.TaskPreview
	if strings.TrimSpace(preview) == "" {
		preview = "(no task preview)"
	}

	return []string{
		fmt.Sprintf("%s%s", marker, preview),
		fmt.Sprintf("  %s subagent · %s · %s · %s",
			value.subagent.Role, value.subagent.State,
			relativeTime(value.subagent.CreatedAt),
			formatInteractionDuration(value.subagent.Duration.Milliseconds())),
	}
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
	blocks := projectTimeline(state)
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

	flow := m.renderTimelineBlocksWithOptions(
		blocks,
		timelineRenderOptions{expandToolResults: true},
	)
	if flow != "" {
		return flow
	}
	if detail == nil {
		return "Loading subagent activity…"
	}

	value := detail.Summary
	if value.State == subagent.StateCreated || value.State == subagent.StateRunning {
		return "✻ " + subagentPhaseLabel(detail.Activity.Phase)
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
		label = "Failed"
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
		return "just now"
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
	content := m.agentsRouteContent()
	if m.route.err != nil {
		content += "\n\nError: " + safeError(m.route.err)
	}

	content = fitScrollableContent(content, max(1, m.width), max(1, m.height), m.route.offset)
	if !m.options.NoColor {
		content = lipgloss.NewStyle().Foreground(paletteFor(m.theme).workspace).Render(content)
	}

	view := tea.NewView(content)
	view.AltScreen = false
	view.MouseMode = tea.MouseModeNone
	view.WindowTitle = appTitle

	return view
}

func (m *Model) updateSubagentRouteKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := message.String()

	switch key {
	case keyCtrlT, keyCtrlC:
		return m, m.closeRouteToParent()
	case keyEscape:
		if m.route.childKind == childTeamWorker {
			if m.composer.Value() != "" {
				m.composer.Reset()
				m.setLayout()

				return m, nil
			}
			if len(m.route.children) == 0 {
				return m, m.closeRouteToParent()
			}
		}
		m.stopTeamWorkerRouteSubscription()
		if len(m.route.children) > 0 {
			m.route.kind = routeAgents
			m.route.loading = false
			m.route.err = nil
			m.route.offset = 0
			m.route.childSessionID = ""
			m.route.childSummary = childSummary{}
			m.route.detail = nil
			m.route.childState = nil
			m.route.refreshing = false
			m.route.refreshPending = false
			m.route.refreshErr = nil

			return m, nil
		}

		return m, m.openAgentsRoute()
	}
	if m.route.childKind == childTeamWorker {
		return m.updateTeamWorkerComposerKey(message)
	}
	if key == "c" && !m.route.controlling {
		return m, m.cancelChild(m.route.childSummary)
	}

	visible := max(1, m.height-3)
	maximum := m.subagentRouteMaximumOffset()

	switch key {
	case "up", "k":
		m.route.offset = max(0, m.route.offset-1)
	case keyDown, "j":
		m.route.offset = min(maximum, m.route.offset+1)
	case "pgup":
		m.route.offset = max(0, m.route.offset-visible)
	case "pgdown":
		m.route.offset = min(maximum, m.route.offset+visible)
	case "home":
		m.route.offset = 0
	case "end":
		m.route.offset = maximum
	}

	return m, nil
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
	case "pgup":
		m.route.offset = max(0, m.route.offset-visible)
	case "pgdown":
		m.route.offset = min(maximum, m.route.offset+visible)
	case "home":
		m.route.offset = 0
	case "end":
		m.route.offset = maximum
	default:
		return nil, false
	}

	return nil, true
}

func (m *Model) cancelChild(value childSummary) tea.Cmd {
	if value.kind == childTeamWorker {
		return m.interruptTeamWorker(value.worker.Target)
	}

	return m.cancelSubagent(value.childSessionID)
}

func (m *Model) cancelSubagent(childSessionID string) tea.Cmd {
	m.route.controlling = true
	generation := m.route.generation

	return func() tea.Msg {
		return subagentCancelResultMsg{
			generation: generation, childSessionID: childSessionID,
			err: m.controller.CancelSubagent(m.ctx, childSessionID),
		}
	}
}

func (m *Model) subagentRouteView() tea.View {
	width := max(1, m.width)
	height := max(1, m.height)

	separator := strings.Repeat("-", width)

	if !m.options.NoColor {
		separator = lipgloss.NewStyle().Foreground(
			paletteFor(m.theme).separator,
		).Render(strings.Repeat("─", width))
	}

	body := "Loading child activity…"

	if m.route.childState != nil {
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

	footerParts := []string{separator}
	if m.route.childKind == childTeamWorker {
		footerParts = append(footerParts, m.composerBox())
	}
	footerParts = append(footerParts, m.subagentRouteStatusLine())
	footer := lipgloss.JoinVertical(lipgloss.Left, footerParts...)
	bodyHeight := max(0, height-lipgloss.Height(footer))

	body = fitScrollableContent(body, width, bodyHeight, m.route.offset)
	if padding := bodyHeight - lipgloss.Height(body); padding > 0 {
		body += strings.Repeat("\n", padding)
	}

	view := tea.NewView(lipgloss.JoinVertical(lipgloss.Left, body, footer))
	view.AltScreen = false
	view.MouseMode = tea.MouseModeNone
	view.WindowTitle = appTitle
	if m.route.childKind == childTeamWorker && !m.route.controlling {
		view.Cursor = m.composer.Cursor()
		if view.Cursor != nil {
			cursorX, cursorY := m.composerBoxCursorOffset()
			view.Cursor.X += cursorX
			view.Cursor.Y += bodyHeight + 1 + cursorY
		}
	}

	return view
}

func (m *Model) subagentRouteStatusLine() string {
	values := []string{"pips", "child Agent", "loading"}

	if m.route.childKind == childTeamWorker {
		values = m.teamWorkerRouteStatusValues()
	} else if m.route.detail != nil {
		summary := m.route.detail.Summary

		values = []string{"pips", string(summary.Role) + " subagent"}

		if model := safeDetailText(summary.Model); model != "" {
			values = append(values, model)
		}

		values = append(values, string(summary.State))
	}

	if m.route.childKind == childTeamWorker {
		values = append(values, "Enter message", "Ctrl+J newline", "PgUp/PgDn scroll", "Ctrl+X interrupt", "Esc Lead")
	} else {
		values = append(values, "Ctrl+T parent", "Esc agents")
	}

	if !m.options.NoColor {
		palette := paletteFor(m.theme)
		values[0] = lipgloss.NewStyle().Bold(true).Foreground(palette.workspace).Render(values[0])
		values[1] = lipgloss.NewStyle().Bold(true).Foreground(palette.session).Render(values[1])
		values[len(values)-2] = lipgloss.NewStyle().Foreground(palette.muted).Render(values[len(values)-2])
		values[len(values)-1] = lipgloss.NewStyle().Foreground(palette.muted).Render(values[len(values)-1])
	}

	return ansi.Truncate(strings.Join(values, "  ·  "), max(1, m.width), "…")
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
	m.routeSeq++
	m.route = routeState{
		kind: routeChild, loading: true, generation: m.routeSeq,
		childKind: childSubagent, childSummary: request.child,
		childSessionID: request.childSessionID, children: request.children,
		query: request.query, cursor: request.cursor,
	}
	if child, ok := m.childStates[request.childSessionID]; ok {
		state := child.Clone()
		m.route.childState = &state
	}
	m.composer.Blur()
	generation := m.route.generation

	return func() tea.Msg {
		state, stateErr := m.controller.InspectSubagentState(m.ctx, request.childSessionID)
		detail, detailErr := m.controller.InspectSubagent(m.ctx, request.childSessionID)

		return subagentRouteDataMsg{
			detail: detail, hasDetail: detailErr == nil,
			state: state, hasState: stateErr == nil,
			err:        errors.Join(stateErr, detailErr),
			generation: generation, childSessionID: request.childSessionID,
		}
	}
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

	m.route.refreshing = true
	generation := m.route.generation
	childSessionID := m.route.childSessionID

	return func() tea.Msg {
		state, stateErr := m.controller.InspectSubagentState(m.ctx, childSessionID)
		detail, detailErr := m.controller.InspectSubagent(m.ctx, childSessionID)

		return subagentRouteDataMsg{
			detail: detail, hasDetail: detailErr == nil,
			state: state, hasState: stateErr == nil,
			err:        errors.Join(stateErr, detailErr),
			background: true, generation: generation, childSessionID: childSessionID,
		}
	}
}

//nolint:nestif // Refresh preserves scroll anchoring while applying two independently available views.
func (m *Model) applySubagentRouteRefresh(message subagentRouteDataMsg) (tea.Model, tea.Cmd) {
	m.route.refreshing = false
	if message.err != nil && !message.hasState && !message.hasDetail {
		m.route.refreshErr = message.err
	} else if message.hasState || message.hasDetail {
		wasAtBottom := m.route.offset >= m.subagentRouteMaximumOffset()
		previousOffset := m.route.offset
		if message.hasState {
			state := message.state
			m.route.childState = &state
		}
		if message.hasDetail {
			detail := message.detail
			m.route.detail = &detail
		}
		m.route.refreshErr = nil

		maximum := m.subagentRouteMaximumOffset()
		if wasAtBottom {
			m.route.offset = maximum
		} else {
			m.route.offset = min(previousOffset, maximum)
		}
	}

	if !m.route.refreshPending {
		return m, nil
	}

	m.route.refreshPending = false

	return m, m.refreshSubagentRoute()
}

func (m *Model) subagentRouteMaximumOffset() int {
	if m.route.childState == nil {
		return 0
	}

	visible := max(1, m.height-3)
	if m.route.childKind == childTeamWorker {
		visible = max(1, m.height-lipgloss.Height(m.composerBox())-2)
	}
	content := m.subagentRouteContent(*m.route.childState, m.route.detail)
	if m.route.childKind == childTeamWorker {
		content = m.teamWorkerRouteContent(*m.route.childState, m.route.childSummary)
	}
	lineCount := strings.Count(content, "\n") + 1

	return max(0, lineCount-visible)
}
