//nolint:wsl_v5 // History fixtures and their measured reads stay adjacent.
package harness_test

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
)

// historyBenchText is representative prose: large enough that a deep message
// copy is visible in the allocation numbers, small enough to build many.
const historyBenchText = "The quick brown fox jumps over the lazy dog. " +
	"Pack my box with five dozen liquor jugs. 0123456789."

// historyBenchSession builds a session with count alternating messages.
func historyBenchSession(tb testing.TB, count int) (*harness.Session, *harness.MemoryStore) {
	tb.Helper()

	store := harness.NewMemoryStore("bench-history")
	session, err := harness.NewSession(store)
	if err != nil {
		tb.Fatal(err)
	}

	for index := range count {
		text := fmt.Sprintf("%04d %s", index, strings.Repeat(historyBenchText, 2))

		var message ai.Message
		if index%2 == 0 {
			message = ai.UserText(text)
		} else {
			message = ai.AssistantText(text)
		}

		if _, err := session.AppendMessage(message, nil); err != nil {
			tb.Fatal(err)
		}
	}

	return session, store
}

func BenchmarkSessionHistoryReads(b *testing.B) {
	for _, size := range []int{2_000, 8_000} {
		session, _ := historyBenchSession(b, size)

		b.Run(fmt.Sprintf("context/size=%d", size), func(b *testing.B) {
			if _, err := session.Context(); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()

			for b.Loop() {
				if _, err := session.Context(); err != nil {
					b.Fatal(err)
				}
			}
		})

		b.Run(fmt.Sprintf("path/size=%d", size), func(b *testing.B) {
			_ = session.Path()
			b.ReportAllocs()
			b.ResetTimer()

			for b.Loop() {
				_ = session.Path()
			}
		})

		b.Run(fmt.Sprintf("entries/size=%d", size), func(b *testing.B) {
			b.ReportAllocs()

			for b.Loop() {
				_ = session.Entries()
			}
		})

		b.Run(fmt.Sprintf("entries_view/size=%d", size), func(b *testing.B) {
			b.ReportAllocs()

			for b.Loop() {
				_ = session.EntriesView()
			}
		})
	}
}

// BenchmarkSessionOpen measures loading a durable history into a Session. A
// single-owner history must not deep-copy the store's entries a second time.
func BenchmarkSessionOpen(b *testing.B) {
	for _, size := range []int{2_000, 8_000} {
		path := b.TempDir() + "/history.jsonl"

		store, err := harness.CreateJSONL(path, "bench-open", nil)
		if err != nil {
			b.Fatal(err)
		}
		session, err := harness.NewSession(store)
		if err != nil {
			b.Fatal(err)
		}
		for index := range size {
			text := fmt.Sprintf("%04d %s", index, strings.Repeat(historyBenchText, 2))
			if _, err := session.AppendMessage(ai.UserText(text), nil); err != nil {
				b.Fatal(err)
			}
		}
		if err := store.Close(); err != nil {
			b.Fatal(err)
		}

		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			b.ReportAllocs()

			for b.Loop() {
				opened, err := harness.OpenJSONL(path)
				if err != nil {
					b.Fatal(err)
				}
				loaded, err := harness.NewSession(opened)
				if err != nil {
					b.Fatal(err)
				}
				if len(loaded.Path()) != size {
					b.Fatalf("loaded %d entries, want %d", len(loaded.Path()), size)
				}
				if err := opened.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkSessionHistoryAppend measures an append into a long history: the
// single-owner path must not copy the whole transcript on every commit.
func BenchmarkSessionHistoryAppend(b *testing.B) {
	for _, size := range []int{2_000, 8_000} {
		session, _ := historyBenchSession(b, size)
		message := ai.UserText("next")

		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			b.ReportAllocs()

			for b.Loop() {
				if _, err := session.AppendMessage(message, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkSessionHistoryRetained measures the heap a session and its store
// keep alive after the conversation has been built. The single-owner history
// keeps one entry slice; a duplicate owner shows up as an extra B/entry.
func BenchmarkSessionHistoryRetained(b *testing.B) {
	for _, size := range []int{2_000, 8_000} {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			b.StopTimer()

			runtime.GC()

			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)

			session, store := historyBenchSession(b, size)

			runtime.GC()
			runtime.ReadMemStats(&after)

			retained := float64(after.HeapAlloc) - float64(before.HeapAlloc)
			b.ReportMetric(retained, "retained-B")
			b.ReportMetric(retained/float64(size), "B/entry")
			b.Logf("retained heap: %.0f bytes (%.0f B/entry)", retained, retained/float64(size))

			b.ResetTimer()

			for range b.N {
				runtime.KeepAlive(session)
				runtime.KeepAlive(store)
			}
		})
	}
}
