//nolint:wsl_v5 // Prompt state, key handling, and its compact rendering are kept together.
package tui

import (
	"context"
	"errors"
	"iter"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/approval"
)

type promptKind uint8

const (
	promptNone promptKind = iota
	promptApproval
	promptCompact
)

type promptState struct {
	kind       promptKind
	cursor     int
	choices    []approval.Choice
	loading    bool
	err        error
	preview    coding.CompactionPreview
	generation uint64
}

type compactPreviewMsg struct {
	generation uint64
	preview    coding.CompactionPreview
	err        error
}

func (m *Model) openCompactPrompt() tea.Cmd {
	m.promptSeq++
	m.prompt = promptState{kind: promptCompact, loading: true, generation: m.promptSeq}
	generation := m.prompt.generation

	return func() tea.Msg {
		preview, err := m.controller.PreviewCompaction(m.ctx)

		return compactPreviewMsg{generation: generation, preview: preview, err: err}
	}
}

func (m *Model) syncApprovalPrompt() {
	if m.state.Approval.Kind == coding.ApprovalNone {
		m.prompt = promptState{}

		return
	}

	if m.picker.kind == pickerCommand {
		m.closeCommandPicker(true)
	}
	m.presentation.pendingRoute = routeOpenRequest{}
	hadRoute := m.route.kind != routeNone
	switch m.route.kind {
	case routeSessions:
		m.dismissSessionPicker(true)
	case routeSkills:
		previousInput := m.route.previousInput
		m.route = routeState{}
		m.composer.SetValue(previousInput)
	default:
		m.route = routeState{}
	}
	if hadRoute {
		m.composer.Focus()
	}
	if m.picker.kind != pickerCommand {
		m.picker = pickerState{}
	}
	choices := approvalChoices(m.state.Approval)
	cursor := 0
	if m.state.Approval.Kind == coding.ApprovalReview {
		if index := slices.Index(choices, approval.ChoiceDeny); index >= 0 {
			cursor = index
		}
	}
	m.prompt = promptState{kind: promptApproval, cursor: cursor, choices: choices}
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

func (m *Model) updatePromptKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch m.prompt.kind {
	case promptApproval:
		return m.updateApprovalPromptKey(message)
	case promptCompact:
		return m.updateCompactPromptKey(message)
	default:
		return m, nil
	}
}

//nolint:gocyclo // Approval choices are deliberately visible as direct terminal bindings.
func (m *Model) updateApprovalPromptKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.prompt.loading || len(m.prompt.choices) == 0 {
		return m, nil
	}

	switch message.String() {
	case "up", "left", "k":
		m.prompt.cursor = wrapIndex(m.prompt.cursor-1, len(m.prompt.choices))
	case keyDown, "right", "j", keyTab:
		m.prompt.cursor = wrapIndex(m.prompt.cursor+1, len(m.prompt.choices))
	case keyEscape:
		if index := slices.Index(m.prompt.choices, approval.ChoiceDeny); index >= 0 {
			m.prompt.cursor = index
		}
	case "o":
		return m.resolvePromptChoice(approval.ChoiceAllowOnce)
	case "s":
		return m.resolvePromptChoice(approval.ChoiceAllowSession)
	case "d":
		return m.resolvePromptChoice(approval.ChoiceDeny)
	case "r":
		return m.resolvePromptChoice(approval.ChoiceRetry)
	case "f":
		return m.resolvePromptChoice(approval.ChoiceMarkFailed)
	case "a":
		return m.resolvePromptChoice(approval.ChoiceAcknowledge)
	case keyEnter:
		return m.resolvePromptChoice(m.prompt.choices[m.prompt.cursor])
	}

	return m, nil
}

func (m *Model) updateCompactPromptKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if message.String() == keyEscape || message.String() == keyCtrlC {
		m.prompt = promptState{}

		return m, m.composer.Focus()
	}
	if message.String() != keyEnter || m.prompt.loading || !m.prompt.preview.Available ||
		m.state.Phase != coding.PhaseIdle {
		return m, nil
	}

	request := coding.CompactionRequest{PreviewToken: m.prompt.preview.Token}
	m.prompt = promptState{}

	return m, m.startStream(func(ctx context.Context) iter.Seq2[coding.Event, error] {
		return m.controller.Compact(ctx, request)
	})
}

func (m *Model) resolvePromptChoice(choice approval.Choice) (tea.Model, tea.Cmd) {
	if !slices.Contains(m.prompt.choices, choice) {
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
		m.prompt.err = errors.New("approval recovery is still being reconciled")

		return m, nil
	}

	m.prompt.loading = true
	return m, m.startStream(func(ctx context.Context) iter.Seq2[coding.Event, error] {
		return m.controller.Resolve(ctx, approval.Resolution{RequestID: requestID, Choice: choice})
	})
}

func (m *Model) promptView() string {
	switch m.prompt.kind {
	case promptApproval:
		return m.approvalPromptView()
	case promptCompact:
		return m.compactPromptView()
	default:
		return ""
	}
}

func (m *Model) approvalPromptView() string {
	lines := []string{"△ Approval required"}
	if value := m.state.Approval.Required; value != nil {
		lines = append(lines,
			value.Tool+": "+strings.Join(value.Command, " "),
			"cwd "+value.CWD+" · reason: "+value.Justification,
		)
	} else if value := m.state.Approval.Unknown; value != nil {
		lines = append(lines,
			"outcome unknown · "+value.Tool+" · "+value.Reason,
		)
	}
	lines = append(lines, "")
	for index, choice := range m.prompt.choices {
		prefix := "  "
		if index == m.prompt.cursor {
			prefix = "> "
		}
		lines = append(lines, prefix+string(choice))
	}
	lines = append(lines, "↑/↓ choose · Enter confirm")
	if m.prompt.loading {
		lines = append(lines, "Working…")
	}
	if m.prompt.err != nil {
		lines = append(lines, safeError(m.prompt.err))
	}

	bar := "▌"
	if !m.options.NoColor {
		bar = lipgloss.NewStyle().Foreground(paletteFor(m.theme).warning).Render(bar)
	}
	for index := range lines {
		lines[index] = bar + " " + ansi.Truncate(lines[index], max(1, m.width-2), "…")
	}

	return strings.Join(lines, "\n")
}

func (m *Model) compactPromptView() string {
	lines := []string{"△ Compact context"}
	switch {
	case m.prompt.loading:
		lines = append(lines, "Loading compaction preview…")
	case m.prompt.err != nil:
		lines = append(lines, "Error: "+safeError(m.prompt.err))
	case !m.prompt.preview.Available:
		lines = append(lines, "Unavailable: "+m.prompt.preview.DisabledReason, "Esc close")
	default:
		preview := m.prompt.preview
		lines = append(lines,
			"Estimated context: "+strconv.Itoa(preview.EstimatedTokens)+" tokens · threshold: "+strconv.Itoa(preview.ThresholdTokens),
			"Summarize "+strconv.Itoa(preview.SummarizedMessages)+" messages · keep "+strconv.Itoa(preview.KeptMessages),
			"Compaction may omit details. The durable branch remains recoverable.",
			"Enter confirm · Esc cancel",
		)
	}

	bar := "▌"
	if !m.options.NoColor {
		bar = lipgloss.NewStyle().Foreground(paletteFor(m.theme).warning).Render(bar)
	}
	for index := range lines {
		lines[index] = bar + " " + ansi.Truncate(lines[index], max(1, m.width-2), "…")
	}

	return strings.Join(lines, "\n")
}
