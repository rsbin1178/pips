//nolint:wsl_v5 // Prompt state, key handling, and its compact rendering are kept together.
package tui

import (
	"context"
	"errors"
	"iter"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/question"
)

type promptKind uint8

const (
	promptNone promptKind = iota
	promptApproval
	promptCompact
	promptQuestion
)

type questionEditKind uint8

const (
	questionEditNone questionEditKind = iota
	questionEditCustom
	questionEditChat
)

type questionPromptState struct {
	request  question.Request
	tab      int
	cursors  []int
	selected [][]bool
	custom   []string
	editing  questionEditKind
	editor   textarea.Model
	loading  bool
	err      error
}

type promptState struct {
	kind       promptKind
	cursor     int
	choices    []approval.Choice
	loading    bool
	err        error
	preview    coding.CompactionPreview
	question   questionPromptState
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
	if m.state.Question.Required != nil {
		if m.prompt.kind == promptQuestion &&
			m.prompt.question.request.ID == m.state.Question.Required.ID &&
			m.prompt.question.request.SchemaDigest == m.state.Question.Required.SchemaDigest {
			return
		}

		m.claimPromptOwner()
		m.prompt = promptState{
			kind:     promptQuestion,
			question: m.newQuestionPrompt(*m.state.Question.Required),
		}
		m.composer.Blur()

		return
	}
	if m.state.Approval.Kind == coding.ApprovalNone {
		if m.prompt.kind == promptApproval || m.prompt.kind == promptQuestion {
			m.prompt = promptState{}
			m.composer.Focus()
		}

		return
	}

	m.claimPromptOwner()
	choices := approvalChoices(m.state.Approval)
	cursor := 0
	if m.state.Approval.Kind == coding.ApprovalReview {
		if index := slices.Index(choices, approval.ChoiceDeny); index >= 0 {
			cursor = index
		}
	}
	m.prompt = promptState{kind: promptApproval, cursor: cursor, choices: choices}
}

func (m *Model) claimPromptOwner() {
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
	case promptQuestion:
		return m.updateQuestionPromptKey(message)
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
	case "up", keyLeft, "k":
		m.prompt.cursor = wrapIndex(m.prompt.cursor-1, len(m.prompt.choices))
	case keyDown, keyRight, "j", keyTab:
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

func (m *Model) newQuestionPrompt(request question.Request) questionPromptState {
	editor := textarea.New()
	editor.Prompt = ""
	editor.Placeholder = ""
	editor.ShowLineNumbers = false
	editor.DynamicHeight = true
	editor.MinHeight = 1
	editor.MaxHeight = 4
	editor.MaxContentHeight = 8
	editor.SetVirtualCursor(false)
	editor.SetWidth(max(1, m.width-4))
	editor.SetStyles(composerStyles(m.theme, m.options.NoColor))

	selected := make([][]bool, len(request.Questions))
	for index, item := range request.Questions {
		selected[index] = make([]bool, len(item.Options))
	}

	return questionPromptState{
		request:  question.CloneRequest(request),
		cursors:  make([]int, len(request.Questions)),
		selected: selected,
		custom:   make([]string, len(request.Questions)),
		editor:   editor,
	}
}

func (m *Model) updateQuestionPromptKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	state := &m.prompt.question
	if state.loading {
		return m, nil
	}
	if state.editing != questionEditNone {
		return m.updateQuestionEditorKey(message)
	}

	questionCount := len(state.request.Questions)
	if questionCount == 0 {
		state.err = errors.New("structured question is unavailable")

		return m, nil
	}

	switch message.String() {
	case keyEscape, keyCtrlC:
		state.loading = true

		return m, m.startStream(func(ctx context.Context) iter.Seq2[coding.Event, error] {
			return m.controller.RejectQuestion(
				ctx,
				state.request.ID,
				state.request.SchemaDigest,
			)
		})
	case keyLeft:
		state.tab = wrapIndex(state.tab-1, questionCount+1)
		state.err = nil
	case keyRight, keyTab:
		state.tab = wrapIndex(state.tab+1, questionCount+1)
		state.err = nil
	case "up", "k":
		if state.tab < questionCount {
			rows := len(state.request.Questions[state.tab].Options) + 2
			state.cursors[state.tab] = wrapIndex(state.cursors[state.tab]-1, rows)
		}
	case keyDown, "j":
		if state.tab < questionCount {
			rows := len(state.request.Questions[state.tab].Options) + 2
			state.cursors[state.tab] = wrapIndex(state.cursors[state.tab]+1, rows)
		}
	case " ", "space":
		m.toggleQuestionSelection()
	case keyEnter:
		return m.activateQuestionSelection()
	}

	return m, nil
}

