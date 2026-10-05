package tui

import (
	"fmt"
	"testing"

	"github.com/rsbin1178/pips/internal/coding"
)

func BenchmarkReadyView(b *testing.B) {
	for _, count := range []int{0, 50} {
		b.Run(fmt.Sprintf("completed=%d/output=32KiB", count), func(b *testing.B) {
			m := newModel(b.Context(), Options{Workspace: "/probe", PinPresentation: true, Screen: ScreenFullscreen, AltScreen: AltScreenAlways})
			m.state = readyState()
			m.state.Phase = coding.PhaseRunning
			m.state.Tools = benchmarkCompletedTools(count, 32<<10)
			m.lifecycle = lifecycleReady
			m.sizeReady = true
			m.width = 100
			m.height = 40
			m.setLayout()
			b.ReportAllocs()

			for b.Loop() {
				if m.View().Content == "" {
					b.Fatal("empty view")
				}
			}
		})
	}
}
