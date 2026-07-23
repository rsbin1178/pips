//nolint:wsl_v5 // Overlay reducers keep keyboard transitions locally visible.
package tui

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/modelcatalog"
	"github.com/rsbin/pips/internal/coding/subagent"
)

const defaultSelectionLabel = "default"

type overlayKind uint8

const (
	overlayNone overlayKind = iota
	overlayApproval
	overlayDiff
	overlayModel
	overlayHelp
	overlayStatus
	overlayTree
	overlayCompact
	overlayToolDetail
	overlayAgents
)

type overlayState struct {
	kind        overlayKind
	cursor      int
	query       string
	err         error
	loading     bool
	choices     []approval.Choice
	models      []modelcatalog.Entry
	selection   modelcatalog.Selection
	controlling bool
	offset      int
	tree        coding.SessionTree
	preview     coding.CompactionPreview
	forkMode    bool
	agents      []subagent.Summary
	agentDetail *subagent.Detail
	toolDetail  *toolDetailView
}

type overlayDataMsg struct {
	kind      overlayKind
	err       error
	tree      coding.SessionTree
	preview   coding.CompactionPreview
	agents    []subagent.Summary
	detail    subagent.Detail
	hasAgents bool
	hasDetail bool
}

type controlOperation uint8

const (
	operationNew controlOperation = iota
	operationResume
	operationModel
	operationReload
	operationFork
)

type controlResultMsg struct {
	operation controlOperation
	err       error
}

func (m *Model) openOverlay(kind overlayKind) tea.Cmd {
	m.overlay = overlayState{kind: kind}

	switch kind {
	case overlayModel:
		state := m.controller.Model()
		m.overlay.models = m.controller.Models()
		m.overlay.selection = state.Selection
		for index, entry := range m.filteredModels() {
			if entry.Ref == state.Resolved.Ref {
				m.overlay.cursor = index
				break
			}
		}
	case overlayTree:
		m.overlay.loading = true

		return func() tea.Msg {
			value, err := m.controller.Tree(m.ctx)

			return overlayDataMsg{kind: overlayTree, tree: value, err: err}
		}
	case overlayCompact:
		m.overlay.loading = true

		return func() tea.Msg {
			value, err := m.controller.PreviewCompaction(m.ctx)

			return overlayDataMsg{kind: overlayCompact, preview: value, err: err}
		}
	case overlayAgents:
		m.overlay.loading = true

		return func() tea.Msg {
			values, err := m.controller.ListSubagents(m.ctx)

			return overlayDataMsg{
				kind: overlayAgents, agents: values, hasAgents: true, err: err,
			}
		}
	case overlayApproval:
		m.overlay.cursor = 0
	case overlayNone, overlayDiff, overlayHelp, overlayStatus, overlayToolDetail:
	}

	return nil
}

func (m *Model) syncApprovalOverlay() {
	if m.state.Approval.Kind == coding.ApprovalNone {
		if m.overlay.kind == overlayApproval {
			m.overlay = overlayState{}
		}

		return
	}

	choices := approvalChoices(m.state.Approval)
	if m.commandPicker.open {
		m.closeCommandPicker(true)
	}
	if m.sessionPicker.open {
		m.closeSessionPicker(true)
	}
	cursor := 0
	if m.state.Approval.Kind == coding.ApprovalReview {
		if index := slices.Index(choices, approval.ChoiceDeny); index >= 0 {
			cursor = index
		}
	}
	m.overlay = overlayState{
		kind: overlayApproval, cursor: cursor, choices: choices,
	}
}

func approvalChoices(state coding.ApprovalState) []approval.Choice {
	if state.Required != nil {
		return slices.Clone(state.Required.Choices)
	}
	if state.Unknown != nil {
		return slices.Clone(state.Unknown.Choices)
	}

	return nil
}

