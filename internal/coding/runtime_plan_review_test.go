//nolint:wsl_v5 // Plan-mode fixtures keep events beside ordering assertions.
package coding

import (
	"context"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/planmode"
	"github.com/rsbin1178/pips/internal/coding/planreview"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimePlanModeEnterApprovalActivatesPlanMode(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel()
	runtime := openTestRuntimeAt(t, t.TempDir(), SessionTarget{}, model)
	setRuntimeResponses(model,
		runtimeToolResponse("enter-plan", planmode.EnterToolName, `{}`),
		runtimeTextResponse("ready to explore the codebase"),
	)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("plan this change")))
	assert.Equal(t, 1, countEventType(events, EventPlanReviewRequired))
	assert.Zero(t, countEventType(events, EventPlanModeChanged))

	paused := runtime.Snapshot()
	require.Equal(t, PhasePaused, paused.Phase)
	require.NotNil(t, paused.PlanReview.Required)
	request := planReviewRequest(t, runtime)
	assert.Equal(t, planreview.KindEnter, request.Kind)
	assert.Equal(t, "enter-plan", request.ToolCallID)
	assert.False(t, request.HasContent)
	assert.Empty(t, request.Content)

	events = collectRuntimeEvents(t, runtime.ResolvePlanReview(t.Context(), planreview.Resolution{
		RequestID: request.ID,
		Decision:  planreview.DecisionApprove,
	}))

	assert.Equal(t, PlanReviewResolved{
		RequestID: request.ID,
		Kind:      planreview.KindEnter,
		Decision:  planreview.DecisionApprove,
	}, payloadOfType[PlanReviewResolved](t, events, EventPlanReviewResolved))
	assert.Equal(t,
		[]PlanModeChanged{{State: planmode.StateActive}},
		payloadsOfType[PlanModeChanged](events, EventPlanModeChanged),
	)
	assert.Equal(t,
		[]ModeChanged{{Mode: ModePlan}},
		payloadsOfType[ModeChanged](events, EventModeChanged),
	)

	state := runtime.Snapshot()
	assert.Equal(t, PhaseIdle, state.Phase)
	assert.Equal(t, planmode.StateActive, state.PlanMode)
	assert.Equal(t, ModePlan, state.Mode)
	assert.Nil(t, state.PlanReview.Required)
	assert.Contains(t, transcriptText(state.Transcript), planmode.EnterResult)

	requests := model.Requests()
	require.Len(t, requests, 2, "the paused turn must resume in the same interaction")
	assert.Equal(t, planmode.EnterResult, lastToolResultText(t, model, "enter-plan"))
	assert.Contains(t, requestSystemText(requests[1]), `"operating_mode": "agent"`)

	// Entering plan mode never fabricates plan content.
	document, err := runtime.planStore.Read(t.Context())
	if err == nil {
		assert.Empty(t, document.Content)
	} else {
		require.ErrorIs(t, err, planmode.ErrNotFound)
	}
}

func TestRuntimePlanModeEnterDeclineKeepsAgentMode(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel()
	runtime := openTestRuntimeAt(t, t.TempDir(), SessionTarget{}, model)
	setRuntimeResponses(model,
		runtimeToolResponse("enter-plan", planmode.EnterToolName, `{}`),
		runtimeTextResponse("continuing in agent mode"),
	)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("plan this change")))
	require.Equal(t, PhasePaused, runtime.Snapshot().Phase)
	request := planReviewRequest(t, runtime)

	events = collectRuntimeEvents(t, runtime.ResolvePlanReview(t.Context(), planreview.Resolution{
		RequestID: request.ID,
		Decision:  planreview.DecisionDecline,
	}))

	assert.Zero(t, countEventType(events, EventPlanModeChanged))
	assert.Zero(t, countEventType(events, EventModeChanged))
	assert.Equal(t, PlanReviewResolved{
		RequestID: request.ID,
		Kind:      planreview.KindEnter,
		Decision:  planreview.DecisionDecline,
	}, payloadOfType[PlanReviewResolved](t, events, EventPlanReviewResolved))

	state := runtime.Snapshot()
	assert.Equal(t, PhaseIdle, state.Phase)
	assert.Equal(t, planmode.StateInactive, state.PlanMode)
	assert.Equal(t, ModeAgent, state.Mode)
	assert.Contains(t, transcriptText(state.Transcript), planmode.DeclineResult)
	assert.Equal(t, planmode.DeclineResult, lastToolResultText(t, model, "enter-plan"))
}

