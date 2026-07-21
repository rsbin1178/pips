package coding

import (
	"testing"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/changes"
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

func reducerEvents() []Event {
	usage := TokenUsage{InputTokens: 10, OutputTokens: 5}
	call := ToolCall{ID: "call-1", Name: "read_file", Arguments: ai.JSON(`{"path":"main.go"}`)}

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
