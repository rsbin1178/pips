//nolint:wsl_v5 // Protocol fixtures keep each action adjacent to its state assertions.
package planreview_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/planmode"
	"github.com/rsbin1178/pips/internal/coding/planreview"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCatalogRegistersThePlanModeToolPair(t *testing.T) {
	t.Parallel()

	controller := newTestController(t, planmode.StateInactive)

	value, err := controller.Catalog()
	require.NoError(t, err)

	policy := catalog.AllowAll("test", catalog.RiskPrivileged)
	descriptors, err := value.Search(t.Context(), policy, "")
	require.NoError(t, err)
	require.Len(t, descriptors, 2)

	for index, name := range []string{planmode.EnterToolName, planmode.ExitToolName} {
		assert.Equal(t, name, descriptors[index].Name)
		assert.Equal(t, catalog.Source{
			Kind: catalog.SourceLocal,
			ID:   tools.PlanCatalogID,
		}, descriptors[index].Source)
		assert.Equal(t, catalog.RiskWrite, descriptors[index].Risk)
	}

	snapshot, err := value.Snapshot(t.Context(), policy)
	require.NoError(t, err)
	require.Len(t, snapshot, 2)

	for _, tool := range snapshot {
		schema := tool.Decl().InputSchema
		require.NotNil(t, schema, tool.Decl().Name)
		assert.Equal(t, "object", schema.Type)
		assert.Empty(t, schema.Properties)
		assert.Empty(t, schema.Required)
		assert.Equal(t, false, schema.AdditionalProperties)
		assert.JSONEq(t, `0`, string(schema.Extra["maxProperties"]))
	}
}

func TestBeforeToolPausesOnlyOnPendingPlanDecisions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		state      planmode.State
		tool       string
		args       string
		batchSize  int
		want       agent.ToolDecisionAction
		wantReason string
	}{
		{
			name: "enter in inactive pauses", state: planmode.StateInactive,
			tool: planmode.EnterToolName, args: `{}`, want: agent.ToolDecisionPause,
		},
		{
			name: "enter in pending passes through", state: planmode.StatePending,
			tool: planmode.EnterToolName, args: `{}`, want: agent.ToolDecisionAllow,
		},
		{
			name: "enter in active passes through", state: planmode.StateActive,
			tool: planmode.EnterToolName, args: `{}`, want: agent.ToolDecisionAllow,
		},
		{
			name: "enter in exit pending passes through", state: planmode.StateExitPending,
			tool: planmode.EnterToolName, args: `{}`, want: agent.ToolDecisionAllow,
		},
		{
			name: "exit in inactive is denied", state: planmode.StateInactive,
			tool: planmode.ExitToolName, args: `{}`, want: agent.ToolDecisionDeny,
			wantReason: "exit_plan_mode requires plan mode to be active",
		},
		{
			name: "exit in pending is denied", state: planmode.StatePending,
			tool: planmode.ExitToolName, args: `{}`, want: agent.ToolDecisionDeny,
			wantReason: "exit_plan_mode requires plan mode to be active",
		},
		{
			name: "exit in active pauses", state: planmode.StateActive,
			tool: planmode.ExitToolName, args: `{}`, want: agent.ToolDecisionPause,
		},
		{
			name: "exit in exit pending pauses", state: planmode.StateExitPending,
			tool: planmode.ExitToolName, args: `{}`, want: agent.ToolDecisionPause,
		},
		{
			name: "enter arguments are rejected", state: planmode.StateInactive,
			tool: planmode.EnterToolName, args: `{"path":"plan.md"}`,
			want: agent.ToolDecisionDeny, wantReason: "arguments must be empty",
		},
		{
			name: "exit arguments are rejected", state: planmode.StateActive,
			tool: planmode.ExitToolName, args: `{"decision":"approve"}`,
			want: agent.ToolDecisionDeny, wantReason: "arguments must be empty",
		},
		{
			name: "undecodable arguments are rejected", state: planmode.StateInactive,
			tool: planmode.EnterToolName, args: `{`,
			want: agent.ToolDecisionDeny, wantReason: "arguments must be empty",
		},
		{
			name: "enter must be called alone", state: planmode.StateInactive,
			tool: planmode.EnterToolName, args: `{}`, batchSize: 2,
			want: agent.ToolDecisionDeny, wantReason: "enter_plan_mode must be called alone",
		},
		{
			name: "exit must be called alone", state: planmode.StateActive,
			tool: planmode.ExitToolName, args: `{}`, batchSize: 3,
			want: agent.ToolDecisionDeny, wantReason: "exit_plan_mode must be called alone",
		},
		{
			name: "unrelated tool passes through", state: planmode.StateActive,
			tool: "apply_patch", args: `{"patch":"*** Begin Patch\n*** End Patch"}`,
			want: agent.ToolDecisionAllow,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			controller := newTestController(t, test.state)
			decision := controller.BeforeTool(t.Context(), agent.ToolCallInfo{
				ToolCall:  agent.ToolCall{ID: "call-1", Name: test.tool, Args: ai.JSON(test.args)},
				BatchSize: test.batchSize,
			})
			assert.Equal(t, test.want, decision.Action)
			if test.wantReason != "" {
				assert.Contains(t, decision.Reason, test.wantReason)
			}
		})
	}
}

