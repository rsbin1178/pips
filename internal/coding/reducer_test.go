//nolint:wsl_v5 // Reducer fixtures keep transition and assertion groups adjacent.
package coding

import (
	"fmt"
	"testing"
	"time"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/changes"
	"github.com/rsbin/pips/internal/coding/question"
	"github.com/rsbin/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReduceLiveAndJSONReplayMatch(t *testing.T) {
	t.Parallel()

	events := reducerEvents()

	var (
		live   State
		replay State
	)

	for _, event := range events {
		var err error

		live, err = Reduce(live, event)
		require.NoError(t, err)

		encoded, err := MarshalEvent(event)
		require.NoError(t, err)
		decoded, err := UnmarshalEvent(encoded)
		require.NoError(t, err)

		replay, err = Reduce(replay, decoded)
		require.NoError(t, err)
	}

	assert.Equal(t, live, replay)
	assert.Equal(t, uint64(len(events)), live.Sequence)
	assert.Equal(t, PhaseClosed, live.Phase)
	assert.False(t, live.SessionOpen)
	assert.False(t, live.Interaction.Active)
	assert.Equal(t, InteractionSucceeded, live.Interaction.Outcome)
	assert.Len(t, live.Transcript, 2)
	assert.Len(t, live.Tools, 1)
	assert.Equal(t, ToolStatusCompleted, live.Tools[0].Status)
	assert.Equal(t, "main.go", live.Changes.Entries[0].Path)
	require.Len(t, live.Diagnostics, 1)
	assert.Equal(t, "connect_failed", live.Diagnostics[0].Code)
	require.NotNil(t, live.LastError)
	assert.Equal(t, "model_failed", live.LastError.Code)
}

func TestReduceReturnsDefensiveState(t *testing.T) {
	t.Parallel()

	events := reducerEvents()

	var state State

	for _, event := range events[:7] {
		var err error

		state, err = Reduce(state, event)
		require.NoError(t, err)
	}

	snapshot := state.Clone()
	messageText, ok := state.Transcript[0].Parts[0].(ai.TextPart)
	require.True(t, ok)

	messageText.Text = "mutated"
	state.Transcript[0].Parts[0] = messageText

	snapshotText, ok := snapshot.Transcript[0].Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, "working", snapshotText.Text)
}

func TestReduceMessageDiscardedClearsOnlyProvisionalDraft(t *testing.T) {
	t.Parallel()

	events := reducerEvents()
	var state State
	for _, event := range events[:6] {
		var err error
		state, err = Reduce(state, event)
		require.NoError(t, err)
	}
	require.NotEmpty(t, state.Draft)

	discarded := newTestEvent(
		EventMessageDiscarded,
		MessageDiscarded{Turn: 1},
	)
	discarded.Sequence = state.Sequence + 1
	state, err := Reduce(state, discarded)
	require.NoError(t, err)
	assert.Empty(t, state.Draft)
	assert.Empty(t, state.Transcript)
}

func TestReduceRecordsDurablePromptRequestTime(t *testing.T) {
	t.Parallel()

	request, err := question.NewRequest("question-1", "call-1", question.Spec{
		Questions: []question.Question{{
			Header: "Scope", Question: "Which scope?",
			Options: []question.Option{
				{Label: "Runtime", Description: "Runtime only"},
				{Label: "TUI", Description: "TUI only"},
			},
		}},
	})
	require.NoError(t, err)

	reducePrompt := func(t *testing.T, payload EventPayload, at time.Time) State {
		t.Helper()

		events := []Event{
			newSessionEvent(EventSessionOpened, SessionOpened{
				Provider: ai.ProviderOpenAI, ModelID: "gpt-test",
			}),
			newInteractionEvent(EventInteractionStarted, InteractionStarted{}),
			newInteractionEvent(eventTypeForPrompt(payload), payload),
		}
		events[2].Time = at
		var state State
		for index, event := range events {
			event.Sequence = uint64(index + 1)
			state, err = Reduce(state, event)
			require.NoError(t, err)
		}

		return state.Clone()
	}

	approvalAt := eventTestTime.Add(time.Second)
	approvalState := reducePrompt(t, ApprovalRequired{
		RequestID: "approval-1", CallID: "call-1", Tool: "shell",
		Choices: []approval.Choice{approval.ChoiceAllowOnce, approval.ChoiceDeny},
	}, approvalAt)
	assert.Equal(t, approvalAt, approvalState.Approval.RequestedAt)

	questionAt := approvalAt.Add(time.Second)
	questionState := reducePrompt(t, QuestionRequired{
		Request: request, Count: len(request.Questions),
	}, questionAt)
	assert.Equal(t, questionAt, questionState.Question.RequestedAt)
}