func (m *Model) toggleQuestionSelection() {
	state := &m.prompt.question
	if state.tab >= len(state.request.Questions) {
		return
	}
	item := state.request.Questions[state.tab]
	cursor := state.cursors[state.tab]
	if cursor >= len(item.Options) {
		return
	}

	state.custom[state.tab] = ""
	if item.Multiple {
		state.selected[state.tab][cursor] = !state.selected[state.tab][cursor]
	} else {
		clear(state.selected[state.tab])
		state.selected[state.tab][cursor] = true
	}
	state.err = nil
}

func (m *Model) activateQuestionSelection() (tea.Model, tea.Cmd) {
	state := &m.prompt.question
	if state.tab == len(state.request.Questions) {
		return m.resolveStructuredQuestion()
	}

	item := state.request.Questions[state.tab]
	cursor := state.cursors[state.tab]
	switch {
	case cursor < len(item.Options):
		if !item.Multiple {
			clear(state.selected[state.tab])
			state.selected[state.tab][cursor] = true
			state.custom[state.tab] = ""
		} else if !slices.Contains(state.selected[state.tab], true) {
			state.err = errors.New("select at least one option")

			return m, nil
		}
		state.tab = min(state.tab+1, len(state.request.Questions))
		state.err = nil

		return m, nil
	case cursor == len(item.Options):
		state.editing = questionEditCustom
		state.editor.Reset()
		state.editor.SetValue(state.custom[state.tab])
		state.editor.Placeholder = "Type a custom answer"
		state.err = nil

		return m, state.editor.Focus()
	default:
		state.editing = questionEditChat
		state.editor.Reset()
		state.editor.Placeholder = "What would you like to discuss?"
		state.err = nil

		return m, state.editor.Focus()
	}
}

func (m *Model) updateQuestionEditorKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	state := &m.prompt.question
	switch message.String() {
	case keyEscape:
		state.editing = questionEditNone
		state.editor.Blur()
		state.err = nil

		return m, nil
	case keyCtrlC:
		state.editing = questionEditNone
		state.editor.Blur()
		state.loading = true

		return m, m.startStream(func(ctx context.Context) iter.Seq2[coding.Event, error] {
			return m.controller.RejectQuestion(
				ctx,
				state.request.ID,
				state.request.SchemaDigest,
			)
		})
	case "ctrl+j", "shift+enter":
		if state.editing == questionEditChat {
			state.editor.InsertString("\n")
		}

		return m, nil
	case keyEnter:
		value := strings.TrimSpace(state.editor.Value())
		if value == "" {
			state.err = errors.New("enter a response before continuing")

			return m, nil
		}
		if state.editing == questionEditChat {
			resolution := question.Resolution{
				RequestID: state.request.ID, SchemaDigest: state.request.SchemaDigest,
				Chat: value,
			}
			if err := question.ValidateResolution(state.request, resolution); err != nil {
				state.err = err

				return m, nil
			}
			state.loading = true

			return m, m.startStream(func(ctx context.Context) iter.Seq2[coding.Event, error] {
				return m.controller.ResolveQuestion(ctx, resolution)
			})
		}

		clear(state.selected[state.tab])
		state.custom[state.tab] = value
		state.editing = questionEditNone
		state.editor.Blur()
		state.tab = min(state.tab+1, len(state.request.Questions))
		state.err = nil

		return m, nil
	}

	var command tea.Cmd
	state.editor, command = state.editor.Update(message)

	return m, command
}

func (m *Model) resolveStructuredQuestion() (tea.Model, tea.Cmd) {
	state := &m.prompt.question
	answers := make([]question.Answer, len(state.request.Questions))
	for questionIndex, item := range state.request.Questions {
		if state.custom[questionIndex] != "" {
			answers[questionIndex].Custom = state.custom[questionIndex]
			continue
		}
		for optionIndex, option := range item.Options {
			if state.selected[questionIndex][optionIndex] {
				answers[questionIndex].Selections = append(
					answers[questionIndex].Selections,
					option.Label,
				)
			}
		}
	}

	resolution := question.Resolution{
		RequestID: state.request.ID, SchemaDigest: state.request.SchemaDigest,
		Answers: answers,
	}
	if err := question.ValidateResolution(state.request, resolution); err != nil {
		state.err = errors.New("answer every question before submitting")

		return m, nil
	}
	state.loading = true
	state.err = nil

	return m, m.startStream(func(ctx context.Context) iter.Seq2[coding.Event, error] {
		return m.controller.ResolveQuestion(ctx, resolution)
	})
}

func (m *Model) promptView() string {
	switch m.prompt.kind {
	case promptApproval:
		return m.approvalPromptView()
	case promptCompact:
		return m.compactPromptView()
	case promptQuestion:
		return m.questionPromptView()
	default:
		return ""
	}
}

