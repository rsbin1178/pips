package approval

import (
	"strings"
	"testing"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/execution"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestControllerReviewAllowOncePersistsExecutionOrder(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t, true, config.ApprovalOnRequest)
	call := controlledCall("call-1", `printf success`)
	fixture.session.addPending(call)

	state, err := fixture.controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	require.Equal(t, StateReview, state.Kind)
	require.NotNil(t, state.Review)
	assert.Equal(t, call.ID, state.Review.Call.ID)
	assert.Zero(t, fixture.executor.runCount)

	state, err = fixture.controller.Resolve(t.Context(), Resolution{
		RequestID: state.Review.RequestID,
		Choice:    ChoiceAllowOnce,
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, StateReady, state.Kind)
	assert.Equal(t, 1, fixture.executor.prepareCount)
	assert.Equal(t, 1, fixture.executor.runCount)
	assert.True(t, fixture.executor.startedBeforeRun)
	assert.Equal(t, []receiptEvent{
		eventRequested,
		eventDecided,
		eventStarted,
		eventCompleted,
	}, receiptEvents(fixture.session.Path()))

	path := fixture.session.Path()
	require.Len(t, path, 5)
	assert.Equal(t, harness.KindMessage, path[3].Kind)
	assert.Equal(t, eventCompleted, receiptEvents(path[4:])[0])
	assert.NotContains(t, string(path[0].Data), "printf success")
}

func TestControllerDoesNotRunWhenStartedReceiptFails(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t, true, config.ApprovalOnRequest)
	fixture.session.addPending(controlledCall("call-1", `true`))

	state, err := fixture.controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	require.NotNil(t, state.Review)

	fixture.session.appendFailures[eventStarted] = 1
	_, err = fixture.controller.Resolve(t.Context(), Resolution{
		RequestID: state.Review.RequestID,
		Choice:    ChoiceAllowOnce,
	}, nil)
	require.ErrorIs(t, err, errInjected)
	assert.Zero(t, fixture.executor.runCount)
	assert.Equal(t, 1, fixture.executor.closeCount)

	state, err = fixture.controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	assert.Equal(t, StateReady, state.Kind)
	assert.Equal(t, 2, fixture.executor.prepareCount)
	assert.Equal(t, 1, fixture.executor.runCount)
}

func TestControllerRepairsCompletionAfterDurableResult(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t, true, config.ApprovalOnRequest)
	fixture.session.addPending(controlledCall("call-1", `true`))

	state, err := fixture.controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	require.NotNil(t, state.Review)

	fixture.session.appendFailures[eventCompleted] = 1
	_, err = fixture.controller.Resolve(t.Context(), Resolution{
		RequestID: state.Review.RequestID,
		Choice:    ChoiceAllowOnce,
	}, nil)
	require.ErrorIs(t, err, errInjected)
	assert.Equal(t, 1, fixture.executor.runCount)
	require.Empty(t, fixture.session.pending)

	state, err = fixture.controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	assert.Equal(t, StateReady, state.Kind)
	assert.Equal(t, 1, fixture.executor.runCount)
	assert.Equal(t, eventCompleted, receiptEvents(fixture.session.Path())[3])
}

func TestControllerRepairsDeniedCompletionAfterDurableResult(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t, true, config.ApprovalOnRequest)
	fixture.session.addPending(controlledCall("call-1", `true`))

	state, err := fixture.controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	require.NotNil(t, state.Review)

	fixture.session.appendFailures[eventCompleted] = 1
	_, err = fixture.controller.Resolve(t.Context(), Resolution{
		RequestID: state.Review.RequestID,
		Choice:    ChoiceDeny,
	}, nil)
	require.ErrorIs(t, err, errInjected)
	assert.Zero(t, fixture.executor.runCount)
	require.Empty(t, fixture.session.pending)

	state, err = fixture.controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	assert.Equal(t, StateReady, state.Kind)
	assert.Equal(t, []receiptEvent{
		eventRequested,
		eventDecided,
		eventCompleted,
	}, receiptEvents(fixture.session.Path()))
}

func TestControllerDenyReasonIsDurableAndModelVisible(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t, true, config.ApprovalOnRequest)
	fixture.session.addPending(controlledCall("call-1", `true`))

	state, err := fixture.controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	require.NotNil(t, state.Review)

	const reason = "repository policy denied this approval"
	state, err = fixture.controller.Resolve(t.Context(), Resolution{
		RequestID: state.Review.RequestID,
		Choice:    ChoiceDeny,
		Reason:    reason,
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, StateReady, state.Kind)

	replay := replayJournal(fixture.session.Path())
	assert.False(t, replay.tainted)
	require.Len(t, replay.lifecycles, 1)
	assert.Equal(t, reason, replay.lifecycles[0].denialReason)

	require.Len(t, fixture.session.resolved, 1)
	resolution := fixture.session.resolved[0]
	assert.True(t, resolution.IsError)
	var foundReason bool
	for _, part := range resolution.Content {
		text, ok := part.(ai.TextPart)
		if ok && strings.Contains(text.Text, reason) {
			foundReason = true
			break
		}
	}
	assert.True(t, foundReason)
}