func (m *Model) updateOverlayKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := message.String()
	if m.overlay.controlling {
		return m, nil
	}
	if command, handled := m.handleDetailOverlayClose(key); handled {
		return m, command
	}
	if m.overlay.kind != overlayApproval && isOverlayDismissKey(key) {
		m.overlay = overlayState{}

		return m, nil
	}

	switch m.overlay.kind {
	case overlayApproval:
		return m.updateApprovalOverlay(key)
	case overlayModel:
		return m.updateModelOverlay(message)
	case overlayTree:
		return m.updateTreeOverlay(message)
	case overlayCompact:
		return m.updateCompactOverlay(key)
	case overlayDiff, overlayHelp, overlayStatus, overlayToolDetail:
		return m.updateReadOnlyOverlay(key)
	case overlayAgents:
		return m.updateAgentsOverlay(message)
	case overlayNone:
		return m, nil
	default:
		return m, nil
	}
}

func (m *Model) handleDetailOverlayClose(key string) (tea.Cmd, bool) {
	if m.overlay.kind == overlayToolDetail &&
		(key == "ctrl+t" || isOverlayDismissKey(key)) {
		m.overlay = overlayState{}

		return m.composer.Focus(), true
	}
	if m.overlay.kind != overlayAgents {
		return nil, false
	}
	if key == "ctrl+t" {
		m.overlay = overlayState{}

		return m.composer.Focus(), true
	}
	if m.overlay.agentDetail == nil || !isOverlayDismissKey(key) {
		return nil, false
	}

	m.overlay.agentDetail = nil
	m.overlay.offset = 0

	return nil, true
}

func isOverlayDismissKey(key string) bool {
	return key == keyEscape || key == keyCtrlC
}

func (m *Model) openTreeOverlay(forkMode bool) tea.Cmd {
	command := m.openOverlay(overlayTree)
	m.overlay.forkMode = forkMode

	return command
}

func (m *Model) updateTreeOverlay(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	values := m.filteredTreeNodes()
	key := message.String()
	switch key {
	case "up", "k":
		m.overlay.cursor = wrapIndex(m.overlay.cursor-1, len(values))
	case keyDown, "j", keyTab:
		m.overlay.cursor = wrapIndex(m.overlay.cursor+1, len(values))
	case keyBackspace:
		m.overlay.query = trimLastRune(m.overlay.query)
		m.overlay.cursor = 0
	case "f":
		m.overlay.forkMode = true
	case keyEnter, "s":
		if len(values) == 0 || m.overlay.loading || m.state.Phase != coding.PhaseIdle {
			return m, nil
		}
		entryID := values[m.overlay.cursor].ID
		if m.overlay.forkMode {
			return m, m.runControl(operationFork, entryID, modelcatalog.Selection{})
		}
		summarize := key == "s"
		m.overlay = overlayState{}

		return m, m.startStream(func(ctx context.Context) iter.Seq2[coding.Event, error] {
			return m.controller.Navigate(ctx, entryID, summarize)
		})
	default:
		if message.Key().Text != "" {
			m.overlay.query += message.Key().Text
			m.overlay.cursor = 0
		}
	}

	return m, nil
}

func (m *Model) updateCompactOverlay(key string) (tea.Model, tea.Cmd) {
	if key != keyEnter || m.overlay.loading || !m.overlay.preview.Available ||
		m.state.Phase != coding.PhaseIdle {
		return m, nil
	}
	request := coding.CompactionRequest{PreviewToken: m.overlay.preview.Token}
	m.overlay = overlayState{}

	return m, m.startStream(func(ctx context.Context) iter.Seq2[coding.Event, error] {
		return m.controller.Compact(ctx, request)
	})
}

func (m *Model) updateReadOnlyOverlay(key string) (tea.Model, tea.Cmd) {
	visible := max(1, m.height-5)
	lineCount := strings.Count(m.overlayContent(), "\n") + 1
	maximum := max(0, lineCount-visible)

	switch key {
	case "up", "k":
		m.overlay.offset = max(0, m.overlay.offset-1)
	case keyDown, "j":
		m.overlay.offset = min(maximum, m.overlay.offset+1)
	case "pgup":
		m.overlay.offset = max(0, m.overlay.offset-visible)
	case "pgdown":
		m.overlay.offset = min(maximum, m.overlay.offset+visible)
	case "home":
		m.overlay.offset = 0
	case "end":
		m.overlay.offset = maximum
	}

	return m, nil
}

