//nolint:wsl_v5 // Benchmarks keep fixture setup and the measured transition adjacent.
package coding

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
)

// transcriptFixturePath holds the recorded real session body used as the
// acceptance fixture. The file carries conversation content, so it is never
// committed; benchmarks skip when it is absent.
const transcriptFixturePath = "/tmp/pipsprobe/transcript.json"

// fixtureMessages returns the first count messages of the recorded session,
// repeating the recorded distribution when a longer conversation is requested.
func fixtureMessages(tb testing.TB, count int) ai.Messages {
	tb.Helper()

	raw, err := os.ReadFile(transcriptFixturePath)
	if err != nil {
		tb.Skipf("transcript fixture unavailable: %v", err)
	}

	var records []json.RawMessage
	if err := json.Unmarshal(raw, &records); err != nil {
		tb.Fatal(err)
	}
	if len(records) == 0 {
		tb.Skip("transcript fixture is empty")
	}

	messages := make(ai.Messages, 0, count)
	for len(messages) < count {
		for _, record := range records {
			if len(messages) == count {
				break
			}

			message, err := ai.UnmarshalMessage(record)
			if err != nil {
				tb.Fatal(err)
			}

			messages = append(messages, message)
		}
	}

	return messages
}

// fixtureTools rebuilds the Runtime's Tools projection from the same
// transcript, so each measured state carries both the committed transcript and
// every committed tool result.
func fixtureTools(messages ai.Messages) []ToolState {
	var tools []ToolState

	for _, message := range messages {
		toolMessage, ok := message.(ai.ToolMessage)
		if !ok {
			continue
		}

		for _, part := range toolMessage.Parts {
			tools = append(tools, ToolState{
				RunID: "bench-run", Turn: 1,
				Call:   ToolCall{ID: part.ToolCallID, Name: part.Name, Arguments: ai.JSON(`{"path":"x"}`)},
				Status: ToolStatusCompleted,
				Result: ai.ToolMessage{Parts: []ai.ToolResultPart{part}},
			})
		}
	}

	return tools
}

// fixtureState is one live streaming state: an open Session and turn, the
// recorded transcript, and the tool projection built from it.
func fixtureState(tb testing.TB, messages ai.Messages) State {
	tb.Helper()

	state := State{
		SessionID: "bench-session", SessionOpen: true,
		Provider: ai.ProviderOpenAI, ModelID: "bench-model",
		Mode: ModeAgent, Phase: PhaseRunning,
		Interaction: InteractionState{ID: "bench-interaction", Active: true},
		Runs:        []RunState{{ID: "bench-run", Active: true, TurnOpen: true, Turn: 1}},
		Transcript:  messages,
		Tools:       fixtureTools(messages),
	}
	state.ensureIndexes()

	return state
}

func fixtureDelta(sequence uint64, text string) Event {
	return Event{
		Schema: EventSchema, Sequence: sequence,
		Time:      time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC),
		SessionID: "bench-session", InteractionID: "bench-interaction", RunID: "bench-run",
		Type: EventMessageDelta, Payload: MessageDelta{Kind: ai.StreamTextDelta, Text: text},
	}
}

// BenchmarkReduceMessageDelta is the long-conversation guard: one streamed
// delta must cost the same whether the Session holds 100 or 2000 messages.
func BenchmarkReduceMessageDelta(b *testing.B) {
	for _, size := range []int{100, 627, 2000} {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			state := fixtureState(b, fixtureMessages(b, size))
			event := fixtureDelta(state.Sequence+1, "token ")
			b.ReportAllocs()

			for b.Loop() {
				if _, err := Reduce(state, event); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkReduceMessageDeltaBatch measures one drained frame: the same deltas
// folded into a single transition, one Draft append, and one sequence advance.
func BenchmarkReduceMessageDeltaBatch(b *testing.B) {
	for _, size := range []int{627, 2000} {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			state := fixtureState(b, fixtureMessages(b, size))
			events := make([]Event, 32)
			for index := range events {
				events[index] = fixtureDelta(state.Sequence+uint64(index)+1, "token ")
			}
			b.ReportAllocs()
			b.ReportMetric(float64(len(events)), "deltas/op")

			for b.Loop() {
				if _, err := ReduceBatch(state, events); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
