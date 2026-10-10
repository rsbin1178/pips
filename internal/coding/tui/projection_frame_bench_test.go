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

// projectionCommitWindow bounds the growing append run in the after_commit case.
// Once this many commits have been appended the model is rolled back to the
// frozen conversation and re-warmed, so the benchmark measures a steady commit
// rate instead of an unbounded conversation.
const projectionCommitWindow = 256

// BenchmarkTranscriptProjectionFrame measures one render tick's projection and
// transcript-store sync against the frozen real session.
//
// draft_only is the streaming frame where only the live tail changed.
//
// after_commit is a real commit: every iteration appends one durable text message
// and its candidate identity to the growing conversation, the way the reducer's
// MessageCommitted does, and re-renders. A frame that can extend the committed
// prefix pays only for the appended message. The append run is rolled back and
// re-warmed every projectionCommitWindow commits, and that rollback is part of
// the measured work, so the reported cost is the amortized cost of one commit.
//
// replace_tail is the fixed-length worst case: the last message's text is
// rewritten in place, so no append can be proven and the whole frame is
// reprojected. It is the shape the pre-append benchmark measured.
func BenchmarkTranscriptProjectionFrame(b *testing.B) {
	messages := projectionBenchMessages(b)
	tools := projectionBenchTools(messages)

	b.Run("draft_only", func(b *testing.B) {
		model := projectionBenchModel(b, messages, tools)
		model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{Kind: ai.StreamReasoningDelta, Text: "thinking"})
		model.rerenderTranscript(false)
		b.ReportAllocs()

		for index := 0; b.Loop(); index++ {
			model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{
				Kind: ai.StreamReasoningDelta, Text: "thinking " + strconv.Itoa(index),
			})
			model.rerenderTranscript(false)
		}
	})

	b.Run("after_commit", func(b *testing.B) {
		model := projectionBenchModel(b, messages, tools)
		transcript := messages
		candidates := make([]coding.CandidateIdentity, len(messages))
		model.state.MessageCandidates = candidates
		model.rerenderTranscript(false)
		b.ReportAllocs()

		for index := 0; b.Loop(); index++ {
			if index > 0 && index%projectionCommitWindow == 0 {
				// Roll the conversation back to the frozen prefix and re-warm, so
				// the working set stays bounded across a long run.
				model.frameCache = timelineFrameCache{}
				transcript = messages
				candidates = make([]coding.CandidateIdentity, len(messages))
				model.state.Transcript = transcript
				model.state.MessageCandidates = candidates
				model.rerenderTranscript(false)
			}

			transcript = append(transcript, ai.AssistantText("committed "+strconv.Itoa(index)))
			candidates = append(candidates, coding.CandidateIdentity{RunID: "run-1", Turn: index + 2})
			model.state.Transcript = transcript
			model.state.MessageCandidates = candidates
			model.rerenderTranscript(false)
		}
	})

	b.Run("after_tool_commit", func(b *testing.B) {
		model := projectionBenchModel(b, messages, tools)
		transcript := messages
		candidates := make([]coding.CandidateIdentity, len(messages))
		model.state.MessageCandidates = candidates
		model.rerenderTranscript(false)
		b.ReportAllocs()

		for index := 0; b.Loop(); index++ {
			if index > 0 && index%projectionCommitWindow == 0 {
				model.frameCache = timelineFrameCache{}
				transcript = messages
				candidates = make([]coding.CandidateIdentity, len(messages))
				model.state.Transcript = transcript
				model.state.MessageCandidates = candidates
				model.rerenderTranscript(false)
			}

			// A tool call and its result land together, so the card is new to the
			// projection and the retained scan can place it.
			callID := "call-bench-" + strconv.Itoa(index)
			transcript = append(
				transcript,
				ai.Assistant(
					ai.Text("calling"),
					ai.ToolCallPart{ID: callID, Name: "read", Args: ai.JSON(`{"path":"main.go"}`)},
				),
				codingToolResultFor(callID, "read", "package main"),
			)
			candidates = append(
				candidates,
				coding.CandidateIdentity{RunID: "run-1", Turn: index + 2},
				coding.CandidateIdentity{},
			)
			model.state.Transcript = transcript
			model.state.MessageCandidates = candidates
			model.rerenderTranscript(false)
		}
	})

	b.Run("replace_tail", func(b *testing.B) {
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