func TestBeforeToolFailsClosedWithoutAController(t *testing.T) {
	t.Parallel()

	var controller *planreview.Controller

	assert.Equal(t,
		agent.ToolDecisionDeny,
		controller.BeforeTool(t.Context(), agent.ToolCallInfo{
			ToolCall: agent.ToolCall{Name: planmode.EnterToolName, Args: ai.JSON(`{}`)},
		}).Action,
	)
	assert.Equal(t,
		agent.ToolDecisionDeny,
		controller.BeforeTool(t.Context(), agent.ToolCallInfo{
			ToolCall: agent.ToolCall{Name: planmode.ExitToolName, Args: ai.JSON(`{}`)},
		}).Action,
	)
	assert.Equal(t,
		agent.ToolDecisionAllow,
		controller.BeforeTool(t.Context(), agent.ToolCallInfo{
			ToolCall: agent.ToolCall{Name: "read"},
		}).Action,
	)

	_, err := controller.Catalog()
	require.Error(t, err)
	_, err = controller.Reconcile(t.Context(), nil)
	require.Error(t, err)
	_, err = controller.Resolve(planreview.Resolution{})
	require.Error(t, err)
	assert.Nil(t, controller.Pending())
}

func TestAdmittedEnterCallExecutesThroughTheService(t *testing.T) {
	t.Parallel()

	service := &stubService{state: planmode.StateActive, enterResult: "Plan mode is already active."}
	controller := newController(t, newStore(t), service, &recordingResolver{})

	decision := controller.BeforeTool(t.Context(), agent.ToolCallInfo{
		ToolCall: agent.ToolCall{ID: "enter-1", Name: planmode.EnterToolName, Args: ai.JSON(`{}`)},
	})
	require.Equal(t, agent.ToolDecisionAllow, decision.Action)

	catalogValue, err := controller.Catalog()
	require.NoError(t, err)
	snapshot, err := catalogValue.Snapshot(
		t.Context(),
		catalog.AllowAll("test", catalog.RiskPrivileged),
	)
	require.NoError(t, err)

	result, err := snapshot[0].Exec(t.Context(), agent.ToolCall{
		ID: "enter-1", Name: planmode.EnterToolName, Args: ai.JSON(`{}`),
	})
	require.NoError(t, err)
	assert.Equal(t, "Plan mode is already active.", resultTextParts(result))
	assert.Equal(t, 1, service.enters)
}

