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
	return m.printInspection("Status", strings.TrimPrefix(m.statusContent(), "Status\n\n"))
}

func (m *Model) printDiff() tea.Cmd {
	return m.printInspection(
		"Pips-attributed changes",
		strings.TrimPrefix(m.diffContent(), "Pips-attributed changes\n\n"),
	)
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
	content := fmt.Sprintf(
		"Status\n\nWorkspace: %s\nSession: %s\nModel: %s\n"+
			"Variant: %s\nReasoning: %s\nProtocol: %s\nEndpoint: %s (%s)\n"+
			"Context: %s\nRequest output: %s\n"+
			"Compaction: %t (reserve %d · keep %d · summary max %d)\n"+
			"Model override: %t\nMode: %s\nConfigured mode: %s\nMode override: %t\n"+
			"Phase: %s\nSandbox: %s\nApproval: %s\n"+
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
		configState.Sandbox,
		configState.Approval,
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

func (m *Model) diffContent() string {
	if m.state.Changes == nil {
		return "Pips-attributed changes\n\nNo attributed change report is available for the current interaction."
	}

	lines := []string{"Pips-attributed changes", ""}
	lines = append(lines, workspaceChangeSummary(*m.state.Changes), "")
	for _, entry := range m.state.Changes.Entries {
		line := workspaceChangeGlyph(entry.Kind) + "  " + workspaceChangePath(entry)
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

func worktreeStatusContent(status changes.WorktreeStatus) string {
	if !status.Repository() {
		return "Workspace changes\n\nThis workspace is not a Git repository."
	}

	lines := []string{"Workspace changes · " + branchStatusLabel(status.Branch()), ""}
	entries := status.Entries()
	lines = appendStatusSection(
		lines, "Staged", status.Staged(), entries,
		func(entry changes.StatusEntry) bool { return entry.Index != changes.PathUnchanged },
		func(entry changes.StatusEntry) changes.PathState { return entry.Index },
	)
	lines = appendStatusSection(
		lines, "Unstaged", status.Unstaged(), entries,
		func(entry changes.StatusEntry) bool {
			return entry.Worktree != changes.PathUnchanged &&
				entry.Worktree != changes.PathUntracked
		},
		func(entry changes.StatusEntry) changes.PathState { return entry.Worktree },
	)
	lines = appendStatusSection(
		lines, "Untracked", status.Untracked(), entries,
		func(entry changes.StatusEntry) bool { return entry.Worktree == changes.PathUntracked },
		func(changes.StatusEntry) changes.PathState { return changes.PathUntracked },
	)
	if status.ProtectedOmitted() > 0 {
		lines = append(lines, fmt.Sprintf(
			"Protected product metadata omitted: %d path(s)",
			status.ProtectedOmitted(),
		))
	}
	if len(entries) == 0 && status.ProtectedOmitted() == 0 {
		lines = append(lines, "Working tree clean.")
	}

	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func appendStatusSection(
	lines []string,
	title string,
	section changes.DiffSection,
	entries []changes.StatusEntry,
	include func(changes.StatusEntry) bool,
	state func(changes.StatusEntry) changes.PathState,
) []string {
	if section.Summary.Files == 0 {
		return lines
	}

	summary := fmt.Sprintf(
		"%s (%d files, +%d -%d)",
		title,
		section.Summary.Files,
		section.Summary.Additions,
		section.Summary.Deletions,
	)
	if section.Summary.Binary > 0 {
		summary += fmt.Sprintf(" · %d binary", section.Summary.Binary)
	}
	if section.Summary.Omitted > 0 {
		summary += fmt.Sprintf(" · %d preview omitted", section.Summary.Omitted)
	}
	if section.Truncated {
		summary += " · truncated"
	}
	lines = append(lines, summary)
	for _, entry := range entries {
		if !include(entry) {
			continue
		}
		path := safeStatusPath(entry.Path)
		if entry.PreviousPath != "" {
			path = safeStatusPath(entry.PreviousPath) + " -> " + path
		}
		lines = append(lines, "  "+pathStateGlyph(state(entry))+"  "+path)
	}
	if section.Diff != "" {
		lines = append(lines, "", section.Diff)
	}
	lines = append(lines, "")

	return lines
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
