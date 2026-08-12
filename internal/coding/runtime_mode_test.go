package coding

import (
	"context"
	"os"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/planreview"
	"github.com/rsbin1178/pips/internal/coding/tasklist"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCatalogPolicyForModeUsesExactPlanWriteException(t *testing.T) {
	t.Parallel()

	read := testCatalogTool("read")
	writePlan := testCatalogTool(tools.WritePlanName)
	applyPatch := testCatalogTool("apply_patch")
	shell := testCatalogTool("shell")
	extensionRead := testCatalogTool("extension_read")
	catalogValue, err := catalog.New(
		catalog.Entry{
			Tool: read, Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding"},
			Risk: catalog.RiskRead,
		},
		catalog.Entry{
			Tool:   writePlan,
			Source: catalog.Source{Kind: catalog.SourceLocal, ID: tools.PlanCatalogID},
			Risk:   catalog.RiskWrite,
		},
		catalog.Entry{
			Tool: applyPatch, Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding"},
			Risk: catalog.RiskWrite,
		},
		catalog.Entry{
			Tool: shell, Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding"},
			Risk: catalog.RiskPrivileged,
		},
		catalog.Entry{
			Tool:   extensionRead,
			Source: catalog.Source{Kind: catalog.SourceExtension, ID: "reviewed"},
			Risk:   catalog.RiskRead,
		},
	)
	require.NoError(t, err)

	planPolicy, err := catalogPolicyForMode(ModePlan, "workspace")
	require.NoError(t, err)
	planTools, err := catalogValue.Snapshot(t.Context(), planPolicy)
	require.NoError(t, err)
	assert.Equal(t, []string{"read", tools.WritePlanName, "extension_read"}, agentToolNames(planTools))

	agentPolicy, err := catalogPolicyForMode(ModeAgent, "workspace")
	require.NoError(t, err)
	agentTools, err := catalogValue.Snapshot(t.Context(), agentPolicy)
	require.NoError(t, err)
	assert.Equal(t,
		[]string{"read", tools.WritePlanName, "apply_patch", "shell", "extension_read"},
		agentToolNames(agentTools),
	)
}

func TestPlanPolicyRejectsForgedWritePlanProvenance(t *testing.T) {
	t.Parallel()

	policy, err := catalogPolicyForMode(ModePlan, "workspace")
	require.NoError(t, err)

	for _, source := range []catalog.Source{
		{Kind: catalog.SourceLocal, ID: "other"},
		{Kind: catalog.SourceExtension, ID: tools.PlanCatalogID},
		{Kind: catalog.SourceMCP, ID: tools.PlanCatalogID},
	} {
		value, err := catalog.New(catalog.Entry{
			Tool: testCatalogTool(tools.WritePlanName), Source: source, Risk: catalog.RiskWrite,
		})
		require.NoError(t, err)
		snapshot, err := value.Snapshot(t.Context(), policy)
		require.NoError(t, err)
		assert.Empty(t, snapshot)
	}
}

func TestLeasedToolGuardDeniesCallsOutsideSnapshot(t *testing.T) {
	t.Parallel()

	guard := leasedToolGuard(ModePlan, []catalog.Descriptor{{Name: "read"}}, false)
	assert.Equal(t, agent.ToolDecisionAllow, guard(t.Context(), agent.ToolCallInfo{
		ToolCall: agent.ToolCall{Name: "read"},
	}).Action)
	denied := guard(t.Context(), agent.ToolCallInfo{
		ToolCall: agent.ToolCall{Name: "apply_patch"},
	})
	assert.Equal(t, agent.ToolDecisionDeny, denied.Action)
	assert.Contains(t, denied.Reason, "plan mode")
}

func TestRuntimeSetModePublishesAndLeasesPlanCapabilities(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	model := newRuntimeModel()
	runtime := openTestRuntimeAt(t, base, SessionTarget{}, model)
	observation, err := runtime.ObserveEvents()
	require.NoError(t, err)

	defer observation.Subscription.Close()

	require.NoError(t, runtime.SetMode(t.Context(), ModePlan))
	assert.Equal(t, ModePlan, runtime.Snapshot().Mode)

	const content = "# Implementation Plan\n\n1. Inspect\n2. Verify"

	document, err := runtime.plans.Replace(t.Context(), runtime.planRef, "", content)
	require.NoError(t, err)
	setPlanReviewResponsesForContent(
		model,
		document.Revision,
		content,
	)

	select {
	case record := <-observation.Subscription.Events():
		assert.Equal(t, EventModeChanged, record.Event.Type)
		assert.Equal(t, ModeChanged{Mode: ModePlan}, record.Event.Payload)
	default:
		t.Fatal("mode.changed was not published")
	}

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), testUserMessage("plan it")))
	assert.Contains(t, eventTypes(events), EventPlanReviewRequired)

	requests := model.Requests()
	require.Len(t, requests, 2)
	toolNames := toolNamesFromRequest(requests[0])
	assert.Contains(t, toolNames, tools.ReadPlanName)
	assert.Contains(t, toolNames, tools.WritePlanName)
	assert.Contains(t, toolNames, planreview.PresentToolName)
	assert.NotContains(t, toolNames, planreview.ToolName)
	assert.Contains(t, toolNames, "run_subagent")
	assert.NotContains(t, toolNames, "apply_patch")
	assert.NotContains(t, toolNames, "shell")
	assert.NotContains(t, toolNames, tasklist.ToolName)
	assert.Contains(t, requestSystemText(requests[0]), `"operating_mode": "plan"`)
	assert.Equal(t, ai.ToolChoice{Mode: ai.ToolChoiceTool, Name: planreview.PresentToolName}, requests[1].ToolChoice)
	assert.Equal(t, []string{planreview.PresentToolName}, toolNamesFromRequest(requests[1]))

	planPath := base + "/home/plans/" + runtime.handle.Metadata().ID + ".md"
	info, err := os.Stat(planPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	document, err = runtime.plans.Read(t.Context(), runtime.planRef)
	require.NoError(t, err)
	assert.Contains(t, document.Content, "Implementation Plan")
}

