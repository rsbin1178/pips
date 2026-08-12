//nolint:wsl_v5 // State-machine fixtures keep events beside ordering assertions.
package coding

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/planflow"
	"github.com/rsbin1178/pips/internal/coding/planreview"
	"github.com/rsbin1178/pips/internal/coding/question"
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
			"present-after-checkpoint",
			planreview.PresentToolName,
			fmt.Sprintf(`{"expected_revision":"","content":%q}`, content),
		),
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
	require.Len(t, requests, 3)
	assert.Equal(t, ai.ToolChoice{Mode: ai.ToolChoiceRequired}, requests[1].ToolChoice)
	assert.Equal(t, []string{question.ToolName, planflow.ToolName}, toolNamesFromRequest(requests[1]))
	assert.Equal(t, ai.ToolChoice{Mode: ai.ToolChoiceTool, Name: planreview.PresentToolName}, requests[2].ToolChoice)
	assert.Equal(t, []string{planreview.PresentToolName}, toolNamesFromRequest(requests[2]))
}

func TestRuntimePlanReviewContinueKeepsPlanMode(t *testing.T) {
	t.Parallel()

	runtime, model, revision := openPlanReviewRuntime(t)
	setPlanReviewResponses(model, revision,
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
	setPlanReviewResponses(model, revision)

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("submit the Plan")))
	requestsBeforeApproval := len(model.Requests())
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
	assert.Len(t, model.Requests(), requestsBeforeApproval, "approval must not call the model again")
}

func TestRuntimePlanReviewApprovalFailsClosedWhenRevisionChanges(t *testing.T) {
	t.Parallel()

	runtime, model, revision := openPlanReviewRuntime(t)
	setPlanReviewResponses(model, revision)

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

func TestRuntimeMalformedQuestionFallsBackToGenericFreeformAndEventuallyPresents(t *testing.T) {
	t.Parallel()

	const content = "# SSO Plan\n\nUse the school identity provider and verify role mapping."
	model := newRuntimeModel(
		runtimeToolResponse("bad-structured", question.ToolName, `{"questions":[]}`),
		runtimeToolResponse("bad-flat", question.TextToolName, `{"question":123}`),
		runtimeToolResponse("checkpoint-after-answer", planflow.ToolName, checkpointFixture),
		runtimeToolResponse(
			"present-after-answer",
			planreview.PresentToolName,
			fmt.Sprintf(`{"expected_revision":"","content":%q}`, content),
		),
	)
	runtime := openTestRuntimeAt(t, t.TempDir(), SessionTarget{}, model)
	require.NoError(t, runtime.SetMode(t.Context(), ModePlan))

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("Plan the system")))
	assert.Equal(t, 1, countEventType(events, EventQuestionRequired))
	paused := runtime.Snapshot()
	require.NotNil(t, paused.Question.Required)
	request := question.CloneRequest(*paused.Question.Required)
	assert.Equal(t, question.RequestFreeform, request.Kind)
	assert.Contains(t, request.Prompt, "could not form a valid structured question")
	assert.NotContains(t, request.Prompt, `{"question":123}`)

	requests := model.Requests()
	require.Len(t, requests, 2)
	assert.Equal(t, ai.ToolChoice{Mode: ai.ToolChoiceTool, Name: question.TextToolName}, requests[1].ToolChoice)
	assert.Equal(t, []string{question.TextToolName}, toolNamesFromRequest(requests[1]))

	events = collectRuntimeEvents(t, runtime.ResolveQuestion(t.Context(), question.Resolution{
		RequestID: request.ID, SchemaDigest: request.SchemaDigest,
		Chat: "Use the school SSO identity provider.",
	}))
	assert.Contains(t, eventTypes(events), EventPlanReviewRequired)
	state := runtime.Snapshot()
	require.NotNil(t, state.PlanReview.Required)
	assert.Equal(t, content, state.PlanReview.Required.Content)
}