//nolint:gocyclo,nestif // The inline question renderer mirrors the explicit navigation state machine.
func (m *Model) questionPromptView() string {
	state := &m.prompt.question
	request := state.request
	if len(request.Questions) == 0 {
		return "△ Structured question unavailable"
	}

	tabs := make([]string, 0, len(request.Questions)+3)
	tabs = append(tabs, "←")
	for index, item := range request.Questions {
		marker := "□"
		if questionAnswered(*state, index) {
			marker = "✓"
		}
		label := marker + " " + item.Header
		if state.tab == index {
			label = "[" + label + "]"
		}
		tabs = append(tabs, label)
	}
	submit := "✓ Submit"
	if state.tab == len(request.Questions) {
		submit = "[" + submit + "]"
	}
	tabs = append(tabs, submit, "→")

	lines := []string{strings.Join(tabs, "  "), ""}
	if state.tab == len(request.Questions) {
		lines = append(lines, "Review your answers", "")
		for index, item := range request.Questions {
			lines = append(lines, strconv.Itoa(index+1)+". "+item.Header+": "+questionAnswerLabel(*state, index))
		}
		lines = append(lines, "", "Enter submit · Tab/←/→ navigate · Esc cancel")
	} else {
		item := request.Questions[state.tab]
		cursor := state.cursors[state.tab]
		lines = append(lines, item.Question, "")
		for index, option := range item.Options {
			focus := "  "
			if cursor == index {
				focus = "› "
			}
			marker := "○"
			if item.Multiple {
				marker = "[ ]"
			}
			if state.selected[state.tab][index] {
				marker = "●"
				if item.Multiple {
					marker = "[✓]"
				}
			}
			line := focus + strconv.Itoa(index+1) + ". " + marker + " " + option.Label
			lines = append(lines, m.styleQuestionChoice(line, cursor == index))
			if cursor == index {
				lines = append(lines, "     "+option.Description)
				if option.Preview != "" {
					preview, err := m.markdown.render(
						option.Preview,
						max(1, m.width-6),
						m.theme,
						m.options.NoColor,
					)
					if err == nil {
						lines = append(lines, truncateHeight(preview, max(2, min(6, m.height/4))))
					}
				}
			}
		}
		customIndex := len(item.Options)
		lines = append(lines, m.styleQuestionChoice(
			questionFocus(cursor == customIndex)+strconv.Itoa(customIndex+1)+". Type something",
			cursor == customIndex,
		))
		lines = append(lines, strings.Repeat("─", max(1, min(m.width-2, 72))))
		chatIndex := customIndex + 1
		lines = append(lines, m.styleQuestionChoice(
			questionFocus(cursor == chatIndex)+strconv.Itoa(chatIndex+1)+". Chat about this",
			cursor == chatIndex,
		))

		switch {
		case state.editing != questionEditNone:
			lines = append(lines, "", state.editor.View(), "Enter submit · Ctrl+J newline · Esc back")
		case item.Multiple:
			lines = append(lines, "", "Space toggle · Enter continue · Tab/←/→ questions · Esc cancel")
		default:
			lines = append(lines, "", "Enter select · Tab/←/→ questions · Esc cancel")
		}
	}

	if state.loading {
		lines = append(lines, "Working…")
	}
	if state.err != nil {
		lines = append(lines, "Error: "+safeError(state.err))
	}
	maximum := max(8, min(18, m.height/2))
	if len(lines) > maximum {
		tail := 3
		lines = append(
			append(slices.Clone(lines[:maximum-tail-1]), "…"),
			lines[len(lines)-tail:]...,
		)
	}

	bar := "▌"
	if !m.options.NoColor {
		bar = lipgloss.NewStyle().Foreground(paletteFor(m.theme).session).Render(bar)
	}
	for index := range lines {
		lines[index] = bar + " " + ansi.Truncate(lines[index], max(1, m.width-2), "…")
	}

	return strings.Join(lines, "\n")
}

func (m *Model) styleQuestionChoice(value string, focused bool) string {
	if m.options.NoColor || !focused {
		return value
	}

	return lipgloss.NewStyle().Bold(true).Foreground(paletteFor(m.theme).session).Render(value)
}

func questionFocus(focused bool) string {
	if focused {
		return "› "
	}

	return "  "
}

func questionAnswered(state questionPromptState, index int) bool {
	if index < 0 || index >= len(state.request.Questions) {
		return false
	}
	if state.custom[index] != "" {
		return true
	}

	return slices.Contains(state.selected[index], true)
}

func questionAnswerLabel(state questionPromptState, index int) string {
	if !questionAnswered(state, index) {
		return "Not answered"
	}
	if state.custom[index] != "" {
		return state.custom[index]
	}

	labels := make([]string, 0, len(state.selected[index]))
	for optionIndex, selected := range state.selected[index] {
		if selected {
			labels = append(labels, state.request.Questions[index].Options[optionIndex].Label)
		}
	}

	return strings.Join(labels, ", ")
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
