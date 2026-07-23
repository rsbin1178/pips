//nolint:wsl_v5 // Reducer fixtures keep transition and assertion groups adjacent.
package coding

import (
	"testing"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/changes"
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