func TestReconcileBindsExitRequestsToPlanFileContent(t *testing.T) {
	t.Parallel()

	const content = "# Plan\n\n1. Implement\n2. Verify\n"

	store := newStore(t)
	controller := newController(t, store, &stubService{state: planmode.StateActive}, &recordingResolver{})
	call := ai.ToolCallPart{ID: "exit-1", Name: planmode.ExitToolName, Args: ai.JSON(`{}`)}

	// A missing plan file is an empty request, never an error.
	request, err := controller.Reconcile(t.Context(), []ai.ToolCallPart{call})
	require.NoError(t, err)
	require.NotNil(t, request)
	assert.Equal(t, planreview.KindExit, request.Kind)
	assert.Equal(t, "exit-1", request.ToolCallID)
	assert.False(t, request.HasContent)
	assert.Empty(t, request.Content)
	assert.Zero(t, request.Size)
	require.NoError(t, planreview.ValidateRequest(*request))
	require.NotNil(t, controller.Pending())
	assert.Equal(t, *request, *controller.Pending())

	_, err = store.Replace(t.Context(), content)
	require.NoError(t, err)

	request, err = controller.Reconcile(t.Context(), []ai.ToolCallPart{call})
	require.NoError(t, err)
	require.NotNil(t, request)
	assert.True(t, request.HasContent)
	assert.Equal(t, content, request.Content)
	assert.Equal(t, int64(len(content)), request.Size)
	assert.Equal(t, request.ID, controller.Pending().ID)
}

func TestReconcileEnterRequestsNeverCarryPlanContent(t *testing.T) {
	t.Parallel()

	store := newStore(t)
	_, err := store.Replace(t.Context(), "# Plan\n\nHidden from the entry request.\n")
	require.NoError(t, err)

	controller := newController(t, store, &stubService{}, &recordingResolver{})
	call := ai.ToolCallPart{ID: "enter-1", Name: planmode.EnterToolName, Args: ai.JSON(`{}`)}

	request, err := controller.Reconcile(t.Context(), []ai.ToolCallPart{call})
	require.NoError(t, err)
	require.NotNil(t, request)
	assert.Equal(t, planreview.KindEnter, request.Kind)
	assert.Equal(t, "enter-1", request.ToolCallID)
	assert.False(t, request.HasContent)
	assert.Empty(t, request.Content)
}

func TestReconcileRejectsAmbiguousOrMalformedPendingCalls(t *testing.T) {
	t.Parallel()

	controller := newController(
		t,
		newStore(t),
		&stubService{state: planmode.StateActive},
		&recordingResolver{},
	)

	// Two plan-mode calls cannot share one decision.
	_, err := controller.Reconcile(t.Context(), []ai.ToolCallPart{
		{ID: "exit-1", Name: planmode.ExitToolName, Args: ai.JSON(`{}`)},
		{ID: "exit-2", Name: planmode.ExitToolName, Args: ai.JSON(`{}`)},
	})
	require.Error(t, err)

	// Arguments are part of the durable call identity.
	_, err = controller.Reconcile(t.Context(), []ai.ToolCallPart{
		{ID: "exit-3", Name: planmode.ExitToolName, Args: ai.JSON(`{"path":"plan.md"}`)},
	})
	require.ErrorIs(t, err, planreview.ErrInvalid)

	_, err = controller.Reconcile(t.Context(), []ai.ToolCallPart{
		{Name: planmode.ExitToolName, Args: ai.JSON(`{}`)},
	})
	require.ErrorIs(t, err, planreview.ErrInvalid)

	// Unrelated pending work never becomes a plan decision.
	request, err := controller.Reconcile(t.Context(), []ai.ToolCallPart{
		{ID: "read-1", Name: "read", Args: ai.JSON(`{}`)},
	})
	require.NoError(t, err)
	assert.Nil(t, request)
	assert.Nil(t, controller.Pending())

	// Reconciliation without pending calls clears any displayed request.
	call := ai.ToolCallPart{ID: "exit-4", Name: planmode.ExitToolName, Args: ai.JSON(`{}`)}
	_, err = controller.Reconcile(t.Context(), []ai.ToolCallPart{call})
	require.NoError(t, err)
	require.NotNil(t, controller.Pending())

	request, err = controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	assert.Nil(t, request)
	assert.Nil(t, controller.Pending())
}

