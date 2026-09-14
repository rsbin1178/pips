//nolint:wsl_v5 // Plan review state, key handling, and its compact rendering are kept together.
package tui

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/planreview"
)

const (
	planReviewTitle = "Plan ready for review"
	planEnterTitle  = "Enter plan mode?"

	// planEmptyReviewBody replaces the preview when the agent exits plan mode
	// without writing a plan file.
	planEmptyReviewBody = "No plan written yet.\n\n" +
		"The agent exited plan mode without writing a plan.\n\n" +
		"- **Approve**: leave plan mode and start implementing\n" +
		"- **Request changes**: send the agent back to planning\n" +
		"- **Quit**: abandon and turn plan mode off"

	planReviewNotesPlaceholder   = "Type revision notes..."
	planReviewCommentPlaceholder = "Type your comment..."
)

// planComment is one pending review comment. Lines are 1-based indexes into
// the rendered preview.
type planComment struct {
	first int
	last  int
	text  string
}

type planReviewPromptState struct {
	request       planreview.Request
	document      coding.PlanDocument
	cursor        int
	anchor        int
	selecting     bool
	offset        int
	loading       bool
	editing       bool
	commenting    bool
	comments      []planComment
	editor        textarea.Model
	commentEditor textarea.Model
	lines         []string
	linesWidth    int
	notice        string
	err           error
}

type planViewPromptState struct {
	document    coding.PlanDocument
	loading     bool
	loadStarted bool
	offset      int
	lines       []string
	linesWidth  int
	err         error
}

type planDocumentMsg struct {
	generation uint64
	preview    bool
	document   coding.PlanDocument
	err        error
}

func (m *Model) newPlanReviewPrompt(request planreview.Request) planReviewPromptState {
	state := planReviewPromptState{
		request:       planreview.CloneRequest(request),
		editor:        m.newPromptEditor(planReviewNotesPlaceholder),
		commentEditor: m.newPromptEditor(planReviewCommentPlaceholder),
	}
	if request.Content != "" {
		state.document = coding.PlanDocument{
			Exists: true, Content: request.Content, Size: request.Size,
		}
	}

	return state
}

func (m *Model) newPromptEditor(placeholder string) textarea.Model {
	editor := textarea.New()
	editor.Prompt = ""
	editor.Placeholder = placeholder
	editor.ShowLineNumbers = false
	editor.DynamicHeight = true
	editor.MinHeight = 1
	editor.MaxHeight = 4
	editor.MaxContentHeight = 8
	editor.SetVirtualCursor(false)
	editor.SetWidth(max(1, m.width-4))
	editor.SetStyles(composerStyles(m.theme, m.options.NoColor))

	return editor
}

// planReviewActiveEditor reports the editor that owns the terminal cursor.
func (m *Model) planReviewActiveEditor() *textarea.Model {
	state := &m.prompt.planReview
	switch {
	case state.commenting:
		return &state.commentEditor
	case state.editing:
		return &state.editor
	default:
		return nil
	}
}

// placePlanReviewCursor moves the terminal cursor into the active plan review
// editor when the review prompt is the visible footer.
func (m *Model) placePlanReviewCursor(
	view *tea.View,
	parts []string,
	promptContent string,
	promptIndex int,
) {
	if promptIndex < 0 || m.prompt.kind != promptPlanReview {
		return
	}
	editor := m.planReviewActiveEditor()
	if editor == nil || editor.View() == "" {
		return
	}
	cursorX, cursorY, ok := editorOffset(promptContent, editor.View())
	if !ok {
		return
	}
	view.Cursor = editor.Cursor()
	if view.Cursor == nil {
		return
	}
	promptOffset := lipgloss.Height(lipgloss.JoinVertical(lipgloss.Left, parts[:promptIndex]...))
	view.Cursor.X += cursorX
	view.Cursor.Y += promptOffset + cursorY
}

func (m *Model) updatePlanReviewPromptKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	state := &m.prompt.planReview
	if state.loading {
		return m, nil
	}
	if state.request.Kind == planreview.KindEnter {
		return m.updatePlanEnterKey(message)
	}

	switch {
	case state.commenting:
		return m.updatePlanReviewCommentKey(message)
	case state.editing:
		return m.updatePlanReviewNotesKey(message)
	default:
		return m.updatePlanReviewSelectionKey(message)
	}
}

