package approval

import (
	"context"
	"sync"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestControllerPermitsMultipleSerialCallsInOneAgentBatch(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t, false, config.ApprovalOnRequest)
	tool, ok := fixture.controller.Tool(controlledToolName)
	require.True(t, ok)

	model := &scriptedApprovalModel{responses: []*ai.Response{
		{
			Message: ai.Assistant(
				ai.ToolCallPart{ID: "call-1", Name: controlledToolName, Args: ai.JSON(`first`)},
				ai.ToolCallPart{ID: "call-2", Name: controlledToolName, Args: ai.JSON(`second`)},
			),
			FinishReason: ai.FinishToolCalls,
		},
		{Message: ai.AssistantText("done"), FinishReason: ai.FinishStop},
	}}
	runtime, err := agent.New(
		model,
		agent.WithTools(tool),
		agent.WithBeforeTool(fixture.controller.BeforeTool),
	)
	require.NoError(t, err)

	result, err := runtime.Run(t.Context(), agent.NewSession(), ai.UserText("run both"))
	require.NoError(t, err)
	assert.Equal(t, agent.StopEndTurn, result.Stop)
	assert.Equal(t, 2, fixture.executor.runCount)
	assert.Equal(t, []receiptEvent{
		eventRequested,
		eventStarted,
		eventRequested,
		eventStarted,
	}, receiptEvents(fixture.session.Path()))
}

func TestControllerPersistsThroughHarnessSession(t *testing.T) {
	t.Parallel()

	ws, err := workspace.Open(t.TempDir())
	require.NoError(t, err)

	session, err := harness.NewSession(harness.NewMemoryStore("approval-integration"))
	require.NoError(t, err)
	_, err = session.AppendMessage(ai.Assistant(
		ai.ToolCallPart{
			ID:   "call-1",
			Name: controlledToolName,
			Args: ai.JSON(`true`),
		},
	), nil)
	require.NoError(t, err)

	runtime, err := harness.New(unusedApprovalModel{}, session)
	require.NoError(t, err)
	policy, err := execution.NewPolicy(ws, execution.PolicyConfig{
		Sandbox:  config.SandboxWorkspaceWrite,
		Approval: config.ApprovalOnRequest,
	})
	require.NoError(t, err)

	handler := testHandler{writeDir: t.TempDir()}
	executor := newFakeControllerExecutor(session)
	controller, err := newController(
		ws,
		session,
		runtime,
		&fakePendingRunner{},
		policy,
		executor,
		handler,
	)
	require.NoError(t, err)

	state, err := controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	require.NotNil(t, state.Review)

	state, err = controller.Resolve(t.Context(), Resolution{
		RequestID: state.Review.RequestID,
		Choice:    ChoiceAllowOnce,
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, StateReady, state.Kind)
	assert.Equal(t, 1, executor.runCount)

	pending, err := session.Pending()
	require.NoError(t, err)
	assert.Empty(t, pending)
	assert.Equal(t, []receiptEvent{
		eventRequested,
		eventDecided,
		eventStarted,
		eventCompleted,
	}, receiptEvents(session.Path()))

	modelContext, err := session.Context()
	require.NoError(t, err)
	require.Len(t, modelContext.Messages, 2)
	assert.IsType(t, ai.AssistantMessage{}, modelContext.Messages[0])
	assert.IsType(t, ai.ToolMessage{}, modelContext.Messages[1])
}

func TestStateNonInteractiveError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		state State
		err   error
	}{
		{state: State{Kind: StateReady}},
		{state: State{Kind: StateReview}, err: ErrApprovalRequired},
		{state: State{Kind: StateUnknown}, err: ErrOutcomeUnknown},
		{state: State{}, err: ErrJournalCorrupt},
	}

	for _, test := range tests {
		err := test.state.NonInteractiveError()
		if test.err == nil {
			require.NoError(t, err)

			continue
		}

		require.ErrorIs(t, err, test.err)
	}
}

type unusedApprovalModel struct{}

func (unusedApprovalModel) Generate(context.Context, ai.Request) (*ai.Response, error) {
	return nil, errInjected
}

func (unusedApprovalModel) Stream(context.Context, ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		yield(ai.StreamEvent{}, errInjected)
	}
}

func (unusedApprovalModel) Provider() ai.Provider { return ai.Provider("test") }

func (unusedApprovalModel) ModelID() string { return "unused" }

func (unusedApprovalModel) Capabilities() ai.Capabilities { return ai.Capabilities{} }

type scriptedApprovalModel struct {
	mutex     sync.Mutex
	responses []*ai.Response
}

func (m *scriptedApprovalModel) Generate(context.Context, ai.Request) (*ai.Response, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if len(m.responses) == 0 {
		return nil, errInjected
	}

	response := m.responses[0]
	m.responses = m.responses[1:]

	return response, nil
}

func (m *scriptedApprovalModel) Stream(context.Context, ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		yield(ai.StreamEvent{}, errInjected)
	}
}

func (*scriptedApprovalModel) Provider() ai.Provider { return ai.Provider("test") }

func (*scriptedApprovalModel) ModelID() string { return "scripted" }

func (*scriptedApprovalModel) Capabilities() ai.Capabilities { return ai.Capabilities{} }
