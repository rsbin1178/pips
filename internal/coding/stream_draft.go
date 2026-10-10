//nolint:wsl_v5 // The immutable draft keeps its append, copy and summary steps adjacent.
package coding

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	"github.com/rsbin1178/pips/ai"
)

// streamDraftChunkSize is the maximum number of records one immutable draft
// chunk holds. It bounds the records a single append copies; the chunk
// directory is O(records/chunk) and is only walked when the whole draft is
// read. It is deliberately independent of the TUI frame batch size.
const streamDraftChunkSize = 32

// streamDraftChunk is one immutable block in a draft's append-only history.
// prev points at the next-older block and is never rewritten. records holds at
// most streamDraftChunkSize entries; only the newest block may be
// underfilled. A published chunk is never mutated, so a retained State keeps
// the prefixes it owns.
type streamDraftChunk struct {
	prev    *streamDraftChunk
	records []MessageDelta
}

// StreamDraft is an immutable, chunked sequence of streaming [MessageDelta]
// records. It is the value stored in State.Draft.
//
// Its zero value is a usable empty draft. An append returns a new value that
// shares every published chunk with its input, so a retained State or Snapshot
// is never rewritten. The block layout stays private: callers read the
// summary, aggregate text, or materialize a caller-owned copy. The summary
// (count, per-kind presence and UTF-8 byte totals) is part of the immutable
// value, so an activity query reads it without scanning the records.
//
// It marshals as the field's legacy form: a JSON array of [MessageDelta]
// records, or null for an empty draft. See [StreamDraft.MarshalJSON].
type StreamDraft struct {
	tail             *streamDraftChunk
	count            int
	textPresent      bool
	reasoningPresent bool
	textBytes        int
	reasoningBytes   int
	// emptyArray marks a draft decoded from a non-nil empty JSON array. The
	// zero value serializes as null, which is what the legacy nil
	// []MessageDelta field emitted; this marker keeps the legacy null-vs-[]
	// distinction across a decode/encode round trip.
	emptyArray bool
}

// NewStreamDraft builds a draft from records in order. It clones each record's
// mutable Usage pointer, so the caller keeps ownership of what it passes. With
// no records the result is the empty draft, which marshals as null.
func NewStreamDraft(deltas ...MessageDelta) StreamDraft {
	return StreamDraft{}.AppendBatch(deltas)
}

// Len returns the number of records the draft holds.
func (draft StreamDraft) Len() int {
	return draft.count
}

// HasTextDelta reports whether any record is a text delta. Presence is not the
// same as nonempty text: an empty text delta still counts, matching the legacy
// per-record kind scan.
func (draft StreamDraft) HasTextDelta() bool {
	return draft.textPresent
}

// HasReasoningDelta reports whether any record is a reasoning delta.
func (draft StreamDraft) HasReasoningDelta() bool {
	return draft.reasoningPresent
}

// Text concatenates the Text of every text delta in order. It grows its buffer
// from the draft's byte summary and reads records in place.
func (draft StreamDraft) Text() string {
	if draft.textBytes == 0 {
		return ""
	}

	var content strings.Builder
	content.Grow(draft.textBytes)

	draft.forEach(func(delta MessageDelta) bool {
		if delta.Kind == ai.StreamTextDelta {
			content.WriteString(delta.Text)
		}

		return true
	})

	return content.String()
}

// ReasoningText concatenates the Text of every reasoning delta in order. It
// grows its buffer from the draft's byte summary.
func (draft StreamDraft) ReasoningText() string {
	if draft.reasoningBytes == 0 {
		return ""
	}

	var content strings.Builder
	content.Grow(draft.reasoningBytes)

	draft.forEach(func(delta MessageDelta) bool {
		if delta.Kind == ai.StreamReasoningDelta {
			content.WriteString(delta.Text)
		}

		return true
	})

	return content.String()
}

// Append returns a new draft with one record added. The input draft and every
// chunk it shares stay unchanged.
func (draft StreamDraft) Append(delta MessageDelta) StreamDraft {
	tail := draft.newTail(1)
	tail.records = append(tail.records, cloneMessageDelta(delta))
	draft.tail = tail
	draft.emptyArray = false
	draft.addRecordSummary(delta)

	return draft
}

// AppendBatch returns a new draft with the records added in order. It copies
// the published partial tail once and then fills canonical fixed-size blocks,
// so the resulting chunk boundaries never depend on the caller's batch
// boundaries. Every input record is preserved.
func (draft StreamDraft) AppendBatch(deltas []MessageDelta) StreamDraft {
	remaining := deltas
	if len(remaining) == 0 {
		return draft
	}

	tail := draft.newTail(min(streamDraftChunkSize, len(remaining)))

	for len(remaining) > 0 {
		take := min(streamDraftChunkSize-len(tail.records), len(remaining))
		for _, delta := range remaining[:take] {
			tail.records = append(tail.records, cloneMessageDelta(delta))
			draft.addRecordSummary(delta)
		}

		remaining = remaining[take:]
		if len(remaining) > 0 {
			tail = &streamDraftChunk{
				prev:    tail,
				records: make([]MessageDelta, 0, min(streamDraftChunkSize, len(remaining))),
			}
		}
	}

	draft.tail = tail
	draft.emptyArray = false

	return draft
}

