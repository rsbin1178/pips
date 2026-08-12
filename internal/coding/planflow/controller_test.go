package planflow

import (
	"context"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/planreview"
	"github.com/rsbin1178/pips/internal/coding/question"
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

func TestControllerEnforcesCheckpointPresentOrder(t *testing.T) {
	t.Parallel()

	controller := newBoundController(t)

	assert.Equal(t, agent.ToolDecisionAllow, controller.BeforeTool(
		t.Context(), toolCall(ToolName, 1, validCheckpoint),
	).Action)
	controller.AfterTool(t.Context(), successfulToolResult(ToolName, 1))

	update := controller.PrepareTurn(t.Context(), agent.RunInfo{})
	require.NotNil(t, update.NextRequest)
	assert.Equal(t, ai.ToolChoice{Mode: ai.ToolChoiceTool, Name: planreview.PresentToolName}, update.NextRequest.ToolChoice)
	assert.Equal(t, []string{planreview.PresentToolName}, toolNames(update.NextRequest.Tools))

	assert.Equal(t, agent.ToolDecisionAllow, controller.BeforeTool(
		t.Context(), toolCall(planreview.PresentToolName, 2, `{"expected_revision":"","content":"# Plan"}`),
	).Action)
	assert.Nil(t, controller.PrepareTurn(t.Context(), agent.RunInfo{}).NextRequest)
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

	malformed := controller.BeforeTool(t.Context(), toolCall(question.ToolName, 2, `{}`))
	assert.Equal(t, agent.ToolDecisionDeny, malformed.Action)
	assert.Contains(t, malformed.Reason, question.TextToolName)

	blocked := controller.BeforeTool(t.Context(), toolCall(ToolName, 3, validCheckpoint))
	assert.Equal(t, agent.ToolDecisionDeny, blocked.Action)
	assert.Contains(t, blocked.Reason, "until the pending user question is answered")
	recovery := controller.PrepareTurn(t.Context(), agent.RunInfo{})
	require.NotNil(t, recovery.NextRequest)
	assert.Equal(t, ai.ToolChoice{Mode: ai.ToolChoiceTool, Name: question.TextToolName}, recovery.NextRequest.ToolChoice)

	controller.ResolveQuestion()
	require.Equal(t, agent.ToolDecisionAllow, controller.BeforeTool(
		t.Context(), toolCall(ToolName, 4, validCheckpoint),
	).Action)
}

func TestControllerRejectsBatchedTransitionAndNewUserIntent(t *testing.T) {
	t.Parallel()

	controller := newBoundController(t)
	call := toolCall(ToolName, 1, validCheckpoint)
	call.BatchSize = 2
	decision := controller.BeforeTool(t.Context(), call)
	assert.Equal(t, agent.ToolDecisionDeny, decision.Action)
	assert.Contains(t, decision.Reason, "must be called alone")
	assert.Nil(t, controller.PrepareTurn(t.Context(), agent.RunInfo{}).NextRequest)

	require.Equal(t, agent.ToolDecisionAllow, controller.BeforeTool(
		t.Context(), toolCall(ToolName, 2, validCheckpoint),
	).Action)
	controller.AfterTool(t.Context(), successfulToolResult(ToolName, 2))
	require.Equal(t, agent.ToolDecisionAllow, controller.BeforeTool(
		t.Context(), toolCall(planreview.PresentToolName, 3, `{"expected_revision":"","content":"# Plan"}`),
	).Action)

	controller.InvalidateUserInput()
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
		namedTool(question.TextToolName),
		controller.checkpoint,
		namedTool(planreview.PresentToolName),
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
