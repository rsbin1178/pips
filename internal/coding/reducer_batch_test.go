//nolint:wsl_v5 // Batch fixtures keep event construction and frame assertions adjacent.
package coding

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// batchFrame is one sequenced stream that must reduce identically whether it is
// applied one event at a time or as drained frames.
type batchFrame struct {
	name   string
	events []Event
}

// sequencedEvents assigns contiguous sequences so a frame boundary can fall
// anywhere in the stream.
func sequencedEvents(events ...Event) []Event {
	for index := range events {
		events[index].Sequence = uint64(index + 1)
	}

	return events
}

func batchPrefix() []Event {
	return []Event{
		newSessionEvent(EventSessionOpened, SessionOpened{
			Provider: ai.ProviderOpenAI, ModelID: "gpt-test",
		}),
		newInteractionEvent(EventInteractionStarted, InteractionStarted{}),
		newStatusEvent(EventStatusChanged, StatusChanged{Phase: PhaseRunning}),
		newTestEvent(EventRunStarted, RunStarted{Agent: "coding"}),
		newTestEvent(EventTurnStarted, TurnStarted{Turn: 1}),
	}
}

func batchFrameCases() []batchFrame {
	call := ToolCall{ID: "call-1", Name: "read", Arguments: ai.JSON(`{"path":"main.go"}`)}
	usage := TokenUsage{InputTokens: 12, OutputTokens: 4, ReasoningTokens: 2}

	interleaved := batchPrefix()
	for index := 0; index < 200; index++ {
		kind := ai.StreamTextDelta
		if index%3 == 0 {
			kind = ai.StreamReasoningDelta
		}

		interleaved = append(interleaved, newTestEvent(EventMessageDelta, MessageDelta{
			Kind: kind, Text: fmt.Sprintf("chunk-%d ", index),
		}))
	}

	withUsage := append(batchPrefix(),
		newTestEvent(EventMessageDelta, MessageDelta{Kind: ai.StreamTextDelta, Text: "answer "}),
		newTestEvent(EventMessageDelta, MessageDelta{
			Kind: ai.StreamTextDelta, Text: "with usage", Usage: &usage,
		}),
		newTestEvent(EventMessageDelta, MessageDelta{Kind: ai.StreamTextDelta, Text: " tail"}),
	)

	withTools := append(batchPrefix(),
		newTestEvent(EventMessageDelta, MessageDelta{Kind: ai.StreamTextDelta, Text: "before tool "}),
		newTestEvent(EventToolStarted, ToolStarted{Turn: 1, Call: call}),
		newTestEvent(EventMessageDelta, MessageDelta{Kind: ai.StreamTextDelta, Text: "mid tool "}),
		newTestEvent(EventToolUpdated, ToolUpdated{
			Turn: 1, Call: call, Update: []ai.Part{ai.Text("half")},
		}),
		newTestEvent(EventMessageDelta, MessageDelta{Kind: ai.StreamTextDelta, Text: "after update "}),
		newTestEvent(EventToolCompleted, ToolCompleted{
			Turn: 1, Call: call, Result: ai.ToolResultText(call.ID, call.Name, "done"),
		}),
		newTestEvent(EventMessageDelta, MessageDelta{Kind: ai.StreamTextDelta, Text: "after tool "}),
		newTestEvent(EventMessageCommitted, MessageCommitted{
			Message: ai.ToolResultText(call.ID, call.Name, "done"),
		}),
	)

	withStatus := append(batchPrefix(),
		newTestEvent(EventMessageDelta, MessageDelta{Kind: ai.StreamTextDelta, Text: "running "}),
		newStatusEvent(EventStatusChanged, StatusChanged{Phase: PhasePaused}),
		newTestEvent(EventMessageDelta, MessageDelta{Kind: ai.StreamTextDelta, Text: "paused "}),
		newStatusEvent(EventStatusChanged, StatusChanged{Phase: PhaseRunning}),
		newTestEvent(EventMessageDelta, MessageDelta{Kind: ai.StreamTextDelta, Text: "resumed "}),
	)

	crossTurn := append(batchPrefix(),
		newTestEvent(EventMessageDelta, MessageDelta{Kind: ai.StreamTextDelta, Text: "turn one "}),
		newTestEvent(EventMessageCommitted, MessageCommitted{Message: ai.AssistantText("turn one")}),
		newTestEvent(EventTurnCompleted, TurnCompleted{Turn: 1, Usage: usage}),
		newTestEvent(EventTurnStarted, TurnStarted{Turn: 2}),
		newTestEvent(EventMessageDelta, MessageDelta{Kind: ai.StreamReasoningDelta, Text: "thinking "}),
		newTestEvent(EventMessageDelta, MessageDelta{Kind: ai.StreamTextDelta, Text: "turn two "}),
		newTestEvent(EventMessageCommitted, MessageCommitted{Message: ai.AssistantText("turn two")}),
	)

	committed := append(batchPrefix(),
		newTestEvent(EventMessageDelta, MessageDelta{Kind: ai.StreamTextDelta, Text: "draft "}),
		newTestEvent(EventMessageCommitted, MessageCommitted{Message: ai.AssistantText("draft")}),
		newTestEvent(EventMessageDelta, MessageDelta{Kind: ai.StreamTextDelta, Text: "second draft "}),
		newTestEvent(EventMessageDiscarded, MessageDiscarded{Turn: 1}),
		newTestEvent(EventMessageCommitted, MessageCommitted{Message: ai.UserText("next question")}),
	)

	return []batchFrame{
		{name: "interleaved text and reasoning", events: sequencedEvents(interleaved...)},
		{name: "delta with usage", events: sequencedEvents(withUsage...)},
		{name: "tools between deltas", events: sequencedEvents(withTools...)},
		{name: "status between deltas", events: sequencedEvents(withStatus...)},
		{name: "cross turn", events: sequencedEvents(crossTurn...)},
		{name: "committed and discarded drafts", events: sequencedEvents(committed...)},
	}
}

