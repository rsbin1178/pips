//nolint:wsl_v5 // Streaming benchmarks keep frame setup and the measured update adjacent.
package tui

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
)

var benchEventTime = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

// streamBenchTranscript loads the recorded session body used as the acceptance
// fixture. It carries conversation content, so it is never committed and the
// benchmark skips when it is absent.
func streamBenchTranscript(b *testing.B) ai.Messages {
	b.Helper()

	raw, err := os.ReadFile("/tmp/pipsprobe/transcript.json")
	if err != nil {
		b.Skipf("transcript fixture unavailable: %v", err)
	}

	var records []json.RawMessage
	if err := json.Unmarshal(raw, &records); err != nil {
		b.Fatal(err)
	}

	messages := make(ai.Messages, 0, len(records))
	for _, record := range records {
		message, err := ai.UnmarshalMessage(record)
		if err != nil {
			b.Fatal(err)
		}

		messages = append(messages, message)
	}

	return messages
}

// streamBenchModel builds a ready fullscreen model holding the recorded
// session body and one open run with an open turn, so a streamed delta is a
// legal transition.
func streamBenchModel(b *testing.B, transcript ai.Messages) *Model {
	b.Helper()

	model := newModel(b.Context(), Options{
		Workspace: "/bench", PinPresentation: true,
		Screen: ScreenFullscreen, AltScreen: AltScreenAlways,
	})
	state := readyState()
	state.Phase = coding.PhaseRunning
	state.Interaction = coding.InteractionState{ID: "interaction-1", Active: true}
	state.Runs = []coding.RunState{{ID: "run-1", Active: true, TurnOpen: true, Turn: 1}}
	state.Transcript = transcript
	model.state = state
	model.lifecycle = lifecycleReady
	model.sizeReady = true
	model.width = 100
	model.height = 40
	model.bridge = &eventBridge{items: make(chan streamItem, bridgeCapacity)}
	model.setLayout()

	return model
}

func streamBenchDelta(sequence uint64) coding.Event {
	return coding.Event{
		Schema: coding.EventSchema, Sequence: sequence, Time: benchEventTime,
		SessionID: "session-1", InteractionID: "interaction-1", RunID: "run-1",
		Type: coding.EventMessageDelta,
		Payload: coding.MessageDelta{
			Kind: ai.StreamReasoningDelta, Text: "streamed reasoning fragment ",
		},
	}
}

// BenchmarkStreamBatchUpdate measures one MVU update per drained frame: the
// delivered record plus every record already queued behind it. It is the TUI
// side of R3, and `deltas/op` reports the frame width.
func BenchmarkStreamBatchUpdate(b *testing.B) {
	transcript := streamBenchTranscript(b)

	for _, batch := range []int{1, 8, 32} {
		b.Run("batch="+itoa(batch), func(b *testing.B) {
			model := streamBenchModel(b, transcript)
			frame := make([]streamItem, batch)
			b.ReportAllocs()
			b.ReportMetric(float64(batch), "deltas/op")

			for b.Loop() {
				next := model.state.Sequence
				for index := range frame {
					next++
					frame[index] = streamItem{event: streamBenchDelta(next)}
				}

				for index := 1; index < batch; index++ {
					model.bridge.items <- frame[index]
				}

				// The live draft itself grows outside the measured transition;
				// reset it so one frame is one Draft append, matching a stream
				// that commits a message per turn.
				model.state.Draft = coding.StreamDraft{}
				model.Update(streamItemMsg{bridge: model.bridge, item: frame[0], ok: true})
				if model.streamErr != nil {
					b.Fatal(model.streamErr)
				}
			}
		})
	}
}
