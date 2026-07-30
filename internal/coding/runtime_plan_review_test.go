//nolint:wsl_v5 // State-machine fixtures keep events beside ordering assertions.
package coding

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/planflow"
	"github.com/rsbin/pips/internal/coding/planreview"
	"github.com/rsbin/pips/internal/coding/question"
	"github.com/rsbin/pips/internal/coding/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeRecoversTextOnlyCandidateIntoPlanDocumentAndReview(t *testing.T) {
	t.Parallel()

	const content = "# Student Management System Plan\n\n1. Confirm scope\n2. Implement\n3. Verify"
	revision := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	model := newRuntimeModel(
		runtimeTextResponse("Here is a generic plan that skipped the Plan tools."),
		runtimeToolResponse("checkpoint-after-retry", planflow.ToolName, checkpointFixture),
		runtimeToolResponse(
			"write-after-checkpoint",
			tools.WritePlanName,
			fmt.Sprintf(`{"expected_revision":"","content":%q}`, content),
		),
		runtimeToolResponse("submit-after-write", planreview.ToolName, submitPlanArgs(revision)),
	)
	runtime := openTestRuntimeAt(t, t.TempDir(), SessionTarget{}, model)
	require.NoError(t, runtime.SetMode(t.Context(), ModePlan))

	events := collectRuntimeEvents(
		t,
		runtime.Prompt(t.Context(), ai.UserText("Plan a full-stack TypeScript student system")),
	)
	assert.Equal(t, 1, countEventType(events, EventMessageDiscarded))
	assert.Equal(t, 1, countEventType(events, EventPlanReviewRequired))
	assert.Equal(t, PhasePaused, runtime.Snapshot().Phase)
	assert.NotContains(t, transcriptText(runtime.Snapshot().Transcript), "generic plan")

	document, err := runtime.plans.Read(t.Context(), runtime.planRef)
	require.NoError(t, err)
	assert.Equal(t, revision, document.Revision)
	assert.Equal(t, content, document.Content)

	requests := model.Requests()
	require.Len(t, requests, 4)
	assert.Equal(t, ai.ToolChoice{Mode: ai.ToolChoiceRequired}, requests[1].ToolChoice)
	assert.Equal(t, []string{question.ToolName, planflow.ToolName}, toolNamesFromRequest(requests[1]))
	assert.Equal(t, ai.ToolChoice{Mode: ai.ToolChoiceTool, Name: tools.WritePlanName}, requests[2].ToolChoice)
	assert.Equal(t, []string{tools.WritePlanName}, toolNamesFromRequest(requests[2]))
	assert.Equal(t, ai.ToolChoice{Mode: ai.ToolChoiceTool, Name: planreview.ToolName}, requests[3].ToolChoice)
	assert.Equal(t, []string{planreview.ToolName}, toolNamesFromRequest(requests[3]))
}

func TestRuntimePlanReviewContinueKeepsPlanMode(t *testing.T) {
	t.Parallel()

	runtime, model, revision := openPlanReviewRuntime(t)
	setPlanReviewResponses(model, revision,
		runtimeToolResponse("submit-continue", planreview.ToolName, submitPlanArgs(revision)),
		runtimeQuestionResponse(t, "question-after-continue"),
	)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("finish the Plan")))
	assert.Contains(t, eventTypes(events), EventPlanReviewRequired)
	paused := runtime.Snapshot()
	require.Equal(t, PhasePaused, paused.Phase)
	require.NotNil(t, paused.PlanReview.Required)

	request := *paused.PlanReview.Required
	events = collectRuntimeEvents(t, runtime.ResolvePlanReview(t.Context(), planreview.Resolution{
		RequestID: request.ID,
		Revision:  request.Revision,
		Decision:  planreview.DecisionContinue,
		Feedback:  "Add a rollback section.",
	}))
	assert.Contains(t, eventTypes(events), EventPlanReviewResolved)
	assert.NotContains(t, eventTypes(events), EventModeChanged)

	state := runtime.Snapshot()
	assert.Equal(t, PhasePaused, state.Phase)
	assert.Equal(t, ModePlan, state.Mode)
	assert.Nil(t, state.PlanReview.Required)
	assert.NotNil(t, state.Question.Required)
}

