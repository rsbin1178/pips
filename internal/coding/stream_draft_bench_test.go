//nolint:wsl_v5 // Benchmarks keep fixture setup and the measured transition adjacent.
package coding

import (
	"runtime"
	"strconv"
	"testing"

	"github.com/rsbin1178/pips/ai"
)

// streamDraftBenchPrior is the set of prior-increment counts the draft
// microbenchmarks sweep. Every operation under test must stay independent of
// the history length.
var streamDraftBenchPrior = []int{1000, 10000, 50000}

func streamDraftBenchDeltas(count int) []MessageDelta {
	records := make([]MessageDelta, count)
	for index := range records {
		if index%4 == 0 {
			records[index] = reasoningDelta("reasoning token ")
		} else {
			records[index] = textDelta("text token ")
		}
	}

	return records
}

// streamDraftBenchBase builds a draft of count records through the canonical
// batch path, matching how the reducer fills a long stream.
func streamDraftBenchBase(count int) StreamDraft {
	records := streamDraftBenchDeltas(count)

	draft := StreamDraft{}
	for start := 0; start < len(records); start += streamDraftChunkSize {
		draft = draft.AppendBatch(records[start:min(start+streamDraftChunkSize, len(records))])
	}

	return draft
}

// BenchmarkStreamDraftAppend measures one increment appended onto increasingly
// long histories. The cost must not grow with the number of prior records.
func BenchmarkStreamDraftAppend(b *testing.B) {
	delta := MessageDelta{Kind: ai.StreamTextDelta, Text: "token"}

	for _, prior := range streamDraftBenchPrior {
		b.Run("prior_"+strconv.Itoa(prior), func(b *testing.B) {
			base := streamDraftBenchBase(prior)
			b.ReportAllocs()

			for b.Loop() {
				_ = base.Append(delta)
			}

			runtime.KeepAlive(base)
		})
	}
}

// BenchmarkStreamDraftAppendBatch measures one frame's worth of increments. It
// copies the partial tail once and fills canonical blocks, so the cost is
// bounded by the batch plus one block, not by the history.
func BenchmarkStreamDraftAppendBatch(b *testing.B) {
	batch := streamDraftBenchDeltas(streamDraftChunkSize)

	for _, prior := range streamDraftBenchPrior {
		b.Run("prior_"+strconv.Itoa(prior), func(b *testing.B) {
			base := streamDraftBenchBase(prior)
			b.ReportAllocs()

			for b.Loop() {
				_ = base.AppendBatch(batch)
			}

			runtime.KeepAlive(base)
		})
	}
}

// BenchmarkStreamDraftSnapshot measures the snapshot path's draft handling:
// State.Clone shares the immutable draft value instead of copying N records.
func BenchmarkStreamDraftSnapshot(b *testing.B) {
	for _, prior := range streamDraftBenchPrior {
		b.Run("prior_"+strconv.Itoa(prior), func(b *testing.B) {
			state := State{Draft: streamDraftBenchBase(prior)}
			b.ReportAllocs()

			for b.Loop() {
				snapshot := state.Clone()
				runtime.KeepAlive(snapshot.Draft)
			}
		})
	}
}

// BenchmarkStreamDraftTextRead measures a full aggregate read (the live draft
// body the TUI renders). It stays O(records); the chunk directory is
// O(records/chunk), so this is where a larger block pays off.
func BenchmarkStreamDraftTextRead(b *testing.B) {
	for _, prior := range streamDraftBenchPrior {
		b.Run("prior_"+strconv.Itoa(prior), func(b *testing.B) {
			draft := streamDraftBenchBase(prior)
			b.ReportAllocs()

			var size int
			for b.Loop() {
				size = len(draft.Text())
			}

			runtime.KeepAlive(size)
		})
	}
}

// BenchmarkStreamDraftActivity measures the activity query the TUI runs every
// frame: it reads the draft's summary without touching the records.
func BenchmarkStreamDraftActivity(b *testing.B) {
	for _, prior := range streamDraftBenchPrior {
		b.Run("prior_"+strconv.Itoa(prior), func(b *testing.B) {
			draft := streamDraftBenchBase(prior)
			b.ReportAllocs()

			for b.Loop() {
				if !draft.HasTextDelta() {
					b.Fatal("draft lost its text summary")
				}
				runtime.KeepAlive(draft.Len())
			}
		})
	}
}