func eventTypeForPrompt(payload EventPayload) EventType {
	switch payload.(type) {
	case ApprovalRequired:
		return EventApprovalRequired
	case QuestionRequired:
		return EventQuestionRequired
	default:
		return ""
	}
}

func TestStateIsSessionProvisional(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state State
		want  bool
	}{
		{name: "empty", state: State{}, want: true},
		{
			name:  "interaction journal",
			state: State{Interaction: InteractionState{ID: "interaction-1"}},
		},
		{name: "legacy transcript", state: State{Transcript: []ai.Message{ai.UserText("hello")}}},
		{name: "durable tree", state: State{Tree: SessionTree{TotalNodes: 1}}},
		{name: "bounded tree node", state: State{Tree: SessionTree{Nodes: []SessionNode{{ID: "node-1"}}}}},
		{name: "tree leaf", state: State{Tree: SessionTree{LeafID: "node-1"}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, test.want, test.state.IsSessionProvisional())
		})
	}
}

func TestReduceSessionTreeAndCompactionLifecycle(t *testing.T) {
	t.Parallel()

	tree := SessionTree{
		SessionID: "session-1", LeafID: "node-1", TotalNodes: 1,
		Nodes: []SessionNode{{
			ID: "node-1", Kind: SessionNodeMessage, CreatedAt: eventTestTime,
			Current: true, OnActivePath: true,
		}},
	}
	preview := CompactionPreview{
		Available: true, Token: "preview-token", EstimatedTokens: 3000,
		ThresholdTokens: 2000, SummarizedMessages: 2, KeptMessages: 1,
		FirstKeptID: "node-1",
	}
	events := []Event{
		newSessionEvent(EventSessionOpened, SessionOpened{
			Provider: ai.ProviderOpenAI, ModelID: "gpt-test",
		}),
		newSessionEvent(EventSessionTreeChanged, SessionTreeChanged{
			Tree: tree, Transcript: []ai.Message{ai.UserText("goal")},
		}),
		newSessionEvent(EventCompactionStarted, CompactionStarted{
			Mode: CompactionManual, Preview: preview,
		}),
		newSessionEvent(EventCompactionCompleted, CompactionCompleted{
			Mode: CompactionManual, TokensBefore: 3000, TokensAfter: 900,
			FirstKeptID: "node-1", DurationMillis: 20,
		}),
		newSessionEvent(EventSessionNavigated, SessionNavigated{
			FromID: "node-2", ToID: "node-1",
		}),
		newSessionEvent(EventSessionForked, SessionForked{
			SourceSessionID: "session-1", TargetSessionID: "session-2", AtEntryID: "node-1",
		}),
	}

	var state State
	for index := range events {
		events[index].Sequence = uint64(index + 1)
		var err error
		state, err = Reduce(state, events[index])
		require.NoError(t, err)
	}

	assert.Equal(t, tree, state.Tree)
	require.Len(t, state.Transcript, 1)
	assert.False(t, state.Compaction.Active)
	assert.Equal(t, CompactionManual, state.Compaction.Mode)
	assert.Equal(t, 3000, state.Compaction.TokensBefore)
	assert.Equal(t, 900, state.Compaction.TokensAfter)
}