func TestResolveRequiresTheExactPendingRequest(t *testing.T) {
	t.Parallel()

	controller := newController(
		t,
		newStore(t),
		&stubService{state: planmode.StateActive},
		&recordingResolver{},
	)

	_, err := controller.Resolve(planreview.Resolution{
		RequestID: "missing", Decision: planreview.DecisionApprove,
	})
	require.ErrorIs(t, err, planreview.ErrNoPending)

	request, err := controller.Reconcile(t.Context(), []ai.ToolCallPart{
		{ID: "enter-1", Name: planmode.EnterToolName, Args: ai.JSON(`{}`)},
	})
	require.NoError(t, err)
	require.NotNil(t, request)

	_, err = controller.Resolve(planreview.Resolution{
		RequestID: "plan-other", Decision: planreview.DecisionApprove,
	})
	require.ErrorIs(t, err, planreview.ErrMismatch)
	assert.Equal(t, request.ID, controller.Pending().ID)
}

func TestResolveRejectsDecisionsThatViolateTheRequestShape(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		kind       planreview.Kind
		decision   planreview.Decision
		comments   []string
		notes      string
		wantErr    error
		wantReason string
	}{
		{
			name: "enter carries no comments", kind: planreview.KindEnter,
			decision: planreview.DecisionApprove, comments: []string{"looks good"},
			wantErr: planreview.ErrInvalid, wantReason: "no comments",
		},
		{
			name: "enter carries no notes", kind: planreview.KindEnter,
			decision: planreview.DecisionDecline, notes: "change it",
			wantErr: planreview.ErrInvalid, wantReason: "no comments",
		},
		{
			name: "enter only approves or declines", kind: planreview.KindEnter,
			decision: planreview.DecisionRevise,
			wantErr:  planreview.ErrInvalid, wantReason: "unsupported entry decision",
		},
		{
			name: "exit approval carries no revision notes", kind: planreview.KindExit,
			decision: planreview.DecisionApprove, notes: "almost",
			wantErr: planreview.ErrInvalid, wantReason: "approval cannot include revision notes",
		},
		{
			name: "exit revision carries notes only", kind: planreview.KindExit,
			decision: planreview.DecisionRevise, comments: []string{"tighten step 2"},
			wantErr: planreview.ErrInvalid, wantReason: "revision requests carry notes only",
		},
		{
			name: "exit revision notes are bounded", kind: planreview.KindExit,
			decision: planreview.DecisionRevise, notes: strings.Repeat("a", 16<<10+1),
			wantErr: planreview.ErrInvalid, wantReason: "revision notes are too large",
		},
		{
			name: "exit quit carries no feedback", kind: planreview.KindExit,
			decision: planreview.DecisionQuit, comments: []string{"stop"},
			wantErr: planreview.ErrInvalid, wantReason: "abandoning carries no feedback",
		},
		{
			name: "exit quit carries no notes", kind: planreview.KindExit,
			decision: planreview.DecisionQuit, notes: "stop",
			wantErr: planreview.ErrInvalid, wantReason: "abandoning carries no feedback",
		},
		{
			name: "exit approval comments are bounded", kind: planreview.KindExit,
			decision: planreview.DecisionApprove, comments: []string{strings.Repeat("a", 4<<10+1)},
			wantErr: planreview.ErrInvalid, wantReason: "review comment is too large",
		},
		{
			name: "exit approval comment count is bounded", kind: planreview.KindExit,
			decision: planreview.DecisionApprove, comments: make([]string, 129),
			wantErr: planreview.ErrInvalid, wantReason: "too many review comments",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			resolver := &recordingResolver{}
			controller := newController(
				t,
				newStore(t),
				&stubService{state: planmode.StateActive},
				resolver,
			)
			request, err := controller.Reconcile(t.Context(), []ai.ToolCallPart{
				{ID: "call-1", Name: toolNameFor(test.kind), Args: ai.JSON(`{}`)},
			})
			require.NoError(t, err)
			require.NotNil(t, request)

			_, err = controller.Resolve(planreview.Resolution{
				RequestID: request.ID,
				Decision:  test.decision,
				Comments:  test.comments,
				Notes:     test.notes,
			})
			require.ErrorIs(t, err, test.wantErr)
			assert.Contains(t, err.Error(), test.wantReason)
			assert.Empty(t, resolver.values)
			assert.NotNil(t, controller.Pending())
		})
	}
}

