package tui

import (
	"context"
	"iter"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/planmode"
	"github.com/rsbin1178/pips/internal/coding/planreview"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const tuiPlanContent = "# Plan\n\n## Steps\n\n1. Add the parser\n2. Add the tests\n"

func TestPlanReviewPromptApprovesWithLineComments(t *testing.T) {
	t.Parallel()

	controller := newPlanPromptController(t, planreview.KindExit, tuiPlanContent)
	model := readyModelWithController(t, controller, true)
	state := &model.prompt.planReview

	view := model.View().Content
	assert.Contains(t, view, "Plan ready for review")
	assert.Contains(t, view, "Add the parser")
	assert.Contains(t, view, "a approve · s request changes · c comment · y copy plan · q quit plan")
	assert.Nil(t, model.View().Cursor)
	assert.NotContains(t, view, "\x1b[")

	for line := range strings.SplitSeq(view, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), model.width)
	}

	model.Update(key("c"))
	require.True(t, state.commenting)
	state.commentEditor.SetValue("Split this into two steps")

	_, command := model.Update(key("enter"))
	assert.Nil(t, command)
	assert.False(t, state.commenting)
	require.Len(t, state.comments, 1)

	view = model.View().Content
	assert.Contains(t, view, "a approve w/ comments")
	assert.Contains(t, view, "Line 1: Split this into two steps")

	_, command = model.Update(key("a"))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	require.Len(t, controller.resolutions, 1)
	assert.Equal(t, planreview.DecisionApprove, controller.resolutions[0].Decision)
	assert.Equal(t, []string{"Line 1: Split this into two steps"}, controller.resolutions[0].Comments)
	assert.Empty(t, controller.resolutions[0].Notes)
	assert.Equal(t, promptNone, model.prompt.kind)
}

func TestPlanReviewPromptScrollsAndCommentsOnARange(t *testing.T) {
	t.Parallel()

	controller := newPlanPromptController(t, planreview.KindExit, longPlanContent(40))
	model := readyModelWithController(t, controller, true)
	model.Update(tea.WindowSizeMsg{Width: 72, Height: 24})
	state := &model.prompt.planReview
	viewport := planPreviewHeight(model.height)

	model.Update(key("j"))
	assert.Equal(t, 1, state.cursor)
	model.Update(key("k"))
	assert.Zero(t, state.cursor)

	model.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	assert.Equal(t, viewport, state.cursor)
	assert.Positive(t, state.offset)

	model.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	assert.Zero(t, state.cursor)
	assert.Zero(t, state.offset)

	model.Update(key("v"))
	require.True(t, state.selecting)
	model.Update(key("j"))
	model.Update(key("j"))
	assert.Contains(t, model.View().Content, "↑/↓ extend")

	model.Update(key("enter"))
	require.True(t, state.commenting)
	assert.Contains(t, model.View().Content, "Comment lines 1-3:")
	state.commentEditor.SetValue("Cover this whole block")
	model.Update(key("enter"))
	require.Len(t, state.comments, 1)
	assert.Equal(t, 1, state.comments[0].first)
	assert.Equal(t, 3, state.comments[0].last)
	assert.False(t, state.selecting)

	model.Update(key("x"))
	assert.Empty(t, state.comments)
	assert.NotContains(t, model.View().Content, "approve w/ comments")
	assert.Contains(t, model.View().Content, "Comment removed.")
}

func TestPlanReviewPromptRequestsChangesWithNotes(t *testing.T) {
	t.Parallel()

	controller := newPlanPromptController(t, planreview.KindExit, tuiPlanContent)
	model := readyModelWithController(t, controller, true)
	state := &model.prompt.planReview

	model.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	require.True(t, state.editing)

	_, command := model.Update(key("enter"))
	assert.Nil(t, command)
	assert.Empty(t, controller.resolutions)
	assert.Contains(t, model.View().Content, "Type revision notes, or press a to approve.")

	state.editor.SetValue("Add migration rollback details.")

	_, command = model.Update(key("enter"))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	require.Len(t, controller.resolutions, 1)
	assert.Equal(t, planreview.DecisionRevise, controller.resolutions[0].Decision)
	assert.Equal(t, "Add migration rollback details.", controller.resolutions[0].Notes)
	assert.Empty(t, controller.resolutions[0].Comments)
}

