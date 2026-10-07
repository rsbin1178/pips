package coding

import (
	"strings"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/planmode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReduceIncompleteReplyRetainsTextOutsideTranscript covers the give-up
// path: the abandoned answer's text is kept for display, the live draft is
// cleared so the interrupt contract holds, and nothing enters the transcript.
func TestReduceIncompleteReplyRetainsTextOutsideTranscript(t *testing.T) {
	t.Parallel()

	// retryEvents leaves the turn with one streamed draft delta.
	events := append(retryEvents(), newTestEvent(EventMessageIncomplete, IncompleteReply{
		Turn: 1, Text: "half", Reason: "stream ended early", Bytes: 4,
	}))

	state := reduceAll(t, events)

	require.Len(t, state.IncompleteReplies, 1)
	assert.Equal(t, IncompleteReply{
		Turn: 1, Text: "half", Reason: "stream ended early", Bytes: 4,
	}, state.IncompleteReplies[0])
	assert.Empty(t, state.Draft, "the give-up path clears the live draft")
	assert.Empty(t, state.DraftCandidate.Key())
	assert.Empty(t, state.Transcript, "retained text never enters the transcript")
}

// TestReduceIncompleteReplyAppendsCopyOnWrite pins the append semantics the
// reducer promises: a later transition allocates a fresh slice, so an earlier
// State never observes it.
func TestReduceIncompleteReplyAppendsCopyOnWrite(t *testing.T) {
	t.Parallel()

	first := reduceAll(t, append(retryEvents(), newTestEvent(EventMessageIncomplete, IncompleteReply{
		Turn: 1, Text: "one", Reason: "stream ended early", Bytes: 3,
	})))

	secondEvent := newTestEvent(EventMessageIncomplete, IncompleteReply{
		Turn: 1, Text: "two", Reason: "stream ended early", Bytes: 3,
	})
	secondEvent.Sequence = first.Sequence + 1

	second, err := Reduce(first, secondEvent)
	require.NoError(t, err)

	require.Len(t, first.IncompleteReplies, 1)
	assert.Equal(t, "one", first.IncompleteReplies[0].Text)
	require.Len(t, second.IncompleteReplies, 2)
	assert.Equal(t, "one", second.IncompleteReplies[0].Text)
	assert.Equal(t, "two", second.IncompleteReplies[1].Text)
}

func TestReduceIncompleteReplyRequiresTheOpenTurn(t *testing.T) {
	t.Parallel()

	prefix := retryEvents()
	state := reduceAll(t, prefix)

	// A reply that names a different turn than the open one is rejected.
	event := newTestEvent(EventMessageIncomplete, IncompleteReply{Turn: 2, Bytes: 0})
	event.Sequence = state.Sequence + 1

	_, err := Reduce(state, event)
	require.ErrorIs(t, err, ErrEventProtocol)

	// Without an active run it is rejected too.
	closed := State{}
	event.Sequence = 1

	_, err = Reduce(closed, event)
	require.ErrorIs(t, err, ErrEventProtocol)
}

func TestIncompleteReplyEventRoundTripsAndRejectsMalformedPayloads(t *testing.T) {
	t.Parallel()

	event := newTestEvent(EventMessageIncomplete, IncompleteReply{
		Turn: 1, Text: "half", Reason: "stream ended early", Bytes: 4,
	})
	require.NoError(t, ValidateEvent(event))

	encoded, err := MarshalEvent(event)
	require.NoError(t, err)

	decoded, err := UnmarshalEvent(encoded)
	require.NoError(t, err)
	assert.Equal(t, EventMessageIncomplete, decoded.Type)
	assert.Equal(t, event.Payload, decoded.Payload)

	bad := []IncompleteReply{
		{Turn: -1},                        // negative turn
		{Turn: 1, Text: "half", Bytes: 3}, // fewer bytes than retained
		{Turn: 1, Text: "half", Bytes: 4, Reason: strings.Repeat("x", maxDiagnosticMessage+1)},
		{Turn: 1, Text: strings.Repeat("x", maxEventTextBytes+1), Bytes: maxEventTextBytes + 1},
	}
	for _, payload := range bad {
		require.ErrorIs(t, ValidateEvent(newTestEvent(EventMessageIncomplete, payload)), ErrInvalidEvent)
	}

	assert.ErrorIs(
		t,
		ValidateEvent(newTestEvent(EventMessageDelta, IncompleteReply{Turn: 1})),
		ErrInvalidEvent,
		"the payload must not ride another type",
	)
}

func TestStopTruncatedMapsToIncompleteInteraction(t *testing.T) {
	t.Parallel()

	assert.True(t, validStopReason(agent.StopTruncated))
	assert.True(t, validInteractionStop(InteractionIncomplete, agent.StopTruncated))
	assert.False(t, validInteractionStop(InteractionSucceeded, agent.StopTruncated))

	outcome, terminal := terminalInteractionOutcome(agent.StopTruncated)
	assert.True(t, terminal)
	assert.Equal(t, InteractionIncomplete, outcome)

	event := newInteractionEvent(EventInteractionCompleted, InteractionCompleted{
		Outcome: InteractionIncomplete, Stop: agent.StopTruncated, DurationMillis: 1,
	})
	require.NoError(t, ValidateEvent(event))
}

func TestIncompleteReplyRecordRoundTripsThroughBootstrap(t *testing.T) {
	t.Parallel()

	session, err := harness.NewSession(harness.NewMemoryStore("session-1"))
	require.NoError(t, err)

	require.NoError(t, appendIncompleteReply(session, incompleteReplyRecord{
		InteractionID: "interaction-1",
		Turn:          2,
		Text:          "half",
		Reason:        "stream ended early",
		Bytes:         4,
	}))

	bootstrapped, err := BootstrapState(BootstrapOptions{
		SessionID: "session-1", PlanMode: planmode.StateInactive, Path: session.Path(),
	})
	require.NoError(t, err)
	require.Len(t, bootstrapped.State.IncompleteReplies, 1)
	assert.Equal(t, IncompleteReply{
		Turn: 2, Text: "half", Reason: "stream ended early", Bytes: 4,
	}, bootstrapped.State.IncompleteReplies[0])
}

func TestBootstrapRejectsCorruptIncompleteReplyRecord(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data string
	}{
		{
			name: "unknown field",
			data: `{"interaction_id":"interaction-1","turn":1,"text":"half","bytes":4,"unknown":true}`,
		},
		{
			name: "fewer bytes than retained",
			data: `{"interaction_id":"interaction-1","turn":1,"text":"half","bytes":3}`,
		},
		{
			name: "negative turn",
			data: `{"interaction_id":"interaction-1","turn":-1,"text":"half","bytes":4}`,
		},
		{
			name: "missing interaction id",
			data: `{"turn":1,"text":"half","bytes":4}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			path := []harness.Entry{{
				Kind: harness.KindCustom, ID: "entry-1",
				Custom: incompleteCustomType, Data: ai.JSON(test.data),
			}}

			_, err := BootstrapState(BootstrapOptions{
				SessionID: "session-1", PlanMode: planmode.StateInactive, Path: path,
			})
			require.ErrorIs(t, err, ErrEventProtocol)
		})
	}
}
