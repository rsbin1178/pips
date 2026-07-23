package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/subagent"
)

type subagentToolEnvelope struct {
	Schema         string `json:"schema"`
	ChildSessionID string `json:"child_session_id"`
}

func subagentChildSessionID(
	tool coding.ToolState,
	children []coding.SubagentState,
) string {
	text := visibleToolMessage(tool.Result)
	for offset := strings.IndexByte(text, '{'); offset >= 0; {
		var envelope subagentToolEnvelope

		decoder := json.NewDecoder(strings.NewReader(text[offset:]))
		if decoder.Decode(&envelope) == nil &&
			envelope.Schema == subagent.ResultSchema && envelope.ChildSessionID != "" {
			return envelope.ChildSessionID
		}

		next := strings.IndexByte(text[offset+1:], '{')
		if next < 0 {
			break
		}

		offset += next + 1
	}

	for _, v := range slices.Backward(children) {
		if v.ParentRunID == tool.RunID {
			return v.ChildSessionID
		}
	}

	return ""
}

func (m *Model) updateAgentsOverlay(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.overlay.loading {
		return m, nil
	}

	if m.overlay.agentDetail != nil {
		return m.updateReadOnlyOverlay(message.String())
	}

	values := m.filteredAgents()

	switch message.String() {
	case "up", "k":
		m.overlay.cursor = wrapIndex(m.overlay.cursor-1, len(values))
	case keyDown, "j", keyTab:
		m.overlay.cursor = wrapIndex(m.overlay.cursor+1, len(values))
	case "ctrl+u":
		m.overlay.query = ""
		m.overlay.cursor = 0
	case keyBackspace:
		m.overlay.query = trimLastRune(m.overlay.query)
		m.overlay.cursor = 0
	case keyEnter:
		if len(values) == 0 {
			return m, nil
		}

		m.overlay.loading = true
		m.overlay.err = nil
		childSessionID := values[m.overlay.cursor].ChildSessionID
		m.overlay.childSessionID = childSessionID
		generation := m.overlay.generation

		return m, func() tea.Msg {
			detail, err := m.controller.InspectSubagent(m.ctx, childSessionID)

			return overlayDataMsg{
				kind: overlayAgents, detail: detail, hasDetail: err == nil, err: err,
				generation: generation, childSessionID: childSessionID,
			}
		}
	default:
		if text := message.Key().Text; text != "" {
			m.overlay.query += text
			m.overlay.cursor = 0
		}
	}

	return m, nil
}

func (m *Model) filteredAgents() []subagent.Summary {
	query := strings.ToLower(strings.TrimSpace(m.overlay.query))

	values := make([]subagent.Summary, 0, len(m.overlay.agents))
	for _, value := range m.overlay.agents {
		if query == "" || strings.Contains(strings.ToLower(value.TaskPreview), query) ||
			strings.Contains(string(value.Role), query) ||
			strings.Contains(string(value.State), query) {
			values = append(values, value)
		}
	}

	return values
}