func reduceSequentially(t *testing.T, events []Event) State {
	t.Helper()

	var state State

	for _, event := range events {
		next, err := Reduce(state, event)
		require.NoError(t, err, "event %d", event.Sequence)

		state = next
	}

	return state
}

func reduceInFrames(t *testing.T, events []Event, frame int) State {
	t.Helper()

	var state State

	for start := 0; start < len(events); start += frame {
		end := min(start+frame, len(events))

		next, err := ReduceBatch(state, events[start:end])
		require.NoError(t, err, "frame starting at %d", start)

		state = next
	}

	return state
}

// assertSameState compares two states field by field and then confirms the
// unexported indexes answer the same queries.
func assertSameState(t *testing.T, want, got State) {
	t.Helper()

	require.Equal(t, want, got)
	require.Equal(t, len(want.activeRuns), len(got.activeRuns), "active run index size")
	require.Equal(t, len(want.openTurns), len(got.openTurns), "open turn index size")
	require.Equal(t, len(want.activeTools), len(got.activeTools), "active tool index size")

	for runID, index := range want.activeRuns {
		gotIndex, err := got.activeRun(runID)
		require.NoError(t, err)
		require.Equal(t, index, gotIndex)
	}

	for _, tool := range want.Tools {
		if tool.Status != ToolStatusRunning {
			continue
		}

		index, err := got.activeTool(tool.RunID, tool.Call)
		require.NoError(t, err)
		require.Equal(t, tool.Call.ID, got.Tools[index].Call.ID)
	}
}

func TestReduceBatchMatchesSequentialReduce(t *testing.T) {
	t.Parallel()

	for _, test := range batchFrameCases() {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			sequential := reduceSequentially(t, test.events)

			for _, frame := range []int{1, 2, 3, 7, 32, len(test.events)} {
				t.Run(fmt.Sprintf("frame=%d", frame), func(t *testing.T) {
					t.Parallel()

					assertSameState(t, sequential, reduceInFrames(t, test.events, frame))
				})
			}
		})
	}
}

func TestReduceBatchRejectsIllegalEventAtomically(t *testing.T) {
	t.Parallel()

	const count = 2*streamDraftChunkSize + 5
	for _, rejectedIndex := range []int{0, streamDraftChunkSize + 2, count - 1} {
		t.Run(fmt.Sprintf("rejected_at_%d", rejectedIndex), func(t *testing.T) {
			t.Parallel()

			state := contractState(t)
			before := state.Clone()
			beforeRecords := state.Draft.Materialize()
			events := make([]Event, count)
			for index := range events {
				events[index] = newTestEvent(EventMessageDelta, reasoningDelta("provisional "))
				events[index].Sequence = state.Sequence + uint64(index) + 1
			}

			// Force an earlier delta run to build unpublished chunks before
			// the middle/end rejection; none may reach the retained state.
			events[streamDraftChunkSize] = sequenceEvent(
				events[streamDraftChunkSize].Sequence,
				newStatusEvent(EventIntegrationDiagnostic, IntegrationDiagnostic{Component: "test", Code: "provisional"}),
			)
			events[rejectedIndex].RunID = "run-unknown"

			rejected, err := ReduceBatch(state, events)
			require.ErrorIs(t, err, ErrEventProtocol)
			assert.Equal(t, State{}, rejected, "a rejected batch must not hand back a partial state")
			requireEqualState(t, before, state, "a rejected batch changed the input state or summary")
			assert.Equal(t, beforeRecords, state.Draft.Materialize(), "a rejected batch rewrote shared records")
			assert.Equal(t, beforeRecords, before.Draft.Materialize(), "a rejected batch rewrote a retained snapshot")
		})
	}
}