func TestControllerRejectsInvalidDenialReason(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t, true, config.ApprovalOnRequest)
	fixture.session.addPending(controlledCall("call-1", `true`))

	state, err := fixture.controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	require.NotNil(t, state.Review)

	tests := []struct {
		name       string
		choice     Choice
		denialNote string
	}{
		{name: "reason on allowance", choice: ChoiceAllowOnce, denialNote: "not permitted"},
		{name: "control character", choice: ChoiceDeny, denialNote: "not permitted\n"},
		{name: "oversized reason", choice: ChoiceDeny, denialNote: strings.Repeat("x", maxDecisionReasonBytes+1)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := fixture.controller.Resolve(t.Context(), Resolution{
				RequestID: state.Review.RequestID,
				Choice:    test.choice,
				Reason:    test.denialNote,
			}, nil)
			require.ErrorIs(t, err, ErrInvalidResolution)
		})
	}

	assert.Equal(t, []receiptEvent{eventRequested}, receiptEvents(fixture.session.Path()))
	assert.Zero(t, fixture.executor.runCount)
}

func TestControllerUnknownRequiresExplicitRetry(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t, true, config.ApprovalOnRequest)
	fixture.session.addPending(controlledCall("call-1", `true`))

	state, err := fixture.controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	require.NotNil(t, state.Review)

	fixture.session.resolveFailures = 1
	_, err = fixture.controller.Resolve(t.Context(), Resolution{
		RequestID: state.Review.RequestID,
		Choice:    ChoiceAllowOnce,
	}, nil)
	require.ErrorIs(t, err, errInjected)
	assert.Equal(t, 1, fixture.executor.runCount)

	state, err = fixture.controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	require.Equal(t, StateUnknown, state.Kind)
	require.NotNil(t, state.Unknown)
	assert.True(t, state.Unknown.Pending)
	assert.Equal(t, 1, state.Unknown.Attempt)
	assert.Equal(t, 1, fixture.executor.runCount)

	state, err = fixture.controller.Resolve(t.Context(), Resolution{
		RequestID: state.Unknown.RequestID,
		Choice:    ChoiceRetry,
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, StateReady, state.Kind)
	assert.Equal(t, 2, fixture.executor.runCount)
	assert.Equal(t, []Choice{ChoiceAllowOnce, ChoiceRetry}, receiptChoices(fixture.session.Path()))
}

