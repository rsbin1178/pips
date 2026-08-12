package agent

import (
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
)

func BenchmarkEventCreation(b *testing.B) {
	meta := RunMetadata{RunID: "run"}
	now := time.Now().UTC()
	stream := ModelStreamEvent{
		Turn: 1,
		Event: ai.StreamEvent{
			Type: ai.StreamTextDelta,
			Text: "small text delta",
		},
	}
	message := MessageCommitted{Turn: 1, Message: ai.AssistantText("committed answer")}

	b.Run("runtime_model_stream", func(b *testing.B) {
		for b.Loop() {
			_ = newEvent(meta, now, stream)
		}
	})

	b.Run("observer_text_stream_snapshot", func(b *testing.B) {
		event := newEvent(meta, now, stream)

		for b.Loop() {
			_ = cloneEvent(event)
		}
	})

	b.Run("observer_stream_usage_snapshot", func(b *testing.B) {
		usage := ai.Usage{InputTokens: 3, OutputTokens: 2}
		event := newEvent(meta, now, ModelStreamEvent{
			Turn: 1,
			Event: ai.StreamEvent{
				Type:  ai.StreamMessageEnd,
				Usage: &usage,
			},
		})

		for b.Loop() {
			_ = cloneEvent(event)
		}
	})

	b.Run("public_model_stream", func(b *testing.B) {
		for b.Loop() {
			_, err := NewEvent(meta, now, stream)
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("runtime_message", func(b *testing.B) {
		for b.Loop() {
			_ = newEvent(meta, now, message)
		}
	})
}