func TestPlanReviewPromptEscapeAndTabReturnToPreview(t *testing.T) {
	t.Parallel()

	controller := newPlanPromptController(t, planreview.KindExit, tuiPlanContent)
	model := readyModelWithController(t, controller, true)
	state := &model.prompt.planReview

	model.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	require.True(t, state.editing)
	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.False(t, state.editing)

	model.Update(key("c"))
	require.True(t, state.commenting)
	state.commentEditor.SetValue("discard me")
	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.False(t, state.commenting)
	assert.Empty(t, state.comments)

	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Empty(t, controller.resolutions)
	assert.Equal(t, promptPlanReview, model.prompt.kind)
}

func TestPlanReviewPromptQuitsOrCopiesThePlan(t *testing.T) {
	t.Parallel()

	controller := newPlanPromptController(t, planreview.KindExit, tuiPlanContent)
	model := readyModelWithController(t, controller, true)
	state := &model.prompt.planReview

	_, command := model.Update(key("y"))
	require.NotNil(t, command)
	assert.Contains(t, model.View().Content, "Plan copied to clipboard.")
	assert.Empty(t, controller.resolutions)

	_, command = model.Update(key("q"))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	require.Len(t, controller.resolutions, 1)
	assert.Equal(t, planreview.DecisionQuit, controller.resolutions[0].Decision)
	assert.Empty(t, state.comments)
}

func TestPlanReviewPromptExplainsAnEmptyPlan(t *testing.T) {
	t.Parallel()

	controller := newPlanPromptController(t, planreview.KindExit, "")
	model := readyModelWithController(t, controller, true)
	state := &model.prompt.planReview

	view := model.View().Content
	assert.Contains(t, view, "Plan ready for review")
	assert.Contains(t, view, "No plan written yet.")
	assert.Contains(t, view, "The agent exited plan mode without writing a plan.")
	assert.Contains(t, view, "plan.md (empty)")
	assert.Contains(t, view, "leave plan mode and start implementing")
	assert.Contains(t, view, "send the agent back to planning")
	assert.Contains(t, view, "abandon and turn plan mode off")

	model.Update(key("c"))
	assert.False(t, state.commenting)
	assert.Contains(t, model.View().Content, "No plan content to comment on.")

	_, command := model.Update(key("y"))
	assert.Nil(t, command)
	assert.Contains(t, model.View().Content, "No plan content to copy.")

	_, command = model.Update(key("a"))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	require.Len(t, controller.resolutions, 1)
	assert.Equal(t, planreview.DecisionApprove, controller.resolutions[0].Decision)
	assert.Empty(t, controller.resolutions[0].Comments)
}

func TestPlanEnterPromptApprovesAndDeclines(t *testing.T) {
	t.Parallel()

	approveController := newPlanPromptController(t, planreview.KindEnter, "")
	model := readyModelWithController(t, approveController, true)
	view := model.View().Content
	assert.Contains(t, view, "Enter plan mode?")
	assert.Contains(t, view, "Plan mode is read-only except for the plan file.")
	assert.Contains(t, view, "a approve · d decline")

	_, command := model.Update(key("a"))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	require.Len(t, approveController.resolutions, 1)
	assert.Equal(t, planreview.DecisionApprove, approveController.resolutions[0].Decision)
	assert.Empty(t, approveController.resolutions[0].Comments)
	assert.Empty(t, approveController.resolutions[0].Notes)

	declineController := newPlanPromptController(t, planreview.KindEnter, "")
	model = readyModelWithController(t, declineController, true)

	_, command = model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	require.Len(t, declineController.resolutions, 1)
	assert.Equal(t, planreview.DecisionDecline, declineController.resolutions[0].Decision)
}