func (m *Model) updateApprovalOverlay(key string) (tea.Model, tea.Cmd) {
	if len(m.overlay.choices) == 0 {
		return m, nil
	}

	switch key {
	case "up", "left", "k":
		m.overlay.cursor = wrapIndex(m.overlay.cursor-1, len(m.overlay.choices))
	case keyDown, "right", "j", keyTab:
		m.overlay.cursor = wrapIndex(m.overlay.cursor+1, len(m.overlay.choices))
	case keyEscape:
		if index := slices.Index(m.overlay.choices, approval.ChoiceDeny); index >= 0 {
			m.overlay.cursor = index
		}
	case "o":
		return m.resolveApprovalChoice(approval.ChoiceAllowOnce)
	case "s":
		return m.resolveApprovalChoice(approval.ChoiceAllowSession)
	case "d":
		return m.resolveApprovalChoice(approval.ChoiceDeny)
	case "r":
		return m.resolveApprovalChoice(approval.ChoiceRetry)
	case "f":
		return m.resolveApprovalChoice(approval.ChoiceMarkFailed)
	case "a":
		return m.resolveApprovalChoice(approval.ChoiceAcknowledge)
	case keyEnter:
		return m.resolveApprovalChoice(m.overlay.choices[m.overlay.cursor])
	}

	return m, nil
}

func (m *Model) resolveApprovalChoice(choice approval.Choice) (tea.Model, tea.Cmd) {
	if !slices.Contains(m.overlay.choices, choice) {
		return m, nil
	}

	requestID := ""
	if m.state.Approval.Required != nil {
		requestID = m.state.Approval.Required.RequestID
	} else if m.state.Approval.Unknown != nil {
		requestID = m.state.Approval.Unknown.RequestID
	}
	if requestID == "" {
		return m, nil
	}
	if m.bridge != nil || m.starting {
		m.overlay.err = errors.New("approval recovery is still being reconciled")

		return m, nil
	}

	m.overlay.loading = true

	return m, m.startStream(func(ctx context.Context) iter.Seq2[coding.Event, error] {
		return m.controller.Resolve(ctx, approval.Resolution{
			RequestID: requestID,
			Choice:    choice,
		})
	})
}

func (m *Model) updateModelOverlay(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := message.String()
	filtered := m.filteredModels()
	switch key {
	case "up":
		m.overlay.cursor = wrapIndex(m.overlay.cursor-1, len(filtered))
	case keyDown, keyTab:
		m.overlay.cursor = wrapIndex(m.overlay.cursor+1, len(filtered))
	case "v":
		m.cycleVariant(1)
	case "V":
		m.cycleVariant(-1)
	case "r":
		m.cycleReasoning(1)
	case "R":
		m.cycleReasoning(-1)
	case "ctrl+u":
		m.overlay.query = ""
		m.overlay.cursor = 0
	case keyBackspace:
		m.overlay.query = trimLastRune(m.overlay.query)
		m.overlay.cursor = 0
	case keyEnter:
		if len(filtered) == 0 {
			return m, nil
		}
		m.selectOverlayModel(filtered[m.overlay.cursor])

		return m, m.runControl(operationModel, "", m.overlay.selection)
	default:
		if message.Key().Text != "" {
			m.overlay.query += message.Key().Text
			m.overlay.cursor = 0
		}
	}

	return m, nil
}

func (m *Model) cycleVariant(direction int) {
	entry, ok := m.overlayModelEntry()
	if !ok {
		return
	}
	m.selectOverlayModel(entry)
	variants := append([]string{""}, entry.Variants...)
	index := slices.Index(variants, m.overlay.selection.Variant)
	m.overlay.selection.Variant = variants[wrapIndex(index+direction, len(variants))]
}

