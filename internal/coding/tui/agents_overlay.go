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
	"github.com/rsbin/pips/ai"
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

		return m, func() tea.Msg {
			detail, err := m.controller.InspectSubagent(m.ctx, childSessionID)

			return overlayDataMsg{
				kind: overlayAgents, detail: detail, hasDetail: true, err: err,
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

	lines = append(lines, "", "↑/↓ choose · type to search · Enter inspect · Esc close")

	return strings.Join(lines, "\n")
}

func (m *Model) agentDetailContent(detail subagent.Detail) string {
	value := detail.Summary

	lines := []string{
		"Subagent detail", "",
		"Task: " + value.TaskPreview,
		fmt.Sprintf("Role: %s", value.Role),
		fmt.Sprintf("State: %s", value.State),
		"Model: " + value.Model,
		"Child session: " + value.ChildSessionID,
		fmt.Sprintf("Duration: %s · Turns: %d · Tools: %d",
			formatInteractionDuration(value.Duration.Milliseconds()), value.Turns, value.ToolCalls),
		fmt.Sprintf("Usage: %d input · %d output · %d reasoning",
			value.Usage.InputTokens, value.Usage.OutputTokens, value.Usage.ReasoningTokens),
	}
	if value.Code != "" {
		lines = append(lines, "Outcome code: "+value.Code)
	}

	lines = append(lines, "", "Transcript")

	for _, message := range detail.Transcript {
		content := renderChildMessage(message)
		if strings.TrimSpace(content) == "" {
			continue
		}

		lines = append(lines, "", strings.ToUpper(string(message.Role))+":", content)
	}

	if detail.Result != nil {
		if data, err := json.MarshalIndent(detail.Result, "", "  "); err == nil {
			lines = append(lines, "", "Structured result", string(data))
		}
	}

	lines = append(lines, "", "↑/↓ or PgUp/PgDn scroll · Esc back")

	return strings.Join(lines, "\n")
}

func renderChildMessage(message ai.Message) string {
	parts := make([]string, 0, len(message.Parts))
	for _, part := range message.Parts {
		switch value := part.(type) {
		case ai.TextPart:
			parts = append(parts, value.Text)
		case ai.ReasoningPart:
			if value.Redacted {
				parts = append(parts, "[reasoning redacted]")
			} else {
				parts = append(parts, "[reasoning]\n"+value.Text)
			}
		case ai.ToolCallPart:
			parts = append(parts, fmt.Sprintf("[tool %s]\n%s", value.Name, value.Args))
		case ai.ToolResultPart:
			parts = append(parts, fmt.Sprintf(
				"[tool result %s]\n%s", value.Name,
				visibleToolMessage(ai.Message{Parts: value.Content}),
			))
		case ai.ImagePart:
			parts = append(parts, "[image]")
		case ai.FilePart:
			parts = append(parts, "[file "+value.Name+"]")
		}
	}

	return strings.TrimSpace(strings.Join(parts, "\n"))
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

	content = fitOverlayContent(content, max(1, m.width), max(1, m.height), m.overlay.offset)
	if !m.options.NoColor {
		content = lipgloss.NewStyle().Foreground(paletteFor(m.theme).workspace).Render(content)
	}

	view := tea.NewView(content)
	view.AltScreen = false
	view.MouseMode = tea.MouseModeNone
	view.WindowTitle = appTitle

	return view
}

func (m *Model) openAgentDetail(childSessionID string) tea.Cmd {
	m.overlay = overlayState{kind: overlayAgents, loading: true}

	return func() tea.Msg {
		values, listErr := m.controller.ListSubagents(m.ctx)
		detail, inspectErr := m.controller.InspectSubagent(m.ctx, childSessionID)

		return overlayDataMsg{
			kind: overlayAgents, agents: values, hasAgents: true,
			detail: detail, hasDetail: inspectErr == nil,
			err: errors.Join(listErr, inspectErr),
		}
	}
}
