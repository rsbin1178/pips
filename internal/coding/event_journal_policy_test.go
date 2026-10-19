package coding

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestJournalDurabilityKeepsLiveProjectionsOutOfTheJournal(t *testing.T) {
	t.Parallel()

	live := []EventType{
		EventMessageDelta,
		EventModelRetry,
		EventSessionTreeChanged,
	}
	for _, eventType := range live {
		assert.Equal(t, EventJournalLiveOnly, JournalDurability(eventType), string(eventType))
	}

	durable := []EventType{
		EventSessionOpened,
		EventSessionClosed,
		EventSessionNavigated,
		EventCompactionCompleted,
		EventInteractionStarted,
		EventInteractionCompleted,
		EventTurnStarted,
		EventTurnCompleted,
		EventMessageCommitted,
		EventToolStarted,
		EventToolCompleted,
		EventApprovalResolved,
		EventStatusChanged,
		EventError,
	}
	for _, eventType := range durable {
		assert.Equal(t, EventJournalDurable, JournalDurability(eventType), string(eventType))
	}
}
