//nolint:wsl_v5 // Overlay reducers keep keyboard transitions locally visible.
package tui

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/openai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/session"
)

type overlayKind uint8

const (
	overlayNone overlayKind = iota
	overlayApproval
	overlayDiff
	overlaySession
	overlayModel
	overlayCommand
	overlayHelp
	overlayStatus
)

type overlayState struct {
	kind        overlayKind
	cursor      int
	query       string
	err         error
	loading     bool
	sessions    []session.Metadata
	choices     []approval.Choice
	model       config.ModelConfig
	modelDirty  bool
	controlling bool
	offset      int
}

type overlayDataMsg struct {
	kind     overlayKind
	sessions []session.Metadata
	err      error
}

type controlOperation uint8

const (
	operationNew controlOperation = iota
	operationResume
	operationModel
	operationReload
)

type controlResultMsg struct {
	operation controlOperation
	err       error
}

type commandDescriptor struct {
	name        string
	description string
	idleOnly    bool
}

var commands = []commandDescriptor{
	{name: "new", description: "start a new session", idleOnly: true},
	{name: "resume", description: "resume a workspace session", idleOnly: true},
	{name: "model", description: "switch the process-local model", idleOnly: true},
	{name: "diff", description: "inspect workspace changes"},
	{name: "reload", description: "reload resources and integrations", idleOnly: true},
	{name: "status", description: "show runtime status"},
	{name: "help", description: "show keyboard help"},
	{name: "quit", description: "exit Pips"},
}