func (m *Model) updatePlanEnterKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch message.String() {
	case "a", keyEnter:
		return m.resolvePlanReview(planreview.DecisionApprove, nil, "")
	case "d", keyEscape, keyCtrlC:
		return m.resolvePlanReview(planreview.DecisionDecline, nil, "")
	default:
		return m, nil
	}
}

//nolint:gocyclo // The preview key map is deliberately visible as direct bindings.
func (m *Model) updatePlanReviewSelectionKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	state := &m.prompt.planReview
	count := len(m.planPreviewLines(state))
	viewport := planPreviewHeight(m.height)
	move := func(delta int) {
		state.cursor = max(0, min(state.cursor+delta, max(0, count-1)))
		state.notice = ""
		planFollowCursor(state, count, viewport)
	}

	switch message.String() {
	case keyEscape:
		state.selecting = false
		state.notice = ""
	case "up", "k":
		move(-1)
	case keyDown, "j":
		move(1)
	case keyPageUp:
		move(-viewport)
	case keyPageDown:
		move(viewport)
	case "v":
		state.selecting = !state.selecting
		if state.selecting {
			state.anchor = state.cursor
		}
	case "c", keyEnter:
		return m.beginPlanComment(state)
	case "x":
		removePlanComment(state)
	case "a":
		return m.resolvePlanReviewResolution(planreview.Resolution{
			RequestID: state.request.ID,
			Decision:  planreview.DecisionApprove,
			Comments:  planReviewComments(state.comments),
		})
	case "s", keyTab:
		state.editing = true
		state.notice = ""
		state.err = nil

		return m, state.editor.Focus()
	case "y":
		return m.copyPlanReview(state)
	case "q", keyCtrlC:
		return m.resolvePlanReview(planreview.DecisionQuit, nil, "")
	}

	return m, nil
}

func (m *Model) beginPlanComment(state *planReviewPromptState) (tea.Model, tea.Cmd) {
	if !planReviewHasContent(state) {
		state.notice = "No plan content to comment on."

		return m, nil
	}

	state.commenting = true
	state.editing = false
	state.editor.Blur()
	state.commentEditor.Reset()
	state.notice = ""
	state.err = nil

	return m, state.commentEditor.Focus()
}

func removePlanComment(state *planReviewPromptState) {
	if len(state.comments) == 0 {
		state.notice = "No comments to remove."

		return
	}

	state.comments = state.comments[:len(state.comments)-1]
	state.notice = "Comment removed."
}

func (m *Model) updatePlanReviewCommentKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	state := &m.prompt.planReview
	switch message.String() {
	case keyEscape:
		state.commenting = false
		state.commentEditor.Blur()
		state.commentEditor.Reset()
		state.notice = ""

		return m, nil
	case keyCtrlJ, keyShiftEnter:
		state.commentEditor.InsertString("\n")

		return m, nil
	case keyEnter:
		text := strings.TrimSpace(state.commentEditor.Value())
		first, last := planSelectionBounds(state)
		state.commenting = false
		state.commentEditor.Blur()
		state.commentEditor.Reset()
		if text == "" {
			state.notice = "Comment discarded."

			return m, nil
		}
		state.comments = append(state.comments, planComment{first: first, last: last, text: text})
		state.selecting = false
		state.notice = "Comment added."

		return m, nil
	}

	var command tea.Cmd
	state.commentEditor, command = state.commentEditor.Update(message)

	return m, command
}

func (m *Model) updatePlanReviewNotesKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	state := &m.prompt.planReview
	switch message.String() {
	case keyEscape, keyTab:
		state.editing = false
		state.editor.Blur()
		state.notice = ""

		return m, nil
	case keyCtrlJ, keyShiftEnter:
		state.editor.InsertString("\n")

		return m, nil
	case keyEnter:
		notes := strings.TrimSpace(state.editor.Value())
		if notes == "" {
			state.notice = "Type revision notes, or press a to approve."

			return m, nil
		}

		return m.resolvePlanReview(planreview.DecisionRevise, nil, notes)
	}

	var command tea.Cmd
	state.editor, command = state.editor.Update(message)

	return m, command
}

// copyPlanReview sends the full plan through OSC52. Terminals without OSC52
// support ignore the sequence.
func (m *Model) copyPlanReview(state *planReviewPromptState) (tea.Model, tea.Cmd) {
	if !planReviewHasContent(state) {
		state.notice = "No plan content to copy."

		return m, nil
	}

	state.notice = "Plan copied to clipboard."

	return m, tea.SetClipboard(strings.TrimSpace(state.document.Content))
}