func TestRuntimePlanModeExitApprovalAppliesThePlanFile(t *testing.T) {
	t.Parallel()

	const content = "# Plan\n\n1. Implement the gate\n2. Verify it"

	model := newRuntimeModel()
	runtime := openTestRuntimeAt(t, t.TempDir(), SessionTarget{}, model)
	planPath := runtime.planStore.Path()
	setRuntimeResponses(model,
		runtimeToolResponse("enter-plan", planmode.EnterToolName, `{}`),
		planWriteResponse(t, runtime, "write-plan", content),
		runtimeToolResponse("exit-plan", planmode.ExitToolName, `{}`),
		runtimeTextResponse("implementing the approved plan"),
	)

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("plan the change")))
	require.Equal(t, PhasePaused, runtime.Snapshot().Phase)
	enterRequest := planReviewRequest(t, runtime)

	events := collectRuntimeEvents(t, runtime.ResolvePlanReview(t.Context(), planreview.Resolution{
		RequestID: enterRequest.ID,
		Decision:  planreview.DecisionApprove,
	}))
	assert.Equal(t,
		[]PlanModeChanged{{State: planmode.StateActive}},
		payloadsOfType[PlanModeChanged](events, EventPlanModeChanged),
	)

	// The model wrote the plan through the admitted apply_patch gate, then
	// asked for review in the same interaction.
	document, err := runtime.planStore.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, content, document.Content)
	assert.Contains(t, transcriptText(runtime.Snapshot().Transcript), "M "+planPath)

	require.Equal(t, PhasePaused, runtime.Snapshot().Phase)
	exitRequest := planReviewRequest(t, runtime)
	assert.Equal(t, planreview.KindExit, exitRequest.Kind)
	assert.Equal(t, "exit-plan", exitRequest.ToolCallID)
	assert.True(t, exitRequest.HasContent)
	assert.Equal(t, content, exitRequest.Content)

	// Every request after an armed turn carries the plan reminder, including
	// the iteration guidance for a user-initiated interaction.
	requests := model.Requests()
	require.Len(t, requests, 3)
	reminder := requestSystemText(requests[2])
	assert.Contains(t, reminder, "Plan mode is active. Do not make any edits or writes to the system.")
	assert.Contains(t, reminder, planPath)
	assert.Contains(t, reminder, "Plan mode is still active.")

	events = collectRuntimeEvents(t, runtime.ResolvePlanReview(t.Context(), planreview.Resolution{
		RequestID: exitRequest.ID,
		Decision:  planreview.DecisionApprove,
	}))

	assert.Equal(t, PlanReviewResolved{
		RequestID: exitRequest.ID,
		Kind:      planreview.KindExit,
		Decision:  planreview.DecisionApprove,
	}, payloadOfType[PlanReviewResolved](t, events, EventPlanReviewResolved))
	assert.Equal(t,
		[]PlanModeChanged{{State: planmode.StateInactive}},
		payloadsOfType[PlanModeChanged](events, EventPlanModeChanged),
	)
	assert.Equal(t,
		[]ModeChanged{{Mode: ModeAgent}},
		payloadsOfType[ModeChanged](events, EventModeChanged),
	)

	state := runtime.Snapshot()
	assert.Equal(t, PhaseIdle, state.Phase)
	assert.Equal(t, planmode.StateInactive, state.PlanMode)
	assert.Equal(t, ModeAgent, state.Mode)
	assert.Contains(t, transcriptText(state.Transcript), planmode.ExitApprovedResult)

	requests = model.Requests()
	require.Len(t, requests, 4, "the approved turn must resume without a new interaction")
	assert.Equal(t, planmode.ExitApprovedResult, lastToolResultText(t, model, "exit-plan"))
	assert.NotContains(t, requestSystemText(requests[3]), "Plan mode is active.")
}