func (m *Model) cycleReasoning(direction int) {
	entry, ok := m.overlayModelEntry()
	if !ok {
		return
	}
	m.selectOverlayModel(entry)
	levels := append([]config.ReasoningLevel{""}, entry.ReasoningLevels...)
	current := config.ReasoningLevel("")
	if m.overlay.selection.ReasoningOverride != nil {
		current = *m.overlay.selection.ReasoningOverride
	}
	next := levels[wrapIndex(slices.Index(levels, current)+direction, len(levels))]
	if next == "" {
		m.overlay.selection.ReasoningOverride = nil
	} else {
		m.overlay.selection.ReasoningOverride = new(next)
	}
}

func (m *Model) runControl(
	operation controlOperation,
	sessionID string,
	selected modelcatalog.Selection,
) tea.Cmd {
	switch {
	case m.commandPicker.open:
		m.commandPicker.loading = true
		m.commandPicker.controlling = true
		m.commandPicker.err = nil
	case m.sessionPicker.open:
		m.sessionPicker.loading = true
		m.sessionPicker.controlling = true
		m.sessionPicker.err = nil
	default:
		m.overlay.loading = true
		m.overlay.controlling = true
		m.overlay.err = nil
	}

	return func() tea.Msg {
		var err error
		switch operation {
		case operationNew:
			err = m.controller.NewSession(m.ctx)
		case operationResume:
			err = m.controller.ResumeSession(m.ctx, sessionID)
		case operationModel:
			err = m.controller.SwitchModel(m.ctx, selected)
		case operationReload:
			err = m.controller.Reload(m.ctx)
		case operationFork:
			err = m.controller.ForkSession(m.ctx, sessionID)
		}

		return controlResultMsg{operation: operation, err: err}
	}
}

func (m *Model) filteredModels() []modelcatalog.Entry {
	query := strings.ToLower(strings.TrimSpace(m.overlay.query))
	filtered := make([]modelcatalog.Entry, 0, len(m.overlay.models))
	for _, entry := range m.overlay.models {
		if query == "" || strings.Contains(strings.ToLower(entry.Ref.String()), query) {
			filtered = append(filtered, entry)
		}
	}

	return filtered
}

func (m *Model) overlayModelEntry() (modelcatalog.Entry, bool) {
	filtered := m.filteredModels()
	if len(filtered) == 0 {
		return modelcatalog.Entry{}, false
	}

	return filtered[m.overlay.cursor], true
}

func (m *Model) selectOverlayModel(entry modelcatalog.Entry) {
	if m.overlay.selection.Ref != entry.Ref {
		m.overlay.selection = modelcatalog.Selection{Ref: entry.Ref}
	}
}

func (m *Model) filteredTreeNodes() []coding.SessionNode {
	query := strings.ToLower(strings.TrimSpace(m.overlay.query))
	filtered := make([]coding.SessionNode, 0, len(m.overlay.tree.Nodes))
	for _, node := range m.overlay.tree.Nodes {
		if query == "" || strings.Contains(strings.ToLower(node.ID), query) ||
			strings.Contains(strings.ToLower(node.Label), query) ||
			strings.Contains(string(node.Kind), query) {
			filtered = append(filtered, node)
		}
	}

	return filtered
}

func (m *Model) renderOverlay(base string) string {
	if m.overlay.kind == overlayNone {
		return base
	}

	content := m.overlayContent()
	boxWidth := min(max(24, m.width-4), 78)
	if m.width < 32 {
		boxWidth = max(1, m.width)
	}
	content = fitOverlayContent(content, boxWidth-4, max(1, m.height-4), m.overlay.offset)

	border := lipgloss.RoundedBorder()
	style := lipgloss.NewStyle().Width(max(1, boxWidth-4)).Padding(1)
	if m.options.NoColor {
		border = lipgloss.NormalBorder()
	} else {
		style = style.
			Foreground(lipgloss.Color("#E6EDF3")).
			Background(lipgloss.Color("#161B22")).
			BorderForeground(lipgloss.Color("#6E7681"))
	}
	box := style.Border(border, true).Render(content)
	x := max(0, (m.width-lipgloss.Width(box))/2)
	y := max(0, (m.height-lipgloss.Height(box))/2)

	return lipgloss.NewCompositor(
		lipgloss.NewLayer(base),
		lipgloss.NewLayer(box).X(x).Y(y).Z(1),
	).Render()
}