func TestRuntimePlanReviewApprovalSwitchesOnlyAtIdleAndFreezesSuffix(t *testing.T) {
	t.Parallel()

	runtime, model, revision := openPlanReviewRuntime(t)
	setPlanReviewResponses(model, revision,
		&ai.Response{
			Provider: ai.ProviderOpenAI,
			Model:    "runtime-test",
			Message: ai.Assistant(
				ai.ToolCallPart{
					ID: "submit-approve", Name: planreview.ToolName,
					Args: ai.JSON(submitPlanArgs(revision)),
				},
				ai.ToolCallPart{
					ID: "late-write", Name: tools.WritePlanName,
					Args: ai.JSON(`{"expected_revision":"` + revision + `","content":"# Changed after approval"}`),
				},
			),
			FinishReason: ai.FinishToolCalls,
			Usage:        ai.Usage{InputTokens: 10, OutputTokens: 2},
		},
		runtimeTextResponse("Plan review complete."),
	)

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("submit the Plan")))
	request := *runtime.Snapshot().PlanReview.Required
	events := collectRuntimeEvents(t, runtime.ResolvePlanReview(t.Context(), planreview.Resolution{
		RequestID: request.ID,
		Revision:  request.Revision,
		Decision:  planreview.DecisionApprove,
	}))

	types := eventTypes(events)
	completed := slices.Index(types, EventInteractionCompleted)
	idle := -1
	for index, event := range events {
		if status, ok := event.Payload.(StatusChanged); ok && status.Phase == PhaseIdle {
			idle = index
			break
		}
	}
	modeChanged := slices.Index(types, EventModeChanged)
	require.GreaterOrEqual(t, completed, 0)
	require.Greater(t, idle, completed)
	require.Greater(t, modeChanged, idle)

	state := runtime.Snapshot()
	assert.Equal(t, PhaseIdle, state.Phase)
	assert.Equal(t, ModeAgent, state.Mode)
	document, err := runtime.plans.Read(t.Context(), runtime.planRef)
	require.NoError(t, err)
	assert.Equal(t, revision, document.Revision)
	assert.NotContains(t, document.Content, "Changed after approval")
	assert.Contains(t, transcriptText(state.Transcript), "approved")
}

func TestRuntimePlanReviewApprovalFailsClosedWhenRevisionChanges(t *testing.T) {
	t.Parallel()

	runtime, model, revision := openPlanReviewRuntime(t)
	setPlanReviewResponses(model, revision,
		runtimeToolResponse("submit-stale", planreview.ToolName, submitPlanArgs(revision)),
		runtimeTextResponse("Plan review complete."),
	)

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("submit the Plan")))
	request := *runtime.Snapshot().PlanReview.Required
	_, err := runtime.plans.Replace(t.Context(), runtime.planRef, revision, "# Revised elsewhere")
	require.NoError(t, err)
	events := collectRuntimeEvents(t, runtime.ResolvePlanReview(t.Context(), planreview.Resolution{
		RequestID: request.ID,
		Revision:  request.Revision,
		Decision:  planreview.DecisionApprove,
	}))

	assert.NotContains(t, eventTypes(events), EventModeChanged)
	assert.Equal(t, ModePlan, runtime.Snapshot().Mode)
	assert.Equal(t, 1, countDiagnostic(events, "plan_review", "accepted_revision_invalidated"))
}

func TestRuntimeDiscardsPseudoToolMarkupAndRequiresNativePlanTransition(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeTextResponse(
			`<function_calls><submit_plan expected_revision="forged" /></function_calls>`,
		),
		runtimeQuestionResponse(t, "native-question"),
	)
	runtime := openTestRuntimeAt(t, t.TempDir(), SessionTarget{}, model)
	require.NoError(t, runtime.SetMode(t.Context(), ModePlan))

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("make a Plan")))
	assert.Equal(t, 1, countEventType(events, EventMessageDiscarded))
	assert.Zero(t, countEventType(events, EventToolStarted), "paused calls do not execute")
	assert.Equal(t, 1, countEventType(events, EventQuestionRequired))
	assert.Zero(t, countEventType(events, EventPlanReviewRequired))
	assert.Equal(t, PhasePaused, runtime.Snapshot().Phase)
	assert.NotNil(t, runtime.Snapshot().Question.Required)
	assert.NotContains(t, transcriptText(runtime.Snapshot().Transcript), "<function_calls>")

	requests := model.Requests()
	require.Len(t, requests, 2)
	assert.Equal(t, ai.ToolChoice{Mode: ai.ToolChoiceRequired}, requests[1].ToolChoice)
	assert.Equal(t, []string{question.ToolName, planflow.ToolName}, toolNamesFromRequest(requests[1]))
}

