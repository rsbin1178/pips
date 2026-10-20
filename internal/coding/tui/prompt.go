//nolint:wsl_v5 // Prompt state, key handling, and its compact rendering are kept together.
package tui

import (
	"context"
	"errors"
	"fmt"
	"image/color"
	"iter"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/approval"
	"github.com/rsbin1178/pips/internal/coding/question"
)

type promptKind uint8

const (
	promptNone promptKind = iota
	promptApproval
	promptCompact
	promptQuestion
	promptPlanReview
	promptPlanView
)

type questionEditKind uint8

const (
	questionEditNone questionEditKind = iota
	questionEditCustom
	questionEditChat
)

const (
	// questionTabHeaderLimit keeps four question tabs and the Submit tab inside
	// a typical frame; a click still needs to reach every one of them.
	questionTabHeaderLimit = 12
	questionUnchecked      = "[ ]"
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

// questionPromptHitKind names one pointer target the question band painted.
type questionPromptHitKind uint8

const (
	questionPromptHitNone questionPromptHitKind = iota
	questionPromptHitTab
	questionPromptHitOption
	questionPromptHitCustom
	questionPromptHitChat
)

// questionPromptTabSegment is the display-column range one tab occupies on the
// tabs row, so a click can address the tab it landed on.
type questionPromptTabSegment struct {
	start int
	end   int
	index int
}

// questionPromptHitRow is what one painted prompt row accepts. Option, custom
// and chat rows accept a click anywhere on the row (and on the description and
// preview rows that explain them); a tabs row resolves the clicked segment.
type questionPromptHitRow struct {
	kind     questionPromptHitKind
	question int
	option   int
	// detail marks explanation and preview rows. They are the first thing a
	// short window gives up, because every choice has to stay visible.
	detail   bool
	segments []questionPromptTabSegment
}

// questionPromptHit is the question band's pointer map for the frame that was
// last painted. top is the frame row of the first painted prompt row; rows are
// aligned with the band's rows after every cap and crop the frame applied.
type questionPromptHit struct {
	top     int
	rows    []questionPromptHitRow
	cursor  bool
	cursorX int
	cursorY int
}

// questionPromptRows collects prompt rows and their pointer targets while the
// band is built, which keeps the hit map aligned with what is actually drawn.
type questionPromptRows struct {
	values  []string
	targets []questionPromptHitRow
	cursor  bool
	cursorX int
	cursorY int
}

func (rows *questionPromptRows) add(value string, target questionPromptHitRow) {
	rows.values = append(rows.values, value)
	rows.targets = append(rows.targets, target)
}

func (rows *questionPromptRows) addBlock(value string, target questionPromptHitRow) {
	for line := range strings.SplitSeq(value, "\n") {
		rows.add(line, target)
	}
}

type promptState struct {
	kind       promptKind
	cursor     int
	choices    []approval.Choice
	resolution approval.Choice
	loading    bool
	err        error
	preview    coding.CompactionPreview
	question   questionPromptState
	planReview planReviewPromptState
	planView   planViewPromptState
	team       *teamPromptSource
	subagent   *subagentPromptSource
	generation uint64
}

type compactPreviewMsg struct {
	generation uint64
	preview    coding.CompactionPreview
	err        error
}

func (m *Model) openCompactPrompt() tea.Cmd {
	activityWasVisible := m.activityClockVisible()
	m.promptSeq++
	m.prompt = promptState{kind: promptCompact, loading: true, generation: m.promptSeq}
	generation := m.prompt.generation

	load := func() tea.Msg {
		preview, err := m.controller.PreviewCompaction(m.ctx)

		return compactPreviewMsg{generation: generation, preview: preview, err: err}
	}

	return tea.Batch(load, m.startActivityClock(activityWasVisible))
}

//nolint:gocyclo // The prompt owners form one mutually exclusive state machine.
func (m *Model) syncApprovalPrompt() {
	if m.syncPlanReviewPrompt() {
		return
	}
	if m.state.Question.Required != nil {
		// A question that is still required means its resolution did not land;
		// drop the optimistic card so the prompt owns the state again.
		delete(m.questionPreviews, m.state.Question.Required.ToolCallID)

		if m.prompt.kind == promptQuestion &&
			m.prompt.team == nil &&
			m.prompt.subagent == nil &&
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
		if m.syncSubagentInteractionPrompt() {
			return
		}
		if entry, ok := m.nextTeamInteraction(); ok {
			m.syncTeamInteractionPrompt(*entry)

			return
		}
		if m.prompt.kind == promptApproval || m.prompt.kind == promptQuestion ||
			m.prompt.kind == promptPlanReview {
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

func (m *Model) syncPlanReviewPrompt() bool {
	if m.state.PlanReview.Required != nil {
		if m.prompt.kind == promptPlanReview &&
			m.prompt.planReview.request.ID == m.state.PlanReview.Required.ID {
			return true
		}

		m.claimPromptOwner()
		m.promptSeq++
		m.prompt = promptState{
			kind:       promptPlanReview,
			generation: m.promptSeq,
			planReview: m.newPlanReviewPrompt(*m.state.PlanReview.Required),
		}
		m.composer.Blur()

		return true
	}

	return false
}

func (m *Model) claimPromptOwner() {
	m.stopTeamWorkerRouteSubscription()
	m.teamPanel.isFocused = false
	m.teamPanel.isPeeking = false
	m.tasksPane.focused = false
	if m.picker.kind == pickerCommand {
		m.closeCommandPicker(true)
	}
	m.presentation.pendingRoute = routeOpenRequest{}
	hadRoute := m.route.kind != routeNone
	switch m.route.kind {
	case routeSessions:
		m.dismissSessionPicker(true)
	case routeSkills, routeMCP:
		previousInput := m.route.previousInput
		previousComposer := m.route.previousComposer
		m.route = routeState{}
		if err := m.composer.Restore(previousComposer); err != nil {
			m.composer.SetValue(previousInput)
			m.streamErr = err
		}
	default:
		previousComposer := m.route.previousComposer
		hasPreviousComposer := m.route.hasPreviousComposer
		m.route = routeState{}
		if hasPreviousComposer {
			if err := m.composer.Restore(previousComposer); err != nil {
				m.streamErr = err
			}
		}
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
	case promptPlanReview:
		return m.updatePlanReviewPromptKey(message)
	case promptPlanView:
		return m.updatePlanViewPromptKey(message)
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
	if m.prompt.team != nil {
		activityWasVisible := m.activityClockVisible()
		command := m.submitTeamApprovalChoice(m.prompt.team.key, choice)
		if command != nil {
			m.prompt.loading = true
			m.prompt.err = nil
			return m, tea.Batch(command, m.startActivityClock(activityWasVisible))
		}

		return m, nil
	}
	if m.prompt.subagent != nil {
		activityWasVisible := m.activityClockVisible()
		command := m.submitSubagentApprovalChoice(m.prompt.subagent, choice)
		if command == nil {
			m.syncApprovalPrompt()

			return m, nil
		}
		m.prompt.loading = true
		m.prompt.err = nil

		return m, tea.Batch(command, m.startActivityClock(activityWasVisible))
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

	m.prompt.resolution = choice
	m.prompt.loading = true
	return m, m.startStream(func(ctx context.Context) iter.Seq2[coding.Event, error] {
		return m.controller.Resolve(ctx, approval.Resolution{RequestID: requestID, Choice: choice})
	})
}

func (m *Model) mainApprovalExecutionStarting() bool {
	if m.prompt.kind != promptApproval || m.prompt.team != nil || m.prompt.subagent != nil || !m.prompt.loading {
		return false
	}

	return m.prompt.resolution == approval.ChoiceAllowOnce ||
		m.prompt.resolution == approval.ChoiceAllowSession
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
	editor.SetWidth(max(1, m.promptBandWidth()-4))
	editor.SetStyles(composerStyles(m.theme, m.options.NoColor))

	selected := make([][]bool, len(request.Questions))
	for index, item := range request.Questions {
		selected[index] = make([]bool, len(item.Options))
	}

	state := questionPromptState{
		request:  question.CloneRequest(request),
		cursors:  make([]int, len(request.Questions)),
		selected: selected,
		custom:   make([]string, len(request.Questions)),
		editor:   editor,
	}
	if request.Kind == question.RequestFreeform {
		state.editing = questionEditChat
		state.editor.Placeholder = "Type the missing preference or constraint"
		state.editor.Focus()
	} else if len(state.request.Questions) > 0 && state.request.Questions[0].IsText() {
		state.editing = questionEditCustom
		state.editor.Placeholder = "Type your answer"
		state.editor.Focus()
	}

	return state
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
		activityWasVisible := m.activityClockVisible()
		state.loading = true

		return m, m.rejectQuestionPromptCommand(state.request, activityWasVisible)
	case keyLeft:
		return m, m.moveQuestionTab(state, -1)
	case keyRight, keyTab:
		return m, m.moveQuestionTab(state, 1)
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
	case " ", keySpace:
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
	if item.IsText() {
		return m, m.enterQuestionTab(state, state.tab)
	}

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

		return m, m.enterQuestionTab(state, state.tab)
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
	if m.questionTabIsText(state) {
		return m.updateQuestionTextKey(message)
	}

	switch message.String() {
	case keyEscape:
		if state.request.Kind == question.RequestFreeform {
			// This prompt has no list to return to, so Esc is the cancel action
			// the hint advertises.
			activityWasVisible := m.activityClockVisible()
			state.editing = questionEditNone
			state.editor.Blur()
			state.loading = true

			return m, m.rejectQuestionPromptCommand(state.request, activityWasVisible)
		}
		state.editing = questionEditNone
		state.editor.Blur()
		state.err = nil

		return m, nil
	case keyCtrlC:
		activityWasVisible := m.activityClockVisible()
		state.editing = questionEditNone
		state.editor.Blur()
		state.loading = true

		return m, m.rejectQuestionPromptCommand(state.request, activityWasVisible)
	case keyCtrlJ, keyShiftEnter:
		state.editor.InsertString("\n")

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
			activityWasVisible := m.activityClockVisible()
			state.loading = true

			return m, m.resolveQuestionPromptCommand(state.request, resolution, activityWasVisible)
		}

		clear(state.selected[state.tab])
		state.custom[state.tab] = value
		state.editing = questionEditNone
		state.editor.Blur()
		state.tab = min(state.tab+1, len(state.request.Questions))
		state.err = nil

		return m, m.enterQuestionTab(state, state.tab)
	}

	var command tea.Cmd
	state.editor, command = state.editor.Update(message)

	return m, command
}

func (m *Model) updateQuestionTextKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	state := &m.prompt.question
	switch message.String() {
	case keyEscape, keyCtrlC:
		activityWasVisible := m.activityClockVisible()
		state.editing = questionEditNone
		state.editor.Blur()
		state.loading = true

		return m, m.rejectQuestionPromptCommand(state.request, activityWasVisible)
	case keyCtrlJ, keyShiftEnter:
		state.editor.InsertString("\n")

		return m, nil
	case keyTab:
		state.custom[state.tab] = strings.TrimSpace(state.editor.Value())
		state.editor.Blur()
		state.editing = questionEditNone
		state.err = nil

		return m, m.enterQuestionTab(state, min(state.tab+1, len(state.request.Questions)))
	case keyShiftTab:
		state.custom[state.tab] = strings.TrimSpace(state.editor.Value())
		state.editor.Blur()
		state.editing = questionEditNone
		state.err = nil

		return m, m.enterQuestionTab(state, max(0, state.tab-1))
	case keyEnter:
		value := strings.TrimSpace(state.editor.Value())
		if value == "" {
			state.err = errors.New("enter a response before continuing")

			return m, nil
		}
		state.custom[state.tab] = value
		state.editor.Blur()
		state.editing = questionEditNone
		state.tab = min(state.tab+1, len(state.request.Questions))
		state.err = nil

		return m, m.enterQuestionTab(state, state.tab)
	}

	var command tea.Cmd
	state.err = nil
	state.editor, command = state.editor.Update(message)

	return m, command
}

// moveQuestionTab moves the active tab and opens the editor when the tab is a
// kind text question. It clears the previous validation error.
func (m *Model) moveQuestionTab(state *questionPromptState, delta int) tea.Cmd {
	state.err = nil

	return m.enterQuestionTab(
		state,
		wrapIndex(state.tab+delta, len(state.request.Questions)+1),
	)
}

// enterQuestionTab focuses the tab and enters text editing for a kind text
// question. Choice questions and Submit keep the list keyboard behavior.
func (m *Model) enterQuestionTab(state *questionPromptState, index int) tea.Cmd {
	count := len(state.request.Questions)
	if index < 0 || index > count {
		return nil
	}

	state.tab = index
	state.editing = questionEditNone
	if state.editor.Focused() {
		state.editor.Blur()
	}
	if index >= count {
		return nil
	}

	item := state.request.Questions[index]
	if !item.IsText() {
		return nil
	}

	state.editing = questionEditCustom
	state.editor.Reset()
	state.editor.SetValue(state.custom[index])
	state.editor.Placeholder = "Type your answer"

	return state.editor.Focus()
}

func (m *Model) questionTabIsText(state *questionPromptState) bool {
	return state.tab >= 0 && state.tab < len(state.request.Questions) &&
		state.request.Questions[state.tab].IsText()
}

func (m *Model) resolveStructuredQuestion() (tea.Model, tea.Cmd) {
	state := &m.prompt.question
	answers := make([]question.Answer, len(state.request.Questions))
	for questionIndex, item := range state.request.Questions {
		if item.IsText() {
			answers[questionIndex].Custom = state.custom[questionIndex]
			continue
		}

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
		var command tea.Cmd
		if index := firstUnansweredQuestion(*state); index >= 0 {
			// Stay with the decision that still needs an answer instead of
			// leaving the reader on a Submit tab that cannot explain itself.
			state.err = fmt.Errorf("answer %q before submitting", state.request.Questions[index].Header)
			command = m.enterQuestionTab(state, index)
		}

		return m, command
	}
	activityWasVisible := m.activityClockVisible()
	state.loading = true
	state.err = nil

	return m, m.resolveQuestionPromptCommand(state.request, resolution, activityWasVisible)
}

func (m *Model) resolveQuestionPromptCommand(
	request question.Request,
	resolution question.Resolution,
	activityWasVisible bool,
) tea.Cmd {
	if m.prompt.team == nil && m.prompt.subagent == nil {
		m.previewQuestionOutcome(request, resolution, false)
		m.dismissMainQuestionPrompt()
	}

	if m.prompt.team != nil {
		command := m.submitTeamQuestionResolution(m.prompt.team.key, resolution, false)
		if command == nil {
			m.syncApprovalPrompt()

			return nil
		}

		return tea.Batch(command, m.startActivityClock(activityWasVisible))
	}
	if m.prompt.subagent != nil {
		command := m.submitSubagentQuestionResolution(m.prompt.subagent, resolution, false)
		if command == nil {
			m.syncApprovalPrompt()

			return nil
		}

		return tea.Batch(command, m.startActivityClock(activityWasVisible))
	}

	return m.startStream(func(ctx context.Context) iter.Seq2[coding.Event, error] {
		return m.controller.ResolveQuestion(ctx, resolution)
	})
}

// dismissMainQuestionPrompt closes the band in the same frame as the answer or
// cancellation. The preview card in the transcript explains what happened while
// the Runtime publishes the durable result.
func (m *Model) dismissMainQuestionPrompt() {
	if m.prompt.kind != promptQuestion || m.prompt.team != nil || m.prompt.subagent != nil {
		return
	}
	m.prompt = promptState{}
	m.composer.Focus()
}

func (m *Model) rejectQuestionPromptCommand(
	request question.Request,
	activityWasVisible bool,
) tea.Cmd {
	if m.prompt.team == nil && m.prompt.subagent == nil {
		m.previewQuestionOutcome(request, question.Resolution{
			RequestID: request.ID, SchemaDigest: request.SchemaDigest,
		}, true)
		m.dismissMainQuestionPrompt()
	}

	if m.prompt.team != nil {
		command := m.submitTeamQuestionResolution(
			m.prompt.team.key,
			question.Resolution{
				RequestID: request.ID, SchemaDigest: request.SchemaDigest,
			},
			true,
		)
		if command == nil {
			m.syncApprovalPrompt()

			return nil
		}

		return tea.Batch(command, m.startActivityClock(activityWasVisible))
	}
	if m.prompt.subagent != nil {
		command := m.submitSubagentQuestionResolution(
			m.prompt.subagent,
			question.Resolution{RequestID: request.ID, SchemaDigest: request.SchemaDigest},
			true,
		)
		if command == nil {
			m.syncApprovalPrompt()

			return nil
		}

		return tea.Batch(command, m.startActivityClock(activityWasVisible))
	}

	return m.startStream(func(ctx context.Context) iter.Seq2[coding.Event, error] {
		return m.controller.RejectQuestion(ctx, request.ID, request.SchemaDigest)
	})
}

func (m *Model) promptView() (string, questionPromptHit) {
	switch m.prompt.kind {
	case promptApproval:
		if m.mainApprovalExecutionStarting() {
			return "", questionPromptHit{}
		}

		return m.approvalPromptView(), questionPromptHit{}
	case promptCompact:
		return m.compactPromptView(), questionPromptHit{}
	case promptQuestion:
		return m.questionPromptView()
	case promptPlanReview:
		return m.planReviewPromptView(), questionPromptHit{}
	case promptPlanView:
		return m.planViewPromptView(), questionPromptHit{}
	default:
		return "", questionPromptHit{}
	}
}

// promptBandWidth is the render width of the modal prompt band. The band shares
// the Composer's text column, so it gives up the same two columns on each side
// that a conversation row is inset by, and its accent gutter then lines up with
// the transcript's accented blocks.
func (m *Model) promptBandWidth() int {
	return max(1, m.width-2*timelineInset(m.width))
}

//nolint:gocyclo,funlen,nestif // The inline question renderer mirrors the explicit navigation state machine.
func (m *Model) questionPromptView() (string, questionPromptHit) {
	state := &m.prompt.question
	request := state.request
	rows := &questionPromptRows{}
	m.resizeQuestionEditor(state)

	if request.Kind == question.RequestFreeform {
		if source := m.promptSourceLine(); source != "" {
			rows.add(source, questionPromptHitRow{})
			rows.add("", questionPromptHitRow{})
		}
		m.appendQueueProgress(rows, request)
		rows.add("←  [□ Input required]", questionPromptHitRow{})
		rows.add("", questionPromptHitRow{})
		rows.addBlock(request.Prompt, questionPromptHitRow{})
		rows.add("", questionPromptHitRow{})
		rows.addEditor(state.editor, state.editing != questionEditNone, 0)
		rows.add("Enter submit · Ctrl+J newline · Esc cancel", questionPromptHitRow{})
		m.appendQuestionStatusRows(rows, state)
		m.applyQuestionGutter(rows)

		return strings.Join(rows.values, "\n"), rows.hit()
	}
	if len(request.Questions) == 0 {
		return "△ Structured question unavailable", questionPromptHit{}
	}

	tabs := "←"
	if len(request.Questions) > 1 {
		tabs += " " + strconv.Itoa(min(state.tab+1, len(request.Questions))) +
			"/" + strconv.Itoa(len(request.Questions))
	}
	tabsWidth := ansi.StringWidth(tabs)
	segments := make([]questionPromptTabSegment, 0, len(request.Questions)+1)

	addTab := func(label string, index int) {
		tabs += "  " + label
		start := tabsWidth + 2
		tabsWidth = start + ansi.StringWidth(label)
		segments = append(segments, questionPromptTabSegment{
			start: start, end: tabsWidth - 1, index: index,
		})
	}
	for index, item := range request.Questions {
		marker := "□"
		if questionAnswered(*state, index) {
			marker = "✓"
		}
		// The tabs row has to keep Submit reachable, so a long header is
		// shortened here; the review pane still prints it in full.
		label := marker + " " + ansi.Truncate(questionOneLine(item.Header), questionTabHeaderLimit, "…")
		if state.tab == index {
			label = "[" + label + "]"
		}
		addTab(label, index)
	}
	submit := "✓ Submit"
	if state.tab == len(request.Questions) {
		submit = "[" + submit + "]"
	}
	addTab(submit, len(request.Questions))
	tabs += "  →"

	if source := m.promptSourceLine(); source != "" {
		rows.add(source, questionPromptHitRow{})
		rows.add("", questionPromptHitRow{})
	}
	m.appendQueueProgress(rows, request)
	rows.add(tabs, questionPromptHitRow{kind: questionPromptHitTab, segments: segments})
	rows.add("", questionPromptHitRow{})

	if state.tab == len(request.Questions) {
		rows.add("Review your answers", questionPromptHitRow{})
		rows.add("", questionPromptHitRow{})
		for index, item := range request.Questions {
			rows.addBlock(
				strconv.Itoa(index+1)+". "+questionOneLine(item.Header)+": "+
					questionAnswerLabel(*state, index),
				questionPromptHitRow{},
			)
		}
		rows.add("", questionPromptHitRow{})
		rows.add("Enter submit · Click Submit again · Tab/←/→ navigate · Esc cancel", questionPromptHitRow{})
	} else {
		item := request.Questions[state.tab]
		cursor := state.cursors[state.tab]
		rows.addBlock(item.Question, questionPromptHitRow{})
		rows.add("", questionPromptHitRow{})

		if item.IsText() {
			rows.addEditor(state.editor, state.editing != questionEditNone, 0)
			rows.add(
				"Enter continue · Ctrl+J newline · Tab/Shift+Tab questions · Esc cancel",
				questionPromptHitRow{},
			)
		} else {
			for index, option := range item.Options {
				target := questionPromptHitRow{
					kind: questionPromptHitOption, question: state.tab, option: index,
				}
				focus := "  "
				if cursor == index {
					focus = "› "
				}
				marker := "○"
				if item.Multiple {
					marker = questionUnchecked
				}
				if state.selected[state.tab][index] {
					marker = "●"
					if item.Multiple {
						marker = "[✓]"
					}
				}
				line := focus + strconv.Itoa(index+1) + ". " + marker + " " + questionOneLine(option.Label)
				rows.add(m.styleQuestionChoice(line, cursor == index), target)

				// The focused option carries the explanation and preview the choice
				// needs; the other rows stay one line so four options remain
				// scannable in a short window.
				if cursor == index {
					m.appendQuestionOptionDetail(rows, option, target)
				}
			}

			customIndex := len(item.Options)
			rows.add(m.styleQuestionChoice(
				questionFocus(cursor == customIndex)+strconv.Itoa(customIndex+1)+". Type something",
				cursor == customIndex,
			), questionPromptHitRow{kind: questionPromptHitCustom, question: state.tab})
			rows.add(
				strings.Repeat("─", max(1, min(m.promptBandWidth()-2, 72))),
				questionPromptHitRow{},
			)
			chatIndex := customIndex + 1
			rows.add(m.styleQuestionChoice(
				questionFocus(cursor == chatIndex)+strconv.Itoa(chatIndex+1)+". Chat about this",
				cursor == chatIndex,
			), questionPromptHitRow{kind: questionPromptHitChat, question: state.tab})

			switch {
			case state.editing != questionEditNone:
				rows.add("", questionPromptHitRow{})
				rows.addEditor(state.editor, true, 0)
				rows.add("Enter submit · Ctrl+J newline · Esc back", questionPromptHitRow{})
			case item.Multiple:
				rows.add("", questionPromptHitRow{})
				rows.add("Space/Click toggle · Enter continue · Tab/←/→ questions · Esc cancel", questionPromptHitRow{})
			default:
				rows.add("", questionPromptHitRow{})
				rows.add("Enter/Click select · Tab/←/→ questions · Esc cancel", questionPromptHitRow{})
			}
		}
	}

	m.appendQuestionStatusRows(rows, state)

	// The band budget owns the final cut; this only keeps an unusually tall
	// preview from building rows no frame can show.
	rows.crop(max(8, min(16, m.height-8)))

	m.applyQuestionGutter(rows)

	return strings.Join(rows.values, "\n"), rows.hit()
}

// applyQuestionGutter paints the accent gutter every question row shares and
// moves the pointer map and caret with it.
func (m *Model) applyQuestionGutter(rows *questionPromptRows) {
	bar := "▌"
	if !m.options.NoColor {
		bar = lipgloss.NewStyle().Foreground(paletteFor(m.theme).session).Render(bar)
	}
	gutter := ansi.StringWidth(bar) + 1
	for index := range rows.values {
		rows.values[index] = bar + " " +
			ansi.Truncate(rows.values[index], max(1, m.promptBandWidth()-2), "…")
	}
	// Pointer columns and the caret are measured against the painted row, so
	// the accent gutter and its separating space move both by the same width.
	for index := range rows.targets {
		for segment := range rows.targets[index].segments {
			rows.targets[index].segments[segment].start += gutter
			rows.targets[index].segments[segment].end += gutter
		}
	}
	if rows.cursor {
		rows.cursorX += gutter
	}
}

// addEditor appends the textarea's rows and records where its caret sits
// relative to the prompt. gutter is the display width the caller will prepend to
// every row before the rows are painted.
func (rows *questionPromptRows) addEditor(editor textarea.Model, active bool, gutter int) {
	start := len(rows.values)
	rows.addBlock(editor.View(), questionPromptHitRow{})

	cursor := editor.Cursor()
	if !active || cursor == nil {
		return
	}
	row := start + cursor.Y
	if row < 0 || row >= len(rows.values) {
		return
	}
	rows.cursor = true
	rows.cursorX = gutter + cursor.X
	rows.cursorY = row
}

func (m *Model) appendQuestionOptionDetail(
	rows *questionPromptRows,
	option question.Option,
	target questionPromptHitRow,
) {
	target.detail = true
	if option.Description != "" {
		rows.add("     "+questionOneLine(option.Description), target)
	}
	if option.Preview == "" {
		return
	}
	preview, err := m.markdown.render(
		option.Preview,
		max(1, m.promptBandWidth()-6),
		m.theme,
		m.options.NoColor,
	)
	if err != nil {
		return
	}
	preview = truncateHeight(preview, max(2, min(4, m.height/6)))
	for line := range strings.SplitSeq(preview, "\n") {
		rows.add("     "+strings.TrimRight(line, " "), target)
	}
}

// appendQueueProgress shows where this prompt sits in a same-response ask_user
// batch. A single call keeps the original compact layout.
func (m *Model) appendQueueProgress(rows *questionPromptRows, request question.Request) {
	if request.QueueSize <= 1 || request.QueueIndex < 1 {
		return
	}

	rows.add(fmt.Sprintf("Question %d/%d", request.QueueIndex, request.QueueSize), questionPromptHitRow{})
	rows.add("", questionPromptHitRow{})
}

func (m *Model) appendQuestionStatusRows(rows *questionPromptRows, state *questionPromptState) {
	if state.loading {
		rows.add(m.activityNotice("Working…"), questionPromptHitRow{})
	}
	if state.err != nil {
		rows.add("Error: "+safeError(state.err), questionPromptHitRow{})
	}
}

// crop keeps at most maximum rows while preserving the hit map and the caret.
// The dropped middle is replaced by one ellipsis row, so a preview can never
// push the question or the editor off the band.
func (rows *questionPromptRows) crop(maximum int) {
	if maximum <= 0 || len(rows.values) <= maximum {
		return
	}

	// Every choice stays on screen. The explanation and preview after the
	// focused option are detail, so a short band spends its remaining rows on as
	// much of that detail as fits and drops the rest.
	essential := 0
	for index := range rows.targets {
		if !rows.targets[index].detail {
			essential++
		}
	}
	if essential <= maximum {
		rows.trimDetails(maximum - essential)

		return
	}

	tail := min(3, maximum)
	head := max(0, maximum-tail-1)
	tailStart := max(head, len(rows.values)-tail)

	mapIndex := func(index int) int {
		switch {
		case index < head:
			return index
		case index >= tailStart:
			return head + 1 + index - tailStart
		default:
			return -1
		}
	}

	if rows.cursor {
		rows.cursorY = mapIndex(rows.cursorY)
		rows.cursor = rows.cursorY >= 0
	}
	values := make([]string, 0, maximum)
	values = append(values, rows.values[:head]...)
	values = append(values, "…")
	values = append(values, rows.values[tailStart:]...)
	targets := make([]questionPromptHitRow, 0, maximum)
	targets = append(targets, rows.targets[:head]...)
	targets = append(targets, questionPromptHitRow{})
	targets = append(targets, rows.targets[tailStart:]...)
	rows.values = values
	rows.targets = targets
}

// trimDetails keeps at most available detail rows in place, preserving the
// order of every essential row and the caret's row.
func (rows *questionPromptRows) trimDetails(available int) {
	if available < 0 {
		available = 0
	}
	values := make([]string, 0, len(rows.values))
	targets := make([]questionPromptHitRow, 0, len(rows.targets))
	cursor := -1
	kept := 0

	for index := range rows.values {
		if rows.targets[index].detail {
			if kept >= available {
				continue
			}
			kept++
		}
		if rows.cursor && index == rows.cursorY {
			cursor = len(values)
		}
		values = append(values, rows.values[index])
		targets = append(targets, rows.targets[index])
	}
	rows.values = values
	rows.targets = targets
	if rows.cursor {
		rows.cursor = cursor >= 0
		rows.cursorY = cursor
	}
}

// hit returns the pointer map for the rows as painted so far. readyView adds the
// frame row the band starts on.
func (rows *questionPromptRows) hit() questionPromptHit {
	return questionPromptHit{
		top: -1, rows: rows.targets,
		cursor: rows.cursor, cursorX: rows.cursorX, cursorY: rows.cursorY,
	}
}

// cropTop drops rows the frame cap removed from the top of the band, keeping
// the hit map and caret aligned with the rows that were actually painted.
func (hit *questionPromptHit) cropTop(dropped int) {
	if dropped <= 0 {
		return
	}
	if dropped >= len(hit.rows) {
		hit.rows = nil
		hit.cursor = false

		return
	}
	hit.rows = hit.rows[dropped:]
	if hit.cursor {
		hit.cursorY -= dropped
		hit.cursor = hit.cursorY >= 0
	}
}

// place resolves the hit map against the frame: top is the frame row the first
// prompt row was painted at, and inset is the transcript column the band shares.
func (hit *questionPromptHit) place(top, inset int) {
	hit.top = -1
	if len(hit.rows) == 0 || top < 0 {
		hit.rows = nil
		hit.cursor = false

		return
	}
	hit.top = top
	for index := range hit.rows {
		for segment := range hit.rows[index].segments {
			hit.rows[index].segments[segment].start += inset
			hit.rows[index].segments[segment].end += inset
		}
	}
	if hit.cursor {
		hit.cursorX += inset
		hit.cursorY += top
	}
}

// resizeQuestionEditor keeps the textarea's wrap width tied to the current
// frame, so a question received before a resize still wraps inside the band.
func (m *Model) resizeQuestionEditor(state *questionPromptState) {
	width := max(1, m.promptBandWidth()-4)
	if state.editor.Width() != width {
		state.editor.SetWidth(width)
	}
}

// questionPromptMouse answers a left click on the question band. It uses the
// same state transitions as the keys, so the pointer can never select something
// the keyboard could not.
//
//nolint:gocyclo // Each hit target is one explicit binding, mirroring the key switch.
func (m *Model) questionPromptMouse(message tea.MouseMsg) (tea.Cmd, bool) {
	if m.prompt.kind != promptQuestion || m.frameHit.question.top < 0 {
		return nil, false
	}
	click, ok := message.(tea.MouseClickMsg)
	if !ok || click.Button != tea.MouseLeft {
		return nil, false
	}

	state := &m.prompt.question
	if state.loading {
		return nil, true
	}
	if state.editing != questionEditNone && !m.questionTabIsText(state) {
		return nil, true
	}

	row := click.Y - m.frameHit.question.top
	if row < 0 || row >= len(m.frameHit.question.rows) {
		return nil, false
	}
	target := m.frameHit.question.rows[row]

	switch target.kind {
	case questionPromptHitTab:
		for _, segment := range target.segments {
			if click.X < segment.start || click.X > segment.end {
				continue
			}
			submitting := segment.index == len(state.request.Questions)
			if submitting && state.tab == segment.index {
				// A second click on the active Submit tab is the pointer form of
				// Enter on the review pane.
				_, command := m.activateQuestionSelection()

				return command, true
			}
			if m.questionTabIsText(state) {
				state.custom[state.tab] = strings.TrimSpace(state.editor.Value())
			}
			state.err = nil
			command := m.enterQuestionTab(state, segment.index)
			if command != nil {
				return tea.Batch(redrawFrame(), command), true
			}

			return redrawFrame(), true
		}

		return nil, true
	case questionPromptHitOption:
		if target.question < 0 || target.question >= len(state.request.Questions) {
			return nil, true
		}
		m.enterQuestionTab(state, target.question)
		state.cursors[target.question] = target.option
		state.err = nil

		if item := state.request.Questions[target.question]; item.Multiple {
			m.toggleQuestionSelection()

			return redrawFrame(), true
		}

		_, command := m.activateQuestionSelection()

		return command, true
	case questionPromptHitCustom, questionPromptHitChat:
		if target.question < 0 || target.question >= len(state.request.Questions) {
			return nil, true
		}
		m.enterQuestionTab(state, target.question)
		cursor := len(state.request.Questions[target.question].Options)
		if target.kind == questionPromptHitChat {
			cursor++
		}
		state.cursors[target.question] = cursor
		state.err = nil
		_, command := m.activateQuestionSelection()

		return command, true
	default:
		return nil, true
	}
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

// questionOneLine keeps model-supplied labels from adding rows to a band whose
// pointer map counts one row per label. Free-form answer text keeps its line
// breaks because it is rendered as a block instead.
func questionOneLine(value string) string {
	value = strings.ReplaceAll(value, "\r\n", " ")
	value = strings.ReplaceAll(value, "\n", " ")

	return strings.ReplaceAll(value, "\r", " ")
}

func firstUnansweredQuestion(state questionPromptState) int {
	for index := range state.request.Questions {
		if !questionAnswered(state, index) {
			return index
		}
	}

	return -1
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
	if source := m.promptSourceLine(); source != "" {
		lines = append(lines, source)
	}
	lines = append(lines, m.approvalPromptRequestLines()...)
	lines = append(lines, "")
	for index, choice := range m.prompt.choices {
		prefix := "  "
		if index == m.prompt.cursor {
			prefix = "> "
		}
		lines = append(lines, prefix+approvalChoiceLabel(choice))
	}
	lines = append(lines, "↑/↓ choose · Enter confirm · or press the key shown in ( )")
	if m.prompt.loading {
		lines = append(lines, m.activityNotice("Working…"))
	}
	if m.prompt.err != nil {
		lines = append(lines, safeError(m.prompt.err))
	}

	return renderGutterLines(lines, m.promptBandWidth(), m.options.NoColor, paletteFor(m.theme).warning)
}

// approvalPromptRequestLines shows exactly what is being approved. The command
// is wrapped inside the accent gutter and never silently truncated: a long
// script is capped with an explicit count of the hidden lines.
func (m *Model) approvalPromptRequestLines() []string {
	maxRows := m.approvalCommandRows()
	if m.prompt.subagent != nil {
		state, ok := m.subagentInteractions.values[m.prompt.subagent.childSessionID]
		switch {
		case ok && state.Approval.Review != nil:
			value := state.Approval.Review
			command := append([]string{value.Operation.Executable()}, value.Operation.Args()...)

			return approvalReviewLines(
				value.Call.Name, command,
				value.Operation.CWD(), value.Operation.Justification(), maxRows,
			)
		case ok && state.Approval.Unknown != nil:
			return approvalUnknownLines(state.Approval.Unknown.Tool, state.Approval.Unknown.Reason)
		default:
			return nil
		}
	}

	state := m.promptApprovalState()
	if value := state.Required; value != nil {
		return approvalReviewLines(
			value.Tool, value.Command, value.CWD, value.Justification, maxRows,
		)
	}
	if value := state.Unknown; value != nil {
		return approvalUnknownLines(value.Tool, value.Reason)
	}

	return nil
}

func (m *Model) approvalCommandRows() int {
	return max(4, m.height-14)
}

// approvalReviewLines names the Tool and reproduces the command being approved
// with its newlines intact, so a multi-step script cannot be approved unseen.
func approvalReviewLines(tool string, command []string, cwd, justification string, maxRows int) []string {
	lines := []string{tool}
	rows := strings.Split(approvalCommandScript(command), "\n")
	if maxRows > 0 && len(rows) > maxRows {
		hidden := len(rows) - maxRows + 1
		rows = append(append([]string{}, rows[:maxRows-1]...),
			fmt.Sprintf("… %d more lines not shown", hidden))
	}

	for index, row := range rows {
		prefix := "  "
		if index == 0 {
			prefix = detailShellPrompt
		}
		lines = append(lines, prefix+row)
	}
	if cwd != "" {
		lines = append(lines, "cwd "+cwd)
	}
	if justification != "" {
		lines = append(lines, "reason "+justification)
	}

	return lines
}

// approvalCommandScript unwraps a `<shell> -c <script>` invocation so the
// reader approves the script rather than the interpreter wrapper. Only the
// first two tokens are inspected: the script keeps its own whitespace and
// newlines so multi-step commands stay readable.
func approvalCommandScript(command []string) string {
	return shellCommandScript(strings.Join(command, " "))
}

// splitLeadingToken splits value at its first whitespace run, returning the
// token and the untrimmed remainder.
func splitLeadingToken(value string) (string, string, bool) {
	value = strings.TrimLeft(value, " \t")
	if value == "" {
		return "", "", false
	}

	index := strings.IndexAny(value, " \t")
	if index < 0 {
		return value, "", false
	}

	return value[:index], value[index+1:], true
}

// approvalUnknownLines explains an operation whose outcome could not be
// confirmed, in prose rather than the raw reason code.
func approvalUnknownLines(tool, reason string) []string {
	return []string{
		"Recovery required · " + tool,
		approvalUnknownReason(reason),
	}
}

func approvalUnknownReason(reason string) string {
	switch reason {
	case "started_without_durable_result":
		return "The command started but its result was not recorded. Retry it, or mark it as failed."
	case "":
		return "The command result could not be confirmed."
	default:
		return humanizeStatusCode(reason)
	}
}

// approvalChoiceLabel pairs each choice with the direct key that selects it.
func approvalChoiceLabel(choice approval.Choice) string {
	switch choice {
	case approval.ChoiceAllowOnce:
		return "Allow once (o)"
	case approval.ChoiceAllowSession:
		return "Allow for this session (s)"
	case approval.ChoiceDeny:
		return "Deny (d)"
	case approval.ChoiceRetry:
		return "Retry the command (r)"
	case approval.ChoiceMarkFailed:
		return "Mark as failed (f)"
	case approval.ChoiceAcknowledge:
		return "Acknowledge (a)"
	default:
		return humanizeStatusCode(string(choice))
	}
}

// renderGutterLines wraps every prompt row inside the accent gutter so a long
// command or justification stays readable instead of being cut off.
func renderGutterLines(lines []string, width int, noColor bool, tone color.Color) string {
	bar := "▌"
	if !noColor {
		bar = lipgloss.NewStyle().Foreground(tone).Render(bar)
	}
	available := max(1, width-2)
	rows := make([]string, 0, len(lines))

	for _, line := range lines {
		line = strings.TrimRight(line, " ")
		if line == "" || ansi.StringWidth(line) <= available {
			rows = append(rows, bar+" "+line)

			continue
		}

		leading := line[:len(line)-len(strings.TrimLeft(line, " "))]
		wrapped := ansi.Wrap(
			strings.TrimLeft(line, " "),
			max(1, available-ansi.StringWidth(leading)),
			"",
		)
		for part := range strings.SplitSeq(wrapped, "\n") {
			rows = append(rows, bar+" "+leading+part)
		}
	}

	return strings.Join(rows, "\n")
}

func (m *Model) promptApprovalState() coding.ApprovalState {
	if m.prompt.team == nil {
		return m.state.Approval
	}
	entry, ok := m.teamInteraction(m.prompt.team.key)
	if !ok {
		return coding.ApprovalState{}
	}

	return entry.approval
}

func (m *Model) teamPromptSourceLine() string {
	if m.prompt.team == nil {
		return ""
	}
	worker := boundedTeamLabel(m.prompt.team.workerName)
	if worker == "" {
		worker = genericTeamWorkerLabel
	}
	task := boundedTeamLabel(m.prompt.team.taskTitle)
	if task == "" {
		task = genericTeamTaskLabel
	}

	return "Team Worker · " + worker + " · Task: " + task
}

func (m *Model) promptSourceLine() string {
	if m.prompt.subagent != nil {
		name := safeDetailText(m.prompt.subagent.agentName)
		if name == "" {
			name = genericSubagentLabel
		}

		return "Subagent · " + truncateText(name, 96)
	}

	return m.teamPromptSourceLine()
}

func (m *Model) compactPromptView() string {
	lines := []string{"△ Compact context"}
	switch {
	case m.prompt.loading:
		lines = append(lines, m.activityNotice("Loading compaction preview…"))
	case m.prompt.err != nil:
		lines = append(lines, "Error: "+safeError(m.prompt.err))
	case !m.prompt.preview.Available:
		lines = append(lines, "Unavailable: "+m.prompt.preview.DisabledReason, "Esc close")
	default:
		preview := m.prompt.preview
		detail := "Summarize " + strconv.Itoa(preview.SummarizedMessages) + " messages · keep " + strconv.Itoa(preview.KeptMessages)
		recovery := "Compaction may omit details. The durable branch remains recoverable."
		if preview.Strategy == coding.CompactionStrategyFull {
			detail = "Rebuild context from " + strconv.Itoa(preview.SummarizedMessages) + " messages · preserve latest user request"
			recovery = "Original history stays in a private searchable archive. Plan, Goal and cumulative usage are not reset."
		}
		lines = append(lines,
			"Estimated context: "+strconv.Itoa(preview.EstimatedTokens)+" tokens · threshold: "+strconv.Itoa(preview.ThresholdTokens),
			detail,
			recovery,
			"Enter confirm · Esc cancel",
		)
	}

	return renderGutterLines(lines, m.promptBandWidth(), m.options.NoColor, paletteFor(m.theme).warning)
}
