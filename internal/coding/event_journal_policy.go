package coding

// EventJournalDurability classifies whether a committed coding event belongs in
// the durable session journal. A live-only event is a projection for connected
// frontends: its content is derivable from the authoritative transcript and it
// must never be persisted as a journal record.
type EventJournalDurability uint8

const (
	// EventJournalDurable means the event is a durable lifecycle or audit fact.
	EventJournalDurable EventJournalDurability = iota
	// EventJournalLiveOnly means the event exists only for the live projection.
	EventJournalLiveOnly
)

// JournalDurability reports the persistence policy for one event type.
//
// Streaming deltas and retry notices build a message that is already recorded
// by message.committed and the transcript. session.tree_changed carries a full
// transcript snapshot; persisting it duplicates the transcript and turns every
// interaction into an O(transcript) journal record. All three stay live-only.
func JournalDurability(eventType EventType) EventJournalDurability {
	switch eventType {
	case EventMessageDelta, EventModelRetry, EventSessionTreeChanged:
		return EventJournalLiveOnly
	default:
		return EventJournalDurable
	}
}