func (m *Model) openOverlay(kind overlayKind) tea.Cmd {
	m.overlay = overlayState{kind: kind}

	switch kind {
	case overlaySession:
		m.overlay.loading = true

		return func() tea.Msg {
			values, err := m.controller.ListSessions(m.ctx)

			return overlayDataMsg{kind: overlaySession, sessions: values, err: err}
		}
	case overlayModel:
		m.overlay.model = m.controller.Model().Config
	case overlayCommand, overlayApproval:
		m.overlay.cursor = 0
	case overlayNone, overlayDiff, overlayHelp, overlayStatus:
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
	if m.overlay.kind != overlayApproval && (key == keyEscape || key == keyCtrlC) {
		m.overlay = overlayState{}

		return m, nil
	}

	switch m.overlay.kind {
	case overlayApproval:
		return m.updateApprovalOverlay(key)
	case overlaySession:
		return m.updateSessionOverlay(message)
	case overlayModel:
		return m.updateModelOverlay(message)
	case overlayCommand:
		return m.updateCommandOverlay(message)
	case overlayDiff, overlayHelp, overlayStatus:
		return m.updateReadOnlyOverlay(key)
	case overlayNone:
		return m, nil
	default:
		return m, nil
	}
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

func (m *Model) updateSessionOverlay(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := message.String()
	filtered := m.filteredSessions()
	switch key {
	case "up":
		m.overlay.cursor = wrapIndex(m.overlay.cursor-1, len(filtered))
	case keyDown, keyTab:
		m.overlay.cursor = wrapIndex(m.overlay.cursor+1, len(filtered))
	case keyEnter:
		if len(filtered) == 0 || m.state.Phase != coding.PhaseIdle {
			return m, nil
		}

		return m, m.runControl(operationResume, filtered[m.overlay.cursor].ID, config.ModelConfig{})
	case "n":
		if m.state.Phase == coding.PhaseIdle {
			return m, m.runControl(operationNew, "", config.ModelConfig{})
		}
	case "backspace":
		m.overlay.query = trimLastRune(m.overlay.query)
		m.overlay.cursor = 0
	default:
		if message.Key().Text != "" {
			m.overlay.query += message.Key().Text
			m.overlay.cursor = 0
		}
	}

	return m, nil
}

func (m *Model) updateModelOverlay(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := message.String()
	switch key {
	case "up":
		m.overlay.cursor = wrapIndex(m.overlay.cursor-1, 3)
	case keyDown, keyTab:
		m.overlay.cursor = wrapIndex(m.overlay.cursor+1, 3)
	case "left":
		m.cycleModelField(-1)
	case "right":
		m.cycleModelField(1)
	case "ctrl+u":
		if m.overlay.cursor == 1 {
			m.overlay.model.ID = ""
			m.overlay.modelDirty = true
		}
	case "backspace":
		if m.overlay.cursor == 1 {
			m.overlay.model.ID = trimLastRune(m.overlay.model.ID)
			m.overlay.modelDirty = true
		}
	case keyEnter:
		return m, m.runControl(operationModel, "", m.overlay.model)
	default:
		if m.overlay.cursor == 1 && message.Key().Text != "" {
			if !m.overlay.modelDirty {
				m.overlay.model.ID = ""
				m.overlay.modelDirty = true
			}
			m.overlay.model.ID += message.Key().Text
		}
	}

	return m, nil
}

func (m *Model) cycleModelField(direction int) {
	switch m.overlay.cursor {
	case 0:
		providers := []ai.Provider{
			ai.ProviderOpenAI,
			ai.ProviderAnthropic,
			ai.ProviderGemini,
		}
		index := slices.Index(providers, m.overlay.model.Provider)
		m.overlay.model.Provider = providers[wrapIndex(index+direction, len(providers))]
		if m.overlay.model.Provider != ai.ProviderOpenAI {
			m.overlay.model.API = openai.APIAuto
		}
	case 2:
		if m.overlay.model.Provider != ai.ProviderOpenAI {
			m.overlay.model.API = openai.APIAuto

			return
		}
		apis := []openai.API{openai.APIAuto, openai.APIResponses, openai.APIChatCompletions}
		index := slices.Index(apis, m.overlay.model.API)
		m.overlay.model.API = apis[wrapIndex(index+direction, len(apis))]
	}
}

func (m *Model) updateCommandOverlay(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := message.String()
	filtered := m.filteredCommands()
	switch key {
	case "up":
		m.overlay.cursor = wrapIndex(m.overlay.cursor-1, len(filtered))
	case keyDown, keyTab:
		m.overlay.cursor = wrapIndex(m.overlay.cursor+1, len(filtered))
	case keyEnter:
		if len(filtered) == 0 {
			return m, nil
		}

		return m.executeCommand(filtered[m.overlay.cursor])
	case "backspace":
		m.overlay.query = trimLastRune(m.overlay.query)
		m.overlay.cursor = 0
	default:
		if message.Key().Text != "" && message.Key().Text != "/" {
			m.overlay.query += message.Key().Text
			m.overlay.cursor = 0
		}
	}

	return m, nil
}

func (m *Model) executeCommand(command commandDescriptor) (tea.Model, tea.Cmd) {
	if command.idleOnly && m.actionContext() != contextIdle {
		m.overlay.err = fmt.Errorf("/%s is available only while idle", command.name)

		return m, nil
	}

	switch command.name {
	case "new":
		return m, m.runControl(operationNew, "", config.ModelConfig{})
	case "resume":
		return m, m.openOverlay(overlaySession)
	case "model":
		return m, m.openOverlay(overlayModel)
	case "diff":
		return m, m.openOverlay(overlayDiff)
	case "reload":
		return m, m.runControl(operationReload, "", config.ModelConfig{})
	case "status":
		return m, m.openOverlay(overlayStatus)
	case "help":
		return m, m.openOverlay(overlayHelp)
	case "quit":
		return m, tea.Quit
	default:
		return m, nil
	}
}

func (m *Model) runControl(
	operation controlOperation,
	sessionID string,
	selected config.ModelConfig,
) tea.Cmd {
	m.overlay.loading = true
	m.overlay.controlling = true
	m.overlay.err = nil

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
		}

		return controlResultMsg{operation: operation, err: err}
	}
}

func (m *Model) filteredCommands() []commandDescriptor {
	query := strings.ToLower(strings.TrimSpace(m.overlay.query))
	filtered := make([]commandDescriptor, 0, len(commands))
	for _, command := range commands {
		if query == "" || strings.Contains(command.name, query) ||
			strings.Contains(command.description, query) {
			filtered = append(filtered, command)
		}
	}

	return filtered
}