// Materialize returns a caller-owned copy of the records in order. Each record
// is copied (including its Usage pointer), so mutating the result cannot reach
// the draft or any State that shares it.
func (draft StreamDraft) Materialize() []MessageDelta {
	if draft.count == 0 {
		return nil
	}

	records := make([]MessageDelta, 0, draft.count)
	draft.forEach(func(delta MessageDelta) bool {
		records = append(records, cloneMessageDelta(delta))

		return true
	})

	return records
}

// Equal reports whether two drafts hold the same records and summary. It
// compares logical contents, not chunk pointer identity, so two drafts built
// by different batch splits compare equal.
func (draft StreamDraft) Equal(other StreamDraft) bool {
	if draft.count != other.count ||
		draft.textPresent != other.textPresent ||
		draft.reasoningPresent != other.reasoningPresent ||
		draft.textBytes != other.textBytes ||
		draft.reasoningBytes != other.reasoningBytes {
		return false
	}

	left := draft.rawRecords()
	right := other.rawRecords()
	for index := range left {
		if !messageDeltaEqual(left[index], right[index]) {
			return false
		}
	}

	return true
}

// MarshalJSON writes the legacy JSON array of MessageDelta records. An empty
// draft writes null, matching the legacy nil []MessageDelta field, unless it
// was decoded from a non-nil empty array, which writes [] again.
func (draft StreamDraft) MarshalJSON() ([]byte, error) {
	if draft.count == 0 {
		if draft.emptyArray {
			return []byte("[]"), nil
		}

		return []byte("null"), nil
	}

	return json.Marshal(draft.rawRecords())
}

// UnmarshalJSON decodes the legacy JSON array, or null. It builds into a
// temporary value and only assigns on success, so a rejected decode leaves the
// receiver unchanged.
func (draft *StreamDraft) UnmarshalJSON(data []byte) error {
	if draft == nil {
		return invalidEvent("cannot decode a stream draft into nil")
	}

	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		*draft = StreamDraft{}

		return nil
	}

	var records []MessageDelta
	if err := json.Unmarshal(data, &records); err != nil {
		return err
	}

	decoded := NewStreamDraft(records...)
	decoded.emptyArray = records != nil
	*draft = decoded

	return nil
}

// newTail returns a writable tail. It copies a published partial tail so that
// chunk stays immutable, or starts a fresh block after a full or absent one.
//
// incoming is how many records the current call may still write. The copied
// tail reserves room for its existing records plus incoming, capped at
// streamDraftChunkSize: a published partial tail is copied again on the next
// append, so spare capacity is never reused. A fresh block reserves the same
// bounded amount.
func (draft StreamDraft) newTail(incoming int) *streamDraftChunk {
	if draft.tail != nil && len(draft.tail.records) < streamDraftChunkSize {
		copied := &streamDraftChunk{
			prev:    draft.tail.prev,
			records: make([]MessageDelta, len(draft.tail.records), min(streamDraftChunkSize, len(draft.tail.records)+incoming)),
		}
		copy(copied.records, draft.tail.records)

		return copied
	}

	return &streamDraftChunk{prev: draft.tail, records: make([]MessageDelta, 0, min(streamDraftChunkSize, incoming))}
}

// addRecordSummary folds one record into the draft's summary. The pointer
// receiver is the value being built, so this never touches a published draft.
func (draft *StreamDraft) addRecordSummary(delta MessageDelta) {
	draft.count++

	switch delta.Kind {
	case ai.StreamTextDelta:
		draft.textPresent = true
		draft.textBytes += len(delta.Text)
	case ai.StreamReasoningDelta:
		draft.reasoningPresent = true
		draft.reasoningBytes += len(delta.Text)
	default:
		// Every other record kind — signatures, metadata, tool-call increments —
		// stays in the log without contributing to the text summary.
	}
}

// forEach visits records oldest-first. The chunk directory is reversed once
// (O(records/chunk)) and record traversal stays O(records); the walk is
// iterative, not recursive.
func (draft StreamDraft) forEach(yield func(MessageDelta) bool) {
	for _, chunk := range draft.chunksOldestFirst() {
		for _, delta := range chunk.records {
			if !yield(delta) {
				return
			}
		}
	}
}

// rawRecords returns the records in order without copying Usage. It is for
// read-only in-process use (serialization, logical comparison), never for a
// caller that keeps the result.
func (draft StreamDraft) rawRecords() []MessageDelta {
	if draft.count == 0 {
		return nil
	}

	records := make([]MessageDelta, 0, draft.count)
	for _, chunk := range draft.chunksOldestFirst() {
		records = append(records, chunk.records...)
	}

	return records
}

// chunksOldestFirst returns the immutable blocks in record order. It walks the
// predecessor links once and reverses the temporary directory in place.
func (draft StreamDraft) chunksOldestFirst() []*streamDraftChunk {
	if draft.tail == nil {
		return nil
	}

	chunks := make([]*streamDraftChunk, 0, (draft.count+streamDraftChunkSize-1)/streamDraftChunkSize)
	for chunk := draft.tail; chunk != nil; chunk = chunk.prev {
		chunks = append(chunks, chunk)
	}

	slices.Reverse(chunks)

	return chunks
}

// messageDeltaEqual compares two records by value, including the pointee of a
// non-nil Usage.
func messageDeltaEqual(left, right MessageDelta) bool {
	if (left.Usage == nil) != (right.Usage == nil) {
		return false
	}

	if left.Usage != nil && *left.Usage != *right.Usage {
		return false
	}

	left.Usage = nil
	right.Usage = nil

	return left == right
}
