package coding

import (
	"encoding/json"
	"slices"
	"strconv"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func textDelta(text string) MessageDelta {
	return MessageDelta{Kind: ai.StreamTextDelta, Text: text}
}

func reasoningDelta(text string) MessageDelta {
	return MessageDelta{Kind: ai.StreamReasoningDelta, Text: text}
}

func TestStreamDraftZeroValueIsEmpty(t *testing.T) {
	t.Parallel()

	var draft StreamDraft
	assert.Equal(t, 0, draft.Len())
	assert.False(t, draft.HasTextDelta())
	assert.False(t, draft.HasReasoningDelta())
	assert.Empty(t, draft.Materialize())
	assert.Empty(t, draft.Text())
	assert.Empty(t, draft.ReasoningText())
}

func TestStreamDraftJSONEmptyAndNull(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(StreamDraft{})
	require.NoError(t, err)
	assert.Equal(t, "null", string(encoded), "the zero value matches the legacy nil slice")

	empty, err := json.Marshal(NewStreamDraft())
	require.NoError(t, err)
	assert.Equal(t, "null", string(empty))

	stateJSON, err := json.Marshal(State{})
	require.NoError(t, err)
	assert.Contains(t, string(stateJSON), `"draft":null`)

	var fromNull StreamDraft
	require.NoError(t, json.Unmarshal([]byte("null"), &fromNull))
	encoded, err = json.Marshal(fromNull)
	require.NoError(t, err)
	assert.Equal(t, "null", string(encoded))

	var fromEmpty StreamDraft
	require.NoError(t, json.Unmarshal([]byte("[]"), &fromEmpty))
	encoded, err = json.Marshal(fromEmpty)
	require.NoError(t, err)
	assert.Equal(t, "[]", string(encoded))

	var state State
	require.NoError(t, json.Unmarshal([]byte(`{"draft":null}`), &state))
	stateJSON, err = json.Marshal(state)
	require.NoError(t, err)
	assert.Contains(t, string(stateJSON), `"draft":null`)

	require.NoError(t, json.Unmarshal([]byte(`{"draft":[]}`), &state))
	stateJSON, err = json.Marshal(state)
	require.NoError(t, err)
	assert.Contains(t, string(stateJSON), `"draft":[]`)
}

func TestStreamDraftAppendAfterEmptyDecodeWritesArray(t *testing.T) {
	t.Parallel()

	for _, wire := range []string{"null", "[]"} {
		var draft StreamDraft
		require.NoError(t, json.Unmarshal([]byte(wire), &draft))

		draft = draft.Append(textDelta("after"))
		encoded, err := json.Marshal(draft)
		require.NoError(t, err)
		assert.JSONEq(t, `[{"kind":"text_delta","text":"after"}]`, string(encoded), "wire=%s", wire)
	}
}

func TestStreamDraftJSONRoundTripPreservesRecords(t *testing.T) {
	t.Parallel()

	records := []MessageDelta{
		{Kind: ai.StreamMessageStart, Provider: ai.ProviderOpenAI, Model: "test-model", ResponseID: "response-1"},
		textDelta("first "),
		{Kind: ai.StreamReasoningDelta, Text: "think", Signature: "sig-1"},
		{Kind: ai.StreamReasoningDelta, Signature: "signature-only"},
		{Kind: ai.StreamTextDelta, Text: ""},
		{Kind: ai.StreamToolCallDelta, ToolCallIndex: 2, ToolCallID: "call-1", ToolCallName: "read", Arguments: `{"path":`},
		{Kind: ai.StreamToolCallDelta, ToolCallIndex: 2, Arguments: `"file.go"}`},
		textDelta("last"),
		{Kind: ai.StreamMessageEnd, FinishReason: ai.FinishStop, Usage: &TokenUsage{
			InputTokens: 5, OutputTokens: 3, ReasoningTokens: 2, CachedInputTokens: 1, CacheWriteTokens: 4,
		}},
	}

	raw, err := json.Marshal(records)
	require.NoError(t, err)

	var draft StreamDraft
	require.NoError(t, json.Unmarshal(raw, &draft))
	require.Equal(t, len(records), draft.Len())

	encoded, err := json.Marshal(draft)
	require.NoError(t, err)
	assert.Equal(t, raw, encoded, "the legacy field order and values must remain unchanged")
	assert.Equal(t, records, draft.Materialize())
	assert.Equal(t, "first last", draft.Text())
	assert.Equal(t, "think", draft.ReasoningText())
}

func TestStreamDraftJSONDecodeFailureLeavesReceiver(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		wire string
	}{
		{name: "wrong top-level type", wire: `{}`},
		{name: "invalid record after valid record", wire: `[{"kind":"text_delta","text":"replacement"},{"text":42}]`},
		{name: "invalid usage", wire: `[{"kind":"text_delta","usage":{"output_tokens":"invalid"}}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			draft := NewStreamDraft(textDelta("keep"), reasoningDelta("thought"))
			before := draft
			beforeRecords := draft.Materialize()

			// Valid JSON reaches StreamDraft.UnmarshalJSON; malformed syntax
			// would be rejected by encoding/json before calling our decoder.
			require.True(t, json.Valid([]byte(test.wire)))
			require.Error(t, json.Unmarshal([]byte(test.wire), &draft))
			assert.Equal(t, beforeRecords, draft.Materialize())
			assert.True(t, before.Equal(draft), "a rejected decode changed the summary")
		})
	}
}

func TestStreamDraftDecodeOwnsOnlyTheCopiedHead(t *testing.T) {
	t.Parallel()

	for _, wire := range []string{"null", "[]", `[{"kind":"reasoning_delta","text":"replacement"}]`} {
		t.Run(wire, func(t *testing.T) {
			t.Parallel()

			original := State{Draft: NewStreamDraft(textDelta("retained"))}
			cloned := original.Clone()
			require.NoError(t, json.Unmarshal([]byte(wire), &cloned.Draft))
			assert.Equal(t, "retained", original.Draft.Text())
			assert.True(t, original.Draft.HasTextDelta())
			assert.False(t, original.Draft.HasReasoningDelta())

			// Cloning and an empty append must preserve the legacy null/[]
			// distinction as well as nonempty decoded records.
			cloned = cloned.Clone()
			cloned.Draft = cloned.Draft.AppendBatch(nil)
			encoded, err := json.Marshal(cloned.Draft)
			require.NoError(t, err)
			assert.JSONEq(t, wire, string(encoded))
		})
	}
}

func TestStreamDraftAppendChunkSeams(t *testing.T) {
	t.Parallel()

	for _, count := range []int{
		streamDraftChunkSize - 1,
		streamDraftChunkSize,
		streamDraftChunkSize + 1,
		3*streamDraftChunkSize + 5,
	} {
		draft := StreamDraft{}
		for index := range count {
			draft = draft.Append(textDelta(strconv.Itoa(index) + ","))
		}

		require.Equal(t, count, draft.Len(), "count=%d", count)
		records := draft.Materialize()
		require.Len(t, records, count)

		for index, record := range records {
			assert.Equal(t, strconv.Itoa(index)+",", record.Text, "count=%d index=%d", count, index)
		}
	}
}

func TestStreamDraftAppendBatchMatchesSequentialAppends(t *testing.T) {
	t.Parallel()

	records := make([]MessageDelta, 3*streamDraftChunkSize+7)
	for index := range records {
		if index%3 == 0 {
			records[index] = reasoningDelta(strconv.Itoa(index) + " ")
		} else {
			records[index] = textDelta(strconv.Itoa(index) + " ")
		}
	}

	for _, size := range []int{
		1,
		2,
		streamDraftChunkSize - 1,
		streamDraftChunkSize,
		streamDraftChunkSize + 1,
		2*streamDraftChunkSize + 3,
		len(records),
	} {
		sequential := StreamDraft{}
		for _, record := range records {
			sequential = sequential.Append(record)
		}

		batched := StreamDraft{}
		for start := 0; start < len(records); start += size {
			batched = batched.AppendBatch(records[start:min(start+size, len(records))])
		}

		assert.True(t, sequential.Equal(batched), "size=%d", size)
		assert.Equal(t, sequential.Materialize(), batched.Materialize(), "size=%d", size)
	}
}

func TestStreamDraftSharedTailStaysIndependent(t *testing.T) {
	t.Parallel()

	for _, count := range []int{2, streamDraftChunkSize - 1, streamDraftChunkSize, streamDraftChunkSize + 1, 3*streamDraftChunkSize + 5} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			t.Parallel()

			var base StreamDraft
			for index := range count {
				base = base.Append(textDelta(strconv.Itoa(index) + ","))
			}

			baseRecords := base.Materialize()
			first := base.Append(textDelta("first-"))
			firstRecords := first.Materialize()
			second := base.Append(reasoningDelta("second-"))
			secondRecords := second.Materialize()
			batch := slices.Repeat([]MessageDelta{textDelta("batch-"), reasoningDelta("thought-")}, streamDraftChunkSize+1)
			batched := base.AppendBatch(batch)

			assert.Equal(t, baseRecords, base.Materialize(), "an append rewrote the shared base")
			assert.Equal(t, firstRecords, first.Materialize(), "a sibling rewrote the first tail")
			assert.Equal(t, secondRecords, second.Materialize(), "a batch rewrote the second tail")
			assert.Equal(t, slices.Concat(baseRecords, batch), batched.Materialize())
			assert.False(t, base.HasReasoningDelta(), "a sibling changed the retained summary")
			assert.False(t, first.HasReasoningDelta())
			assert.True(t, second.HasReasoningDelta())
		})
	}
}

func TestStreamDraftMaterializeOwnsUsage(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		append func(StreamDraft, MessageDelta) StreamDraft
	}{
		{name: "single append", append: StreamDraft.Append},
		{name: "batch append", append: func(draft StreamDraft, delta MessageDelta) StreamDraft {
			return draft.AppendBatch([]MessageDelta{delta})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			usage := &TokenUsage{OutputTokens: 7}
			draft := test.append(StreamDraft{}, MessageDelta{Kind: ai.StreamTextDelta, Text: "answer", Usage: usage})
			sibling := draft.Append(reasoningDelta("thought"))
			usage.OutputTokens = 99
			records := draft.Materialize()
			require.Len(t, records, 1)
			require.NotNil(t, records[0].Usage)
			assert.Equal(t, 7, records[0].Usage.OutputTokens, "ingestion must clone the caller's Usage")

			records[0].Usage.OutputTokens = 123
			records[0].Text = "mutated"
			again := draft.Materialize()
			assert.Equal(t, 7, again[0].Usage.OutputTokens, "materialization aliased the draft's Usage")
			assert.Equal(t, "answer", again[0].Text, "materialization aliased the draft's record")
			assert.Equal(t, again[0], sibling.Materialize()[0], "materialization rewrote a shared sibling record")
		})
	}
}

func TestStreamDraftSummaryCountsPresenceAndBytes(t *testing.T) {
	t.Parallel()

	draft := NewStreamDraft(
		textDelta("héllo"),
		textDelta(""),
		reasoningDelta("think"),
		MessageDelta{Kind: ai.StreamToolCallDelta, Text: "ignored"},
	)

	assert.Equal(t, 4, draft.Len())
	assert.True(t, draft.HasTextDelta(), "an empty text delta still sets presence")
	assert.True(t, draft.HasReasoningDelta())
	assert.Equal(t, "héllo", draft.Text())
	assert.Equal(t, "think", draft.ReasoningText())
	assert.Equal(t, len("héllo"), draft.textBytes, "byte total must count UTF-8 bytes, not runes")
	assert.Equal(t, len("think"), draft.reasoningBytes)

	present := NewStreamDraft(MessageDelta{Kind: ai.StreamTextDelta})
	assert.True(t, present.HasTextDelta(), "presence survives an empty text delta")
	assert.Equal(t, 1, present.Len())
	assert.Empty(t, present.Text())
}

func TestStreamDraftForwardTraversalIsOrdered(t *testing.T) {
	t.Parallel()

	count := 4*streamDraftChunkSize + 3

	draft := StreamDraft{}
	for index := range count {
		draft = draft.Append(textDelta(strconv.Itoa(index)))
	}

	for _, limit := range []int{streamDraftChunkSize + 1, count} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			t.Parallel()

			var visited []string

			draft.forEach(func(delta MessageDelta) bool {
				visited = append(visited, delta.Text)

				return len(visited) < limit
			})

			require.Len(t, visited, limit)

			for position, value := range visited {
				assert.Equal(t, strconv.Itoa(position), value)
			}
		})
	}
}

func TestStreamDraftEqualComparesLogicalContents(t *testing.T) {
	t.Parallel()

	left := NewStreamDraft(textDelta("a"), reasoningDelta("b"))
	right := StreamDraft{}
	right = right.Append(textDelta("a")).Append(reasoningDelta("b"))

	assert.True(t, left.Equal(right))
	assert.False(t, left.Equal(NewStreamDraft(textDelta("a"))))
	assert.False(t, left.Equal(StreamDraft{}))
}