func TestReduceRejectsProtocolViolations(t *testing.T) {
	t.Parallel()

	t.Run("sequence gap", func(t *testing.T) {
		t.Parallel()

		event := reducerEvents()[0]
		event.Sequence = 2

		_, err := Reduce(State{}, event)
		require.ErrorIs(t, err, ErrEventProtocol)
	})

	t.Run("tool completion without start", func(t *testing.T) {
		t.Parallel()

		events := reducerEvents()

		var state State

		for _, event := range events[:7] {
			var err error

			state, err = Reduce(state, event)
			require.NoError(t, err)
		}

		completion := events[9]
		completion.Sequence = state.Sequence + 1
		_, err := Reduce(state, completion)
		require.ErrorIs(t, err, ErrEventProtocol)
	})

	t.Run("interaction completes with active run", func(t *testing.T) {
		t.Parallel()

		events := reducerEvents()

		var state State

		for _, event := range events[:5] {
			var err error

			state, err = Reduce(state, event)
			require.NoError(t, err)
		}

		completion := events[18]
		completion.Sequence = state.Sequence + 1
		_, err := Reduce(state, completion)
		require.ErrorIs(t, err, ErrEventProtocol)
	})
}

func TestReduceSubagentLifecycleIsBoundedAndStrict(t *testing.T) {
	t.Parallel()

	events := []Event{
		newSessionEvent(EventSessionOpened, SessionOpened{
			Provider: ai.ProviderOpenAI, ModelID: "gpt-test",
		}),
		newInteractionEvent(EventInteractionStarted, InteractionStarted{}),
		newStatusEvent(EventStatusChanged, StatusChanged{Phase: PhaseRunning}),
		newTestEvent(EventRunStarted, RunStarted{Agent: "coding"}),
		newTestEvent(EventSubagentCreated, SubagentLifecycle{
			Role: subagent.RoleExplore, State: subagent.StateCreated,
			ChildSessionID: "child-1", ParentRunID: "run-1",
			Model: "openai/test", TaskPreview: "inspect",
		}),
		newTestEvent(EventSubagentStarted, SubagentLifecycle{
			Role: subagent.RoleExplore, State: subagent.StateRunning,
			ChildSessionID: "child-1", ParentRunID: "run-1", ChildRunID: "child-run",
			Model: "openai/test", TaskPreview: "inspect",
		}),
		newTestEvent(EventSubagentProgress, SubagentLifecycle{
			Role: subagent.RoleExplore, State: subagent.StateRunning,
			ChildSessionID: "child-1", ParentRunID: "run-1", ChildRunID: "child-run",
			Model: "openai/test", TaskPreview: "inspect", Turns: 1, ToolCalls: 2,
			Activity: subagent.ActivitySummary{
				Action: subagent.ActivityActionRead, Target: "runtime.go",
			},
		}),
		newTestEvent(EventSubagentCompleted, SubagentLifecycle{
			Role: subagent.RoleExplore, State: subagent.StateSucceeded,
			ChildSessionID: "child-1", ParentRunID: "run-1", ChildRunID: "child-run",
			Model: "openai/test", TaskPreview: "inspect", Code: "ok",
			Stop: agent.StopEndTurn, Turns: 1, ToolCalls: 2,
		}),
	}
	var state State
	for index := range events {
		events[index].Sequence = uint64(index + 1)
		var err error
		state, err = Reduce(state, events[index])
		require.NoError(t, err)
	}
	require.Len(t, state.Subagents, 1)
	assert.Equal(t, subagent.StateSucceeded, state.Subagents[0].State)
	assert.Equal(t, subagent.ActivitySummary{
		Action: subagent.ActivityActionRead, Target: "runtime.go",
	}, state.Subagents[0].Activity)

	late := events[6]
	late.Sequence = state.Sequence + 1
	_, err := Reduce(state, late)
	require.ErrorIs(t, err, ErrEventProtocol)
	assert.Equal(t, uint64(len(events)), state.Sequence)

	progressBeforeStart := events[:5]
	progress := events[6]
	progress.Sequence = uint64(len(progressBeforeStart) + 1)
	var before State
	for _, event := range progressBeforeStart {
		before, err = Reduce(before, event)
		require.NoError(t, err)
	}
	_, err = Reduce(before, progress)
	require.ErrorIs(t, err, ErrEventProtocol)
}