func (m *Model) agentsOverlayContent() string {
	if m.overlay.agentDetail != nil {
		return m.agentDetailContent(*m.overlay.agentDetail)
	}

	lines := []string{"Subagents", "", "Search: " + m.overlay.query, ""}
	if m.overlay.loading {
		return strings.Join(append(lines, "Loading…"), "\n")
	}

	values := m.filteredAgents()
	if len(values) == 0 {
		lines = append(lines, "No specialist runs in this session.")
	}

	for index, value := range values {
		marker := "  "
		if index == m.overlay.cursor {
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

	lines = append(lines, "", "↑/↓ choose · type to search · Enter inspect · Ctrl+T/Esc close")

	return strings.Join(lines, "\n")
}

func (m *Model) agentDetailContent(detail subagent.Detail) string {
	value := detail.Summary
	active := value.State == subagent.StateCreated || value.State == subagent.StateRunning

	duration := value.Duration
	if active && !detail.Activity.StartedAt.IsZero() {
		duration = max(0, time.Since(detail.Activity.StartedAt))
	}

	heading := subagentStateGlyph(value.State) + " " + subagentActivityLabel(value.Role, value.State)

	facts := []string{formatInteractionDuration(duration.Milliseconds())}
	if value.ToolCalls > 0 {
		facts = append(facts, fmt.Sprintf("%d tools", value.ToolCalls))
	}

	if tokens := value.Usage.InputTokens + value.Usage.OutputTokens; tokens > 0 {
		facts = append(facts, compactTokenCount(tokens)+" tokens")
	}

	heading += " · " + strings.Join(facts, " · ")
	if !m.options.NoColor {
		heading = subagentTitleStyle(string(value.State), m.theme).Render(heading)
	}

	lines := []string{heading, "", "Task", "  " + safeDetailText(value.TaskPreview)}
	activities := projectSubagentDetailTools(detail)
	running, completed := partitionSubagentDetailTools(activities)

	if active {
		lines = append(lines, "", "Now")
		if len(running) > 0 {
			lines = append(lines, renderSubagentDetailTools(
				running, m.width, m.theme, m.options.NoColor,
			))
		} else {
			lines = append(lines, "  "+subagentPhaseLabel(detail.Activity.Phase))
		}
	}

	if value.State == subagent.StateSucceeded {
		lines = append(lines, "", "Result")
		lines = append(lines, renderSubagentResult(detail.Result)...)
	} else if isTerminalSubagent(value.State) {
		lines = append(lines, "", "Outcome", "  "+subagentOutcomeText(value))
	}

	lines = append(lines, "", "Activity")
	if len(completed) == 0 {
		lines = append(lines, "  No completed tool activity recorded.")
	} else {
		lines = append(lines, renderSubagentDetailTools(
			completed, m.width, m.theme, m.options.NoColor,
		))
	}

	lines = append(lines,
		"", "Details",
		fmt.Sprintf("  %s · turn %d · %d tools", safeDetailText(value.Model), value.Turns, value.ToolCalls),
		fmt.Sprintf("  %d input · %d output · %d reasoning",
			value.Usage.InputTokens, value.Usage.OutputTokens, value.Usage.ReasoningTokens),
		"  child session "+safeDetailText(value.ChildSessionID),
	)
	if value.Code != "" {
		lines = append(lines, "  outcome "+humanizeStatusCode(value.Code))
	}

	lines = append(lines, "", "↑/↓ or PgUp/PgDn scroll · Esc back · Ctrl+T close")

	return strings.Join(lines, "\n")
}

func projectSubagentDetailTools(detail subagent.Detail) []toolActivity {
	state := coding.State{Transcript: detail.Transcript}

	state.Tools = make([]coding.ToolState, 0, len(detail.Activity.Tools))
	for _, value := range detail.Activity.Tools {
		status := coding.ToolStatusCompleted
		if value.Status == subagent.ToolStatusRunning {
			status = coding.ToolStatusRunning
		}

		state.Tools = append(state.Tools, coding.ToolState{
			RunID: value.RunID,
			Turn:  value.Turn,
			Call: coding.ToolCall{
				ID: value.Call.ID, Name: value.Call.Name, Arguments: slices.Clone(value.Call.Args),
			},
			Status: status,
			Update: value.Update,
			Result: value.Result,
		})
	}

	return projectToolActivities(state, nil)
}

func partitionSubagentDetailTools(values []toolActivity) ([]toolActivity, []toolActivity) {
	running := make([]toolActivity, 0, len(values))

	completed := make([]toolActivity, 0, len(values))
	for _, value := range values {
		if value.state == toolStateRunning {
			running = append(running, value)
			continue
		}

		completed = append(completed, value)
	}

	return running, completed
}

func renderSubagentDetailTools(
	values []toolActivity,
	width int,
	theme colorTheme,
	noColor bool,
) string {
	blocks := make([]timelineBlock, 0, len(values))
	for _, value := range values {
		blocks = append(blocks, projectToolActivity(value))
	}

	blocks = groupExploreBlocks(blocks)

	rendered := make([]string, 0, len(blocks))
	for _, block := range blocks {
		rendered = append(rendered, renderDetailedToolActivityBlock(
			block, max(1, width), theme, noColor,
		))
	}

	return strings.Join(rendered, "\n\n")
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

func (m *Model) agentsView() tea.View {
	content := m.agentsOverlayContent()
	if m.overlay.err != nil {
		content += "\n\nError: " + safeError(m.overlay.err)
	}

	if m.overlay.refreshErr != nil {
		content += "\n\nRefresh: " + safeError(m.overlay.refreshErr)
	}

	content = fitOverlayContent(content, max(1, m.width), max(1, m.height), m.overlay.offset)
	if !m.options.NoColor && m.overlay.agentDetail == nil {
		content = lipgloss.NewStyle().Foreground(paletteFor(m.theme).workspace).Render(content)
	}

	view := tea.NewView(content)
	view.AltScreen = false
	view.MouseMode = tea.MouseModeNone
	view.WindowTitle = appTitle

	return view
}

func (m *Model) openAgentDetail(childSessionID string) tea.Cmd {
	generation := m.nextOverlayGeneration()
	m.overlay = overlayState{
		kind: overlayAgents, loading: true, generation: generation,
		childSessionID: childSessionID,
	}

	return func() tea.Msg {
		values, listErr := m.controller.ListSubagents(m.ctx)
		detail, inspectErr := m.controller.InspectSubagent(m.ctx, childSessionID)

		return overlayDataMsg{
			kind: overlayAgents, agents: values, hasAgents: true,
			detail: detail, hasDetail: inspectErr == nil,
			err:        errors.Join(listErr, inspectErr),
			generation: generation, childSessionID: childSessionID,
		}
	}
}

func (m *Model) invalidateAgentDetail(item streamItem) tea.Cmd {
	if item.err != nil || m.overlay.kind != overlayAgents ||
		m.overlay.childSessionID == "" {
		return nil
	}

	lifecycle, ok := item.event.Payload.(coding.SubagentLifecycle)
	if !ok || lifecycle.ChildSessionID != m.overlay.childSessionID {
		return nil
	}

	return m.refreshAgentDetail()
}

func (m *Model) refreshAgentDetail() tea.Cmd {
	if m.overlay.kind != overlayAgents || m.overlay.childSessionID == "" {
		return nil
	}

	if m.overlay.loading || m.overlay.refreshing {
		m.overlay.refreshPending = true

		return nil
	}

	m.overlay.refreshing = true
	generation := m.overlay.generation
	childSessionID := m.overlay.childSessionID

	return func() tea.Msg {
		detail, err := m.controller.InspectSubagent(m.ctx, childSessionID)

		return overlayDataMsg{
			kind: overlayAgents, detail: detail, hasDetail: err == nil, err: err,
			background: true, generation: generation, childSessionID: childSessionID,
		}
	}
}

func (m *Model) applyAgentDetailRefresh(message overlayDataMsg) (tea.Model, tea.Cmd) {
	m.overlay.refreshing = false
	if message.err != nil {
		m.overlay.refreshErr = message.err
	} else if message.hasDetail {
		wasAtBottom := m.overlay.offset >= m.agentDetailMaximumOffset()
		previousOffset := m.overlay.offset
		detail := message.detail
		m.overlay.agentDetail = &detail
		m.overlay.refreshErr = nil

		maximum := m.agentDetailMaximumOffset()
		if wasAtBottom {
			m.overlay.offset = maximum
		} else {
			m.overlay.offset = min(previousOffset, maximum)
		}
	}

	if !m.overlay.refreshPending {
		return m, nil
	}

	m.overlay.refreshPending = false

	return m, m.refreshAgentDetail()
}

func (m *Model) agentDetailMaximumOffset() int {
	if m.overlay.agentDetail == nil {
		return 0
	}

	visible := max(1, m.height-5)
	lineCount := strings.Count(m.agentDetailContent(*m.overlay.agentDetail), "\n") + 1

	return max(0, lineCount-visible)
}
