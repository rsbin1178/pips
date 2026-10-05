package tui

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
)

// projectionFixturePath is the frozen 627-message session snapshot shared with
// the 10-04 CPU investigation. It is probe input, never a repository file, so
// the benchmarks below skip when the probe workspace is not set up.
const projectionFixturePath = "/tmp/pipsprobe/transcript.json"

func projectionBenchMessages(b *testing.B) ai.Messages {
	b.Helper()

	raw, err := os.ReadFile(projectionFixturePath)
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

// projectionBenchTools mirrors the durable tool results a live session holds:
// one completed ToolState per tool call the transcript recorded.
func projectionBenchTools(messages ai.Messages) []coding.ToolState {
	type call struct {
		name string
		args ai.JSON
	}

	calls := make(map[string]call)
	results := make(map[string]ai.ToolResultPart)
	order := make([]string, 0, 64)

	for _, message := range messages {
		for _, part := range portableMessageParts(message) {
			switch value := part.(type) {
			case ai.ToolCallPart:
				if _, ok := calls[value.ID]; !ok {
					order = append(order, value.ID)
				}

				calls[value.ID] = call{name: value.Name, args: value.Args}
			case ai.ToolResultPart:
				results[value.ToolCallID] = value
			}
		}
	}

	tools := make([]coding.ToolState, 0, len(order))
	for _, id := range order {
		entry, ok := calls[id]
		if !ok {
			continue
		}

		tool := coding.ToolState{
			Call:   coding.ToolCall{ID: id, Name: entry.name, Arguments: entry.args},
			Status: coding.ToolStatusCompleted,
		}
		if result, ok := results[id]; ok {
			tool.Result = ai.ToolMessage{Parts: []ai.ToolResultPart{result}}
		}

		tools = append(tools, tool)
	}

	return tools
}

func projectionBenchModel(b *testing.B, messages ai.Messages, tools []coding.ToolState) *Model {
	b.Helper()

	model := newModel(b.Context(), Options{
		Workspace: "/probe", PinPresentation: true, Screen: ScreenFullscreen, AltScreen: AltScreenAlways,
	})
	state := readyState()
	state.Phase = coding.PhaseRunning
	state.Interaction = coding.InteractionState{ID: "interaction-1", Active: true}
	state.Runs = []coding.RunState{{ID: "run-1", Active: true, TurnOpen: true, Turn: 1}}
	state.Transcript = messages
	state.Tools = tools
	model.state = state
	model.lifecycle = lifecycleReady
	model.sizeReady = true
	model.width = 100
	model.height = 40
	model.subscription = &subscriptionBridge{}
	model.setLayout()

	return model
}

// BenchmarkTranscriptProjectionFrame measures one render tick's projection and
// transcript-store sync against the frozen real session. draft_only is the
// streaming frame where only the live tail changed; after_commit appends one
// durable message first, so the frame has to absorb a new conversation entry.
func BenchmarkTranscriptProjectionFrame(b *testing.B) {
	messages := projectionBenchMessages(b)
	tools := projectionBenchTools(messages)

	b.Run("draft_only", func(b *testing.B) {
		model := projectionBenchModel(b, messages, tools)
		model.state.Draft = []coding.MessageDelta{{Kind: ai.StreamReasoningDelta, Text: "thinking"}}
		model.rerenderTranscript(false)
		b.ReportAllocs()

		for index := 0; b.Loop(); index++ {
			model.state.Draft[0].Text = "thinking " + strconv.Itoa(index)
			model.rerenderTranscript(false)
		}
	})

	b.Run("after_commit", func(b *testing.B) {
		model := projectionBenchModel(b, messages, tools)
		model.rerenderTranscript(false)
		b.ReportAllocs()
		committed := make(ai.Messages, len(messages)+1, len(messages)+1)
		copy(committed, messages)

		for index := 0; b.Loop(); index++ {
			committed[len(messages)] = ai.AssistantText("committed " + strconv.Itoa(index))
			model.state.Transcript = committed
			model.rerenderTranscript(false)
		}
	})
}