func (m *Model) overlayContent() string {
	content := m.baseOverlayContent()
	if m.overlay.loading {
		content += "\n\nWorking…"
	}
	if m.overlay.err != nil {
		content += "\n\nError: " + safeError(m.overlay.err)
	}

	return content
}

func (m *Model) baseOverlayContent() string {
	var content string

	switch m.overlay.kind {
	case overlayApproval:
		content = m.approvalOverlayContent()
	case overlayDiff:
		content = m.diffOverlayContent()
	case overlayModel:
		content = m.modelOverlayContent()
	case overlayHelp:
		content = "Help\n\n" + renderActionHelp(defaultActions, m.actionContext()) +
			"\n\nCtrl+J / Shift+Enter newline\n" +
			"The terminal owns conversation history: use its wheel or scrollback keys to navigate, " +
			"and drag normally to select and copy text."
	case overlayStatus:
		content = m.statusOverlayContent()
	case overlayTree:
		content = m.treeOverlayContent()
	case overlayCompact:
		content = m.compactOverlayContent()
	case overlayToolDetail:
		content = m.toolDetailOverlayContent()
	case overlayAgents:
		content = m.agentsOverlayContent()
	case overlayNone:
	}

	return content
}

func (m *Model) toolDetailOverlayContent() string {
	if m.overlay.toolDetail == nil {
		return "Tool details\n\nNo Tool activity is available."
	}

	detail := m.overlay.toolDetail
	return detail.title + "\n\n" + detail.content +
		"\n\n↑/↓ or PgUp/PgDn scroll · Ctrl+T/Esc close"
}

