package coding

import (
	"fmt"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// windowTestState builds a full live window plus the projections that must move
// with it: candidates, synthetic indexes, compaction boundary and Tool cards.
func windowTestState() State {
	total := retainedTranscriptItems + 1
	state := State{
		SessionID: "session-1", SessionOpen: true, Sequence: 1, Phase: PhaseIdle,
		Transcript:        make(ai.Messages, total),
		MessageCandidates: make([]CandidateIdentity, total),
		transcriptBase:    10,
	}

	for index := range total {
		state.Transcript[index] = ai.UserText(fmt.Sprintf("message %d", index))
		state.MessageCandidates[index] = CandidateIdentity{RunID: "run-1", Turn: index + 1}
	}

	for _, index := range []int{0, 500, 1000, 2000} {
		state.Transcript[index] = ai.Assistant(ai.ToolCallPart{
			ID: fmt.Sprintf("call-%d", index), Name: "read",
		})
	}

	state.Tools = []ToolState{
		{Call: ToolCall{ID: "call-0", Name: "read"}, Status: ToolStatusCompleted},
		{Call: ToolCall{ID: "call-500", Name: "read"}, Status: ToolStatusRunning},
		{Call: ToolCall{ID: "call-1000", Name: "read"}, Status: ToolStatusCompleted},
		{Call: ToolCall{ID: "call-2000", Name: "read"}, Status: ToolStatusCompleted},
	}
	state.SyntheticMessages = []int{0, 1024, 1025, total - 1}
	state.Compaction.BoundaryIndex = 2000

	return state
}

func TestTrimTranscriptWindowRebasesProjections(t *testing.T) {
	t.Parallel()

	state := windowTestState()
	drop := len(state.Transcript) - retainedTranscriptLowWater
	state.trimTranscriptWindow()

	require.Len(t, state.Transcript, retainedTranscriptLowWater)
	assert.Equal(t, 10+drop, state.TranscriptBase())
	assert.Equal(t, uint64(1), state.Sequence, "trimming is projection maintenance, not a transition")
	assert.Equal(t, "message 1025", runtimeMessageText(state.Transcript[0]))
	assert.Equal(t, 1026, state.MessageCandidates[0].Turn)
	require.Len(t, state.MessageCandidates, len(state.Transcript))
	assert.Equal(t, []int{0, retainedTranscriptLowWater - 1}, state.SyntheticMessages)
	assert.Equal(t, 2000-drop, state.Compaction.BoundaryIndex)

	require.Len(t, state.Tools, 2)
	assert.Equal(t, "call-500", state.Tools[0].Call.ID, "a running Tool stays in the window")
	assert.Equal(t, "call-2000", state.Tools[1].Call.ID)
	assert.Equal(t, 2, state.ToolsBase(), "the two completed dropped calls were released")
}

func TestMessageCommittedTrimsTheLiveTranscript(t *testing.T) {
	t.Parallel()

	state := State{
		SessionID: "session-1", SessionOpen: true, Sequence: 1, Phase: PhaseIdle,
		Runs:              []RunState{{ID: "run-1", Active: true, TurnOpen: true, Turn: 1}},
		Transcript:        make(ai.Messages, retainedTranscriptItems),
		MessageCandidates: make([]CandidateIdentity, retainedTranscriptItems),
	}
	for index := range state.Transcript {
		state.Transcript[index] = ai.UserText(fmt.Sprintf("message %d", index))
	}

	event := Event{
		Schema: EventSchema, Sequence: 2, Time: time.Unix(1, 0).UTC(),
		SessionID: "session-1", InteractionID: "interaction-1", RunID: "run-1",
		Type:    EventMessageCommitted,
		Payload: MessageCommitted{Message: ai.UserText("newest")},
	}
	next, err := Reduce(state, event)
	require.NoError(t, err)

	require.Len(t, next.Transcript, retainedTranscriptLowWater)
	assert.Equal(t, retainedTranscriptItems+1-retainedTranscriptLowWater, next.TranscriptBase())
	assert.Equal(t, "newest", runtimeMessageText(next.Transcript[len(next.Transcript)-1]))
	require.Len(t, next.MessageCandidates, len(next.Transcript))
	assert.Equal(t, uint64(2), next.Sequence)
}

func TestSessionTreeChangedCarriesTheTranscriptBase(t *testing.T) {
	t.Parallel()

	state := State{SessionID: "session-1", SessionOpen: true, Sequence: 1}
	event := Event{
		Schema: EventSchema, Sequence: 2, Time: time.Unix(1, 0).UTC(),
		SessionID: "session-1", Type: EventSessionTreeChanged,
		Payload: SessionTreeChanged{
			Tree:           SessionTree{SessionID: "session-1"},
			Transcript:     []ai.Message{ai.UserText("windowed")},
			TranscriptBase: 42,
		},
	}
	next, err := Reduce(state, event)
	require.NoError(t, err)
	assert.Equal(t, 42, next.TranscriptBase())
	assert.Equal(t, "windowed", runtimeMessageText(next.Transcript[0]))
}

func TestTranscriptWindowMayBeTruncatedReportsTheBase(t *testing.T) {
	t.Parallel()

	short := State{Transcript: make(ai.Messages, retainedTranscriptLowWater)}
	assert.False(t, TranscriptWindowMayBeTruncated(short))

	windowed := State{
		Transcript:     make(ai.Messages, retainedTranscriptLowWater),
		transcriptBase: 1025,
	}
	assert.True(t, TranscriptWindowMayBeTruncated(windowed))
	assert.True(t, TranscriptWindowMayBeTruncated(
		State{Transcript: make(ai.Messages, retainedTranscriptItems)},
	))
}