func TestRuntimeGenericFreeformFallbackSurvivesRestart(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	firstModel := newRuntimeModel(
		runtimeToolResponse("bad-structured", question.ToolName, `{"questions":[]}`),
		runtimeToolResponse("bad-flat", question.TextToolName, `{"question":123}`),
	)
	first := openTestRuntimeAt(t, base, SessionTarget{}, firstModel)
	require.NoError(t, first.SetMode(t.Context(), ModePlan))
	collectRuntimeEvents(t, first.Prompt(t.Context(), ai.UserText("Plan the system")))
	require.NotNil(t, first.Snapshot().Question.Required)
	before := question.CloneRequest(*first.Snapshot().Question.Required)
	sessionID := first.handle.Metadata().ID
	abruptRuntimeStop(t, first)

	secondModel := newRuntimeModel()
	second := openTestRuntimeAt(t, base, SessionTarget{ID: sessionID}, secondModel)
	second.mu.Lock()
	second.config.Mode = ModePlan
	second.state.Mode = ModePlan
	second.mu.Unlock()
	events := collectRuntimeEvents(t, second.Continue(t.Context()))
	assert.Contains(t, eventTypes(events), EventQuestionRequired)
	require.NotNil(t, second.Snapshot().Question.Required)
	assert.Equal(t, before, *second.Snapshot().Question.Required)
	assert.Empty(t, secondModel.Requests())
}

func TestRuntimePlanQuestionCancellationStopsIncompleteWithoutModelResume(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(runtimeQuestionResponse(t, "plan-question"))
	runtime := openTestRuntimeAt(t, t.TempDir(), SessionTarget{}, model)
	require.NoError(t, runtime.SetMode(t.Context(), ModePlan))
	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("Plan the system")))
	request := question.CloneRequest(*runtime.Snapshot().Question.Required)
	requestsBeforeCancel := len(model.Requests())

	events := collectRuntimeEvents(t, runtime.RejectQuestion(
		t.Context(), request.ID, request.SchemaDigest,
	))
	assert.Contains(t, eventTypes(events), EventQuestionRejected)
	state := runtime.Snapshot()
	assert.Equal(t, PhaseIdle, state.Phase)
	assert.Equal(t, ModePlan, state.Mode)
	assert.Equal(t, InteractionIncomplete, state.Interaction.Outcome)
	assert.Equal(t, agent.StopWhen, state.Interaction.Stop)
	assert.Len(t, model.Requests(), requestsBeforeCancel)
}

func TestRuntimePlanQuestionBypassHasFiniteTurnFuse(t *testing.T) {
	t.Parallel()

	responses := make([]*ai.Response, 0, planInteractionMaxTurns)
	responses = append(responses, runtimeToolResponse("bad-structured", question.ToolName, `{"questions":[]}`))
	for index := 1; index < planInteractionMaxTurns; index++ {
		arguments := strings.Replace(
			checkpointFixture,
			"Deliver the requested implementation",
			fmt.Sprintf("Deliver the requested implementation attempt %d", index),
			1,
		)
		responses = append(responses, runtimeToolResponse(
			fmt.Sprintf("bypass-%d", index),
			planflow.ToolName,
			arguments,
		))
	}

	model := newRuntimeModel(responses...)
	runtime := openTestRuntimeAt(t, t.TempDir(), SessionTarget{}, model)
	require.NoError(t, runtime.SetMode(t.Context(), ModePlan))

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("Plan without answering")))
	state := runtime.Snapshot()
	assert.Equal(t, PhaseIdle, state.Phase)
	assert.Equal(t, InteractionIncomplete, state.Interaction.Outcome)
	assert.Equal(t, agent.StopMaxTurns, state.Interaction.Stop)
	assert.Len(t, model.Requests(), planInteractionMaxTurns)
	assert.Zero(t, countEventType(events, EventPlanReviewRequired))
}

func TestRuntimePlanReviewSurvivesRestartWithoutModelReplay(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	firstModel := newRuntimeModel()
	first := openTestRuntimeAt(t, base, SessionTarget{}, firstModel)
	require.NoError(t, first.SetMode(t.Context(), ModePlan))
	document, err := first.plans.Replace(t.Context(), first.planRef, "", "# Restart-safe Plan")
	require.NoError(t, err)
	setPlanReviewResponsesForContent(firstModel, document.Revision, document.Content)
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
			"present-plan",
			planreview.PresentToolName,
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