func TestControllerRetryDecisionSurvivesPrepareFailure(t *testing.T) {
	t.Parallel()

	fixture, unknown := unknownPendingFixture(t)
	fixture.executor.prepareErr = errInjected

	_, err := fixture.controller.Resolve(t.Context(), Resolution{
		RequestID: unknown.RequestID,
		Choice:    ChoiceRetry,
	}, nil)
	require.ErrorIs(t, err, errInjected)
	assert.Equal(t, 1, fixture.executor.runCount)

	state, err := fixture.controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	require.NotNil(t, state.Unknown)

	state, err = fixture.controller.Resolve(t.Context(), Resolution{
		RequestID: state.Unknown.RequestID,
		Choice:    ChoiceRetry,
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, StateReady, state.Kind)
	assert.Equal(t, 2, fixture.executor.runCount)
	assert.Equal(t, []Choice{ChoiceAllowOnce, ChoiceRetry}, receiptChoices(fixture.session.Path()))
}

func TestControllerMarkFailedAndOrphanAcknowledge(t *testing.T) {
	t.Parallel()

	t.Run("pending", func(t *testing.T) {
		t.Parallel()

		fixture, unknown := unknownPendingFixture(t)
		fixture.session.pending[0].Name = "changed-handler"

		state, err := fixture.controller.Resolve(t.Context(), Resolution{
			RequestID: unknown.RequestID,
			Choice:    ChoiceMarkFailed,
		}, nil)
		require.NoError(t, err)
		assert.Equal(t, StateReady, state.Kind)
		assert.Equal(t, 1, fixture.executor.runCount)
		require.Len(t, fixture.session.resolved, 1)
		assert.True(t, fixture.session.resolved[0].IsError)
	})

	t.Run("orphan", func(t *testing.T) {
		t.Parallel()

		fixture, unknown := unknownPendingFixture(t)
		fixture.session.removePending(unknown.CallID)

		state, err := fixture.controller.Reconcile(t.Context(), nil)
		require.NoError(t, err)
		require.NotNil(t, state.Unknown)
		assert.False(t, state.Unknown.Pending)

		_, err = fixture.controller.Resolve(t.Context(), Resolution{
			RequestID: state.Unknown.RequestID,
			Choice:    ChoiceRetry,
		}, nil)
		require.ErrorIs(t, err, ErrInvalidResolution)

		state, err = fixture.controller.Resolve(t.Context(), Resolution{
			RequestID: state.Unknown.RequestID,
			Choice:    ChoiceAcknowledge,
		}, nil)
		require.NoError(t, err)
		assert.Equal(t, StateReady, state.Kind)
		assert.Equal(t, 1, fixture.executor.runCount)
	})
}

func TestControllerSessionGrantMatchesExactFingerprint(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t, true, config.ApprovalOnRequest)
	fixture.session.addPending(controlledCall("call-1", `same operation`))

	state, err := fixture.controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	require.NotNil(t, state.Review)

	state, err = fixture.controller.Resolve(t.Context(), Resolution{
		RequestID: state.Review.RequestID,
		Choice:    ChoiceAllowSession,
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, StateReady, state.Kind)

	fixture.session.addPending(controlledCall("call-2", `same operation`))
	state, err = fixture.controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	assert.Equal(t, StateReady, state.Kind)
	assert.Equal(t, 2, fixture.executor.runCount)

	fixture.session.addPending(controlledCall("call-3", `changed operation`))
	state, err = fixture.controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	assert.Equal(t, StateReview, state.Kind)
	assert.Equal(t, 2, fixture.executor.runCount)
}

func TestControllerSessionGrantSurvivesControllerRestart(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t, true, config.ApprovalOnRequest)
	fixture.session.addPending(controlledCall("call-1", `same operation`))

	state, err := fixture.controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	require.NotNil(t, state.Review)

	state, err = fixture.controller.Resolve(t.Context(), Resolution{
		RequestID: state.Review.RequestID,
		Choice:    ChoiceAllowSession,
	}, nil)
	require.NoError(t, err)
	require.Equal(t, StateReady, state.Kind)

	fixture.session.addPending(controlledCall("call-2", `same operation`))
	restartedExecutor := newFakeControllerExecutor(fixture.session)
	restarted, err := newController(
		fixture.controller.workspace,
		fixture.session,
		fixture.session,
		&fakePendingRunner{},
		fixture.controller.policy,
		restartedExecutor,
		fixture.handler,
	)
	require.NoError(t, err)

	state, err = restarted.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	assert.Equal(t, StateReady, state.Kind)
	assert.Equal(t, 1, restartedExecutor.runCount)
}

func TestControllerRestartExposesUnknownWithoutReexecution(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t, true, config.ApprovalOnRequest)
	fixture.session.addPending(controlledCall("call-1", `true`))

	state, err := fixture.controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	require.NotNil(t, state.Review)

	fixture.session.resolveFailures = 1
	_, err = fixture.controller.Resolve(t.Context(), Resolution{
		RequestID: state.Review.RequestID,
		Choice:    ChoiceAllowOnce,
	}, nil)
	require.ErrorIs(t, err, errInjected)
	require.Equal(t, 1, fixture.executor.runCount)

	restartedExecutor := newFakeControllerExecutor(fixture.session)
	restarted, err := newController(
		fixture.controller.workspace,
		fixture.session,
		fixture.session,
		&fakePendingRunner{},
		fixture.controller.policy,
		restartedExecutor,
		fixture.handler,
	)
	require.NoError(t, err)

	state, err = restarted.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	require.Equal(t, StateUnknown, state.Kind)
	require.NotNil(t, state.Unknown)
	assert.Equal(t, 1, state.Unknown.Attempt)
	assert.Zero(t, restartedExecutor.prepareCount)
	assert.Zero(t, restartedExecutor.runCount)
}