func (m *Model) resolvePlanReview(
	decision planreview.Decision,
	comments []string,
	notes string,
) (tea.Model, tea.Cmd) {
	return m.resolvePlanReviewResolution(planreview.Resolution{
		RequestID: m.prompt.planReview.request.ID,
		Decision:  decision,
		Comments:  comments,
		Notes:     notes,
	})
}

func (m *Model) resolvePlanReviewResolution(
	resolution planreview.Resolution,
) (tea.Model, tea.Cmd) {
	state := &m.prompt.planReview
	if err := planreview.ValidateResolution(state.request, resolution); err != nil {
		state.err = err

		return m, nil
	}
	reviewer, ok := m.controller.(planReviewController)
	if !ok {
		state.err = errors.New("plan review is unavailable")

		return m, nil
	}

	state.loading = true
	state.err = nil

	return m, m.startStream(func(ctx context.Context) iter.Seq2[coding.Event, error] {
		return reviewer.ResolvePlanReview(ctx, resolution)
	})
}

func (m *Model) planReviewPromptView() string {
	if m.prompt.planReview.request.Kind == planreview.KindEnter {
		return m.planEnterPromptView()
	}

	return m.planExitPromptView()
}

func (m *Model) planEnterPromptView() string {
	state := &m.prompt.planReview
	lines := []string{planEnterTitle, "", "Plan mode is read-only except for the plan file."}
	if state.loading {
		lines = append(lines, "", m.activityNotice("Working…"))
	}
	if state.err != nil {
		lines = append(lines, "", "Error: "+safeError(state.err))
	}
	if !state.loading {
		lines = append(lines, "", "a approve · d decline")
	}

	return m.renderPlanPromptLines(lines)
}

func (m *Model) planExitPromptView() string {
	state := &m.prompt.planReview
	if state.loading {
		return m.renderPlanPromptLines([]string{
			planReviewTitle, "", m.activityNotice("Working…"),
		})
	}

	return m.renderPlanPromptLines(append(
		[]string{planReviewTitle, ""},
		m.planExitReviewLines(state)...,
	))
}

func (m *Model) planExitReviewLines(state *planReviewPromptState) []string {
	all := m.planPreviewLines(state)
	viewport := planPreviewHeight(m.height)
	planFollowCursor(state, len(all), viewport)
	end := min(len(all), state.offset+viewport)
	numberWidth := len(strconv.Itoa(max(1, len(all))))

	lines := make([]string, 0, end-state.offset+8)
	for index := state.offset; index < end; index++ {
		if !planReviewHasContent(state) {
			lines = append(lines, "  "+ansi.Truncate(all[index], max(1, m.width-4), "…"))

			continue
		}
		lines = append(lines, m.planPreviewRow(state, index, numberWidth, all[index]))
	}
	lines = append(lines, "", planReviewPosition(state, len(all), viewport))
	for index, comment := range state.comments {
		lines = append(lines, strconv.Itoa(index+1)+". "+planCommentText(comment))
	}

	lines = append(lines, "", planReviewActions(state))
	switch {
	case state.commenting:
		lines = append(
			lines,
			planCommentPromptLabel(state),
			state.commentEditor.View(),
			"Enter save comment · Esc cancel",
		)
	case state.editing:
		lines = append(
			lines,
			state.editor.View(),
			"Enter request changes · Ctrl+J newline · Esc back",
		)
	case state.selecting:
		lines = append(lines, "v clear range · ↑/↓ extend · Enter comment · Tab notes")
	default:
		lines = append(lines, "↑/↓ scroll · Enter comment · v select range · Tab notes")
	}
	if state.notice != "" {
		lines = append(lines, state.notice)
	}
	if state.err != nil {
		lines = append(lines, "Error: "+safeError(state.err))
	}

	return lines
}

func planReviewActions(state *planReviewPromptState) string {
	approve := "a approve"
	if len(state.comments) > 0 {
		approve = "a approve w/ comments"
	}

	return approve + " · s request changes · c comment · y copy plan · q quit plan"
}

