package tui

import (
	"context"
	"testing"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/question"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCustomChildApprovalPromptResolvesOnlyTargetChild(t *testing.T) {
	t.Parallel()

	controller := newSubagentInteractionController()
	controller.controls["child-approval"] = coding.ChildControlState{
		ChildSessionID: "child-approval",
		Pause:          coding.ChildPauseApproval,
		Approval: approval.State{
			Kind: approval.StateReview,
			Review: &approval.Review{
				RequestID: "approval-request",
				Call:      agent.ToolCall{ID: "call-1", Name: "shell"},
			},
		},
	}
	model := readyModelWithController(t, controller, true)
	model.route = routeState{kind: routeChild, childSessionID: "child-approval"}

	driveModelCommands(t, model, model.observeSubagentControl(coding.Event{
		SessionID: "child-approval",
		Payload:   coding.StatusChanged{Phase: coding.PhasePaused},
	}))

	require.Equal(t, promptApproval, model.prompt.kind)
	require.NotNil(t, model.prompt.subagent)
	assert.Equal(t, "child-approval", model.prompt.subagent.childSessionID)
	assert.Equal(t, routeNone, model.route.kind)
	assert.Contains(t, model.View().Content, "Subagent · child Agent")

	_, command := model.Update(key("o"))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	require.Len(t, controller.approvals, 1)
	assert.Equal(t, "child-approval", controller.approvals[0].childSessionID)
	assert.Equal(t, "approval-request", controller.approvals[0].resolution.RequestID)
	assert.Equal(t, approval.ChoiceAllowOnce, controller.approvals[0].resolution.Choice)
	assert.Equal(t, promptNone, model.prompt.kind)
}

func TestCustomChildQuestionPromptResolvesOnlyTargetChild(t *testing.T) {
	t.Parallel()

	request := testQuestionRequest(t)
	controller := newSubagentInteractionController()
	controller.controls["child-question"] = coding.ChildControlState{
		ChildSessionID: "child-question",
		Pause:          coding.ChildPauseQuestion,
		Question:       &request,
	}
	model := readyModelWithController(t, controller, true)

	driveModelCommands(t, model, model.observeSubagentControl(coding.Event{
		SessionID: "child-question",
		Payload:   coding.StatusChanged{Phase: coding.PhasePaused},
	}))

	require.Equal(t, promptQuestion, model.prompt.kind)
	require.NotNil(t, model.prompt.subagent)
	assert.Equal(t, "child-question", model.prompt.subagent.childSessionID)
	assert.Contains(t, model.View().Content, "Subagent · child Agent")

	model.Update(key("enter"))
	_, command := model.Update(key("enter"))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	require.Len(t, controller.questions, 1)
	assert.Equal(t, "child-question", controller.questions[0].childSessionID)
	assert.Equal(t, request.ID, controller.questions[0].resolution.RequestID)
	assert.Equal(t, []string{"React"}, controller.questions[0].resolution.Answers[0].Selections)
	assert.Equal(t, promptNone, model.prompt.kind)
}

type subagentInteractionController struct {
	stubController
	controls  map[string]coding.ChildControlState
	approvals []subagentApprovalCall
	questions []subagentQuestionCall
}

type subagentApprovalCall struct {
	childSessionID string
	resolution     approval.Resolution
}

type subagentQuestionCall struct {
	childSessionID string
	resolution     question.Resolution
}

func newSubagentInteractionController() *subagentInteractionController {
	return &subagentInteractionController{
		stubController: stubController{state: readyState()},
		controls:       make(map[string]coding.ChildControlState),
	}
}

func (c *subagentInteractionController) SubagentControlState(
	_ context.Context,
	childSessionID string,
) (coding.ChildControlState, error) {
	return c.controls[childSessionID].Clone(), nil
}

func (c *subagentInteractionController) ResolveSubagentApproval(
	_ context.Context,
	childSessionID string,
	resolution approval.Resolution,
) (coding.ChildControlState, error) {
	c.approvals = append(c.approvals, subagentApprovalCall{
		childSessionID: childSessionID, resolution: resolution,
	})
	delete(c.controls, childSessionID)

	return coding.ChildControlState{ChildSessionID: childSessionID}, nil
}

func (c *subagentInteractionController) ResolveSubagentQuestion(
	_ context.Context,
	childSessionID string,
	resolution question.Resolution,
) (coding.ChildControlState, error) {
	c.questions = append(c.questions, subagentQuestionCall{
		childSessionID: childSessionID, resolution: question.CloneResolution(resolution),
	})
	delete(c.controls, childSessionID)

	return coding.ChildControlState{ChildSessionID: childSessionID}, nil
}

func (c *subagentInteractionController) RejectSubagentQuestion(
	_ context.Context,
	childSessionID string,
	_ string,
	_ string,
) (coding.ChildControlState, error) {
	delete(c.controls, childSessionID)

	return coding.ChildControlState{ChildSessionID: childSessionID}, nil
}