func TestRuntimePlanModeExitRevisionKeepsPlanModeActive(t *testing.T) {
	t.Parallel()

	const content = "# Plan\n\n1. Implement"
	const notes = "Fold the rollback path into step 2."

	model, runtime := openActivePlanModeRuntime(t, content)
	exitRequest := pauseOnExitPlanMode(t, runtime, model)

	events := collectRuntimeEvents(t, runtime.ResolvePlanReview(t.Context(), planreview.Resolution{
		RequestID: exitRequest.ID,
		Decision:  planreview.DecisionRevise,
		Notes:     notes,
	}))

	assert.Zero(t, countEventType(events, EventPlanModeChanged))
	assert.Equal(t, PlanReviewResolved{
		RequestID: exitRequest.ID,
		Kind:      planreview.KindExit,
		Decision:  planreview.DecisionRevise,
	}, payloadOfType[PlanReviewResolved](t, events, EventPlanReviewResolved))

	state := runtime.Snapshot()
	assert.Equal(t, PhaseIdle, state.Phase)
	assert.Equal(t, planmode.StateActive, state.PlanMode)
	assert.Equal(t, ModePlan, state.Mode)
	assert.Contains(t, transcriptText(state.Transcript), "User revision notes:\n"+notes)
	assert.Contains(t, lastToolResultText(t, model, "exit-plan"), "User revision notes:\n"+notes)
}

func TestRuntimePlanModeExitQuitDisablesPlanMode(t *testing.T) {
	t.Parallel()

	model, runtime := openActivePlanModeRuntime(t, "# Plan\n\n1. Implement")
	exitRequest := pauseOnExitPlanMode(t, runtime, model)

	events := collectRuntimeEvents(t, runtime.ResolvePlanReview(t.Context(), planreview.Resolution{
		RequestID: exitRequest.ID,
		Decision:  planreview.DecisionQuit,
	}))

	assert.Equal(t,
		[]PlanModeChanged{{State: planmode.StateInactive}},
		payloadsOfType[PlanModeChanged](events, EventPlanModeChanged),
	)
	assert.Equal(t,
		[]ModeChanged{{Mode: ModeAgent}},
		payloadsOfType[ModeChanged](events, EventModeChanged),
	)

	state := runtime.Snapshot()
	assert.Equal(t, PhaseIdle, state.Phase)
	assert.Equal(t, planmode.StateInactive, state.PlanMode)
	assert.Equal(t, ModeAgent, state.Mode)
	assert.Contains(t, transcriptText(state.Transcript), "The user chose to abandon the plan entirely")
	assert.Contains(t, lastToolResultText(t, model, "exit-plan"), "The user chose to abandon the plan entirely")
}

func TestRuntimePlanModeExitApprovalWithoutPlanContent(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel()
	runtime := openTestRuntimeAt(t, t.TempDir(), SessionTarget{}, model)
	setRuntimeResponses(model,
		runtimeToolResponse("enter-plan", planmode.EnterToolName, `{}`),
		runtimeToolResponse("exit-plan", planmode.ExitToolName, `{}`),
		runtimeTextResponse("what should the plan cover?"),
	)

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("plan the change")))
	require.Equal(t, PhasePaused, runtime.Snapshot().Phase)
	enterRequest := planReviewRequest(t, runtime)
	collectRuntimeEvents(t, runtime.ResolvePlanReview(t.Context(), planreview.Resolution{
		RequestID: enterRequest.ID,
		Decision:  planreview.DecisionApprove,
	}))

	require.Equal(t, PhasePaused, runtime.Snapshot().Phase)
	exitRequest := planReviewRequest(t, runtime)
	assert.False(t, exitRequest.HasContent)
	assert.Empty(t, exitRequest.Content)

	collectRuntimeEvents(t, runtime.ResolvePlanReview(t.Context(), planreview.Resolution{
		RequestID: exitRequest.ID,
		Decision:  planreview.DecisionApprove,
	}))

	assert.Contains(t,
		transcriptText(runtime.Snapshot().Transcript),
		planmode.ExitApprovedEmptyResult,
	)
	assert.Equal(t, planmode.ExitApprovedEmptyResult, lastToolResultText(t, model, "exit-plan"))
}

