package coding

import (
	"context"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/planmode"
	"github.com/rsbin1178/pips/internal/coding/planreview"
	"github.com/rsbin1178/pips/internal/coding/tasklist"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCatalogPolicyForModeUsesTheCompletePrivilegedToolset(t *testing.T) {
	t.Parallel()

	read := testCatalogTool("read")
	applyPatch := testCatalogTool(tools.ApplyPatchName)
	shell := testCatalogTool("shell")
	extensionRead := testCatalogTool("extension_read")
	catalogValue, err := catalog.New(
		catalog.Entry{
			Tool: read, Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding"},
			Risk: catalog.RiskRead,
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

	want := []string{"read", tools.ApplyPatchName, "shell", "extension_read"}
	for _, mode := range []OperatingMode{ModeAgent, ModePlan} {
		policy, err := catalogPolicyForMode(mode, "workspace")
		require.NoError(t, err)

		snapshot, err := catalogValue.Snapshot(t.Context(), policy)
		require.NoError(t, err)
		assert.Equal(t, want, agentToolNames(snapshot), string(mode))
	}

	_, err = catalogPolicyForMode("other", "workspace")
	require.ErrorIs(t, err, ErrRuntimeInvalid)
}

func TestLeasedToolGuardDeniesCallsOutsideSnapshot(t *testing.T) {
	t.Parallel()

	known := []agent.Tool{
		agent.NewTool("read", "", func(context.Context, struct{}) (string, error) { return "", nil }),
		agent.NewTool("apply_patch", "", func(context.Context, struct{}) (string, error) { return "", nil }),
	}
	guard := leasedToolGuard(ModePlan, []catalog.Descriptor{{Name: "read"}}, catalog.DefaultToolSearchName, known)
	assert.Equal(t, agent.ToolDecisionAllow, guard(t.Context(), agent.ToolCallInfo{
		ToolCall: agent.ToolCall{Name: "read"},
	}).Action)
	assert.Equal(t, agent.ToolDecisionAllow, guard(t.Context(), agent.ToolCallInfo{
		ToolCall: agent.ToolCall{Name: catalog.DefaultToolSearchName},
	}).Action)
	denied := guard(t.Context(), agent.ToolCallInfo{
		ToolCall: agent.ToolCall{Name: "apply_patch"},
	})
	assert.Equal(t, agent.ToolDecisionDeny, denied.Action)
	assert.Contains(t, denied.Reason, "plan mode")

	// A misspelled name is a model slip, not a mode restriction, and the
	// denial points at the closest known name.
	unknown := guard(t.Context(), agent.ToolCallInfo{
		ToolCall: agent.ToolCall{Name: "search_tools"},
	})
	assert.Equal(t, agent.ToolDecisionDeny, unknown.Action)
	assert.Equal(t, `unknown tool "search_tools"; did you mean "pips_tool_search"?`, unknown.Reason)
	assert.NotContains(t, unknown.Reason, "mode")

	// A name with no close match gets the plain unknown message.
	plain := guard(t.Context(), agent.ToolCallInfo{
		ToolCall: agent.ToolCall{Name: "deploy_kubernetes_cluster"},
	})
	assert.Equal(t, agent.ToolDecisionDeny, plain.Action)
	assert.Equal(t, `unknown tool "deploy_kubernetes_cluster"`, plain.Reason)

	// Disabled search never authorizes the discovery name.
	disabled := leasedToolGuard(ModePlan, []catalog.Descriptor{{Name: "read"}}, "", known)
	hidden := disabled(t.Context(), agent.ToolCallInfo{
		ToolCall: agent.ToolCall{Name: catalog.DefaultToolSearchName},
	})
	assert.Equal(t, agent.ToolDecisionDeny, hidden.Action)
	assert.Contains(t, hidden.Reason, "unknown tool")
}

func TestSuggestToolNames(t *testing.T) {
	t.Parallel()

	known := []string{
		"apply_patch", "read", "grep", "glob", "ls", "shell", "ask_user",
		"pips_tool_search", "run_subagent", "spawn_agent", "update_plan",
	}

	tests := []struct {
		name string
		want []string
	}{
		{name: "search_tools", want: []string{"pips_tool_search"}},
		{name: "tool_search", want: []string{"pips_tool_search"}},
		{name: "toolSearch", want: []string{"pips_tool_search"}},
		{name: "raed", want: []string{"read"}},
		{name: "gerp", want: []string{"grep"}},
		{name: "patch_apply", want: []string{"apply_patch"}},
		// One shared token out of three is below the overlap threshold and
		// the edit distance is too large, so no guess is offered.
		{name: "spawn_subagent", want: nil},
		{name: "deploy_kubernetes_cluster", want: nil},
		{name: "", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, suggestToolNames(tt.name, known, 3))
		})
	}

	assert.Equal(t, `unknown tool "x"`, unknownToolReason("x", known))
	// A model-supplied name is escaped so the denial stays a single line.
	assert.Equal(t, `unknown tool "re\nad"; did you mean "read"?`, unknownToolReason("re\nad", known))
	// Closest first: one edit beats two, and the limit bounds the list.
	files := []string{"write_file", "read_files", "read_file", "read_fill", "read_filler"}
	assert.Equal(t, []string{"read_file", "read_fill", "read_files"}, suggestToolNames("read_fil", files, 3))
	assert.Equal(
		t,
		`unknown tool "read_fil"; did you mean "read_file", "read_files"?`,
		unknownToolReason("read_fil", files[:3]),
	)
	assert.Len(t, suggestToolNames("agent", []string{"run_agent", "spawn_agent", "list_agents", "stop_agent"}, 3), 3)
}

func TestRuntimePlanModeInteractionLeasesTheFullToolCatalog(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(runtimeTextResponse("read-only exploration"))
	runtime := openPlanConfiguredRuntimeWithModel(
		t, t.TempDir(), SessionTarget{}, ModePlan, model,
	)
	require.Equal(t, ModePlan, runtime.Snapshot().Mode)

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), testUserMessage("plan it")))

	requests := model.Requests()
	require.Len(t, requests, 1)
	toolNames := toolNamesFromRequest(requests[0])
	for _, name := range []string{
		tools.ApplyPatchName, "shell", planmode.EnterToolName, planmode.ExitToolName,
	} {
		assert.Contains(t, toolNames, name)
	}
	assert.NotContains(t, toolNames, "write_plan")
	assert.NotContains(t, toolNames, "present_plan")
	assert.Contains(t, requestSystemText(requests[0]), `"operating_mode": "plan"`)
}