func planReviewPosition(state *planReviewPromptState, count, viewport int) string {
	if !planReviewHasContent(state) {
		return "plan.md (empty)"
	}

	first := state.offset + 1
	last := min(count, state.offset+viewport)
	label := "plan.md · lines " + strconv.Itoa(first) + "-" + strconv.Itoa(last) +
		"/" + strconv.Itoa(count)
	switch {
	case len(state.comments) == 1:
		return label + " · 1 comment"
	case len(state.comments) > 1:
		return label + " · " + strconv.Itoa(len(state.comments)) + " comments"
	default:
		return label
	}
}

func planCommentPromptLabel(state *planReviewPromptState) string {
	first, last := planSelectionBounds(state)
	if first == last {
		return "Comment line " + strconv.Itoa(first) + ":"
	}

	return "Comment lines " + strconv.Itoa(first) + "-" + strconv.Itoa(last) + ":"
}

func (m *Model) planPreviewRow(
	state *planReviewPromptState,
	index, numberWidth int,
	content string,
) string {
	marker := " "
	first, last := planSelectionBounds(state)
	if index >= first && index <= last && index != state.cursor {
		marker = "│"
	}
	if index == state.cursor {
		marker = "›"
	}
	gutter := marker + " " + fmt.Sprintf("%*d", numberWidth, index+1) + " │ "
	available := max(1, max(1, m.width-2)-ansi.StringWidth(gutter))

	return gutter + ansi.Truncate(content, available, "…")
}

// planSelectionBounds reports the selected preview lines as 1-based numbers.
func planSelectionBounds(state *planReviewPromptState) (int, int) {
	first := state.cursor
	last := state.cursor
	if state.selecting {
		first = min(state.anchor, state.cursor)
		last = max(state.anchor, state.cursor)
	}

	return first + 1, last + 1
}

func (m *Model) planPreviewLines(state *planReviewPromptState) []string {
	if state.lines != nil && state.linesWidth == m.width {
		return state.lines
	}

	body := planReviewBody(state)
	rendered, err := m.markdown.render(body, max(1, m.width-8), m.theme, m.options.NoColor)
	if err != nil {
		rendered = body
	}
	state.lines = strings.Split(strings.TrimRight(rendered, "\n"), "\n")
	state.linesWidth = m.width

	return state.lines
}

func planReviewBody(state *planReviewPromptState) string {
	if planReviewHasContent(state) {
		return state.document.Content
	}

	return planEmptyReviewBody
}

func planReviewHasContent(state *planReviewPromptState) bool {
	return state.request.HasContent && strings.TrimSpace(state.document.Content) != ""
}

func planPreviewHeight(height int) int {
	return max(4, min(16, height/2))
}

// planFollowCursor keeps the selected preview line inside the viewport.
func planFollowCursor(state *planReviewPromptState, count, viewport int) {
	state.offset = clampPlanOffset(state.offset, count, viewport)
	state.offset = min(state.offset, state.cursor)
	state.offset = max(state.offset, state.cursor-viewport+1)
	state.offset = clampPlanOffset(state.offset, count, viewport)
}

func clampPlanOffset(offset, count, viewport int) int {
	if count <= 0 {
		return 0
	}

	return max(0, min(offset, max(0, count-viewport)))
}

func planReviewComments(comments []planComment) []string {
	if len(comments) == 0 {
		return nil
	}

	result := make([]string, 0, len(comments))
	for _, comment := range comments {
		result = append(result, planCommentText(comment))
	}

	return result
}

func planCommentText(comment planComment) string {
	text := strings.Join(strings.Fields(comment.text), " ")
	if comment.last <= comment.first {
		return "Line " + strconv.Itoa(comment.first) + ": " + text
	}

	return "Lines " + strconv.Itoa(comment.first) + "-" + strconv.Itoa(comment.last) +
		": " + text
}

func (m *Model) renderPlanPromptLines(lines []string) string {
	bar := "▌"
	if !m.options.NoColor {
		bar = lipgloss.NewStyle().Foreground(paletteFor(m.theme).session).Render(bar)
	}
	for index := range lines {
		lines[index] = bar + " " + ansi.Truncate(lines[index], max(1, m.width-2), "…")
	}

	return strings.Join(lines, "\n")
}

