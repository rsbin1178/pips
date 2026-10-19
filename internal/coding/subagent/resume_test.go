package subagent

import (
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResumeFromActiveChildIsRejected(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixture(t, &testModel{})
	fixture.blockChildren()

	execution, err := fixture.start(t.Context(), Request{
		AgentID: "explore", Task: "Wait.",
	}, nil)
	require.NoError(t, err)
	<-fixture.started

	_, err = fixture.start(t.Context(), Request{
		AgentID: "explore", Task: "Continue.", ResumeFrom: execution.child.Metadata().ID,
	}, nil)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "still running")

	execution.Cancel()
	_, _ = execution.Wait(t.Context())
}

func TestResumeFromUnknownChildIsRejected(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixture(t, &testModel{})
	_, err := fixture.start(t.Context(), Request{
		AgentID: "explore", Task: "Continue.", ResumeFrom: "s-missing-source",
	}, nil)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "no completed child")
}

func TestResumableStates(t *testing.T) {
	t.Parallel()

	for _, state := range []State{StateSucceeded, StateFailed, StateCanceled} {
		assert.True(t, resumableState(state), state)
	}

	for _, state := range []State{StateCreated, StateRunning, StateInterrupted} {
		assert.False(t, resumableState(state), state)
	}
}

func TestResumeFromSourceWithoutTranscriptIsRejected(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixture(t, &testModel{})
	fixture.dispatcher.newRunner = func() *stubRunner {
		return &stubRunner{
			skipTranscript: true,
			run:            &agent.RunResult{Stop: agent.StopEndTurn, Turns: 1},
			text:           "no transcript",
			value:          "no transcript",
		}
	}

	completed, err := fixture.start(t.Context(), Request{
		AgentID: "explore", Task: "First.",
	}, nil)
	require.NoError(t, err)
	source, err := completed.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, OutcomeSucceeded, source.Outcome)

	_, err = fixture.start(t.Context(), Request{
		AgentID: "explore", Task: "Continue.", ResumeFrom: completed.child.Metadata().ID,
	}, nil)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "no transcript")
}

func TestResumeCarriesTheSourceIdentityUsageAndDirectory(t *testing.T) {
	t.Parallel()

	fixture := newManagerFixture(t, &testModel{})
	fixture.dispatcher.newRunner = func() *stubRunner {
		return &stubRunner{
			run:  &agent.RunResult{Stop: agent.StopEndTurn, Turns: 1, Usage: ai.Usage{InputTokens: 7}},
			text: "stub summary", value: "stub summary",
		}
	}

	completed, err := fixture.start(t.Context(), Request{
		AgentID: "explore", Task: "First.",
	}, nil)
	require.NoError(t, err)
	source, err := completed.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, OutcomeSucceeded, source.Outcome)
	require.Equal(t, 7, source.Usage.InputTokens)

	var opened *DispatchInput

	fixture.dispatcher.onOpen = func(input DispatchInput) { opened = &input }

	resumed, err := fixture.start(t.Context(), Request{
		AgentID: "general-purpose", Task: "Continue.", ResumeFrom: completed.child.Metadata().ID,
		Model: "openai/other", Cwd: "/tmp/ignored",
	}, nil)
	require.NoError(t, err)

	// The caller's type, model, and directory are replaced by the source's.
	assert.Equal(t, "explore", resumed.request.AgentID)
	assert.Equal(t, source.Identity.ID, resumed.request.AgentID)
	assert.Equal(t, completed.plan.Model, resumed.request.Model)
	assert.Empty(t, resumed.request.Cwd)
	assert.Equal(t, completed.child.Metadata().ID, resumed.plan.ResumeFrom)
	assert.Equal(t, completed.plan.Identity.ID, resumed.plan.Identity.ID)

	result, err := resumed.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, OutcomeSucceeded, result.Outcome)
	require.NotNil(t, opened)
	require.NotNil(t, opened.Resume)
	assert.Equal(t, completed.child.Metadata().ID, opened.Resume.ChildSessionID)
	assert.Equal(t, completed.plan.Model, opened.Resume.Model)
	// The resumed child reports its lineage's cumulative usage.
	assert.Equal(t, 14, result.Usage.InputTokens)
}

func TestResumePlanMustNameTheSourceType(t *testing.T) {
	t.Parallel()

	source := &ResumeSource{ChildSessionID: "s-source", AgentID: "explore"}
	require.ErrorIs(t, validateResumePlan(source, ExecutionPlan{Identity: AgentIdentity{ID: "plan"}}), ErrInvalid)
	require.ErrorIs(t, validateResumePlan(source, ExecutionPlan{
		Identity: AgentIdentity{ID: "explore"}, ResumeFrom: "s-other",
	}), ErrInvalid)
	require.NoError(t, validateResumePlan(source, ExecutionPlan{
		Identity: AgentIdentity{ID: "explore"}, ResumeFrom: "s-source",
	}))
	require.NoError(t, validateResumePlan(nil, ExecutionPlan{}))
}

func TestApplyResumeSourcePinsTypeModelAndEnvironment(t *testing.T) {
	t.Parallel()

	source := &ResumeSource{
		ChildSessionID: "s-source", AgentID: "explore", Model: "openai/pinned",
		WorktreePath: "/tmp/source-worktree",
	}
	pinned := applyResumeSource(Request{
		AgentID: "general-purpose", Model: "openai/caller", Cwd: "/tmp/caller",
		Isolation: IsolationWorktree, ResumeFrom: "s-source",
	}, source)
	assert.Equal(t, "explore", pinned.AgentID)
	assert.Equal(t, "openai/pinned", pinned.Model)
	assert.Empty(t, pinned.Cwd)
	assert.Equal(t, IsolationWorktree, pinned.Isolation)

	// A source without a Worktree keeps the shared environment, so a caller's
	// worktree request is dropped rather than applied to an un-isolated child.
	shared := applyResumeSource(Request{
		AgentID: "general-purpose", Isolation: IsolationWorktree, ResumeFrom: "s-source",
	}, &ResumeSource{ChildSessionID: "s-source", AgentID: "explore", Model: "openai/pinned"})
	assert.Empty(t, shared.Isolation)
}