func TestReduceTeamLifecycleIsCompactBoundedAndStrict(t *testing.T) {
	t.Parallel()

	teamID := team.ID("team-1")
	beforeOpen := newSessionEvent(EventTeamLifecycle, TeamLifecycle{
		TeamID: teamID, State: TeamLifecycleProposed,
	})
	_, err := Reduce(State{}, beforeOpen)
	require.ErrorIs(t, err, ErrEventProtocol)

	attempt := TeamLifecycle{
		TeamID: teamID, MemberID: "worker-1", TaskID: "task-1", AttemptID: "attempt-1",
	}
	events := []Event{
		newSessionEvent(EventSessionOpened, SessionOpened{
			Provider: ai.ProviderOpenAI, ModelID: "gpt-test",
		}),
		newSessionEvent(EventTeamLifecycle, TeamLifecycle{
			TeamID: teamID, State: TeamLifecycleProposed,
		}),
		newSessionEvent(EventTeamLifecycle, TeamLifecycle{
			TeamID: teamID, State: TeamLifecycleAdmitted,
		}),
		newSessionEvent(EventTeamLifecycle, withTeamLifecycle(
			attempt, TeamLifecycleWaiting, TeamActivityPreparing,
		)),
		newSessionEvent(EventTeamLifecycle, withTeamLifecycle(
			attempt, TeamLifecycleRunning, TeamActivityWorking,
		)),
		newSessionEvent(EventTeamLifecycle, withTeamLifecycle(
			attempt, TeamLifecyclePaused, TeamActivityAwaitingQuestion,
		)),
		newSessionEvent(EventTeamLifecycle, withTeamLifecycle(
			attempt, TeamLifecycleRunning, TeamActivityWorking,
		)),
		newSessionEvent(EventTeamLifecycle, withTeamLifecycle(
			attempt, TeamLifecycleCapturing, TeamActivityCapturing,
		)),
		newSessionEvent(EventTeamLifecycle, TeamLifecycle{
			TeamID: teamID, MemberID: attempt.MemberID, TaskID: attempt.TaskID,
			AttemptID: attempt.AttemptID, State: TeamLifecycleCompleted,
			Turns: 2, Usage: TokenUsage{InputTokens: 10, OutputTokens: 4},
			DurationMillis: 20,
		}),
		newSessionEvent(EventTeamLifecycle, TeamLifecycle{
			TeamID: teamID, State: TeamLifecycleCompleted,
		}),
	}

	var state State
	for index := range events {
		events[index].Sequence = uint64(index + 1)
		var err error
		state, err = Reduce(state, events[index])
		require.NoError(t, err)
	}
	require.Len(t, state.Teams, 2)
	assert.Equal(t, TeamLifecycleCompleted, state.Teams[0].State)
	assert.Equal(t, TeamLifecycleCompleted, state.Teams[1].State)
	assert.Equal(t, 10, state.Teams[1].Usage.InputTokens)

	late := newSessionEvent(EventTeamLifecycle, withTeamLifecycle(
		attempt, TeamLifecycleRunning, TeamActivityWorking,
	))
	late.Sequence = state.Sequence + 1
	_, err = Reduce(state, late)
	require.ErrorIs(t, err, ErrEventProtocol)

	for index := 0; index < maxRecentTeamLifecycle+1; index++ {
		event := newSessionEvent(EventTeamLifecycle, TeamLifecycle{
			TeamID: team.ID(fmt.Sprintf("bulk-team-%d", index)),
			State:  TeamLifecycleInterrupted, Code: "startup_interrupted",
		})
		event.Sequence = state.Sequence + 1
		state, err = Reduce(state, event)
		require.NoError(t, err)
	}
	assert.Len(t, state.Teams, maxRecentTeamLifecycle)
}

