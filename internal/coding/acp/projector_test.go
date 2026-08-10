package acp

import (
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/changes"
	"github.com/rsbin/pips/internal/coding/tasklist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectEventPreservesOrderedACPUpdates(t *testing.T) {
	t.Parallel()

	out := &fakeOutbound{}
	projection := newProjectionState("session-1")
	snapshot := coding.State{ContextWindow: 200_000, ContextTokens: 1_200}

	events := []coding.Event{
		{Payload: coding.MessageDelta{Kind: ai.StreamTextDelta, Text: "answer"}},
		{Payload: coding.MessageDelta{Kind: ai.StreamReasoningDelta, Text: "thinking"}},
		{Payload: coding.ToolStarted{Call: coding.ToolCall{
			ID: "call-1", Name: "read_file", Arguments: ai.JSON(`{"path":"main.go"}`),
		}}},
		{Payload: coding.ToolUpdated{
			Call:   coding.ToolCall{ID: "call-1", Name: "read_file"},
			Update: []ai.Part{ai.Text("progress")},
		}},
		{Payload: coding.ToolCompleted{
			Call:   coding.ToolCall{ID: "call-1", Name: "read_file"},
			Result: ai.ToolResultError("call-1", "read_file", "failed"),
		}},
		{
			SessionID: "session-1", InteractionID: "interaction-1", Sequence: 7,
			Payload: coding.WorkspaceChanged{
				Entries: []coding.WorkspaceChange{{Path: "main.go", Kind: changes.KindModified}},
				Diff:    "--- a/main.go\n+++ b/main.go\n",
			},
		},
		{Payload: coding.SessionTreeChanged{
			Transcript: []ai.Message{ai.UserText("Implement ACP support")},
			Tasks: tasklist.Snapshot{
				Items:      []tasklist.Item{{Step: "Implement", Status: tasklist.StatusInProgress}},
				InProgress: 1, Total: 1,
			},
		}},
		{
			Time: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC),
			Payload: coding.InteractionCompleted{
				Outcome: coding.InteractionSucceeded, Stop: agent.StopEndTurn,
			},
		},
	}
	for _, event := range events {
		require.NoError(t, projectEvent(
			t.Context(), out, "session-1", &projection, event, snapshot, nil,
		))
	}

	require.Len(t, out.updates, 11)
	assert.Equal(t, "answer", out.updates[0].AgentMessageChunk.Content.Text.Text)
	assert.Equal(t, "thinking", out.updates[1].AgentThoughtChunk.Content.Text.Text)
	require.NotNil(t, out.updates[0].AgentMessageChunk.MessageId)
	require.NotNil(t, out.updates[1].AgentThoughtChunk.MessageId)
	assert.Equal(t,
		*out.updates[0].AgentMessageChunk.MessageId,
		*out.updates[1].AgentThoughtChunk.MessageId,
	)
	assert.Equal(t, acpsdk.ToolCallStatusInProgress, out.updates[2].ToolCall.Status)
	assert.Equal(t, acpsdk.ToolCallStatusInProgress, *out.updates[3].ToolCallUpdate.Status)
	assert.Equal(t, acpsdk.ToolCallStatusFailed, *out.updates[4].ToolCallUpdate.Status)
	assert.Equal(t, acpsdk.ToolKindEdit, out.updates[5].ToolCall.Kind)
	assert.Equal(t, out.updates[5].ToolCall.ToolCallId, out.updates[6].ToolCallUpdate.ToolCallId)
	assert.Contains(t,
		out.updates[6].ToolCallUpdate.Content[0].Content.Content.Text.Text,
		"+++ b/main.go",
	)
	assert.Equal(t, "Implement", out.updates[7].Plan.Entries[0].Content)
	assert.Equal(t, "Implement ACP support", *out.updates[8].SessionInfoUpdate.Title)
	assert.Equal(t, "2026-08-07T12:00:00Z", *out.updates[9].SessionInfoUpdate.UpdatedAt)
	assert.Equal(t, 200_000, out.updates[10].UsageUpdate.Size)
	assert.Equal(t, 1_200, out.updates[10].UsageUpdate.Used)
}