func TestRuntimeCloseParkedOnPlanReviewIsClean(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	model := newRuntimeModel()
	runtime := openTestRuntimeAt(t, base, SessionTarget{}, model)
	sessionID := runtime.handle.Metadata().ID
	setRuntimeResponses(model, runtimeToolResponse("enter-plan", planmode.EnterToolName, `{}`))

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("plan the change")))
	require.Equal(t, PhasePaused, runtime.Snapshot().Phase)
	planReviewRequest(t, runtime)

	// Closing a parked runtime must retire the displayed review without
	// protocol violations, and leave the durable decision for a resume.
	require.NoError(t, runtime.Close(t.Context()))

	second := openTestRuntimeAt(t, base, SessionTarget{ID: sessionID}, newRuntimeModel())
	t.Cleanup(func() { _ = second.Close(context.Background()) })
	require.Equal(t, PhasePaused, second.Snapshot().Phase)
	events := collectRuntimeEvents(t, second.Continue(t.Context()))
	assert.Contains(t, eventTypes(events), EventPlanReviewRequired)
	require.NoError(t, second.Close(t.Context()))
}

func TestRuntimePlanModeEditGateRejectsWorkspaceWrites(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel()
	runtime := openTestRuntimeAt(t, t.TempDir(), SessionTarget{}, model)
	planPath := runtime.planStore.Path()
	workspaceFile := filepath.Join(runtime.workspace.Root(), "note.md")
	// The patch tool addresses workspace files relatively; only the plan file
	// itself is accepted as an absolute path.
	setRuntimeResponses(model,
		runtimeToolResponse("enter-plan", planmode.EnterToolName, `{}`),
		runtimeToolResponse(
			"write-workspace",
			tools.ApplyPatchName,
			planPatchArgs(planPatchAdd("note.md", "edits are not allowed yet\n")),
		),
		runtimeTextResponse("staying read-only"),
	)

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("plan the change")))
	require.Equal(t, PhasePaused, runtime.Snapshot().Phase)
	enterRequest := planReviewRequest(t, runtime)
	events := collectRuntimeEvents(t, runtime.ResolvePlanReview(t.Context(), planreview.Resolution{
		RequestID: enterRequest.ID,
		Decision:  planreview.DecisionApprove,
	}))
	assert.Zero(t, countEventType(events, EventPlanReviewRequired))

	state := runtime.Snapshot()
	assert.Equal(t, PhaseIdle, state.Phase)
	assert.Equal(t, planmode.StateActive, state.PlanMode)
	assert.Contains(t, transcriptText(state.Transcript), planmode.EditRejection(planPath))
	assert.Contains(t, transcriptText(state.Transcript), "file edits are not allowed in plan mode")

	_, err := os.Lstat(workspaceFile)
	require.ErrorIs(t, err, os.ErrNotExist)
	document, readErr := runtime.planStore.Read(t.Context())
	if readErr == nil {
		assert.Empty(t, document.Content, "the denied write must not touch the plan file")
	} else {
		require.ErrorIs(t, readErr, planmode.ErrNotFound)
	}
}

func TestRuntimePlanModeActiveStateSurvivesRestart(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	model := newRuntimeModel()
	first := openTestRuntimeAt(t, base, SessionTarget{}, model)
	sessionID := first.handle.Metadata().ID
	planPath := first.planStore.Path()
	setRuntimeResponses(model,
		runtimeToolResponse("enter-plan", planmode.EnterToolName, `{}`),
		runtimeTextResponse("planning"),
	)

	collectRuntimeEvents(t, first.Prompt(t.Context(), ai.UserText("plan the change")))
	require.Equal(t, PhasePaused, first.Snapshot().Phase)
	request := planReviewRequest(t, first)
	collectRuntimeEvents(t, first.ResolvePlanReview(t.Context(), planreview.Resolution{
		RequestID: request.ID,
		Decision:  planreview.DecisionApprove,
	}))
	require.Equal(t, planmode.StateActive, first.Snapshot().PlanMode)

	durable, err := first.planStore.LoadState(t.Context())
	require.NoError(t, err)
	assert.Equal(t, planmode.StateActive, durable)
	abruptRuntimeStop(t, first)

	second := openPlanConfiguredRuntime(t, base, SessionTarget{ID: sessionID}, ModePlan)
	state := second.Snapshot()
	assert.Equal(t, planmode.StateActive, state.PlanMode)
	assert.Equal(t, ModePlan, state.Mode)
	assert.Equal(t, planPath, second.planStore.Path())
}