func TestRuntimeAgentModeDoesNotAdvertisePlanSubmission(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(runtimeTextResponse("done"))
	runtime := openTestRuntime(t, model)
	collectRuntimeEvents(t, runtime.Prompt(t.Context(), testUserMessage("inspect")))

	requests := model.Requests()
	require.Len(t, requests, 1)
	toolNames := toolNamesFromRequest(requests[0])
	assert.NotContains(t, toolNames, planreview.ToolName)
	assert.Contains(t, toolNames, tasklist.ToolName)
}

func TestRuntimeSetModeIsIdleOnlyAndIdempotent(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntimeAt(
		t, t.TempDir(), SessionTarget{}, newRuntimeModel(runtimeTextResponse("done")),
	)
	require.NoError(t, runtime.SetMode(t.Context(), ModeAgent))

	runtime.mu.Lock()
	runtime.state.Phase = PhaseRunning
	runtime.mu.Unlock()
	require.ErrorIs(t, runtime.SetMode(t.Context(), ModePlan), ErrRuntimeBusy)
	assert.Equal(t, ModeAgent, runtime.Snapshot().Mode)
}

func TestRuntimeReplacementPreflightUsesCompleteIdleGate(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntimeAt(
		t, t.TempDir(), SessionTarget{}, newRuntimeModel(runtimeTextResponse("done")),
	)
	require.NoError(t, runtime.ReplacementPreflight(t.Context()))

	runtime.mu.Lock()
	runtime.state.Phase = PhaseRunning
	runtime.mu.Unlock()
	require.ErrorIs(t, runtime.ReplacementPreflight(t.Context()), ErrRuntimeBusy)

	runtime.mu.Lock()
	runtime.state.Phase = PhaseIdle
	runtime.state.Approval = ApprovalState{Kind: ApprovalReview}
	runtime.mu.Unlock()
	require.ErrorIs(t, runtime.ReplacementPreflight(t.Context()), ErrRuntimeBusy)

	runtime.mu.Lock()
	runtime.state.Approval = ApprovalState{}
	runtime.recovery.PendingID = "pending-1"
	runtime.mu.Unlock()
	require.ErrorIs(t, runtime.ReplacementPreflight(t.Context()), ErrRuntimeBusy)
}

func testUserMessage(value string) ai.Message { return ai.UserText(value) }

func testCatalogTool(name string) agent.Tool {
	return agent.NewTool(name, name, func(context.Context, struct{}) (string, error) {
		return "ok", nil
	})
}
