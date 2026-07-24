//nolint:wsl_v5 // Inspection print formatting remains together for stable scrollback output.
package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func (m *Model) printHelp() tea.Cmd {
	return m.printInspection(
		"Help",
		renderActionHelp(defaultActions, m.actionContext())+"\n\n"+
			"Ctrl+J / Shift+Enter newline\n"+
			"The terminal owns conversation history: use its wheel or scrollback keys to navigate, "+
			"and drag normally to select and copy text.",
	)
}

func (m *Model) printStatus() tea.Cmd {
	return m.printInspection("Status", strings.TrimPrefix(m.statusContent(), "Status\n\n"))
}

func (m *Model) printDiff() tea.Cmd {
	return m.printInspection("Workspace changes", strings.TrimPrefix(m.diffContent(), "Workspace changes\n\n"))
}

func (m *Model) printInspection(heading, body string) tea.Cmd {
	if !m.options.NoColor {
		heading = lipgloss.NewStyle().Bold(true).Foreground(paletteFor(m.theme).session).Render(heading)
	}
	lines := strings.Split(strings.TrimSpace(body), "\n")
	for index := range lines {
		if lines[index] != "" {
			lines[index] = "  " + lines[index]
		}
	}

	return m.printScrollback(heading + "\n" + strings.Join(lines, "\n"))
}

func (m *Model) statusContent() string {
	modelState := m.controller.Model()
	configState := m.controller.Config()
	content := fmt.Sprintf(
		"Status\n\nWorkspace: %s\nSession: %s\nModel: %s\n"+
			"Variant: %s\nReasoning: %s\nProtocol: %s\nEndpoint: %s (%s)\n"+
			"Context: %s\nRequest output: %s\n"+
			"Compaction: %t (reserve %d · keep %d · summary max %d)\n"+
			"Process override: %t\nPhase: %s\nSandbox: %s\nApproval: %s\n"+
			"Tool search: %t\nPending approval: %s\nDetached: %t",
		m.options.Workspace,
		m.state.SessionID,
		modelState.Resolved.Ref,
		valueOrDefault(modelState.Resolved.Variant),
		reasoningOrDefault(modelState.Resolved.ReasoningLevel),
		modelState.Resolved.Protocol,
		modelState.Resolved.Endpoint.BaseURL,
		modelState.Resolved.Endpoint.Origin,
		knownLimit(modelState.Resolved.Limits.ContextWindow),
		optionalInt(modelState.Resolved.Options.MaxOutputTokens),
		configState.Compaction.Enabled,
		configState.Compaction.ReserveTokens,
		configState.Compaction.KeepRecentTokens,
		configState.Compaction.SummaryMaxTokens,
		modelState.Overridden,
		m.state.Phase,
		configState.Sandbox,
		configState.Approval,
		configState.ToolSearch,
		m.state.Approval.Kind,
		m.controller.Detached(),
	)
	if len(m.state.Diagnostics) == 0 {
		return content
	}

	lines := make([]string, 0, len(m.state.Diagnostics))
	for _, diagnostic := range m.state.Diagnostics {
		lines = append(lines, fmt.Sprintf(
			"- %s/%s: %s",
			diagnostic.Component,
			diagnostic.Code,
			diagnostic.Message,
		))
	}

	return content + "\n\nIntegrations:\n" + strings.Join(lines, "\n")
}

func (m *Model) diffContent() string {
	if m.state.Changes == nil {
		return "Workspace changes\n\nNo change report is available for the current interaction."
	}

	lines := []string{"Workspace changes", ""}
	for _, entry := range m.state.Changes.Entries {
		line := fmt.Sprintf("%s  %s", entry.Kind, entry.Path)
		if entry.PreviousPath != "" {
			line += " <- " + entry.PreviousPath
		}
		lines = append(lines, line)
	}
	if m.state.Changes.Truncated {
		lines = append(lines, "", "The report was truncated.")
	}
	if m.state.Changes.Diff != "" {
		lines = append(lines, "", m.state.Changes.Diff)
	}

	return strings.Join(lines, "\n")
}

func knownLimit(value int) string {
	if value == 0 {
		return "unknown"
	}

	return strconv.Itoa(value)
}

func optionalInt(value *int) string {
	if value == nil {
		return "provider default"
	}

	return strconv.Itoa(*value)
}
