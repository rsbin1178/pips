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
	"github.com/rsbin/pips/internal/coding/changes"
	"github.com/rsbin/pips/internal/coding/config"
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
	lines := strings.Split(strings.TrimSpace(sanitizeInspectionText(body)), "\n")
	for index := range lines {
		if lines[index] != "" {
			lines[index] = "  " + lines[index]
		}
	}

	return m.printScrollback(heading + "\n" + strings.Join(lines, "\n"))
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
	content := fmt.Sprintf(
		"Status\n\nWorkspace: %s\nSession: %s\nModel: %s\n"+
			"Variant: %s\nReasoning: %s\nProtocol: %s\nEndpoint: %s (%s)\n"+
			"Context: %s\nRequest output: %s\n"+
			"Compaction: %t (reserve %d · keep %d · summary max %d)\n"+
			"Model override: %t\nMode: %s\nConfigured mode: %s\nMode override: %t\n"+
			"Phase: %s\nSandbox: %s\nApproval: %s\nNetwork: %s\n"+
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
		modeState.Current,
		modeState.Configured,
		modeState.Overridden,
		m.state.Phase,
		permissionStatusText(
			string(permissions.SandboxProfile.Filesystem.Effective),
			string(permissions.SandboxProfile.Filesystem.Configured),
			permissions.SandboxProfile.Filesystem.EffectiveSource,
			permissions.SandboxProfile.Filesystem.ConfiguredSource,
			permissions.SandboxProfile.Filesystem.Overridden,
			false,
		),
		permissionStatusText(
			string(permissions.ApprovalPolicy.Effective),
			string(permissions.ApprovalPolicy.Configured),
			permissions.ApprovalPolicy.EffectiveSource,
			permissions.ApprovalPolicy.ConfiguredSource,
			permissions.ApprovalPolicy.Overridden,
			false,
		),
		permissionStatusText(
			string(permissions.SandboxProfile.Network.Effective),
			string(permissions.SandboxProfile.Network.Configured),
			permissions.SandboxProfile.Network.EffectiveSource,
			permissions.SandboxProfile.Network.ConfiguredSource,
			permissions.SandboxProfile.Network.Overridden,
			!permissions.SandboxProfile.NetworkEnforced,
		),
		configState.ToolSearch,
		m.state.Approval.Kind,
		m.controller.Detached(),
	)
	if m.worktreeSummary != "" {
		content += "\nRepository: " + m.worktreeSummary
	}
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

func permissionStatusText(
	current string,
	configured string,
	source config.SourceKind,
	configuredSource config.SourceKind,
	overridden bool,
	inactive bool,
) string {
	value := current + " (configured " + configured + "; " + permissionSourceText(source, configuredSource, overridden)
	if inactive {
		value += "; inactive under full-access"
	}

	return value + ")"
}
