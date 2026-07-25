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
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/subagent"
)

type subagentToolEnvelope struct {
	Schema         string `json:"schema"`
	ChildSessionID string `json:"child_session_id"`
	AgentID        string `json:"agent_id"`
}

type agentsRouteDataMsg struct {
	generation uint64
	agents     []subagent.Summary
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

	return func() tea.Msg {
		values, err := m.controller.ListSubagents(m.ctx)

		return agentsRouteDataMsg{generation: generation, agents: values, err: err}
	}
}

//nolint:gocyclo // The keyboard map is kept explicit for the full-width route.
func (m *Model) updateAgentsRouteKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if key := message.String(); key == keyEscape || key == keyCtrlT || key == keyCtrlC {
		return m, m.closeRouteToParent()
	}
	if m.route.loading {
		return m, nil
	}

	values := m.filteredAgents()

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

		childSessionID := values[m.route.cursor].ChildSessionID

		return m, m.openSubagentRoute(childSessionID)
	case "c":
		if len(values) == 0 || m.route.controlling {
			return m, nil
		}

		childSessionID := values[m.route.cursor].ChildSessionID

		return m, m.cancelSubagent(childSessionID)
	default:
		if text := message.Key().Text; text != "" {
			m.route.query += text
			m.route.cursor = 0
		}
	}

	return m, nil
}

func (m *Model) filteredAgents() []subagent.Summary {
	query := strings.ToLower(strings.TrimSpace(m.route.query))

	values := make([]subagent.Summary, 0, len(m.route.agents))
	for _, value := range m.route.agents {
		if query == "" || strings.Contains(strings.ToLower(value.TaskPreview), query) ||
			strings.Contains(string(value.Role), query) ||
			strings.Contains(string(value.State), query) {
			values = append(values, value)
		}
	}
	sort.SliceStable(values, func(left, right int) bool {
		leftRunning := values[left].State == subagent.StateCreated ||
			values[left].State == subagent.StateRunning
		rightRunning := values[right].State == subagent.StateCreated ||
			values[right].State == subagent.StateRunning
		if leftRunning != rightRunning {
			return leftRunning
		}

		return values[left].CreatedAt.After(values[right].CreatedAt)
	})

	return values
}

func (m *Model) agentsRouteContent() string {
	lines := []string{"Subagents", "", "Search: " + m.route.query, ""}
	if m.route.loading {
		return strings.Join(append(lines, "Loading…"), "\n")
	}

	values := m.filteredAgents()
	if len(values) == 0 {
		lines = append(lines, "No specialist runs in this session.")
	}

	for index, value := range values {
		marker := "  "
		if index == m.route.cursor {
			marker = "› "
		}

		preview := value.TaskPreview
		if strings.TrimSpace(preview) == "" {
			preview = "(no task preview)"
		}

		lines = append(lines,
			fmt.Sprintf("%s%s", marker, preview),
			fmt.Sprintf("  %s · %s · %s · %s",
				value.Role, value.State, relativeTime(value.CreatedAt),
				formatInteractionDuration(value.Duration.Milliseconds())),
		)
	}

	lines = append(lines, "", "↑/↓ choose · type to search · Enter inspect · c cancel · Ctrl+T/Esc close")

	return strings.Join(lines, "\n")
}

func (m *Model) updateSubagentRouteSummary(event coding.Event) {
	if m.route.kind != routeAgents && m.route.kind != routeSubagent {
		return
	}
	lifecycle, ok := event.Payload.(coding.SubagentLifecycle)
	if !ok || lifecycle.ChildSessionID == "" {
		return
	}

	index := -1
	for candidate := range m.route.agents {
		if m.route.agents[candidate].ChildSessionID == lifecycle.ChildSessionID {
			index = candidate

			break
		}
	}
	if index < 0 {
		m.route.agents = append(m.route.agents, subagent.Summary{
			ChildSessionID: lifecycle.ChildSessionID,
			CreatedAt:      event.Time,
		})
		index = len(m.route.agents) - 1
	}

	value := &m.route.agents[index]
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

	if m.route.kind == routeSubagent && m.route.childSessionID == lifecycle.ChildSessionID &&
		m.route.detail != nil {
		m.route.detail.Summary = *value
	}
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
		if len(m.route.agents) > 0 {
			m.route.kind = routeAgents
			m.route.loading = false
			m.route.err = nil
			m.route.offset = 0
			m.route.childSessionID = ""
			m.route.detail = nil
			m.route.childState = nil
			m.route.refreshing = false
			m.route.refreshPending = false
			m.route.refreshErr = nil

			return m, nil
		}

		return m, m.openAgentsRoute()
	}
	if key == "c" && !m.route.controlling {
		return m, m.cancelSubagent(m.route.childSessionID)
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

	body := "Loading subagent activity…"

	if m.route.childState != nil {
		body = m.subagentRouteContent(*m.route.childState, m.route.detail)
	}

	if m.route.err != nil {
		body += "\n\nError: " + safeError(m.route.err)
	}

	if m.route.refreshErr != nil {
		body += "\n\nRefresh: " + safeError(m.route.refreshErr)
	}

	footer := m.subagentRouteStatusLine()
	bodyHeight := max(0, height-2)

	body = fitScrollableContent(body, width, bodyHeight, m.route.offset)
	if padding := bodyHeight - lipgloss.Height(body); padding > 0 {
		body += strings.Repeat("\n", padding)
	}

	view := tea.NewView(lipgloss.JoinVertical(lipgloss.Left, body, separator, footer))
	view.AltScreen = false
	view.MouseMode = tea.MouseModeNone
	view.WindowTitle = appTitle

	return view
}

func (m *Model) subagentRouteStatusLine() string {
	values := []string{"pips", "subagent", "loading"}

	if m.route.detail != nil {
		summary := m.route.detail.Summary

		values = []string{"pips", string(summary.Role) + " subagent"}

		if model := safeDetailText(summary.Model); model != "" {
			values = append(values, model)
		}

		values = append(values, string(summary.State))
	}

	values = append(values, "Ctrl+T parent", "Esc subagents")

	if !m.options.NoColor {
		palette := paletteFor(m.theme)
		values[0] = lipgloss.NewStyle().Bold(true).Foreground(palette.workspace).Render(values[0])
		values[1] = lipgloss.NewStyle().Bold(true).Foreground(palette.session).Render(values[1])
		values[len(values)-2] = lipgloss.NewStyle().Foreground(palette.muted).Render(values[len(values)-2])
		values[len(values)-1] = lipgloss.NewStyle().Foreground(palette.muted).Render(values[len(values)-1])
	}

	return ansi.Truncate(strings.Join(values, "  ·  "), max(1, m.width), "…")
}

func (m *Model) openSubagentRoute(childSessionID string) tea.Cmd {
	previous := m.route

	return m.requestRouteOpen(newSubagentRouteRequest(previous, childSessionID))
}

func (m *Model) activateSubagentRoute(request routeOpenRequest) tea.Cmd {
	m.routeSeq++
	m.route = routeState{
		kind: routeSubagent, loading: true, generation: m.routeSeq,
		childSessionID: request.childSessionID, agents: request.agents,
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
	lineCount := strings.Count(
		m.subagentRouteContent(*m.route.childState, m.route.detail), "\n",
	) + 1

	return max(0, lineCount-visible)
}
