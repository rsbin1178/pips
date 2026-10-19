package coding

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEventLogSinkSkipsLiveProjections(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), session.JournalFileName)
	sink := openEventLogSink(path, "s-journal")

	transcript := make(ai.Messages, 0, 5)
	for range 5 {
		transcript = append(transcript, ai.UserText(strings.Repeat("x", 1<<20)))
	}

	sink.Append(Event{
		Type: EventSessionTreeChanged,
		Payload: SessionTreeChanged{
			Transcript: transcript,
		},
	})
	sink.Append(Event{
		Type:    EventMessageDelta,
		Payload: MessageDelta{Kind: ai.StreamTextDelta, Text: "live"},
	})
	sink.Append(Event{
		Type:    EventModelRetry,
		Payload: ModelRetry{Attempt: 1, MaxRetries: 1},
	})

	require.NoError(t, sink.Close())

	records, err := session.ReadJournal(path, 0)
	require.NoError(t, err)
	assert.Empty(t, records, "live projections never enter the durable journal")
}

func TestEventLogSinkJournalsLargeDurableRecord(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), session.JournalFileName)
	sink := openEventLogSink(path, "s-journal")
	sink.Append(Event{
		Type: EventMessageCommitted,
		Payload: MessageCommitted{
			Message: ai.UserText(strings.Repeat("x", 4<<20)),
		},
	})
	sink.Append(Event{
		Type:    EventTurnStarted,
		Payload: TurnStarted{Turn: 1},
	})

	require.NoError(t, sink.Close())

	records, err := session.ReadJournal(path, 0)
	require.NoError(t, err)
	require.Len(t, records, 2, "a large durable record is written, not skipped")
	assert.Equal(t, string(EventMessageCommitted), records[0].Type)
	assert.Greater(t, len(records[0].Payload), 4<<20)
	assert.Equal(t, uint64(1), records[0].Seq)
	assert.Equal(t, uint64(2), records[1].Seq)
}

func TestEventLogSinkJournalsDurableFacts(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), session.JournalFileName)
	sink := openEventLogSink(path, "s-journal")
	sink.Append(Event{
		Type:    EventTurnStarted,
		Payload: TurnStarted{Turn: 1},
	})
	require.NoError(t, sink.Close())

	records, err := session.ReadJournal(path, 0)
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, string(EventTurnStarted), records[0].Type)
}