func TestReduceTeamIntegrationLifecyclePreservesEvidenceAndIsStrict(t *testing.T) {
	t.Parallel()

	events := []Event{
		newSessionEvent(EventSessionOpened, SessionOpened{
			Provider: ai.ProviderOpenAI, ModelID: "gpt-test",
		}),
		newSessionEvent(EventTeamIntegrationLifecycle, TeamIntegrationLifecycle{
			TeamID: "team-1", IntegrationID: "int-1", State: TeamIntegrationVerified,
			VerificationState: "passed", Attempts: 2,
			Files: 3, Added: 1, Changed: 1, Deleted: 1,
		}),
		newSessionEvent(EventTeamIntegrationLifecycle, TeamIntegrationLifecycle{
			TeamID: "team-1", IntegrationID: "int-1", State: TeamIntegrationApplying,
		}),
		newSessionEvent(EventTeamIntegrationLifecycle, TeamIntegrationLifecycle{
			TeamID: "team-1", IntegrationID: "int-1", State: TeamIntegrationApplied,
		}),
	}
	var state State
	for index := range events {
		events[index].Sequence = uint64(index + 1)
		var err error
		state, err = Reduce(state, events[index])
		require.NoError(t, err)
	}
	require.Len(t, state.TeamIntegrations, 1)
	integration := state.TeamIntegrations[0]
	assert.Equal(t, TeamIntegrationApplied, integration.State)
	assert.Equal(t, 2, integration.Attempts)
	assert.Equal(t, 3, integration.Files)
	assert.Equal(t, "passed", integration.VerificationState)

	late := newSessionEvent(EventTeamIntegrationLifecycle, TeamIntegrationLifecycle{
		TeamID: "team-1", IntegrationID: "int-1", State: TeamIntegrationApplying,
	})
	late.Sequence = state.Sequence + 1
	_, err := Reduce(state, late)
	require.ErrorIs(t, err, ErrEventProtocol)

	invalidInitial := newSessionEvent(EventTeamIntegrationLifecycle, TeamIntegrationLifecycle{
		TeamID: "team-1", IntegrationID: "int-invalid", State: TeamIntegrationApplying,
	})
	invalidInitial.Sequence = state.Sequence + 1
	_, err = Reduce(state, invalidInitial)
	require.ErrorIs(t, err, ErrEventProtocol)

	changedTeam := newSessionEvent(EventTeamIntegrationLifecycle, TeamIntegrationLifecycle{
		TeamID: "team-2", IntegrationID: "int-1", State: TeamIntegrationRecoverable,
		Code: "recovery_available",
	})
	changedTeam.Sequence = state.Sequence + 1
	_, err = Reduce(state, changedTeam)
	require.ErrorIs(t, err, ErrEventProtocol)

	for index := range maxRecentTeamIntegrations + 1 {
		event := newSessionEvent(EventTeamIntegrationLifecycle, TeamIntegrationLifecycle{
			TeamID: "team-1", IntegrationID: fmt.Sprintf("bulk-int-%d", index),
			State: TeamIntegrationReady,
		})
		event.Sequence = state.Sequence + 1
		state, err = Reduce(state, event)
		require.NoError(t, err)
	}
	assert.Len(t, state.TeamIntegrations, maxRecentTeamIntegrations)
}

func withTeamLifecycle(
	value TeamLifecycle,
	state TeamLifecycleStatus,
	activity TeamActivity,
) TeamLifecycle {
	value.State = state
	value.Activity = activity

	return value
}