func TestRuntimePlanReviewSurvivesRestartWithoutModelReplay(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	firstModel := newRuntimeModel()
	first := openTestRuntimeAt(t, base, SessionTarget{}, firstModel)
	require.NoError(t, first.SetMode(t.Context(), ModePlan))
	document, err := first.plans.Replace(t.Context(), first.planRef, "", "# Restart-safe Plan")
	require.NoError(t, err)
	setPlanReviewResponsesForContent(firstModel, document.Revision, document.Content, runtimeToolResponse(
		"submit-restart", planreview.ToolName, submitPlanArgs(document.Revision),
	))
	collectRuntimeEvents(t, first.Prompt(t.Context(), ai.UserText("submit")))
	before := *first.Snapshot().PlanReview.Required
	sessionID := first.handle.Metadata().ID
	abruptRuntimeStop(t, first)

	model := newRuntimeModel(runtimeQuestionResponse(t, "continued-after-review"))
	second := openTestRuntimeAt(t, base, SessionTarget{ID: sessionID}, model)
	// This fixture reopens with the same process-level Plan selection. The
	// generic test helper otherwise defaults every new Runtime to Agent Mode.
	second.mu.Lock()
	second.config.Mode = ModePlan
	second.state.Mode = ModePlan
	second.mu.Unlock()

	events := collectRuntimeEvents(t, second.Continue(t.Context()))
	assert.Contains(t, eventTypes(events), EventPlanReviewRequired)
	after := second.Snapshot().PlanReview.Required
	require.NotNil(t, after)
	assert.Equal(t, before, *after)
	assert.Empty(t, model.Requests(), "reconciliation must not replay model work")

	collectRuntimeEvents(t, second.ResolvePlanReview(t.Context(), planreview.Resolution{
		RequestID: after.ID,
		Revision:  after.Revision,
		Decision:  planreview.DecisionContinue,
	}))
	assert.Len(t, model.Requests(), 1)
	assert.Equal(t, ModePlan, second.Snapshot().Mode)
}

func openPlanReviewRuntime(t *testing.T) (*Runtime, *runtimeModel, string) {
	t.Helper()
	model := newRuntimeModel()
	runtime := openTestRuntimeAt(t, t.TempDir(), SessionTarget{}, model)
	require.NoError(t, runtime.SetMode(t.Context(), ModePlan))
	document, err := runtime.plans.Replace(t.Context(), runtime.planRef, "", planReviewFixture)
	require.NoError(t, err)

	return runtime, model, document.Revision
}

const planReviewFixture = "# Implementation Plan\n\n1. Inspect\n2. Implement\n3. Verify"

const checkpointFixture = `{
  "goal":"Deliver the requested implementation",
  "success_criteria":["The scoped behavior is implemented and verified"],
  "audience":["The requesting developer"],
  "in_scope":["The requested implementation"],
  "out_of_scope":["Unrequested product changes"],
  "constraints":["Follow repository conventions"],
  "low_impact_assumptions":[],
  "unresolved_material_decisions":[]
}`

func setPlanReviewResponses(
	model *runtimeModel,
	revision string,
	responses ...*ai.Response,
) {
	setPlanReviewResponsesForContent(model, revision, planReviewFixture, responses...)
}

func setPlanReviewResponsesForContent(
	model *runtimeModel,
	revision string,
	content string,
	responses ...*ai.Response,
) {
	prefix := []*ai.Response{
		runtimeToolResponse("checkpoint", planflow.ToolName, checkpointFixture),
		runtimeToolResponse(
			"write-plan",
			tools.WritePlanName,
			fmt.Sprintf(`{"expected_revision":%q,"content":%q}`, revision, content),
		),
	}
	setRuntimeResponses(model, append(prefix, responses...)...)
}

func setRuntimeResponses(model *runtimeModel, responses ...*ai.Response) {
	model.mu.Lock()
	model.responses = append(model.responses, responses...)
	model.mu.Unlock()
}

func submitPlanArgs(revision string) string {
	return fmt.Sprintf(`{"expected_revision":%q}`, revision)
}