func TestResolveWritesTheExactDecisionResultText(t *testing.T) {
	t.Parallel()

	const content = "# Plan\n\nShip it safely.\n"
	comments := []string{"Name the rollback path.", "Add a verification step."}

	tests := []struct {
		name     string
		content  string
		kind     planreview.Kind
		decision planreview.Decision
		comments []string
		notes    string
		want     string
	}{
		{
			name: "enter approved", kind: planreview.KindEnter,
			decision: planreview.DecisionApprove,
			want: "You have entered plan mode. You should now focus on exploring the codebase " +
				"and creating an implementation plan.",
		},
		{
			name: "enter declined", kind: planreview.KindEnter,
			decision: planreview.DecisionDecline,
			want:     "The user declined to enter plan mode. Continue in normal mode without it.",
		},
		{
			name: "exit approved", content: content, kind: planreview.KindExit,
			decision: planreview.DecisionApprove,
			want:     "Your plan has been approved. You can now start coding.",
		},
		{
			name: "exit approved with review comments", content: content,
			kind: planreview.KindExit, decision: planreview.DecisionApprove, comments: comments,
			want: "Your plan has been approved. You can now start coding.\n\n" +
				"The user approved the plan with the following review comments:\n" +
				"Name the rollback path.\nAdd a verification step.",
		},
		{
			name: "exit approved without plan content", kind: planreview.KindExit,
			decision: planreview.DecisionApprove,
			want:     "Plan mode exit approved. No plan content was found - you can proceed.",
		},
		{
			name: "exit approved without content ignores comments", kind: planreview.KindExit,
			decision: planreview.DecisionApprove, comments: comments,
			want: "Plan mode exit approved. No plan content was found - you can proceed.",
		},
		{
			name: "exit revised with notes", content: content, kind: planreview.KindExit,
			decision: planreview.DecisionRevise, notes: "Fold rollback into step 2.",
			want: "The user does not want to exit plan mode. Continue planning and ask " +
				"the user what they would like to do.\n\nUser revision notes:\nFold rollback into step 2.",
		},
		{
			name: "exit revised without notes", content: content, kind: planreview.KindExit,
			decision: planreview.DecisionRevise,
			want: "The user does not want to exit plan mode. Continue planning and ask " +
				"the user what they would like to do.",
		},
		{
			name: "exit quit", content: content, kind: planreview.KindExit,
			decision: planreview.DecisionQuit,
			want: "The user chose to abandon the plan entirely (via the Abandon option in " +
				"the plan approval dialog). Plan mode has been disabled. Do not call " +
				"exit_plan_mode again unless the user explicitly asks to re-enter plan mode.",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			store := newStore(t)
			if test.content != "" {
				_, err := store.Replace(t.Context(), test.content)
				require.NoError(t, err)
			}

			resolver := &recordingResolver{}
			controller := newController(
				t,
				store,
				&stubService{state: planmode.StateActive},
				resolver,
			)
			request, err := controller.Reconcile(t.Context(), []ai.ToolCallPart{
				{ID: "call-1", Name: toolNameFor(test.kind), Args: ai.JSON(`{}`)},
			})
			require.NoError(t, err)
			require.NotNil(t, request)

			outcome, err := controller.Resolve(planreview.Resolution{
				RequestID: request.ID,
				Decision:  test.decision,
				Comments:  test.comments,
				Notes:     test.notes,
			})
			require.NoError(t, err)
			assert.Equal(t, planreview.Outcome{Kind: test.kind, Decision: test.decision}, outcome)

			require.Len(t, resolver.values, 1)
			assert.Equal(t, request.ToolCallID, resolver.values[0].ToolCallID)
			assert.Equal(t, test.want, resultText(resolver.values[0].Content))
			assert.Nil(t, controller.Pending())
		})
	}
}