func TestControllerStopsPendingSuffixAtReviewBarrier(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t, true, config.ApprovalOnRequest)
	trace := []string(nil)
	fixture.pending.trace = &trace
	fixture.executor.trace = &trace
	fixture.session.addPending(
		ai.ToolCallPart{ID: "ordinary-1", Name: "read", Args: ai.JSON(`{}`)},
		controlledCall("call-1", `true`),
		ai.ToolCallPart{ID: "ordinary-2", Name: "ls", Args: ai.JSON(`{}`)},
	)

	state, err := fixture.controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	require.NotNil(t, state.Review)
	assert.Equal(t, []string{"pending:ordinary-1"}, trace)
	assert.Equal(t, []string{"ordinary-1"}, fixture.pending.calls)

	state, err = fixture.controller.Resolve(t.Context(), Resolution{
		RequestID: state.Review.RequestID,
		Choice:    ChoiceAllowOnce,
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, StateReady, state.Kind)
	assert.Equal(t, []string{"pending:ordinary-1", "controlled", "pending:ordinary-2"}, trace)
}

func TestControllerDirectExecutionRepairsAfterHarnessPersistence(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t, false, config.ApprovalOnRequest)
	tool, ok := fixture.controller.Tool(controlledToolName)
	require.True(t, ok)

	call := agent.ToolCall{ID: "call-1", Name: controlledToolName, Args: ai.JSON(`true`)}
	decision := fixture.controller.BeforeTool(t.Context(), agent.ToolCallInfo{ToolCall: call})
	assert.Equal(t, agent.ToolDecisionAllow, decision.Action)

	parts, err := tool.Exec(t.Context(), call)
	require.NoError(t, err)
	require.NotEmpty(t, parts)
	assert.Equal(t, 1, fixture.executor.runCount)
	assert.Equal(t, []receiptEvent{eventRequested, eventStarted}, receiptEvents(fixture.session.Path()))

	nextCall := agent.ToolCall{ID: "call-2", Name: controlledToolName, Args: ai.JSON(`true`)}
	decision = fixture.controller.BeforeTool(t.Context(), agent.ToolCallInfo{ToolCall: nextCall})
	assert.Equal(t, agent.ToolDecisionPause, decision.Action)
	decision = fixture.controller.BeforeTool(t.Context(), agent.ToolCallInfo{
		ToolCall: agent.ToolCall{ID: "ordinary", Name: "read", Args: ai.JSON(`{}`)},
	})
	assert.Equal(t, agent.ToolDecisionPause, decision.Action)

	state, err := fixture.controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	require.NotNil(t, state.Unknown)
	assert.False(t, state.Unknown.Pending)

	fixture.session.appendToolResult(call, false)
	state, err = fixture.controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	assert.Equal(t, StateReady, state.Kind)
	assert.Equal(t, 1, fixture.executor.runCount)
	assert.Equal(t, eventCompleted, receiptEvents(fixture.session.Path())[2])
}

func TestControlledToolRequiresOneShotGatePermit(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t, false, config.ApprovalOnRequest)
	tool, ok := fixture.controller.Tool(controlledToolName)
	require.True(t, ok)

	call := agent.ToolCall{ID: "call-1", Name: controlledToolName, Args: ai.JSON(`true`)}

	_, err := tool.Exec(t.Context(), call)
	require.ErrorIs(t, err, ErrDenied)
	assert.Zero(t, fixture.executor.runCount)

	decision := fixture.controller.BeforeTool(t.Context(), agent.ToolCallInfo{ToolCall: call})
	require.Equal(t, agent.ToolDecisionAllow, decision.Action)

	changed := call
	changed.Args = ai.JSON(`changed`)
	_, err = tool.Exec(t.Context(), changed)
	require.ErrorIs(t, err, ErrDenied)
	assert.Zero(t, fixture.executor.runCount)

	_, err = tool.Exec(t.Context(), call)
	require.ErrorIs(t, err, ErrDenied)
	assert.Zero(t, fixture.executor.runCount)
}

func TestControllerMalformedJournalFailsClosed(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t, false, config.ApprovalOnRequest)
	entryID, err := fixture.session.AppendCustom(journalCustomType, ai.JSON(`{
		"event":"requested",
		"request_id":"00000000000000000000000000000000",
		"call_id":"call-1",
		"tool":"controlled",
		"fingerprint":"0000000000000000000000000000000000000000000000000000000000000000",
		"unexpected":true
	}`))
	require.NoError(t, err)

	state, err := fixture.controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	require.NotNil(t, state.Unknown)
	assert.Equal(t, entryID, state.Unknown.RequestID)
	assert.False(t, state.Unknown.Recoverable)

	_, err = fixture.controller.Resolve(t.Context(), Resolution{
		RequestID: state.Unknown.RequestID,
		Choice:    ChoiceAcknowledge,
	}, nil)
	require.ErrorIs(t, err, ErrJournalCorrupt)
}