func (m *Model) statusOverlayContent() string {
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

func (m *Model) approvalOverlayContent() string {
	var header string
	if m.state.Approval.Required != nil {
		value := m.state.Approval.Required
		header = fmt.Sprintf(
			"Approval required\n\nTool: %s\nCommand: %s\nCWD: %s\nReason: %s",
			value.Tool,
			strings.Join(value.Command, " "),
			value.CWD,
			value.Justification,
		)
	} else if m.state.Approval.Unknown != nil {
		value := m.state.Approval.Unknown
		header = fmt.Sprintf(
			"Outcome unknown\n\nTool: %s\nAttempt: %d\nReason: %s",
			value.Tool,
			value.Attempt,
			value.Reason,
		)
	}

	return header + "\n\n" + renderChoices(m.overlay.choices, m.overlay.cursor) +
		"\n\n↑/↓ choose · Enter confirm"
}

func (m *Model) diffOverlayContent() string {
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

func (m *Model) treeOverlayContent() string {
	title := "Session tree"
	if m.overlay.forkMode {
		title = "Fork session from node"
	}
	lines := []string{title, "", "Filter: " + m.overlay.query, ""}
	if m.overlay.loading {
		return strings.Join(lines, "\n")
	}
	values := m.filteredTreeNodes()
	if len(values) == 0 {
		lines = append(lines, "No matching nodes.")
	}
	for index, node := range values {
		cursor := "  "
		if index == m.overlay.cursor {
			cursor = "> "
		}
		path := " "
		if node.OnActivePath {
			path = "*"
		}
		current := ""
		if node.Current {
			current = " [current]"
		}
		label := ""
		if node.Label != "" {
			label = "  " + node.Label
		}
		indent := strings.Repeat("  ", min(node.Depth, 12))
		lines = append(lines, fmt.Sprintf(
			"%s%s%s%s  %s  %s%s%s",
			cursor, path, indent, treeConnector(node.Depth), shortDisplayID(node.ID),
			node.Kind, label, current,
		))
	}
	if m.overlay.tree.Truncated {
		lines = append(lines, "", fmt.Sprintf(
			"Showing %d of %d nodes (bounded).",
			len(m.overlay.tree.Nodes), m.overlay.tree.TotalNodes,
		))
	}
	if m.overlay.forkMode {
		lines = append(lines, "", "↑/↓ choose · type search · Enter fork · Esc cancel")
	} else {
		lines = append(lines, "", "Enter navigate · s navigate with summary · f fork mode · Esc close")
	}

	return strings.Join(lines, "\n")
}

func treeConnector(depth int) string {
	if depth == 0 {
		return "─ "
	}

	return "└ "
}

func shortDisplayID(value string) string {
	const limit = 12
	if utf8.RuneCountInString(value) <= limit {
		return value
	}

	runes := []rune(value)

	return string(runes[:limit]) + "…"
}

func (m *Model) compactOverlayContent() string {
	lines := []string{"Compact context", ""}
	if m.overlay.loading {
		return strings.Join(lines, "\n")
	}
	preview := m.overlay.preview
	if !preview.Available {
		lines = append(lines, "Unavailable: "+preview.DisabledReason, "", "Esc close")

		return strings.Join(lines, "\n")
	}
	lines = append(lines,
		fmt.Sprintf("Estimated context: %d tokens", preview.EstimatedTokens),
		fmt.Sprintf("Automatic threshold: %d tokens", preview.ThresholdTokens),
		fmt.Sprintf("Messages summarized: %d", preview.SummarizedMessages),
		fmt.Sprintf("Messages retained: %d", preview.KeptMessages),
	)
	if preview.SplitTurn {
		lines = append(lines, "The cut splits a large turn; its prefix is summarized separately.")
	}
	lines = append(lines, "", "Compaction may omit details. The durable branch remains recoverable.",
		"Enter confirm · Esc cancel")

	return strings.Join(lines, "\n")
}

func (m *Model) modelOverlayContent() string {
	lines := []string{"Switch model (current process only)", "", "Filter: " + m.overlay.query, ""}
	values := m.filteredModels()
	for index, entry := range values {
		prefix := "  "
		if index == m.overlay.cursor {
			prefix = "> "
		}
		lines = append(lines, prefix+entry.Ref.String())
	}
	if len(values) == 0 {
		lines = append(lines, "No matching models.")
	} else if entry, ok := m.overlayModelEntry(); ok {
		variant := defaultSelectionLabel
		reasoning := defaultSelectionLabel
		if m.overlay.selection.Ref == entry.Ref {
			variant = valueOrDefault(m.overlay.selection.Variant)
			reasoning = reasoningOrDefault(m.overlay.selection.ReasoningOverride)
		}
		lines = append(
			lines,
			"",
			"Variant: "+variant,
			"Reasoning: "+reasoning,
		)
	}
	lines = append(lines, "", "↑/↓ model · type search · v/V variant · r/R reasoning · Enter apply")

	return strings.Join(lines, "\n")
}

func valueOrDefault(value string) string {
	if value == "" {
		return defaultSelectionLabel
	}

	return value
}

func reasoningOrDefault(value *config.ReasoningLevel) string {
	if value == nil || *value == "" {
		return defaultSelectionLabel
	}

	return string(*value)
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

func renderChoices(choices []approval.Choice, cursor int) string {
	lines := make([]string, 0, len(choices))
	for index, choice := range choices {
		prefix := "  "
		if index == cursor {
			prefix = "> "
		}
		lines = append(lines, prefix+string(choice))
	}

	return strings.Join(lines, "\n")
}

func fitOverlayContent(content string, width, height, offset int) string {
	lines := strings.Split(content, "\n")
	if len(lines) > height {
		visible := max(1, height-1)
		start := min(max(0, offset), len(lines)-visible)
		end := min(len(lines), start+visible)
		window := slices.Clone(lines[start:end])
		window = append(window, fmt.Sprintf("… lines %d-%d/%d …", start+1, end, len(lines)))
		lines = window
	}
	for index := range lines {
		lines[index] = ansi.Truncate(lines[index], max(1, width), "…")
	}

	return strings.Join(lines, "\n")
}

func wrapIndex(value, length int) int {
	if length <= 0 {
		return 0
	}

	return (value%length + length) % length
}

func trimLastRune(value string) string {
	_, size := utf8.DecodeLastRuneInString(value)
	if size == 0 {
		return value
	}

	return value[:len(value)-size]
}
