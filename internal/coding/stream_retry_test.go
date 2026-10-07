package coding

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reduceAll applies events in order, assigning the sequences a live stream
// would carry.
func reduceAll(t *testing.T, events []Event) State {
	t.Helper()

	var state State

	for index, event := range events {
		event.Sequence = uint64(index + 1)

		next, err := Reduce(state, event)
		require.NoError(t, err)

		state = next
	}

	return state
}

// retryEvents opens a run whose first attempt streamed one delta, which is the
// state a retry notice arrives in.
func retryEvents(extra ...Event) []Event {
	events := []Event{
		newSessionEvent(EventSessionOpened, SessionOpened{Provider: ai.ProviderOpenAI, ModelID: "gpt-test"}),
		newInteractionEvent(EventInteractionStarted, InteractionStarted{}),
		newStatusEvent(EventStatusChanged, StatusChanged{Phase: PhaseRunning}),
		newTestEvent(EventRunStarted, RunStarted{Agent: "coding"}),
		newTestEvent(EventTurnStarted, TurnStarted{Turn: 1}),
		newTestEvent(EventMessageDelta, MessageDelta{Kind: ai.StreamTextDelta, Text: "partial"}),
	}

	return append(events, extra...)
}

func TestReduceModelRetryTracksWaitUntilTheNextEvent(t *testing.T) {
	t.Parallel()

	notice := ModelRetry{Turn: 1, Attempt: 2, MaxRetries: 3, DelayMillis: 4_000, Reason: "stream ended early"}

	waiting := reduceAll(t, retryEvents(newTestEvent(EventModelRetry, notice)))
	require.True(t, waiting.Retry.Active)
	assert.Equal(t, 2, waiting.Retry.Attempt)
	assert.Equal(t, 3, waiting.Retry.MaxRetries)
	assert.Equal(t, "stream ended early", waiting.Retry.Reason)
	assert.Equal(t, eventTestTime.Add(4*time.Second), waiting.Retry.Deadline,
		"the deadline anchors the countdown a frontend renders")

	// A re-issued turn discards the output that streamed before the failure,
	// and the first delta of the new attempt ends the wait.
	resumed := reduceAll(t, retryEvents(
		newTestEvent(EventMessageDiscarded, MessageDiscarded{Turn: 1}),
		newTestEvent(EventModelRetry, notice),
		newTestEvent(EventMessageDelta, MessageDelta{Kind: ai.StreamTextDelta, Text: "retried"}),
	))
	assert.False(t, resumed.Retry.Active)
	assert.Zero(t, resumed.Retry.Deadline)
	require.Len(t, resumed.Draft, 1)
	assert.Equal(t, "retried", resumed.Draft[0].Text)
}

func TestReduceBatchEndsTheWaitWhenADeltaFollows(t *testing.T) {
	t.Parallel()

	prefix := retryEvents()
	state := reduceAll(t, prefix)

	notice := newTestEvent(EventModelRetry, ModelRetry{Turn: 1, Attempt: 2, MaxRetries: 3, Reason: "connection error"})
	notice.Sequence = uint64(len(prefix) + 1)

	waiting, err := ReduceBatch(state, []Event{notice})
	require.NoError(t, err)
	require.True(t, waiting.Retry.Active)

	delta := newTestEvent(EventMessageDelta, MessageDelta{Kind: ai.StreamTextDelta, Text: "again"})
	delta.Sequence = notice.Sequence + 1

	// The live path reduces one frame per batch, which folds delta runs; the
	// folded run is output, so it must end the wait too.
	resumed, err := ReduceBatch(state, []Event{notice, delta})
	require.NoError(t, err)
	assert.False(t, resumed.Retry.Active)
	require.NotEmpty(t, resumed.Draft)
	assert.Equal(t, "again", resumed.Draft[len(resumed.Draft)-1].Text)
}

func TestReduceModelRetryRequiresAnActiveTurn(t *testing.T) {
	t.Parallel()

	// A retry notice belongs to an open turn, because a stream cannot have an
	// attempt to retry without one.
	events := []Event{
		newSessionEvent(EventSessionOpened, SessionOpened{Provider: ai.ProviderOpenAI, ModelID: "gpt-test"}),
		newInteractionEvent(EventInteractionStarted, InteractionStarted{}),
		newStatusEvent(EventStatusChanged, StatusChanged{Phase: PhaseRunning}),
		newTestEvent(EventRunStarted, RunStarted{Agent: "coding"}),
		newTestEvent(EventModelRetry, ModelRetry{Turn: 1, Attempt: 1, MaxRetries: 1}),
	}

	prefix := events[:len(events)-1]
	state := reduceAll(t, prefix)

	last := events[len(events)-1]
	last.Sequence = uint64(len(events))

	_, err := Reduce(state, last)
	require.ErrorIs(t, err, ErrEventProtocol)
}

func TestModelRetryEventRoundTripsAndRejectsMalformedPayloads(t *testing.T) {
	t.Parallel()

	event := newTestEvent(EventModelRetry, ModelRetry{
		Turn: 1, Attempt: 2, MaxRetries: 6, DelayMillis: 1_500, Reason: "connection error",
	})
	require.NoError(t, ValidateEvent(event))

	encoded, err := MarshalEvent(event)
	require.NoError(t, err)

	decoded, err := UnmarshalEvent(encoded)
	require.NoError(t, err)
	assert.Equal(t, EventModelRetry, decoded.Type)
	assert.Equal(t, event.Payload, decoded.Payload)

	bad := []ModelRetry{
		{Turn: 1, Attempt: 0, MaxRetries: 1}, // no attempt number
		{Turn: 1, Attempt: 3, MaxRetries: 2}, // unreachable attempt
		{Turn: 1, Attempt: 1, MaxRetries: 1, DelayMillis: -1},
		{Turn: -1, Attempt: 1, MaxRetries: 1}, // negative turn
		{Turn: 1, Attempt: 1, MaxRetries: 1, Reason: strings.Repeat("x", maxDiagnosticMessage+1)},
	}
	for _, payload := range bad {
		require.ErrorIs(t, ValidateEvent(newTestEvent(EventModelRetry, payload)), ErrInvalidEvent)
	}

	assert.ErrorIs(t, ValidateEvent(newTestEvent(EventMessageDelta, ModelRetry{Attempt: 1, MaxRetries: 1})),
		ErrInvalidEvent, "the payload must not ride another type")
}

func TestAgentProjectorMapsModelStreamRetryToModelRetryEvent(t *testing.T) {
	t.Parallel()

	writer, err := newEventWriter("session-1", func() time.Time { return eventTestTime })
	require.NoError(t, err)

	projector, err := newAgentProjector(writer, "interaction-1")
	require.NoError(t, err)

	event, err := agent.NewEvent(
		agent.RunMetadata{RunID: "run-1", Agent: "coding"},
		eventTestTime,
		agent.ModelStreamEvent{Turn: 1, Event: ai.StreamEvent{
			Type:  ai.StreamRetry,
			Retry: ai.NewRetryNotice(2, 6, 4*time.Second, io.ErrUnexpectedEOF),
		}},
	)
	require.NoError(t, err)

	projected, err := projector.project(event)
	require.NoError(t, err)
	assert.Equal(t, EventModelRetry, projected.Type)

	retry, ok := projected.Payload.(ModelRetry)
	require.True(t, ok)
	assert.Equal(t, 1, retry.Turn)
	assert.Equal(t, 2, retry.Attempt)
	assert.Equal(t, 6, retry.MaxRetries)
	assert.Equal(t, int64(4_000), retry.DelayMillis)
	assert.Equal(t, "stream ended early", retry.Reason)
}