// TestReduceBatchKeepsEveryFrameDelta pins the "no dropped character inside a
// frame" rule: folding deltas must reproduce the exact concatenation.
func TestReduceBatchKeepsEveryFrameDelta(t *testing.T) {
	t.Parallel()

	for _, test := range batchFrameCases() {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			sequential := reduceSequentially(t, test.events)

			for _, frame := range []int{2, 7, 32} {
				batched := reduceInFrames(t, test.events, frame)
				assert.Equal(t, draftText(sequential.Draft), draftText(batched.Draft),
					"frame=%d", frame)
				assert.True(t, sequential.Draft.Equal(batched.Draft), "frame=%d", frame)
			}
		})
	}
}

func draftText(draft StreamDraft) string {
	var builder strings.Builder

	for _, delta := range draft.Materialize() {
		builder.WriteString(delta.Text)
	}

	return builder.String()
}

// requireEqualState compares two states field-for-field while comparing the
// immutable draft by logical contents: two equal drafts built by different
// batch splits need not share chunk pointers.
func requireEqualState(t *testing.T, want, got State, msg string, args ...any) {
	t.Helper()

	wantDraft, gotDraft := want.Draft, got.Draft
	want.Draft, got.Draft = StreamDraft{}, StreamDraft{}

	message := append([]any{msg}, args...)
	require.Equal(t, want, got, message...)
	require.True(t, wantDraft.Equal(gotDraft), message...)
}

// FuzzReduceBatchEquivalence drives legal event sequences through both
// transitions. Illegal candidates are skipped while the script is built, so the
// comparison only ever runs on sequences the reducer accepts.
func FuzzReduceBatchEquivalence(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7})
	f.Add([]byte{3, 3, 3, 2, 1, 0})
	f.Add([]byte{5, 5, 1})

	f.Fuzz(func(t *testing.T, script []byte) {
		events := reduceScriptEvents(t, script)
		if len(events) < 2 {
			return
		}

		sequential := reduceSequentially(t, events)

		for _, frame := range []int{2, 3, 5, 32} {
			batched := reduceInFrames(t, events, frame)
			requireEqualState(t, sequential, batched, "frame=%d script=%v", frame, script)
			require.Equal(t, draftText(sequential.Draft), draftText(batched.Draft),
				"frame=%d", frame)
		}
	})
}

// reduceScriptEvents turns fuzz bytes into the longest legal event sequence
// they describe. Each byte selects one candidate transition; candidates the
// reducer rejects leave the running state untouched.
func reduceScriptEvents(t *testing.T, script []byte) []Event {
	t.Helper()

	base := sequencedEvents(batchPrefix()...)
	state := reduceSequentially(t, base)
	events := make([]Event, 0, len(base)+len(script))
	events = append(events, base...)

	for index, choice := range script {
		candidate := reduceScriptCandidate(state, choice, index)
		candidate.Sequence = state.Sequence + 1

		next, err := Reduce(state, candidate)
		if err != nil {
			continue
		}

		events = append(events, candidate)
		state = next
	}

	return events
}

func reduceScriptCandidate(state State, choice byte, index int) Event {
	call := ToolCall{ID: fmt.Sprintf("call-%d", index%4), Name: "read", Arguments: ai.JSON(`{"path":"f.go"}`)}

	switch choice % 9 {
	case 0:
		return newTestEvent(EventMessageDelta, MessageDelta{
			Kind: ai.StreamTextDelta, Text: fmt.Sprintf("text-%d ", index),
		})
	case 1:
		return newTestEvent(EventMessageDelta, MessageDelta{
			Kind: ai.StreamReasoningDelta, Text: fmt.Sprintf("reason-%d ", index),
		})
	case 2:
		usage := TokenUsage{InputTokens: index, OutputTokens: 1}

		return newTestEvent(EventMessageDelta, MessageDelta{
			Kind: ai.StreamTextDelta, Text: "usage ", Usage: &usage,
		})
	case 3:
		return newTestEvent(EventMessageCommitted, MessageCommitted{
			Message: ai.AssistantText(fmt.Sprintf("commit-%d", index)),
		})
	case 4:
		return newTestEvent(EventToolStarted, ToolStarted{Turn: 1, Call: call})
	case 5:
		return newTestEvent(EventToolUpdated, ToolUpdated{
			Turn: 1, Call: call, Update: []ai.Part{ai.Text("progress")},
		})
	case 6:
		return newTestEvent(EventToolCompleted, ToolCompleted{
			Turn: 1, Call: call, Result: ai.ToolResultText(call.ID, call.Name, "done"),
		})
	case 7:
		phase := PhasePaused
		if state.Phase != PhaseRunning {
			phase = PhaseRunning
		}

		return newStatusEvent(EventStatusChanged, StatusChanged{Phase: phase})
	default:
		if state.openTurns["run-1"] != 0 {
			return newTestEvent(EventTurnCompleted, TurnCompleted{
				Turn: state.openTurns["run-1"], Usage: TokenUsage{InputTokens: 1},
			})
		}

		return newTestEvent(EventTurnStarted, TurnStarted{Turn: state.Runs[0].Turn + 1})
	}
}
