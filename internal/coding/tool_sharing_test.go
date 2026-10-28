package coding

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func toolSharingEvent(state State, eventType EventType, payload EventPayload) Event {
	return Event{
		Schema: EventSchema, Sequence: state.Sequence + 1, Time: eventTestTime,
		SessionID: state.SessionID, InteractionID: state.Interaction.ID, RunID: "bench-run",
		Type: eventType, Payload: payload,
	}
}

func toolSharingCall(id string, arguments ai.JSON) ToolCall {
	return ToolCall{ID: id, Name: "read", Arguments: arguments}
}

// toolSharingState is a live run whose transcript already holds one assistant
// tool call, with no ToolState yet: ToolStarted is the next event.
func toolSharingState(tb testing.TB, arguments ai.JSON) State {
	tb.Helper()

	return fixtureState(tb, ai.Messages{
		ai.Assistant(ai.ToolCallPart{ID: "call-share", Name: "read", Args: arguments}),
	})
}

func structuredToolResult(callID string, data ai.JSON) ai.ToolMessage {
	return ai.ToolResults(ai.ToolResultPart{
		ToolCallID: callID, Name: "read",
		Content: []ai.Part{ai.StructuredContentPart{Data: data}},
	})
}

func structuredData(tb testing.TB, message ai.ToolMessage) ai.JSON {
	tb.Helper()

	require.Len(tb, message.Parts, 1)
	require.Len(tb, message.Parts[0].Content, 1)
	part, ok := message.Parts[0].Content[0].(ai.StructuredContentPart)
	require.True(tb, ok)

	return part.Data
}

// TestCommittedToolResultSharesTranscriptPayload pins the ownership transfer:
// the pre-commit window keeps its own result copy, and MessageCommitted makes
// the transcript the single owner of the result part.
func TestCommittedToolResultSharesTranscriptPayload(t *testing.T) {
	t.Parallel()

	arguments := ai.JSON(`{"path":"big.txt"}`)
	state := toolSharingState(t, arguments)
	call := toolSharingCall("call-share", arguments)

	started, err := Reduce(state, toolSharingEvent(state, EventToolStarted, ToolStarted{
		Turn: 1, Call: call,
	}))
	require.NoError(t, err)
	require.Len(t, started.Tools, 1)

	assistant, ok := started.Transcript[0].(ai.AssistantMessage)
	require.True(t, ok)
	transcriptCall, ok := assistant.Parts[0].(ai.ToolCallPart)
	require.True(t, ok)
	assert.Same(t, &transcriptCall.Args[0], &started.Tools[0].Call.Arguments[0],
		"the Tool call shares the committed argument bytes")

	data := ai.JSON(`{"items":"` + strings.Repeat("x", 64<<10) + `"}`)
	result := structuredToolResult("call-share", data)

	completed, err := Reduce(started, toolSharingEvent(started, EventToolCompleted, ToolCompleted{
		Turn: 1, Call: call, Result: result,
	}))
	require.NoError(t, err)
	beforeCommit := structuredData(t, completed.Tools[0].Result)
	assert.NotSame(t, &data[0], &beforeCommit[0],
		"the pre-commit result owns a copy of the payload")

	committed, err := Reduce(completed, toolSharingEvent(completed, EventMessageCommitted, MessageCommitted{
		Message: result,
	}))
	require.NoError(t, err)
	require.Len(t, committed.Transcript, 2)

	transcriptMessage, ok := committed.Transcript[1].(ai.ToolMessage)
	require.True(t, ok)
	shared := structuredData(t, committed.Tools[0].Result)
	transcriptData := structuredData(t, transcriptMessage)
	assert.Same(t, &transcriptData[0], &shared[0],
		"the committed transcript part is the Tool result's single owner")

	// Clone is still a deep defensive copy; CloneReadOnly shares.
	cloned := committed.Clone()
	cloneData := structuredData(t, cloned.Tools[0].Result)
	assert.NotSame(t, &shared[0], &cloneData[0])
	cloneData[0] = 'X'

	assert.NotEqual(t, byte('X'), shared[0], "mutating the clone cannot reach the original")

	view := committed.CloneReadOnly()
	viewData := structuredData(t, view.Tools[0].Result)
	assert.Same(t, &shared[0], &viewData[0])

	// Sharing is an ownership change, not a protocol change: the JSON projection
	// of the shared State and a deep copy are identical.
	sharedJSON, err := json.Marshal(committed)
	require.NoError(t, err)
	deepJSON, err := json.Marshal(committed.Clone())
	require.NoError(t, err)
	assert.JSONEq(t, string(sharedJSON), string(deepJSON))
}

// TestSharedToolPayloadKeepsSiblingStatesIsolated pins the copy-on-write
// boundary: the State retained before MessageCommitted never observes the
// sharing or a later transition.
func TestSharedToolPayloadKeepsSiblingStatesIsolated(t *testing.T) {
	t.Parallel()

	arguments := ai.JSON(`{"path":"big.txt"}`)
	state := toolSharingState(t, arguments)
	call := toolSharingCall("call-share", arguments)

	started, err := Reduce(state, toolSharingEvent(state, EventToolStarted, ToolStarted{
		Turn: 1, Call: call,
	}))
	require.NoError(t, err)

	result := structuredToolResult("call-share", ai.JSON(`{"value":"shared"}`))
	completed, err := Reduce(started, toolSharingEvent(started, EventToolCompleted, ToolCompleted{
		Turn: 1, Call: call, Result: result,
	}))
	require.NoError(t, err)

	before := structuredData(t, completed.Tools[0].Result)
	committed, err := Reduce(completed, toolSharingEvent(completed, EventMessageCommitted, MessageCommitted{
		Message: result,
	}))
	require.NoError(t, err)

	assert.Equal(t, string(before), string(structuredData(t, completed.Tools[0].Result)),
		"the sibling state keeps its own pre-commit result")
	assert.Len(t, completed.Transcript, 1)
	assert.Len(t, committed.Transcript, 2)
}

// TestRewrittenToolArgumentsKeepTheirOwnCopy covers the hook path: the ToolStarted
// call may differ from the committed model call, so it cannot share those bytes
// and ToolCompleted must still match the active call.
func TestRewrittenToolArgumentsKeepTheirOwnCopy(t *testing.T) {
	t.Parallel()

	committedArgs := ai.JSON(`{"command":"original"}`)
	state := toolSharingState(t, committedArgs)
	rewritten := ai.JSON(`{"command":"rewritten"}`)

	started, err := Reduce(state, toolSharingEvent(state, EventToolStarted, ToolStarted{
		Turn: 1, Call: ToolCall{ID: "call-share", Name: "shell", Arguments: rewritten},
	}))
	require.NoError(t, err)
	require.Len(t, started.Tools, 1)
	assert.Equal(t, string(rewritten), string(started.Tools[0].Call.Arguments))
	assert.NotSame(t, &rewritten[0], &started.Tools[0].Call.Arguments[0])

	_, err = Reduce(started, toolSharingEvent(started, EventToolCompleted, ToolCompleted{
		Turn: 1, Call: ToolCall{ID: "call-share", Name: "shell", Arguments: rewritten},
		Result: ai.ToolResultText("call-share", "shell", "done"),
	}))
	require.NoError(t, err, "the rewritten call stays the active Tool identity")
}