func TestRuntimeAgentModeLeasesTheSameToolCatalog(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(runtimeTextResponse("done"))
	runtime := openTestRuntime(t, model)
	collectRuntimeEvents(t, runtime.Prompt(t.Context(), testUserMessage("inspect")))

	requests := model.Requests()
	require.Len(t, requests, 1)
	toolNames := toolNamesFromRequest(requests[0])
	for _, name := range []string{
		tools.ApplyPatchName, "shell", planmode.EnterToolName, planmode.ExitToolName,
		tasklist.ToolName,
	} {
		assert.Contains(t, toolNames, name)
	}
	assert.NotContains(t, toolNames, "write_plan")
	assert.NotContains(t, toolNames, "present_plan")
	assert.Contains(t, requestSystemText(requests[0]), `"operating_mode": "agent"`)
}

func TestRuntimeSetModePublishesPlanModeTransitions(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(runtimeTextResponse("done"))
	runtime := openTestRuntimeAt(t, t.TempDir(), SessionTarget{}, model)
	observation, err := runtime.ObserveEvents()
	require.NoError(t, err)

	defer observation.Subscription.Close()

	require.NoError(t, runtime.SetMode(t.Context(), ModePlan))

	state := runtime.Snapshot()
	assert.Equal(t, planmode.StatePending, state.PlanMode)
	assert.Equal(t, ModePlan, state.Mode)

	events := drainRuntimeRecords(observation.Subscription.Events())
	assert.Equal(t,
		[]PlanModeChanged{{State: planmode.StatePending}},
		payloadsOfType[PlanModeChanged](events, EventPlanModeChanged),
	)
	assert.Equal(t,
		[]ModeChanged{{Mode: ModePlan}},
		payloadsOfType[ModeChanged](events, EventModeChanged),
	)

	// Pending is transient: the durable projection waits for the first prompt.
	durable, err := runtime.planStore.LoadState(t.Context())
	require.NoError(t, err)
	assert.Equal(t, planmode.StateInactive, durable)

	require.NoError(t, runtime.SetMode(t.Context(), ModeAgent))

	state = runtime.Snapshot()
	assert.Equal(t, planmode.StateInactive, state.PlanMode)
	assert.Equal(t, ModeAgent, state.Mode)

	events = drainRuntimeRecords(observation.Subscription.Events())
	assert.Equal(t,
		[]PlanModeChanged{{State: planmode.StateInactive}},
		payloadsOfType[PlanModeChanged](events, EventPlanModeChanged),
	)
	assert.Equal(t,
		[]ModeChanged{{Mode: ModeAgent}},
		payloadsOfType[ModeChanged](events, EventModeChanged),
	)
}