func TestStopReasonUsesTerminalOutcomeAndProviderFinish(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		completed coding.InteractionCompleted
		finish    ai.FinishReason
		want      acpsdk.StopReason
	}{
		{name: "normal", completed: coding.InteractionCompleted{
			Outcome: coding.InteractionSucceeded, Stop: agent.StopEndTurn,
		}, want: acpsdk.StopReasonEndTurn},
		{name: "length", completed: coding.InteractionCompleted{
			Outcome: coding.InteractionSucceeded, Stop: agent.StopEndTurn,
		}, finish: ai.FinishLength, want: acpsdk.StopReasonMaxTokens},
		{name: "content filter", completed: coding.InteractionCompleted{
			Outcome: coding.InteractionSucceeded, Stop: agent.StopEndTurn,
		}, finish: ai.FinishContentFilter, want: acpsdk.StopReasonRefusal},
		{name: "max turns", completed: coding.InteractionCompleted{
			Outcome: coding.InteractionIncomplete, Stop: agent.StopMaxTurns,
		}, want: acpsdk.StopReasonMaxTurnRequests},
		{name: "budget", completed: coding.InteractionCompleted{
			Outcome: coding.InteractionIncomplete, Stop: agent.StopBudget,
		}, want: acpsdk.StopReasonMaxTokens},
		{name: "failure guard", completed: coding.InteractionCompleted{
			Outcome: coding.InteractionIncomplete, Stop: agent.StopWhen,
		}, want: acpsdk.StopReasonRefusal},
		{name: "cancelled", completed: coding.InteractionCompleted{
			Outcome: coding.InteractionCanceled,
		}, want: acpsdk.StopReasonCancelled},
		{name: "failed", completed: coding.InteractionCompleted{
			Outcome: coding.InteractionFailed,
		}, want: acpsdk.StopReasonRefusal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, test.want, stopReason(test.completed, test.finish))
		})
	}
}

func TestReplayTranscriptSkipsSystemSyntheticAndSignatures(t *testing.T) {
	t.Parallel()

	out := &fakeOutbound{}
	messages := []ai.Message{
		ai.SystemText("secret system"),
		ai.UserText("visible"),
		ai.Assistant(ai.ReasoningPart{Text: "reason", Signature: "provider-secret"}),
		ai.UserText("synthetic"),
	}
	require.NoError(t, replayTranscript(t.Context(), out, "session-1", messages, []int{3}))
	require.Len(t, out.updates, 2)
	assert.Equal(t, "visible", out.updates[0].UserMessageChunk.Content.Text.Text)
	assert.Equal(t, "reason", out.updates[1].AgentThoughtChunk.Content.Text.Text)
	require.NotNil(t, out.updates[0].UserMessageChunk.MessageId)
	require.NotNil(t, out.updates[1].AgentThoughtChunk.MessageId)
	assert.NotEqual(t,
		*out.updates[0].UserMessageChunk.MessageId,
		*out.updates[1].AgentThoughtChunk.MessageId,
	)
}

func TestProjectionMessageIDChangesOnlyAtMessageBoundary(t *testing.T) {
	t.Parallel()

	projection := newProjectionState("session-1")
	events := []coding.Event{
		{Sequence: 1, InteractionID: "interaction-1", RunID: "run-1", Payload: coding.MessageDelta{
			Kind: ai.StreamMessageStart, ResponseID: "response-1",
		}},
		{Sequence: 2, InteractionID: "interaction-1", RunID: "run-1", Payload: coding.MessageDelta{
			Kind: ai.StreamReasoningDelta, Text: "reason",
		}},
		{Sequence: 3, InteractionID: "interaction-1", RunID: "run-1", Payload: coding.MessageDelta{
			Kind: ai.StreamTextDelta, Text: "answer",
		}},
		{Sequence: 4, InteractionID: "interaction-1", RunID: "run-1", Payload: coding.MessageDelta{
			Kind: ai.StreamMessageEnd,
		}},
		{Sequence: 5, InteractionID: "interaction-1", RunID: "run-1", Payload: coding.MessageDelta{
			Kind: ai.StreamMessageStart, ResponseID: "response-2",
		}},
		{Sequence: 6, InteractionID: "interaction-1", RunID: "run-1", Payload: coding.MessageDelta{
			Kind: ai.StreamTextDelta, Text: "second",
		}},
	}

	var updates []acpsdk.SessionUpdate
	for _, event := range events {
		updates = append(updates, projection.updatesForEvent(event, nil)...)
	}

	require.Len(t, updates, 3)
	firstID := requireMessageID(t, updates[0])
	assert.Equal(t, firstID, requireMessageID(t, updates[1]))
	assert.NotEqual(t, firstID, requireMessageID(t, updates[2]))
}

func requireMessageID(t *testing.T, update acpsdk.SessionUpdate) string {
	t.Helper()

	switch {
	case update.UserMessageChunk != nil:
		return *update.UserMessageChunk.MessageId
	case update.AgentMessageChunk != nil:
		return *update.AgentMessageChunk.MessageId
	case update.AgentThoughtChunk != nil:
		return *update.AgentThoughtChunk.MessageId
	default:
		t.Fatal("update is not a message chunk")

		return ""
	}
}
