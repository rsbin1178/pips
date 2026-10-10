//nolint:wsl_v5 // Contract fixtures keep the transition and its aliasing assertion adjacent.
package coding

import (
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// contractState returns a state that already carries every projection the
// copy-on-write reduction shares: a committed transcript (with a []byte tool
// argument), tools, a live draft, diagnostics and synthetic message indexes.
func contractState(t *testing.T) State {
	t.Helper()

	call := ToolCall{ID: "call-1", Name: "read", Arguments: ai.JSON(`{"path":"main.go"}`)}
	events := sequencedEvents(append(batchPrefix(),
		newTestEvent(EventMessageDelta, MessageDelta{Kind: ai.StreamTextDelta, Text: "draft one "}),
		newTestEvent(EventMessageDelta, MessageDelta{Kind: ai.StreamReasoningDelta, Text: "draft two "}),
		newTestEvent(EventMessageCommitted, MessageCommitted{Message: ai.Assistant(ai.Text("inline"), ai.ToolCallPart{
			ID: "call-inline", Name: "read", Args: ai.JSON(`{"path":"inline.go"}`),
		})}),
		newTestEvent(EventMessageCommitted, MessageCommitted{Message: ai.UserText("synthetic"), Synthetic: true}),
		newTestEvent(EventMessageDelta, MessageDelta{Kind: ai.StreamTextDelta, Text: "second draft "}),
		newTestEvent(EventToolStarted, ToolStarted{Turn: 1, Call: call}),
		newTestEvent(EventToolCompleted, ToolCompleted{
			Turn: 1, Call: call, Result: ai.ToolResultText(call.ID, call.Name, "done"),
		}),
		newStatusEvent(EventIntegrationDiagnostic, IntegrationDiagnostic{Component: "mcp", Code: "connect_failed"}),
	)...)

	return reduceSequentially(t, events)
}

// withSpareCapacity reproduces the shape a projection has after a few appends:
// Go doubles the backing array, so any length that is not a power of two keeps
// spare capacity that a naive append would write into.
func withSpareCapacity[T any](values []T) []T {
	headroom := make([]T, len(values), len(values)+2)
	copy(headroom, values)

	return headroom
}

// TestReduceKeepsSiblingStatesIndependent pins the copy-on-write trap: two
// states derived from one base must not write into each other's draft, even
// when the base shares an underfilled immutable tail.
func TestReduceKeepsSiblingStatesIndependent(t *testing.T) {
	t.Parallel()

	base := contractState(t)
	retained := base.Draft
	retainedRecords := retained.Materialize()
	require.Less(t, retained.Len(), streamDraftChunkSize, "base must expose an underfilled draft tail")
	transcriptCopy := append(ai.Messages(nil), base.Transcript...)

	first, err := Reduce(base, deltaEvent(base.Sequence+1, "first "))
	require.NoError(t, err)

	second, err := Reduce(base, deltaEvent(base.Sequence+1, "second "))
	require.NoError(t, err)

	assert.Equal(t, retainedRecords, retained.Materialize(), "base draft changed")
	assert.Equal(t, transcriptCopy, base.Transcript, "base transcript changed")
	assert.Equal(t, "first ", lastDraftText(first), "sibling Reduce overwrote the first draft tail")
	assert.Equal(t, "second ", lastDraftText(second))
	require.Equal(t, retained.Len()+1, first.Draft.Len())
	require.Equal(t, retained.Len()+1, second.Draft.Len())
}

// TestReduceAppendKeepsRetainedSlicesIntact proves that every append-type
// projection is copied before it grows, so slices held by an earlier state keep
// their contents and length.
func TestReduceAppendKeepsRetainedSlicesIntact(t *testing.T) {
	t.Parallel()

	base := contractState(t)

	t.Run("draft", func(t *testing.T) {
		t.Parallel()

		base := base
		require.Less(t, base.Draft.Len(), streamDraftChunkSize, "base must expose an underfilled draft tail")
		retained := base.Draft
		retainedRecords := retained.Materialize()

		first, err := Reduce(base, deltaEvent(base.Sequence+1, "first "))
		require.NoError(t, err)

		second, err := Reduce(base, deltaEvent(base.Sequence+1, "second "))
		require.NoError(t, err)

		assert.Equal(t, retainedRecords, retained.Materialize())
		assert.Equal(t, "first ", lastDraftText(first))
		assert.Equal(t, "second ", lastDraftText(second))
	})

	t.Run("transcript", func(t *testing.T) {
		t.Parallel()

		base := base
		base.Transcript = withSpareCapacity(base.Transcript)
		retained := base.Transcript
		retainedCopy := append(ai.Messages(nil), retained...)

		first, err := Reduce(base, sequenceEvent(base.Sequence+1, newTestEvent(
			EventMessageCommitted, MessageCommitted{Message: ai.UserText("first append")},
		)))
		require.NoError(t, err)

		second, err := Reduce(base, sequenceEvent(base.Sequence+1, newTestEvent(
			EventMessageCommitted, MessageCommitted{Message: ai.UserText("second append")},
		)))
		require.NoError(t, err)

		assert.Equal(t, retainedCopy, retained)
		assert.Len(t, retained, len(retainedCopy))
		require.Len(t, first.Transcript, len(retained)+1)
		require.Len(t, second.Transcript, len(retained)+1)
		assert.Equal(t, ai.UserText("first append"), first.Transcript[len(retained)])
		assert.Equal(t, ai.UserText("second append"), second.Transcript[len(retained)],
			"sibling Reduce overwrote the first committed message")
	})

	t.Run("tools", func(t *testing.T) {
		t.Parallel()

		base := base
		base.Tools = withSpareCapacity(base.Tools)
		retained := base.Tools
		retainedCopy := append([]ToolState(nil), retained...)

		first, err := Reduce(base, sequenceEvent(base.Sequence+1, newTestEvent(
			EventToolStarted, ToolStarted{Turn: 1, Call: ToolCall{ID: "first-call", Name: "read"}},
		)))
		require.NoError(t, err)

		second, err := Reduce(base, sequenceEvent(base.Sequence+1, newTestEvent(
			EventToolStarted, ToolStarted{Turn: 1, Call: ToolCall{ID: "second-call", Name: "read"}},
		)))
		require.NoError(t, err)

		assert.Equal(t, retainedCopy, retained)
		assert.Len(t, retained, len(retainedCopy))
		require.Len(t, first.Tools, len(retained)+1)
		require.Len(t, second.Tools, len(retained)+1)
		assert.Equal(t, "first-call", first.Tools[len(retained)].Call.ID)
		assert.Equal(t, "second-call", second.Tools[len(retained)].Call.ID,
			"sibling Reduce overwrote the first tool state")
	})

	t.Run("diagnostics", func(t *testing.T) {
		t.Parallel()

		base := base
		base.Diagnostics = withSpareCapacity(base.Diagnostics)
		retained := base.Diagnostics
		retainedCopy := append([]IntegrationDiagnostic(nil), retained...)

		first, err := Reduce(base, sequenceEvent(base.Sequence+1, newStatusEvent(
			EventIntegrationDiagnostic, IntegrationDiagnostic{Component: "extension", Code: "first"},
		)))
		require.NoError(t, err)

		second, err := Reduce(base, sequenceEvent(base.Sequence+1, newStatusEvent(
			EventIntegrationDiagnostic, IntegrationDiagnostic{Component: "extension", Code: "second"},
		)))
		require.NoError(t, err)

		assert.Equal(t, retainedCopy, retained)
		assert.Len(t, retained, len(retainedCopy))
		require.Len(t, first.Diagnostics, len(retained)+1)
		assert.Equal(t, "first", first.Diagnostics[len(retained)].Code)
		assert.Equal(t, "second", second.Diagnostics[len(retained)].Code,
			"sibling Reduce overwrote the first diagnostic")
	})

	t.Run("synthetic messages", func(t *testing.T) {
		t.Parallel()

		base := base
		base.SyntheticMessages = withSpareCapacity(base.SyntheticMessages)
		retained := base.SyntheticMessages
		retainedCopy := append([]int(nil), retained...)

		first, err := Reduce(base, sequenceEvent(base.Sequence+1, newTestEvent(
			EventMessageCommitted, MessageCommitted{Message: ai.UserText("first"), Synthetic: true},
		)))
		require.NoError(t, err)

		// A second sibling that commits one more message first appends a
		// different synthetic index, so a shared array would be visible.
		second, err := ReduceBatch(base, []Event{
			sequenceEvent(base.Sequence+1, newTestEvent(
				EventMessageCommitted, MessageCommitted{Message: ai.UserText("not synthetic")},
			)),
			sequenceEvent(base.Sequence+2, newTestEvent(
				EventMessageCommitted, MessageCommitted{Message: ai.UserText("second"), Synthetic: true},
			)),
		})
		require.NoError(t, err)

		assert.Equal(t, retainedCopy, retained)
		assert.Len(t, retained, len(retainedCopy))
		require.Len(t, first.SyntheticMessages, len(retained)+1)
		assert.Equal(t, len(retained)+1, first.SyntheticMessages[len(retained)])
		assert.Equal(t, len(retained)+2, second.SyntheticMessages[len(retained)],
			"sibling Reduce overwrote the first synthetic index")
	})

	t.Run("message candidates", func(t *testing.T) {
		t.Parallel()

		base := base
		base.MessageCandidates = withSpareCapacity(base.MessageCandidates)
		retained := base.MessageCandidates
		retainedCopy := append([]CandidateIdentity(nil), retained...)

		first, err := Reduce(base, sequenceEvent(base.Sequence+1, newTestEvent(
			EventMessageCommitted, MessageCommitted{Message: ai.UserText("first")},
		)))
		require.NoError(t, err)

		// The assistant commit carries a candidate identity, the user commit
		// does not, so a shared array would be visible.
		second, err := Reduce(base, sequenceEvent(base.Sequence+1, newTestEvent(
			EventMessageCommitted, MessageCommitted{Message: ai.AssistantText("second")},
		)))
		require.NoError(t, err)

		assert.Equal(t, retainedCopy, retained)
		assert.Len(t, retained, len(retainedCopy))
		require.Len(t, first.MessageCandidates, len(retained)+1)
		assert.Equal(t, CandidateIdentity{}, first.MessageCandidates[len(retained)])
		assert.Equal(t, CandidateIdentity{RunID: "run-1", Turn: 1}, second.MessageCandidates[len(retained)],
			"sibling Reduce overwrote the first candidate identity")
	})

	t.Run("runs", func(t *testing.T) {
		t.Parallel()

		base := base
		base.Runs = withSpareCapacity(base.Runs)
		retained := base.Runs
		retainedCopy := append([]RunState(nil), retained...)

		// TurnCompleted updates an existing element, so a shared array would be
		// rewritten in place instead of copied.
		first, err := Reduce(base, sequenceEvent(base.Sequence+1, newTestEvent(
			EventTurnCompleted, TurnCompleted{Turn: 1, Usage: TokenUsage{InputTokens: 1}},
		)))
		require.NoError(t, err)

		assert.Equal(t, retainedCopy, retained)
		assert.Len(t, retained, len(retainedCopy))
		require.Len(t, first.Runs, len(retained))
		assert.True(t, retained[0].TurnOpen, "base run state was rewritten in place")
		assert.False(t, first.Runs[0].TurnOpen)
	})
}

// TestStateCloneIsDeepAgainstCallerMutation pins the defensive contract of the
// value handed to Snapshots and event observations: the caller may rewrite any
// part of its copy, including []byte payloads, without reaching the runtime.
func TestStateCloneIsDeepAgainstCallerMutation(t *testing.T) {
	t.Parallel()

	state := contractState(t)
	wantDraftText := state.Draft.Materialize()[0].Text
	snapshot := state.Clone()

	require.NotEmpty(t, snapshot.Transcript)
	require.NotEmpty(t, snapshot.Tools)
	require.NotEmpty(t, snapshot.Draft)
	require.NotEmpty(t, snapshot.Tools[0].Call.Arguments)

	inline, ok := snapshot.Transcript[0].(ai.AssistantMessage)
	require.True(t, ok)
	require.Len(t, inline.Parts, 2)

	inlinePart, ok := inline.Parts[1].(ai.ToolCallPart)
	require.True(t, ok)
	inlinePart.Args[0] = 'X'
	inline.Parts[1] = inlinePart
	snapshot.Transcript[0] = inline
	snapshot.Tools[0].Call.Arguments[0] = 'X'
	snapshotDraft := snapshot.Draft.Materialize()
	snapshotDraft[0].Text = "mutated"
	snapshot.SyntheticMessages = append(snapshot.SyntheticMessages, 99)

	original, ok := state.Transcript[0].(ai.AssistantMessage)
	require.True(t, ok)
	originalPart, ok := original.Parts[1].(ai.ToolCallPart)
	require.True(t, ok)
	assert.Equal(t, byte('{'), originalPart.Args[0], "transcript []byte argument aliased the snapshot")
	assert.Equal(t, byte('{'), state.Tools[0].Call.Arguments[0], "tool []byte argument aliased the snapshot")
	assert.Equal(t, wantDraftText, state.Draft.Materialize()[0].Text)
	assert.Equal(t, wantDraftText, snapshot.Draft.Materialize()[0].Text, "materialized draft aliased its snapshot")
	assert.NotContains(t, state.SyntheticMessages, 99)

	// The reverse direction: continuing to reduce the original must not rewrite
	// the snapshot the caller already holds.
	next, err := Reduce(state, deltaEvent(state.Sequence+1, "later "))
	require.NoError(t, err)
	require.Equal(t, snapshot.Draft.Len()+1, next.Draft.Len())
	assert.Equal(t, state.Draft.Len(), snapshot.Draft.Len())
}

// TestRuntimeSnapshotIsDefensive checks the same contract through the Runtime
// boundary the frontends actually use.
func TestRuntimeSnapshotIsDefensive(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel())
	runtime.recordDiagnostic(t.Context(), IntegrationDiagnostic{Component: "test", Code: "one"})

	snapshot := runtime.Snapshot()
	require.Len(t, snapshot.Diagnostics, 1)
	require.Equal(t, "one", snapshot.Diagnostics[0].Code)

	// A caller rewriting its copy must not reach the Runtime.
	snapshot.Diagnostics[0].Code = "mutated"
	snapshot.Diagnostics = append(snapshot.Diagnostics, IntegrationDiagnostic{Component: "test", Code: "injected"})

	runtime.recordDiagnostic(t.Context(), IntegrationDiagnostic{Component: "test", Code: "two"})

	after := runtime.Snapshot()
	require.Len(t, after.Diagnostics, 2, "caller mutation leaked into the Runtime")
	assert.Equal(t, "one", after.Diagnostics[0].Code)
	assert.Equal(t, "two", after.Diagnostics[1].Code)

	// Publishing more events must not rewrite an already returned snapshot.
	require.Len(t, snapshot.Diagnostics, 2)
	assert.Equal(t, "mutated", snapshot.Diagnostics[0].Code)
}

func deltaEvent(sequence uint64, text string) Event {
	event := newTestEvent(EventMessageDelta, MessageDelta{Kind: ai.StreamTextDelta, Text: text})
	event.Sequence = sequence

	return event
}

func sequenceEvent(sequence uint64, event Event) Event {
	event.Sequence = sequence

	return event
}

func lastDraftText(state State) string {
	records := state.Draft.Materialize()
	if len(records) == 0 {
		return ""
	}

	return records[len(records)-1].Text
}