func TestRuntimePlanModeExitDefersUntilTheTurnSettles(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel()
	runtime := openTestRuntimeAt(t, t.TempDir(), SessionTarget{}, model)
	setRuntimeResponses(model,
		runtimeToolResponse("enter-plan", planmode.EnterToolName, `{}`),
		runtimeToolResponse("exit-plan", planmode.ExitToolName, `{}`),
		runtimeTextResponse("finished planning"),
	)

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), testUserMessage("plan it")))
	require.Equal(t, PhasePaused, runtime.Snapshot().Phase)
	enterRequest := planReviewRequest(t, runtime)
	collectRuntimeEvents(t, runtime.ResolvePlanReview(t.Context(), planreview.Resolution{
		RequestID: enterRequest.ID,
		Decision:  planreview.DecisionApprove,
	}))

	require.Equal(t, PhasePaused, runtime.Snapshot().Phase)
	exitRequest := planReviewRequest(t, runtime)

	// Leaving plan mode while a turn is in flight holds the gate until it ends.
	require.NoError(t, runtime.SetMode(t.Context(), ModeAgent))
	assert.Equal(t, planmode.StateExitPending, runtime.Snapshot().PlanMode)
	assert.Equal(t, ModePlan, runtime.Snapshot().Mode)

	events := collectRuntimeEvents(t, runtime.ResolvePlanReview(t.Context(), planreview.Resolution{
		RequestID: exitRequest.ID,
		Decision:  planreview.DecisionRevise,
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
}

func TestRuntimeSetModeIsIdempotentAndRejectsConcurrentOperations(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntimeAt(
		t, t.TempDir(), SessionTarget{}, newRuntimeModel(runtimeTextResponse("done")),
	)

	require.NoError(t, runtime.SetMode(t.Context(), ModeAgent))
	assert.Equal(t, ModeAgent, runtime.Snapshot().Mode)
	require.ErrorIs(t, runtime.SetMode(t.Context(), "unsupported"), ErrRuntimeInvalid)

	runtime.mu.Lock()
	blocking := &runtimeOperation{cancel: func() {}, done: make(chan struct{})}
	runtime.active = blocking
	runtime.mu.Unlock()

	require.ErrorIs(t, runtime.SetMode(t.Context(), ModePlan), ErrRuntimeBusy)

	runtime.mu.Lock()
	runtime.active = nil
	runtime.mu.Unlock()
	close(blocking.done)
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

// openPlanConfiguredRuntime opens a Runtime whose configured operating mode is
// the process-level --mode selection.
func openPlanConfiguredRuntime(
	t *testing.T,
	base string,
	target SessionTarget,
	mode config.OperatingMode,
) *Runtime {
	t.Helper()

	return openPlanConfiguredRuntimeWithModel(t, base, target, mode, newRuntimeModel())
}

func openPlanConfiguredRuntimeWithModel(
	t *testing.T,
	base string,
	target SessionTarget,
	mode config.OperatingMode,
	model ai.LanguageModel,
) *Runtime {
	t.Helper()

	return openTestRuntimeConfiguredWithSandboxAndConfig(
		t,
		base,
		target,
		model,
		nil,
		nil,
		nil,
		false,
		config.SandboxWorkspaceWrite,
		func(cfg *config.Config) { cfg.Mode = mode },
	)
}

// drainRuntimeRecords collects every event already broadcast to the
// subscription without waiting for more.
func drainRuntimeRecords(events <-chan EventRecord) []Event {
	var collected []Event
	for {
		select {
		case record := <-events:
			collected = append(collected, record.Event)
		default:
			return collected
		}
	}
}

func testUserMessage(value string) ai.Message { return ai.UserText(value) }

func testCatalogTool(name string) agent.Tool {
	return agent.NewTool(name, name, func(context.Context, struct{}) (string, error) {
		return "ok", nil
	})
}