func reducerEvents() []Event {
	usage := TokenUsage{InputTokens: 10, OutputTokens: 5}
	call := ToolCall{ID: "call-1", Name: "read", Arguments: ai.JSON(`{"path":"main.go"}`)}

	events := []Event{
		newSessionEvent(EventSessionOpened, SessionOpened{
			Provider: ai.ProviderOpenAI, ModelID: "gpt-test",
		}),
		newInteractionEvent(EventInteractionStarted, InteractionStarted{}),
		newStatusEvent(EventStatusChanged, StatusChanged{Phase: PhaseRunning}),
		newTestEvent(EventRunStarted, RunStarted{Agent: "coding"}),
		newTestEvent(EventTurnStarted, TurnStarted{Turn: 1}),
		newTestEvent(EventMessageDelta, MessageDelta{Kind: ai.StreamTextDelta, Text: "working"}),
		newTestEvent(EventMessageCommitted, MessageCommitted{Message: ai.AssistantText("working")}),
		newTestEvent(EventToolStarted, ToolStarted{Turn: 1, Call: call}),
		newTestEvent(EventToolUpdated, ToolUpdated{
			Turn: 1, Call: call,
			Update: ai.Message{Role: ai.RoleTool, Parts: []ai.Part{ai.Text("half")}},
		}),
		newTestEvent(EventToolCompleted, ToolCompleted{
			Turn: 1, Call: call, Result: ai.ToolResultText(call.ID, call.Name, "done"),
		}),
		newTestEvent(EventMessageCommitted, MessageCommitted{
			Message: ai.ToolResultText(call.ID, call.Name, "done"),
		}),
		newTestEvent(EventTurnCompleted, TurnCompleted{Turn: 1, Usage: usage}),
		newTestEvent(EventRunCompleted, RunCompleted{
			Stop: agent.StopEndTurn, Turns: 1, Usage: usage,
		}),
		newInteractionEvent(EventApprovalRequired, ApprovalRequired{
			RequestID: "request-1", CallID: call.ID, Tool: call.Name,
			Choices: []approval.Choice{approval.ChoiceAllowOnce, approval.ChoiceDeny},
		}),
		newInteractionEvent(EventApprovalResolved, ApprovalResolved{
			RequestID: "request-1", Choice: approval.ChoiceAllowOnce,
		}),
		newInteractionEvent(EventApprovalUnknown, ApprovalUnknown{
			RequestID: "request-2", CallID: call.ID, Tool: call.Name,
			Fingerprint: "abcdef", Attempt: 1, Pending: true, Recoverable: true,
			Choices: []approval.Choice{approval.ChoiceRetry, approval.ChoiceMarkFailed},
		}),
		newInteractionEvent(EventApprovalResolved, ApprovalResolved{
			RequestID: "request-2", Choice: approval.ChoiceRetry,
		}),
		newInteractionEvent(EventWorkspaceChanged, WorkspaceChanged{
			Entries: []WorkspaceChange{{Path: "main.go", Kind: changes.KindModified}},
		}),
		newInteractionEvent(EventInteractionCompleted, InteractionCompleted{
			Outcome: InteractionSucceeded, Usage: usage, DurationMillis: 100,
		}),
		newStatusEvent(EventStatusChanged, StatusChanged{Phase: PhaseIdle}),
		newStatusEvent(EventIntegrationDiagnostic, IntegrationDiagnostic{
			Component: "mcp", Code: "connect_failed", Disabled: true,
		}),
		newStatusEvent(EventError, RuntimeError{Code: "model_failed", Fatal: true}),
		newStatusEvent(EventStatusChanged, StatusChanged{Phase: PhaseClosing}),
		newStatusEvent(EventStatusChanged, StatusChanged{Phase: PhaseClosed}),
		newSessionEvent(EventSessionClosed, SessionClosed{Reason: SessionClosedNormally}),
	}
	for index := range events {
		events[index].Sequence = uint64(index + 1)
	}

	return events
}