func (m *Model) filteredSessions() []session.Metadata {
	query := strings.ToLower(strings.TrimSpace(m.overlay.query))
	filtered := make([]session.Metadata, 0, len(m.overlay.sessions))
	for _, value := range m.overlay.sessions {
		created := strings.ToLower(value.CreatedAt.Local().Format(time.DateTime))
		if query == "" || strings.Contains(strings.ToLower(value.ID), query) ||
			strings.Contains(created, query) {
			filtered = append(filtered, value)
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
	var content string
	switch m.overlay.kind {
	case overlayApproval:
		content = m.approvalOverlayContent()
	case overlayDiff:
		content = m.diffOverlayContent()
	case overlaySession:
		content = m.sessionOverlayContent()
	case overlayModel:
		content = m.modelOverlayContent()
	case overlayCommand:
		content = m.commandOverlayContent()
	case overlayHelp:
		content = "Help\n\n" + renderActionHelp(defaultActions, m.actionContext()) +
			"\n\nCtrl+J / Shift+Enter newline\nMouse wheel scrolls; all other mouse input is ignored."
	case overlayStatus:
		modelState := m.controller.Model()
		configState := m.controller.Config()
		content = fmt.Sprintf(
			"Status\n\nWorkspace: %s\nSession: %s\nModel: %s/%s\n"+
				"Process override: %t\nPhase: %s\nSandbox: %s\nApproval: %s\n"+
				"Tool search: %t\nPending approval: %s\nDetached: %t",
			m.options.Workspace,
			m.state.SessionID,
			modelState.Config.Provider,
			modelState.Config.ID,
			modelState.Overridden,
			m.state.Phase,
			configState.Sandbox,
			configState.Approval,
			configState.ToolSearch,
			m.state.Approval.Kind,
			m.controller.Detached(),
		)
		if len(m.state.Diagnostics) > 0 {
			content += "\n\nIntegrations:"
			lines := make([]string, 0, len(m.state.Diagnostics))
			for _, diagnostic := range m.state.Diagnostics {
				lines = append(lines, fmt.Sprintf(
					"- %s/%s: %s",
					diagnostic.Component,
					diagnostic.Code,
					diagnostic.Message,
				))
			}
			content += "\n" + strings.Join(lines, "\n")
		}
	case overlayNone:
	}
	if m.overlay.loading {
		content += "\n\nWorking…"
	}
	if m.overlay.err != nil {
		content += "\n\nError: " + safeError(m.overlay.err)
	}

	return content
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

func (m *Model) sessionOverlayContent() string {
	lines := []string{"Sessions", "", "Filter: " + m.overlay.query, ""}
	values := m.filteredSessions()
	if m.overlay.loading {
		return strings.Join(lines, "\n")
	}
	if len(values) == 0 {
		lines = append(lines, "No matching sessions.")
	}
	for index, value := range values {
		prefix := "  "
		if index == m.overlay.cursor {
			prefix = "> "
		}
		lines = append(lines, fmt.Sprintf(
			"%s%s  %s",
			prefix,
			value.ID,
			value.CreatedAt.Local().Format(time.DateTime),
		))
	}
	lines = append(lines, "", "Enter resume · n new · Esc close")

	return strings.Join(lines, "\n")
}

func (m *Model) modelOverlayContent() string {
	values := []string{
		"Provider: " + string(m.overlay.model.Provider),
		"Model ID: " + m.overlay.model.ID,
		"OpenAI API: " + string(m.overlay.model.API),
	}
	for index := range values {
		prefix := "  "
		if index == m.overlay.cursor {
			prefix = "> "
		}
		values[index] = prefix + values[index]
	}

	return "Switch model (current process only)\n\n" + strings.Join(values, "\n") +
		"\n\n↑/↓ field · ←/→ choice · type model ID · Ctrl+U clear · Enter apply"
}

func (m *Model) commandOverlayContent() string {
	lines := []string{"Commands", "", "/" + m.overlay.query, ""}
	for index, command := range m.filteredCommands() {
		prefix := "  "
		if index == m.overlay.cursor {
			prefix = "> "
		}
		disabled := ""
		if command.idleOnly && m.state.Phase != coding.PhaseIdle {
			disabled = " (idle only)"
		}
		lines = append(lines, fmt.Sprintf(
			"%s/%-8s %s%s",
			prefix,
			command.name,
			command.description,
			disabled,
		))
	}

	return strings.Join(lines, "\n")
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
