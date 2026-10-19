//go:build darwin || linux

package session_test

import (
	"runtime"
	"strconv"
	"testing"
)

// BenchmarkListSubagentsIndexed measures the two ways one parent's children are
// read, per store size. The scan sub-benchmark removes the parent's index before
// every iteration, so it measures the real fallback including its backfill; the
// index sub-benchmark is warmed once and then measures a store that is only read.
func BenchmarkListSubagentsIndexed(b *testing.B) {
	for _, total := range []int{100, 1000, 5000} {
		fixture := newChildIndexFixture(b, total)
		b.Run("sessions_"+strconv.Itoa(total)+"/scan", func(b *testing.B) {
			b.ReportAllocs()

			for b.Loop() {
				runtime.KeepAlive(fixture.scan(b))
			}
		})
		b.Run("sessions_"+strconv.Itoa(total)+"/index", func(b *testing.B) {
			fixture.warm(b)
			b.ReportAllocs()

			for b.Loop() {
				runtime.KeepAlive(fixture.indexed(b))
			}
		})
	}
}