func TestControllerApprovalNeverDeniesWithoutExecution(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t, true, config.ApprovalNever)
	fixture.session.addPending(controlledCall("call-1", `true`))

	state, err := fixture.controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	assert.Equal(t, StateReady, state.Kind)
	assert.Zero(t, fixture.executor.prepareCount)
	assert.Zero(t, fixture.executor.runCount)
	require.Len(t, fixture.session.resolved, 1)
	assert.True(t, fixture.session.resolved[0].IsError)
}

func TestControllerSandboxUnavailableDoesNotExecute(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t, true, config.ApprovalOnRequest)
	fixture.session.addPending(controlledCall("call-1", `true`))

	state, err := fixture.controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	require.NotNil(t, state.Review)

	fixture.executor.prepareErr = execution.ErrSandboxUnavailable
	_, err = fixture.controller.Resolve(t.Context(), Resolution{
		RequestID: state.Review.RequestID,
		Choice:    ChoiceAllowOnce,
	}, nil)
	require.ErrorIs(t, err, execution.ErrSandboxUnavailable)
	assert.Equal(t, 1, fixture.executor.prepareCount)
	assert.Zero(t, fixture.executor.runCount)
}

func TestControllerNonInteractiveReviewFailsWithoutExecution(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t, true, config.ApprovalOnRequest)
	fixture.session.addPending(controlledCall("call-1", `true`))

	state, err := fixture.controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	require.Equal(t, StateReview, state.Kind)
	require.ErrorIs(t, state.NonInteractiveError(), ErrApprovalRequired)
	assert.Zero(t, fixture.executor.prepareCount)
	assert.Zero(t, fixture.executor.runCount)
}

func TestControllerStaleReviewCannotAuthorizeChangedOperation(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t, true, config.ApprovalOnRequest)
	fixture.session.addPending(controlledCall("call-1", `original`))

	state, err := fixture.controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	require.NotNil(t, state.Review)
	requestID := state.Review.RequestID

	fixture.session.pending[0].Args = ai.JSON(`changed`)
	_, err = fixture.controller.Resolve(t.Context(), Resolution{
		RequestID: requestID,
		Choice:    ChoiceAllowOnce,
	}, nil)
	require.ErrorIs(t, err, ErrInvalidResolution)
	assert.Zero(t, fixture.executor.runCount)

	state, err = fixture.controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	require.NotNil(t, state.Review)
	assert.NotEqual(t, requestID, state.Review.RequestID)
}

func unknownPendingFixture(t *testing.T) (*controllerFixture, Unknown) {
	t.Helper()

	fixture := newControllerFixture(t, true, config.ApprovalOnRequest)
	fixture.session.addPending(controlledCall("call-1", `true`))

	state, err := fixture.controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	require.NotNil(t, state.Review)

	fixture.session.resolveFailures = 1
	_, err = fixture.controller.Resolve(t.Context(), Resolution{
		RequestID: state.Review.RequestID,
		Choice:    ChoiceAllowOnce,
	}, nil)
	require.ErrorIs(t, err, errInjected)

	state, err = fixture.controller.Reconcile(t.Context(), nil)
	require.NoError(t, err)
	require.NotNil(t, state.Unknown)

	return fixture, *state.Unknown
}

func TestApprovalReceiptsExcludeSensitiveContent(t *testing.T) {
	t.Parallel()

	fixture := newControllerFixture(t, false, config.ApprovalOnRequest)
	call := agent.ToolCall{
		ID:   "call-1",
		Name: controlledToolName,
		Args: ai.JSON(`API_KEY=sentinel-secret`),
	}
	tool, ok := fixture.controller.Tool(controlledToolName)
	require.True(t, ok)
	decision := fixture.controller.BeforeTool(t.Context(), agent.ToolCallInfo{ToolCall: call})
	require.Equal(t, agent.ToolDecisionAllow, decision.Action)

	_, err := tool.Exec(t.Context(), call)
	require.NoError(t, err)

	for _, entry := range fixture.session.Path() {
		if entry.Kind == harness.KindCustom {
			assert.NotContains(t, string(entry.Data), "sentinel-secret")
			assert.NotContains(t, string(entry.Data), "API_KEY")
		}
	}
}

func TestApprovalErrorsExposeStableToolCodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		err  error
		code string
	}{
		{err: ErrApprovalRequired, code: "approval_required"},
		{err: ErrOutcomeUnknown, code: "outcome_unknown"},
		{err: ErrInvalidResolution, code: "invalid_resolution"},
		{err: ErrJournalCorrupt, code: "journal_corrupt"},
		{err: ErrDenied, code: "approval_denied"},
	}

	for _, test := range tests {
		var coded interface{ ToolErrorCode() string }
		require.ErrorAs(t, test.err, &coded)
		assert.Equal(t, test.code, coded.ToolErrorCode())
	}
}