// startPlanView opens the read-only preview of the saved session plan.
func (m *Model) startPlanView() tea.Cmd {
	activityWasVisible := m.activityClockVisible()
	m.promptSeq++
	m.prompt = promptState{
		kind:       promptPlanView,
		generation: m.promptSeq,
		planView:   planViewPromptState{loading: true},
	}
	m.setLayout()
	if m.controller == nil {
		m.prompt.planView.loading = false
		m.prompt.planView.err = errors.New("plan preview is unavailable")

		return m.startActivityClock(activityWasVisible)
	}

	return tea.Batch(m.loadPlanViewIfNeeded(), m.startActivityClock(activityWasVisible))
}

func (m *Model) loadPlanViewIfNeeded() tea.Cmd {
	if m.prompt.kind != promptPlanView || !m.prompt.planView.loading ||
		m.prompt.planView.loadStarted || m.controller == nil {
		return nil
	}
	reviewer, ok := m.controller.(planReviewController)
	if !ok {
		m.prompt.planView.loading = false
		m.prompt.planView.err = errors.New("plan preview is unavailable")

		return nil
	}

	activityWasVisible := m.activityClockVisible()
	m.prompt.planView.loadStarted = true
	generation := m.prompt.generation
	load := func() tea.Msg {
		document, err := reviewer.ReadPlanDocument(m.ctx)

		return planDocumentMsg{generation: generation, preview: true, document: document, err: err}
	}

	return tea.Batch(load, m.startActivityClock(activityWasVisible))
}

func (m *Model) applyPlanDocument(message planDocumentMsg) (tea.Model, tea.Cmd) {
	if !message.preview || m.prompt.kind != promptPlanView ||
		message.generation != m.prompt.generation {
		return m, nil
	}

	m.prompt.planView.loading = false
	m.prompt.planView.err = message.err
	if message.err == nil {
		m.prompt.planView.document = message.document
		m.prompt.planView.lines = nil
	}
	m.setLayout()

	return m, nil
}

func (m *Model) updatePlanViewPromptKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	state := &m.prompt.planView
	viewport := planPreviewHeight(m.height)
	switch message.String() {
	case keyEscape, "q", keyCtrlC:
		m.prompt = promptState{}

		return m, m.composer.Focus()
	case "r":
		if state.err == nil {
			return m, nil
		}
		activityWasVisible := m.activityClockVisible()
		state.loading = true
		state.loadStarted = false
		state.err = nil

		return m, tea.Batch(m.loadPlanViewIfNeeded(), m.startActivityClock(activityWasVisible))
	case "up", "k":
		state.offset = max(0, state.offset-1)
	case keyDown, "j":
		state.offset = clampPlanOffset(state.offset+1, len(m.planViewLines(state)), viewport)
	case keyPageUp:
		state.offset = max(0, state.offset-viewport)
	case keyPageDown:
		state.offset = clampPlanOffset(
			state.offset+viewport,
			len(m.planViewLines(state)),
			viewport,
		)
	default:
		return m, nil
	}

	return m, nil
}

func (m *Model) planViewPromptView() string {
	state := &m.prompt.planView
	lines := []string{"Plan (read-only)"}
	switch {
	case state.loading:
		lines = append(lines, m.activityNotice("Loading plan.md…"))
	case state.err != nil:
		lines = append(lines, "Unable to load plan.md: "+safeError(state.err), "R retry · Esc close")
	default:
		all := m.planViewLines(state)
		viewport := planPreviewHeight(m.height)
		state.offset = clampPlanOffset(state.offset, len(all), viewport)
		end := min(len(all), state.offset+viewport)
		for index := state.offset; index < end; index++ {
			lines = append(lines, "  "+ansi.Truncate(all[index], max(1, m.width-4), "…"))
		}
		if len(all) == 0 {
			lines = append(lines, "No plan written yet.")
		}
		lines = append(lines, "", "↑/↓ scroll · Esc close")
	}

	return m.renderPlanPromptLines(lines)
}

func (m *Model) planViewLines(state *planViewPromptState) []string {
	if state.lines != nil && state.linesWidth == m.width {
		return state.lines
	}
	content := strings.TrimSpace(state.document.Content)
	if !state.document.Exists || content == "" {
		state.lines = []string{}
		state.linesWidth = m.width

		return state.lines
	}
	rendered, err := m.markdown.render(content, max(1, m.width-4), m.theme, m.options.NoColor)
	if err != nil {
		rendered = content
	}
	state.lines = strings.Split(strings.TrimRight(rendered, "\n"), "\n")
	state.linesWidth = m.width

	return state.lines
}
