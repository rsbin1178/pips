package planflow

import (
	"context"
	"testing"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/planreview"
	"github.com/rsbin/pips/internal/coding/question"
	"github.com/rsbin/pips/internal/coding/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validCheckpoint = `{
  "goal":"Deliver a student management system",
  "success_criteria":["Administrators can manage student records"],
  "audience":["School administrators"],
  "in_scope":["Student record CRUD"],
  "out_of_scope":["Learning management"],
  "constraints":["Full-stack TypeScript"],
  "low_impact_assumptions":["Use repository naming conventions"],
  "unresolved_material_decisions":[]
}`

func TestControllerEnforcesCheckpointWriteSubmitOrder(t *testing.T) {
	t.Parallel()

	controller := newBoundController(t)

	assert.Equal(t, agent.ToolDecisionAllow, controller.BeforeTool(
		t.Context(), toolCall(ToolName, 1, validCheckpoint),
	).Action)
	controller.AfterTool(t.Context(), successfulToolResult(ToolName, 1))

	update := controller.PrepareTurn(t.Context(), agent.RunInfo{})
	require.NotNil(t, update.NextRequest)
	assert.Equal(t, ai.ToolChoice{Mode: ai.ToolChoiceTool, Name: tools.WritePlanName}, update.NextRequest.ToolChoice)
	assert.Equal(t, []string{tools.WritePlanName}, toolNames(update.NextRequest.Tools))

	assert.Equal(t, agent.ToolDecisionAllow, controller.BeforeTool(
		t.Context(), toolCall(tools.WritePlanName, 2, `{}`),
	).Action)
	controller.AfterTool(t.Context(), successfulToolResult(tools.WritePlanName, 2))

	update = controller.PrepareTurn(t.Context(), agent.RunInfo{})
	require.NotNil(t, update.NextRequest)
	assert.Equal(t, ai.ToolChoice{Mode: ai.ToolChoiceTool, Name: planreview.ToolName}, update.NextRequest.ToolChoice)
	assert.Equal(t, []string{planreview.ToolName}, toolNames(update.NextRequest.Tools))

	assert.Equal(t, agent.ToolDecisionAllow, controller.BeforeTool(
		t.Context(), toolCall(planreview.ToolName, 3, `{}`),
	).Action)
}

func TestControllerRejectsIncompleteCheckpointAndInvalidatesSameTurnQuestion(t *testing.T) {
	t.Parallel()

	controller := newBoundController(t)
	invalid := `{
  "goal":"Student system",
  "success_criteria":["Works"],
  "audience":["Unknown users"],
  "in_scope":["Students"],
  "out_of_scope":[],
  "constraints":["TypeScript"],
  "low_impact_assumptions":[],
  "unresolved_material_decisions":["Who may edit grades?"]
}`
	decision := controller.BeforeTool(t.Context(), toolCall(ToolName, 1, invalid))
	assert.Equal(t, agent.ToolDecisionDeny, decision.Action)
	assert.Contains(t, decision.Reason, "call ask_user")

	require.Equal(t, agent.ToolDecisionAllow, controller.BeforeTool(
		t.Context(), toolCall(ToolName, 2, validCheckpoint),
	).Action)
	controller.BeforeTool(t.Context(), toolCall(question.ToolName, 2, `{}`))
	controller.AfterTool(t.Context(), successfulToolResult(ToolName, 2))

	assert.Nil(t, controller.PrepareTurn(t.Context(), agent.RunInfo{}).NextRequest)
	assert.Equal(t, agent.ToolDecisionDeny, controller.BeforeTool(
		t.Context(), toolCall(tools.WritePlanName, 3, `{}`),
	).Action)
}

func TestControllerRejectsBatchedTransitionAndNewUserIntent(t *testing.T) {
	t.Parallel()

	controller := newBoundController(t)
	require.Equal(t, agent.ToolDecisionAllow, controller.BeforeTool(
		t.Context(), toolCall(ToolName, 1, validCheckpoint),
	).Action)
	decision := controller.BeforeTool(t.Context(), toolCall("read", 1, `{}`))
	assert.Equal(t, agent.ToolDecisionDeny, decision.Action)
	assert.Contains(t, decision.Reason, "must be called alone")
	controller.AfterTool(t.Context(), successfulToolResult(ToolName, 1))
	assert.Nil(t, controller.PrepareTurn(t.Context(), agent.RunInfo{}).NextRequest)

	require.Equal(t, agent.ToolDecisionAllow, controller.BeforeTool(
		t.Context(), toolCall(ToolName, 2, validCheckpoint),
	).Action)
	controller.AfterTool(t.Context(), successfulToolResult(ToolName, 2))
	require.Equal(t, agent.ToolDecisionAllow, controller.BeforeTool(
		t.Context(), toolCall(tools.WritePlanName, 3, `{}`),
	).Action)

	controller.InvalidateUserInput()
	controller.AfterTool(t.Context(), successfulToolResult(tools.WritePlanName, 3))
	assert.Nil(t, controller.PrepareTurn(t.Context(), agent.RunInfo{}).NextRequest)
	retry := controller.CandidateAnswer(t.Context(), agent.CandidateAnswerInfo{})
	require.NotNil(t, retry.Retry)
	assert.Equal(t, ai.ToolChoice{Mode: ai.ToolChoiceRequired}, retry.Retry.ToolChoice)
}

func TestControllerRetriesIncompleteCandidateWithBoundedRequiredChoice(t *testing.T) {
	t.Parallel()

	controller := newBoundController(t)
	for range maxCandidateRetries {
		decision := controller.CandidateAnswer(t.Context(), agent.CandidateAnswerInfo{})
		require.NoError(t, decision.Err)
		require.NotNil(t, decision.Retry)
		assert.Equal(t, ai.ToolChoice{Mode: ai.ToolChoiceRequired}, decision.Retry.ToolChoice)
		assert.Equal(t, []string{question.ToolName, ToolName}, toolNames(decision.Retry.Tools))
	}

	decision := controller.CandidateAnswer(t.Context(), agent.CandidateAnswerInfo{})
	require.ErrorIs(t, decision.Err, ErrProtocol)
	assert.Nil(t, decision.Retry)

	controller.ResolvePlanReview(planreview.DecisionApprove)
	assert.Equal(t, agent.CandidateAnswerDecision{}, controller.CandidateAnswer(
		t.Context(), agent.CandidateAnswerInfo{},
	))
}

func newBoundController(t *testing.T) *Controller {
	t.Helper()

	controller, err := NewController()
	require.NoError(t, err)

	values := []agent.Tool{
		namedTool(question.ToolName),
		controller.checkpoint,
		namedTool(tools.WritePlanName),
		namedTool(planreview.ToolName),
	}
	require.NoError(t, controller.BindTools(values))

	return controller
}

func namedTool(name string) agent.Tool {
	return agent.NewTool(name, name, func(context.Context, struct{}) (string, error) {
		return "ok", nil
	})
}

func toolCall(name string, turn int, arguments string) agent.ToolCallInfo {
	return agent.ToolCallInfo{
		ToolCall: agent.ToolCall{ID: name + "-call", Name: name, Args: ai.JSON(arguments)},
		Turn:     turn,
	}
}

func successfulToolResult(name string, turn int) agent.ToolResultInfo {
	return agent.ToolResultInfo{
		ToolCall: agent.ToolCall{ID: name + "-call", Name: name},
		Turn:     turn,
		Result:   ai.ToolResultPart{ToolCallID: name + "-call"},
	}
}

func toolNames(values []agent.Tool) []string {
	result := make([]string, len(values))
	for index, tool := range values {
		result[index] = tool.Decl().Name
	}

	return result
}
