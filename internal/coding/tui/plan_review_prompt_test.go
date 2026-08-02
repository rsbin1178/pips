package tui

import (
	"context"
	"crypto/sha256"
	"fmt"
	"iter"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/planreview"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const tuiPlanRevision = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

func TestPlanReviewPromptLoadsExactDocumentAndConfirmsApproval(t *testing.T) {
	t.Parallel()

	request := testPlanReviewRequest(t)
	controller := &planPromptController{
		stubController: stubController{state: planPromptStateSnapshot(request)},
		document: coding.PlanDocument{
			Revision: request.Revision,
			Content:  "# Plan\n\n" + strings.Repeat("A review line\n", 20),
			Size:     request.Size,
		},
	}
	model := readyModelWithController(t, controller, true)
	require.Equal(t, promptPlanReview, model.prompt.kind)
	model.prompt.planReview.loadStarted = false
	driveModelCommands(t, model, model.loadPlanReviewIfNeeded())

	view := model.View().Content
	assert.Contains(t, view, "Plan ready for review")
	assert.Contains(t, view, "Approve & switch to Agent Mode")
	assert.Contains(t, view, "Keep planning")
	assert.NotContains(t, view, tuiPlanRevision)
	assert.Nil(t, model.View().Cursor)

	model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	assert.Positive(t, model.prompt.planReview.offset)
	_, command := model.Update(key("enter"))
	assert.Nil(t, command)
	assert.True(t, model.prompt.planReview.confirming)
	_, command = model.Update(key("enter"))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	require.Len(t, controller.resolutions, 1)
	assert.Equal(t, planreview.DecisionApprove, controller.resolutions[0].Decision)
	assert.Equal(t, promptNone, model.prompt.kind)
}

func TestPlanReviewPromptUsesPresentedContentWithoutSecondRead(t *testing.T) {
	t.Parallel()

	const content = "# Atomic Plan\n\nThe full proposal is already bound to this review."

	revision := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	request, err := planreview.NewProposal("present-tui", revision, content)
	require.NoError(t, err)

	controller := &planPromptController{
		stubController: stubController{state: planPromptStateSnapshot(request)},
	}
	model := readyModelWithController(t, controller, true)

	assert.False(t, model.prompt.planReview.loading)
	assert.Nil(t, model.loadPlanReviewIfNeeded())
	assert.Contains(t, model.View().Content, "Atomic Plan")
	assert.Contains(t, model.View().Content, "full proposal")
}

func TestPlanReviewPromptKeepsPlanningOnEscapeOrWithFeedback(t *testing.T) {
	t.Parallel()

	request := testPlanReviewRequest(t)
	newController := func() *planPromptController {
		return &planPromptController{
			stubController: stubController{state: planPromptStateSnapshot(request)},
			document: coding.PlanDocument{
				Revision: request.Revision, Content: "# Plan", Size: request.Size,
			},
		}
	}

	controller := newController()
	model := readyModelWithController(t, controller, true)
	model.prompt.planReview.loadStarted = false
	driveModelCommands(t, model, model.loadPlanReviewIfNeeded())
	_, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	driveModelCommands(t, model, command)
	require.Len(t, controller.resolutions, 1)
	assert.Equal(t, planreview.DecisionContinue, controller.resolutions[0].Decision)
	assert.Empty(t, controller.resolutions[0].Feedback)

	controller = newController()
	model = readyModelWithController(t, controller, true)
	model.prompt.planReview.loadStarted = false
	driveModelCommands(t, model, model.loadPlanReviewIfNeeded())
	model.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	model.Update(key("enter"))
	require.True(t, model.prompt.planReview.editing)
	model.prompt.planReview.editor.SetValue("Add migration rollback details.")
	_, command = model.Update(key("enter"))
	driveModelCommands(t, model, command)
	require.Len(t, controller.resolutions, 1)
	assert.Equal(t, "Add migration rollback details.", controller.resolutions[0].Feedback)
}

func TestPlanReviewPromptBlocksApprovalWhenExactDocumentCannotLoad(t *testing.T) {
	t.Parallel()

	request := testPlanReviewRequest(t)
	controller := &planPromptController{
		stubController: stubController{state: planPromptStateSnapshot(request)},
		document: coding.PlanDocument{
			Revision: strings.Repeat("0", len(request.Revision)), Content: "# Stale", Size: request.Size,
		},
	}
	model := readyModelWithController(t, controller, true)
	model.prompt.planReview.loadStarted = false
	driveModelCommands(t, model, model.loadPlanReviewIfNeeded())

	view := model.View().Content
	assert.Contains(t, view, "Unable to review plan.md")
	assert.Contains(t, view, "R retry")

	_, command := model.Update(key("enter"))
	assert.Nil(t, command)
	assert.Empty(t, controller.resolutions)

	_, command = model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotNil(t, command)
	driveModelCommands(t, model, command)
	require.Len(t, controller.resolutions, 1)
	assert.Equal(t, planreview.DecisionContinue, controller.resolutions[0].Decision)
}

func testPlanReviewRequest(t *testing.T) planreview.Request {
	t.Helper()

	request, err := planreview.NewRequest("submit-tui", tuiPlanRevision, 512)
	require.NoError(t, err)

	return request
}

func planPromptStateSnapshot(request planreview.Request) coding.State {
	state := readyState()
	state.Mode = coding.ModePlan
	state.Phase = coding.PhasePaused
	state.Interaction = coding.InteractionState{
		ID: "interaction-plan", Active: true, Mode: coding.ModePlan,
	}
	state.PlanReview.Required = &request

	return state
}

type planPromptController struct {
	stubController
	document    coding.PlanDocument
	resolutions []planreview.Resolution
}

func (c *planPromptController) ReadPlanDocument(
	_ context.Context,
	revision string,
) (coding.PlanDocument, error) {
	if revision != c.document.Revision {
		return coding.PlanDocument{}, planreview.ErrMismatch
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

	if resolution.Decision == planreview.DecisionApprove {
		c.state.Mode = coding.ModeAgent
	}

	return emptyEventSequence()
}