func TestViewPlanCommandPreviewsTheSavedPlan(t *testing.T) {
	t.Parallel()

	for _, query := range []string{"view-plan", "show-plan", "plan-view"} {
		t.Run(query, func(t *testing.T) {
			t.Parallel()

			const content = "# Saved Plan\n\nThe saved body is read-only."

			controller := &planPromptController{
				stubController: stubController{state: readyState()},
				document: coding.PlanDocument{
					Exists: true, Content: content, Size: int64(len(content)),
				},
			}
			model := readyModelWithController(t, controller, true)

			model.openCommandPicker()
			model.picker.query = query
			_, command := model.Update(key("enter"))
			require.NotNil(t, command)
			driveModelCommands(t, model, command)

			assert.Equal(t, promptPlanView, model.prompt.kind)
			view := model.View().Content
			assert.Contains(t, view, "Saved Plan")
			assert.Contains(t, view, "The saved body is read-only.")
			assert.Contains(t, view, "Esc close")

			_, command = model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
			assert.Equal(t, promptNone, model.prompt.kind)
			assert.Nil(t, command)
		})
	}
}

func TestViewPlanPromptReportsAMissingPlan(t *testing.T) {
	t.Parallel()

	controller := &planPromptController{stubController: stubController{state: readyState()}}
	model := readyModelWithController(t, controller, true)
	driveModelCommands(t, model, model.startPlanView())

	require.Equal(t, promptPlanView, model.prompt.kind)
	assert.Contains(t, model.View().Content, "No plan written yet.")

	_, command := model.Update(key("q"))
	assert.Equal(t, promptNone, model.prompt.kind)
	assert.Nil(t, command)
}

func TestPlanReviewPromptTakesPriorityOverStructuredQuestion(t *testing.T) {
	t.Parallel()

	request, err := planreview.NewRequest(planreview.KindExit, "exit-tui", tuiPlanContent)
	require.NoError(t, err)

	state := planPromptStateSnapshot(request)
	questionRequest := testQuestionRequest(t)
	state.Question.Required = &questionRequest

	controller := &planPromptController{stubController: stubController{state: state}}
	model := readyModelWithController(t, controller, true)

	assert.Equal(t, promptPlanReview, model.prompt.kind)
	assert.Contains(t, model.View().Content, "Plan ready for review")
}

func newPlanPromptController(
	t *testing.T,
	kind planreview.Kind,
	content string,
) *planPromptController {
	t.Helper()

	toolCallID := "exit-tui"
	if kind == planreview.KindEnter {
		toolCallID = "enter-tui"
	}

	request, err := planreview.NewRequest(kind, toolCallID, content)
	require.NoError(t, err)

	return &planPromptController{
		stubController: stubController{state: planPromptStateSnapshot(request)},
	}
}

func planPromptStateSnapshot(request planreview.Request) coding.State {
	state := readyState()
	state.Mode = coding.ModePlan
	state.PlanMode = planmode.StateActive
	state.Phase = coding.PhasePaused
	state.Interaction = coding.InteractionState{
		ID: "interaction-plan", Active: true, Mode: coding.ModePlan,
	}
	state.PlanReview.Required = &request

	return state
}

func longPlanContent(rows int) string {
	var builder strings.Builder
	builder.WriteString("# Long Plan\n\n")

	for index := range rows {
		builder.WriteString("- step ")
		builder.WriteString(strings.Repeat("x", index%7+1))
		builder.WriteString(" line\n")
	}

	return builder.String()
}

type planPromptController struct {
	stubController
	document    coding.PlanDocument
	documentErr error
	resolutions []planreview.Resolution
}

func (c *planPromptController) ReadPlanDocument(context.Context) (coding.PlanDocument, error) {
	if c.documentErr != nil {
		return coding.PlanDocument{}, c.documentErr
	}

	return c.document, nil
}

func (c *planPromptController) ResolvePlanReview(
	_ context.Context,
	resolution planreview.Resolution,
) iter.Seq2[coding.Event, error] {
	c.resolutions = append(c.resolutions, planreview.CloneResolution(resolution))
	c.state.PlanReview.Required = nil
	c.state.Phase = coding.PhaseIdle
	c.state.Interaction.Active = false

	return emptyEventSequence()
}
