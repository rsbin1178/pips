package coding

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var retainedToolSharingSink State

// committedToolBatch reduces one live batch of completed tools and commits
// their shared tool message. With share=false it then re-copies every result
// into its ToolState, which is the retention the sharing path removes.
func committedToolBatch(tb testing.TB, tools, payload int, share bool) State {
	tb.Helper()

	messages := make(ai.Messages, 0, tools)
	for index := range tools {
		messages = append(messages, ai.Assistant(ai.ToolCallPart{
			ID: fmt.Sprintf("call-%d", index), Name: "read",
			Args: ai.JSON(fmt.Sprintf(`{"path":"file-%d.txt"}`, index)),
		}))
	}

	state := fixtureState(tb, messages)
	parts := make([]ai.ToolResultPart, 0, tools)

	for index := range tools {
		call := toolSharingCall(fmt.Sprintf("call-%d", index), ai.JSON(fmt.Sprintf(`{"path":"file-%d.txt"}`, index)))

		started, err := Reduce(state, toolSharingEvent(state, EventToolStarted, ToolStarted{
			Turn: 1, Call: call,
		}))
		require.NoError(tb, err)

		state = started

		result := structuredToolResult(call.ID, ai.JSON(`{"data":"`+strings.Repeat("y", payload)+`"}`))
		completed, err := Reduce(state, toolSharingEvent(state, EventToolCompleted, ToolCompleted{
			Turn: 1, Call: call, Result: result,
		}))
		require.NoError(tb, err)

		state = completed

		parts = append(parts, result.Parts[0])
	}

	committed, err := Reduce(state, toolSharingEvent(state, EventMessageCommitted, MessageCommitted{
		Message: ai.ToolMessage{Parts: parts},
	}))
	require.NoError(tb, err)

	if share {
		return committed
	}

	committed.Tools = cloneBeforeWrite(committed.Tools)
	for index := range committed.Tools {
		message, cloneErr := ai.CloneMessage(committed.Tools[index].Result)
		require.NoError(tb, cloneErr)

		cloned, ok := message.(ai.ToolMessage)
		require.True(tb, ok)

		committed.Tools[index].Result = cloned
	}

	return committed
}

// TestCommittedToolBatchRetainsOnePayloadCopy is the deterministic half of the
// byte proof: every Tool result shares a transcript part instead of owning a
// second copy of the output.
func TestCommittedToolBatchRetainsOnePayloadCopy(t *testing.T) {
	t.Parallel()

	state := committedToolBatch(t, 8, 4096, true)
	transcript, ok := state.Transcript[len(state.Transcript)-1].(ai.ToolMessage)
	require.True(t, ok)
	require.Len(t, transcript.Parts, 8)

	owned := make(map[*byte]struct{}, len(transcript.Parts))
	for index := range transcript.Parts {
		part, ok := transcript.Parts[index].Content[0].(ai.StructuredContentPart)
		require.True(t, ok)

		owned[&part.Data[0]] = struct{}{}
	}

	for index := range state.Tools {
		data := structuredData(t, state.Tools[index].Result)
		_, shared := owned[&data[0]]
		assert.True(t, shared, "tool %d shares its transcript payload", index)
	}

	assert.Len(t, owned, 8, "one payload per committed part")
}

// BenchmarkRetainedCompletedToolBatch reports the retained heap for one
// committed batch. The copied variant re-creates the duplicate result each
// ToolState used to pin; the shared variant keeps only the transcript part.
// Run with -benchtime=1x so the reported retained-B is one state, not a sum.
func BenchmarkRetainedCompletedToolBatch(b *testing.B) {
	const (
		tools   = 32
		payload = 64 << 10
	)

	for _, share := range []bool{true, false} {
		name := "shared"
		if !share {
			name = "copied"
		}

		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()

			for b.Loop() {
				state := committedToolBatch(b, tools, payload, share)
				retainedToolSharingSink = state

				runtime.GC()

				var stats runtime.MemStats
				runtime.ReadMemStats(&stats)
				b.ReportMetric(float64(stats.HeapAlloc), "retained-B")
			}
		})
	}
}
