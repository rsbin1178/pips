package tui

import (
	"fmt"
	"testing"

	"github.com/rsbin1178/pips/internal/coding"
)

// BenchmarkSubscriptionDuplicateDelivery measures the cost of one successful
// operation-iterator delivery while the subscription owns the projection - the
// Runtime's dual-publication path - with the framework's View call after every
// update. The delivery renders nothing, so the benchmark's cost is the frame
// composition it must not repeat.
func BenchmarkSubscriptionDuplicateDelivery(b *testing.B) {
	model := newModel(b.Context(), Options{
		Workspace: "/probe", PinPresentation: true, Screen: ScreenFullscreen, AltScreen: AltScreenAlways,
	})
	model.state = readyState()
	model.state.Phase = coding.PhaseRunning
	model.state.Interaction = coding.InteractionState{ID: "interaction-1", Active: true}
	model.state.Runs = []coding.RunState{{ID: "run-1", Active: true, TurnOpen: true, Turn: 1}}
	model.lifecycle = lifecycleReady
	model.sizeReady = true
	model.width = 100
	model.height = 40
	model.setLayout()
	model.subscriptionMode = true
	bridge := &eventBridge{cancel: func() {}, items: make(chan streamItem, 1)}
	model.bridge = bridge

	if model.View().Content == "" {
		b.Fatal("empty view")
	}

	b.ReportAllocs()

	for b.Loop() {
		if _, command := model.Update(streamItemMsg{
			bridge: bridge, ok: true,
			item: streamItem{event: streamTestTextDelta(1)},
		}); command == nil {
			b.Fatal("the iterator wait did not continue")
		}

		if model.View().Content == "" {
			b.Fatal("empty view")
		}
	}
}

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
