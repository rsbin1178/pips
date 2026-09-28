//nolint:wsl_v5 // Inspection print formatting remains together for stable scrollback output.
package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/changes"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/modelcatalog"
	"github.com/rsbin1178/pips/internal/coding/runtimecontrol"
)

type workspaceStatusResultMsg struct {
	generation uint64
	status     changes.WorktreeStatus
	err        error
}

func (m *Model) printHelp() tea.Cmd {
	return m.printInspection(
		"Help",
		renderActionHelp(defaultActions, m.actionContext())+"\n\n"+
			"Ctrl+J / Shift+Enter newline\n"+
			"/team [objective] proposes or inspects a Team; recovery, Integration, and cleanup always require review.\n"+
			"/resume may show a read-only Team recovery badge; Enter resumes only the conversation.\n"+
			"The terminal owns conversation history: use its wheel or scrollback keys to navigate, "+
			"and drag normally to select and copy text.",
	)
}

func (m *Model) printStatus() tea.Cmd {
	return m.loadWorkspaceStatus()
}

func (m *Model) loadWorkspaceStatus() tea.Cmd {
	if m.worktreeLoading {
		return nil
	}
	m.worktreeLoading = true
	m.worktreeGeneration++
	generation := m.worktreeGeneration
	queryContext, cancel := context.WithCancel(m.ctx)
	m.worktreeCancel = cancel
	m.setLayout()

	return func() tea.Msg {
		status, err := m.controller.WorkspaceStatus(queryContext)

		return workspaceStatusResultMsg{generation: generation, status: status, err: err}
	}
}

func (m *Model) cancelWorkspaceStatus() {
	if m.worktreeCancel != nil {
		m.worktreeCancel()
		m.worktreeCancel = nil
	}
	m.worktreeGeneration++
	m.worktreeLoading = false
	m.setLayout()
}

func (m *Model) printInspection(heading, body string) tea.Cmd {
	if !m.options.NoColor {
		heading = lipgloss.NewStyle().Bold(true).Foreground(paletteFor(m.theme).session).Render(heading)
	}
	lines := wrapInspectionLines(sanitizeInspectionText(body), max(1, m.width))

	return m.printScrollback(heading + "\n" + strings.Join(lines, "\n"))
}

// wrapInspectionLines wraps long inspection rows with a hanging indent so a
// sentence breaks at a word boundary rather than mid-word.
func wrapInspectionLines(body string, width int) []string {
	indent := "  "
	available := max(1, width-len(indent))
	lines := make([]string, 0, 8)

	for line := range strings.SplitSeq(strings.TrimSpace(body), "\n") {
		line = strings.TrimRight(line, " ")
		if line == "" || ansi.StringWidth(line) <= available {
			lines = append(lines, indent+line)

			continue
		}

		leading := line[:len(line)-len(strings.TrimLeft(line, " "))]
		wrapped := ansi.Wrap(
			strings.TrimLeft(line, " "),
			max(1, available-ansi.StringWidth(leading)),
			"",
		)
		for part := range strings.SplitSeq(wrapped, "\n") {
			lines = append(lines, indent+leading+part)
		}
	}

	return lines
}

func sanitizeInspectionText(value string) string {
	value = ansi.Strip(value)

	return strings.Map(func(character rune) rune {
		if character == '\n' || character == '\t' || !unicode.IsControl(character) {
			return character
		}

		return -1
	}, value)
}