func TestRuntimePlanModeTransientStatesCollapseOnRestart(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		raw  string
	}{
		{name: "pending", raw: `{"state":"pending"}`},
		{name: "exit pending", raw: `{"state":"exit_pending"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			base := t.TempDir()
			first := openTestRuntimeAt(t, base, SessionTarget{}, newRuntimeModel(runtimeTextResponse("done")))
			sessionID := first.handle.Metadata().ID
			statePath := filepath.Join(filepath.Dir(first.planStore.Path()), "plan-mode.json")
			collectRuntimeEvents(t, first.Prompt(t.Context(), ai.UserText("start")))
			abruptRuntimeStop(t, first)
			require.NoError(t, os.WriteFile(statePath, []byte(test.raw), 0o600))

			second := openTestRuntimeAt(t, base, SessionTarget{ID: sessionID}, newRuntimeModel())
			state := second.Snapshot()
			assert.Equal(t, planmode.StateInactive, state.PlanMode)
			assert.Equal(t, ModeAgent, state.Mode)
		})
	}
}

func TestRuntimePlanReviewRejectsResolutionsThatDoNotMatch(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel()
	runtime := openTestRuntimeAt(t, t.TempDir(), SessionTarget{}, model)
	setRuntimeResponses(model, runtimeToolResponse("enter-plan", planmode.EnterToolName, `{}`))

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("plan the change")))
	require.Equal(t, PhasePaused, runtime.Snapshot().Phase)
	request := planReviewRequest(t, runtime)

	_, err := collectRuntimeEventsAndError(t, runtime.ResolvePlanReview(t.Context(), planreview.Resolution{
		RequestID: "plan-missing",
		Decision:  planreview.DecisionApprove,
	}))
	require.ErrorIs(t, err, ErrRuntimeInvalid)
	assert.Equal(t, request.ID, runtime.Snapshot().PlanReview.Required.ID)

	_, err = collectRuntimeEventsAndError(t, runtime.ResolvePlanReview(t.Context(), planreview.Resolution{
		RequestID: request.ID,
		Decision:  planreview.DecisionRevise,
	}))
	require.ErrorIs(t, err, ErrRuntimeInvalid)
	assert.Equal(t, request.ID, runtime.Snapshot().PlanReview.Required.ID)
}

func planReviewRequest(t *testing.T, runtime *Runtime) planreview.Request {
	t.Helper()

	request := runtime.Snapshot().PlanReview.Required
	require.NotNil(t, request, "no plan review is displayed")

	return *request
}

// openActivePlanModeRuntime drives the model-facing entry approval so the edit
// gate is armed and the plan file holds content.
func openActivePlanModeRuntime(t *testing.T, content string) (*runtimeModel, *Runtime) {
	t.Helper()

	model := newRuntimeModel()
	runtime := openTestRuntimeAt(t, t.TempDir(), SessionTarget{}, model)
	setRuntimeResponses(model,
		runtimeToolResponse("enter-plan", planmode.EnterToolName, `{}`),
		planWriteResponse(t, runtime, "write-plan", content),
		runtimeTextResponse("plan written"),
	)

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("plan the change")))
	require.Equal(t, PhasePaused, runtime.Snapshot().Phase)
	enterRequest := planReviewRequest(t, runtime)
	collectRuntimeEvents(t, runtime.ResolvePlanReview(t.Context(), planreview.Resolution{
		RequestID: enterRequest.ID,
		Decision:  planreview.DecisionApprove,
	}))
	require.Equal(t, planmode.StateActive, runtime.Snapshot().PlanMode)

	document, err := runtime.planStore.Read(t.Context())
	require.NoError(t, err)
	require.Equal(t, content, document.Content)

	return model, runtime
}

// pauseOnExitPlanMode prompts the model to request plan approval and returns
// the displayed exit request.
func pauseOnExitPlanMode(
	t *testing.T,
	runtime *Runtime,
	model *runtimeModel,
) planreview.Request {
	t.Helper()

	setRuntimeResponses(model,
		runtimeToolResponse("exit-plan", planmode.ExitToolName, `{}`),
		runtimeTextResponse("awaiting the decision"),
	)
	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("present the plan")))
	require.Equal(t, PhasePaused, runtime.Snapshot().Phase)

	request := planReviewRequest(t, runtime)
	require.Equal(t, planreview.KindExit, request.Kind)
	require.True(t, request.HasContent)

	return request
}

func setRuntimeResponses(model *runtimeModel, responses ...*ai.Response) {
	model.mu.Lock()
	model.responses = append(model.responses, responses...)
	model.mu.Unlock()
}

func planPatchArgs(document string) string {
	return fmt.Sprintf(`{"patch":%q}`, document)
}

// planWriteResponse scripts one apply_patch call that fills the session plan
// file. The empty plan file is created up front so the patch stays valid
// whether or not entering plan mode seeds it.
func planWriteResponse(t *testing.T, runtime *Runtime, id string, content string) *ai.Response {
	t.Helper()

	_, err := runtime.planStore.Replace(t.Context(), "")
	require.NoError(t, err)

	var document strings.Builder
	document.WriteString("*** Begin Patch\n*** Update File: " + runtime.planStore.Path() + "\n@@\n")
	for _, line := range strings.Split(strings.TrimSuffix(content, "\n"), "\n") {
		document.WriteString("+" + line + "\n")
	}
	document.WriteString("*** End Patch")

	return runtimeToolResponse(id, tools.ApplyPatchName, planPatchArgs(document.String()))
}

func planPatchAdd(path string, content string) string {
	var builder strings.Builder
	builder.WriteString("*** Begin Patch\n*** Add File: " + path + "\n")
	for _, line := range strings.Split(strings.TrimSuffix(content, "\n"), "\n") {
		builder.WriteString("+" + line + "\n")
	}
	builder.WriteString("*** End Patch")

	return builder.String()
}

// collectRuntimeEventsAndError drains a Runtime sequence, preserving the first
// failure instead of asserting it away.
func collectRuntimeEventsAndError(
	t *testing.T,
	sequence iter.Seq2[Event, error],
) ([]Event, error) {
	t.Helper()

	var (
		events  []Event
		failure error
	)
	sequence(func(event Event, err error) bool {
		if err != nil {
			failure = err

			return false
		}
		events = append(events, event)

		return true
	})

	return events, failure
}

// lastToolResultText returns the result text the model saw for one tool call
// in the most recent request.
func lastToolResultText(t *testing.T, model *runtimeModel, toolCallID string) string {
	t.Helper()

	requests := model.Requests()
	require.NotEmpty(t, requests)

	return requestToolResultText(requests[len(requests)-1], toolCallID)
}

// requestToolResultText returns the text the model saw for one tool call.
func requestToolResultText(request ai.Request, toolCallID string) string {
	var text strings.Builder
	for _, message := range request.Messages {
		parts, err := ai.MessageParts(message)
		if err != nil {
			continue
		}
		for _, part := range parts {
			result, ok := part.(ai.ToolResultPart)
			if !ok || result.ToolCallID != toolCallID {
				continue
			}
			for _, content := range result.Content {
				if value, ok := content.(ai.TextPart); ok {
					text.WriteString(value.Text)
				}
			}
		}
	}

	return text.String()
}

func payloadOfType[T EventPayload](t *testing.T, events []Event, eventType EventType) T {
	t.Helper()

	values := payloadsOfType[T](events, eventType)
	require.NotEmpty(t, values, "event %s was not published", eventType)

	return values[0]
}

func payloadsOfType[T EventPayload](events []Event, eventType EventType) []T {
	var values []T
	for _, event := range events {
		if event.Type != eventType {
			continue
		}
		if payload, ok := event.Payload.(T); ok {
			values = append(values, payload)
		}
	}

	return values
}