func TestResolveKeepsPendingWhenPersistenceFails(t *testing.T) {
	t.Parallel()

	resolver := &recordingResolver{err: errors.New("session is closed")}
	controller := newController(t, newStore(t), &stubService{}, resolver)

	request, err := controller.Reconcile(t.Context(), []ai.ToolCallPart{
		{ID: "enter-1", Name: planmode.EnterToolName, Args: ai.JSON(`{}`)},
	})
	require.NoError(t, err)
	require.NotNil(t, request)

	_, err = controller.Resolve(planreview.Resolution{
		RequestID: request.ID, Decision: planreview.DecisionApprove,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "persist resolution")
	assert.NotNil(t, controller.Pending())
}

func TestCloneHelpersReturnIndependentValues(t *testing.T) {
	t.Parallel()

	request, err := planreview.NewRequest(planreview.KindExit, "exit-1", "# Plan\n")
	require.NoError(t, err)
	assert.Equal(t, request, planreview.CloneRequest(request))

	resolution := planreview.Resolution{
		RequestID: "plan-1", Decision: planreview.DecisionApprove,
		Comments: []string{"one"},
	}
	cloned := planreview.CloneResolution(resolution)
	cloned.Comments[0] = "two"
	assert.Equal(t, []string{"one"}, resolution.Comments)
}

func TestValidDecisionMatchesTheRequestKind(t *testing.T) {
	t.Parallel()

	assert.True(t, planreview.ValidDecision(planreview.KindEnter, planreview.DecisionApprove))
	assert.True(t, planreview.ValidDecision(planreview.KindEnter, planreview.DecisionDecline))
	assert.False(t, planreview.ValidDecision(planreview.KindEnter, planreview.DecisionRevise))
	assert.False(t, planreview.ValidDecision(planreview.KindEnter, planreview.DecisionQuit))
	assert.True(t, planreview.ValidDecision(planreview.KindExit, planreview.DecisionApprove))
	assert.True(t, planreview.ValidDecision(planreview.KindExit, planreview.DecisionRevise))
	assert.True(t, planreview.ValidDecision(planreview.KindExit, planreview.DecisionQuit))
	assert.False(t, planreview.ValidDecision(planreview.KindExit, planreview.DecisionDecline))
	assert.False(t, planreview.ValidDecision(planreview.Kind("other"), planreview.DecisionApprove))
}

type stubService struct {
	state       planmode.State
	enterResult string
	exitResult  string
	enters      int
	exits       int
}

func (s *stubService) PlanState() planmode.State {
	if s.state.Valid() {
		return s.state
	}

	return planmode.StateInactive
}

func (s *stubService) EnterPlanMode(context.Context) (string, error) {
	s.enters++

	return s.enterResult, nil
}

func (s *stubService) ExitPlanMode(context.Context) (string, error) {
	s.exits++

	return s.exitResult, nil
}

type recordingResolver struct {
	values []agent.ToolResolution
	err    error
}

func (r *recordingResolver) ResolveToolCalls(values ...agent.ToolResolution) error {
	if r.err != nil {
		return r.err
	}

	r.values = append(r.values, values...)

	return nil
}

func newStore(t *testing.T) *planmode.Store {
	t.Helper()

	store, err := planmode.NewStore(t.TempDir(), "session-1", planmode.DefaultLimits())
	require.NoError(t, err)

	return store
}

func newController(
	t *testing.T,
	store *planmode.Store,
	service planreview.Service,
	resolver planreview.Resolver,
) *planreview.Controller {
	t.Helper()

	controller, err := planreview.NewController(store, service, resolver)
	require.NoError(t, err)

	return controller
}

func newTestController(t *testing.T, state planmode.State) *planreview.Controller {
	t.Helper()

	return newController(t, newStore(t), &stubService{state: state}, &recordingResolver{})
}

func toolNameFor(kind planreview.Kind) string {
	if kind == planreview.KindEnter {
		return planmode.EnterToolName
	}

	return planmode.ExitToolName
}

func resultText(parts []ai.Part) string {
	if len(parts) != 1 {
		return ""
	}
	value, _ := parts[0].(ai.TextPart)

	return value.Text
}

func resultTextParts(parts []ai.Part) string { return resultText(parts) }