func (m *Model) statusContent() string {
	modelState := m.controller.Model()
	modeState := m.controller.Mode()
	configState := m.controller.Config()
	permissions := m.permissionState()
	// Rows with nothing to report are omitted, and yes/no flags read as words,
	// so the report stays scannable instead of listing empty fields.
	lines := []string{
		"Status",
		"",
		"Workspace: " + m.options.Workspace,
		"Session: " + m.state.SessionID,
		"Model: " + modelState.Resolved.Ref.String(),
		"Variant: " + valueOrDefault(modelState.Resolved.Variant),
		"Reasoning: " + reasoningOrDefault(modelState.Resolved.ReasoningLevel),
		"Mode: " + string(modeState.Current) + modeStatusSuffix(modeState),
		"Phase: " + phaseText(m.state.Phase),
	}
	lines = append(lines, optionalStatusLine(
		"Protocol", strings.TrimSpace(string(modelState.Resolved.Protocol)),
	))
	lines = append(lines, optionalStatusLine("Endpoint", resolvedEndpointText(modelState.Resolved.Endpoint)))
	lines = append(lines,
		"Context: "+knownLimit(modelState.Resolved.Limits.ContextWindow),
		"Request output: "+optionalInt(modelState.Resolved.Options.MaxOutputTokens),
		"Compaction: "+compactionStatusText(configState.Compaction),
		"Sandbox: "+permissionModeText(permissions.SandboxProfile.Filesystem.Effective),
		"Approval: "+permissionApprovalText(permissions.ApprovalPolicy.Effective),
		"Network: "+permissionStatusNetworkText(
			permissions.SandboxProfile.Network.Effective,
			permissions.SandboxProfile.NetworkEnforced,
		),
	)
	if modelState.Overridden || modeState.Overridden {
		overrides := make([]string, 0, 2)
		if modelState.Overridden {
			overrides = append(overrides, "model")
		}
		if modeState.Overridden {
			overrides = append(overrides, "mode")
		}
		lines = append(lines, "Overrides: "+strings.Join(overrides, ", "))
	}
	if !configState.ToolSearch {
		lines = append(lines, "Tool search: off")
	}
	if kind := strings.TrimSpace(string(m.state.Approval.Kind)); kind != "" {
		lines = append(lines, "Pending approval: "+humanizeStatusCode(kind))
	}
	if m.controller.Detached() {
		lines = append(lines, "Detached: the runtime is no longer attached to this session")
	}

	content := strings.Join(lines, "\n")
	if m.worktreeSummary != "" {
		content += "\nRepository: " + m.worktreeSummary
	}
	if len(m.state.Diagnostics) == 0 {
		return content
	}

	diagnostics := make([]string, 0, len(m.state.Diagnostics))
	for _, diagnostic := range m.state.Diagnostics {
		line := "- " + diagnosticTitle(diagnostic)
		if message := diagnosticBody(diagnostic); message != "" {
			line += ": " + message
		}
		diagnostics = append(diagnostics, line)
	}

	return content + "\n\nIntegrations:\n" + strings.Join(diagnostics, "\n")
}

// modeStatusSuffix reports a mode that differs from the configured default
// without exposing where either value came from.
func modeStatusSuffix(state runtimecontrol.ModeState) string {
	if !state.Overridden {
		return ""
	}

	return " (session override)"
}

// optionalStatusLine drops a row whose value is unknown rather than printing an
// empty field.
func optionalStatusLine(label, value string) string {
	if value == "" {
		return label + ": unknown"
	}

	return label + ": " + value
}

func resolvedEndpointText(endpoint modelcatalog.Endpoint) string {
	base := strings.TrimSpace(endpoint.BaseURL)
	origin := strings.TrimSpace(endpoint.Origin)
	switch {
	case base == "" && origin == "":
		return ""
	case origin == "" || origin == base:
		return base
	case base == "":
		return origin
	default:
		return base + " (" + origin + ")"
	}
}

func compactionStatusText(config config.CompactionConfig) string {
	if !config.Enabled {
		return "off"
	}

	return fmt.Sprintf(
		"on (reserve %d · keep %d · summary max %d)",
		config.ReserveTokens, config.KeepRecentTokens, config.SummaryMaxTokens,
	)
}

func phaseText(phase coding.Phase) string {
	switch phase {
	case coding.PhaseIdle:
		return "idle"
	case coding.PhaseRunning:
		return "running"
	case coding.PhasePaused:
		return "paused"
	default:
		return humanizeStatusCode(string(phase))
	}
}

func compactWorktreeSummary(status changes.WorktreeStatus) string {
	if !status.Repository() {
		return "not a Git repository"
	}
	entries := status.Entries()
	if len(entries) == 0 && status.ProtectedOmitted() == 0 {
		return branchStatusLabel(status.Branch()) + " · clean"
	}

	return fmt.Sprintf(
		"%s · %d changed · %d protected omitted",
		branchStatusLabel(status.Branch()),
		len(entries),
		status.ProtectedOmitted(),
	)
}

func branchStatusLabel(branch changes.Branch) string {
	name := branch.Head
	switch {
	case branch.Detached:
		name = "detached"
	case branch.Unborn && name == "":
		name = "unborn"
	case name == "":
		name = "unknown branch"
	}
	if branch.Ahead > 0 || branch.Behind > 0 {
		name += fmt.Sprintf(" ↑%d ↓%d", branch.Ahead, branch.Behind)
	}

	return name
}

func pathStateGlyph(state changes.PathState) string {
	switch state {
	case changes.PathAdded:
		return "A"
	case changes.PathModified:
		return "M"
	case changes.PathDeleted:
		return "D"
	case changes.PathRenamed:
		return "R"
	case changes.PathCopied:
		return "C"
	case changes.PathTypeChanged:
		return "T"
	case changes.PathUnmerged:
		return "U"
	case changes.PathUntracked:
		return "?"
	default:
		return "·"
	}
}

func safeStatusPath(value string) string {
	for _, character := range value {
		if unicode.IsControl(character) {
			return strconv.QuoteToGraphic(value)
		}
	}

	return value
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

func permissionStatusNetworkText(
	mode config.SandboxNetworkMode,
	active bool,
) string {
	if !active {
		return "Unrestricted under Full access"
	}

	return permissionNetworkText(mode, true) + " (active)"
}
