//nolint:wsl_v5 // Each ordering fixture keeps its event chain and assertion adjacent.
package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// completionOrderTime is one instant for every synthetic event, so the fixtures
// only depend on the fields the ordering reads.
var completionOrderTime = time.Date(2026, time.October, 22, 12, 0, 0, 0, time.UTC)

// completionEventChain builds the coding events of one complete interaction:
// start, run, turn, the user prompt, the assistant answer, the turn and run
// completion, then the interaction completion.
type completionEventChain struct {
	interactionID string
	runID         string
	prompt        string
	answer        string
	// sequence is the sequence the chain starts from; it is advanced in place.
	sequence uint64
}

// events appends the chain's events to dst and returns it with the last used
// sequence.
func (chain *completionEventChain) events(dst []coding.Event) ([]coding.Event, uint64) {
	next := func(eventType coding.EventType, payload coding.EventPayload) coding.Event {
		chain.sequence++

		event := coding.Event{
			Schema: coding.EventSchema, Sequence: chain.sequence, Time: completionOrderTime,
			SessionID: "session-1", InteractionID: chain.interactionID,
			Type: eventType, Payload: payload,
		}
		switch eventType {
		case coding.EventInteractionStarted, coding.EventInteractionCompleted:
			// The protocol carries no run on the interaction's own lifecycle.
		default:
			event.RunID = chain.runID
		}

		return event
	}

	dst = append(dst,
		next(coding.EventInteractionStarted, coding.InteractionStarted{Mode: coding.ModeAgent}),
		next(coding.EventRunStarted, coding.RunStarted{Agent: "coding"}),
		next(coding.EventTurnStarted, coding.TurnStarted{Turn: 1}),
		next(coding.EventMessageCommitted, coding.MessageCommitted{Message: ai.UserText(chain.prompt)}),
		next(coding.EventMessageCommitted, coding.MessageCommitted{Message: ai.AssistantText(chain.answer)}),
		next(coding.EventTurnCompleted, coding.TurnCompleted{Turn: 1}),
		next(coding.EventRunCompleted, coding.RunCompleted{Stop: agent.StopEndTurn, Turns: 1}),
		next(coding.EventInteractionCompleted, coding.InteractionCompleted{
			Outcome: coding.InteractionSucceeded, DurationMillis: 7_000,
		}),
	)

	return dst, chain.sequence
}

// assistantCandidate returns the candidate identity the reducer assigns to a
// committed assistant message of one run's first turn.
func assistantCandidate(runID string) coding.CandidateIdentity {
	return coding.CandidateIdentity{RunID: runID, Turn: 1}
}

func streamItemsFor(events ...coding.Event) []streamItem {
	items := make([]streamItem, 0, len(events))
	for _, event := range events {
		items = append(items, streamItem{event: event})
	}

	return items
}

// completeInteraction records a marker for one interaction the way the event
// stream does: the run is tracked first, then the completion arrives.
func completeInteraction(model *Model, interactionID, runID string) {
	model.trackInteractionRun(coding.Event{
		InteractionID: interactionID, RunID: runID, Type: coding.EventRunStarted,
	})
	model.recordCompletion(coding.Event{
		InteractionID: interactionID,
		Type:          coding.EventInteractionCompleted,
		Payload:       coding.InteractionCompleted{Outcome: coding.InteractionSucceeded, DurationMillis: 7_000},
	})
}

// TestCompletionMarkerKeepsItsPlaceAcrossIdenticalAnswers pins the digest map's
// defect: a later answer with the same text used to capture the earlier marker
// because the map kept the last position for one digest.
func TestCompletionMarkerKeepsItsPlaceAcrossIdenticalAnswers(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.Transcript = []ai.Message{ai.UserText("first"), ai.AssistantText("Done.")}
	model.state.MessageCandidates = []coding.CandidateIdentity{{}, assistantCandidate("run-a")}
	completeInteraction(model, "first", "run-a")
	require.Len(t, model.completionMarkers, 1)
	require.Equal(t, 2, model.completionMarkers[0].afterMessages)

	model.state.Transcript = append(
		model.state.Transcript, ai.UserText("second"), ai.AssistantText("Done."),
	)
	model.state.MessageCandidates = append(
		model.state.MessageCandidates, coding.CandidateIdentity{}, assistantCandidate("run-b"),
	)
	model.rebaseCompletionMarkers()

	assert.Equal(t, 2, model.completionMarkers[0].afterMessages,
		"the first answer's marker stays after the first answer")

	rendered := renderTimeline(model.timelineBlocks(), model.markdown, 80, themeDark, true)
	firstAnswer := strings.Index(rendered, "Done.")
	marker := strings.Index(rendered, completionGlyph+" ")
	require.NotEqual(t, -1, firstAnswer)
	require.NotEqual(t, -1, marker)
	assert.Less(t, firstAnswer, marker, "the completion row sits below the first answer")
}

// TestCompletionMarkerAnchorsToItsOwnRunWhenSnapshotLeads pins the snapshot
// defect: the subscription path adopts one snapshot per frame, and that snapshot
// can already hold the next interaction. The marker must follow the run the
// completion event names, not the snapshot's tail.
func TestCompletionMarkerAnchorsToItsOwnRunWhenSnapshotLeads(t *testing.T) {
	t.Parallel()

	// The Runtime is ahead: it already holds both interactions, both answered
	// with the same text.
	state := readyState()
	state.Sequence = 20
	state.Transcript = []ai.Message{
		ai.UserText("first"), ai.AssistantText("Done."),
		ai.UserText("second"), ai.AssistantText("Done."),
	}
	state.MessageCandidates = []coding.CandidateIdentity{
		{}, assistantCandidate("run-a"), {}, assistantCandidate("run-b"),
	}

	controller := runtimeStateController{stubController: stubController{state: state}}
	model := readyModelWithController(t, controller, true)
	model.subscription = &subscriptionBridge{}
	model.subscriptionMode = true
	model.runtimeState = true
	model.observedSequence = 16

	// Events 17-20 arrive while the snapshot already holds all four messages: the
	// first interaction's answer, its turn/run completion and the completion.
	chain := completionEventChain{interactionID: "first", runID: "run-a", prompt: "first", answer: "Done.", sequence: 12}
	events, _ := chain.events(nil)
	require.Len(t, events, 8)
	model.reduceStreamBatch(streamItemsFor(events[4:]...))

	require.NoError(t, model.streamErr)
	require.Len(t, model.completionMarkers, 1)
	assert.Equal(t, "first", model.completionMarkers[0].interactionID)
	assert.Equal(t, 2, model.completionMarkers[0].afterMessages,
		"the marker follows the first interaction's answer, not the snapshot tail")
}

// TestCompletionMarkersAnchorAcrossABatchSpanningTwoInteractions pins the
// ReduceBatch path: the batch advances the final State first, then runs the
// per-event bookkeeping. Both markers must land at their own boundaries.
func TestCompletionMarkersAnchorAcrossABatchSpanningTwoInteractions(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.runtimeState = false
	model.subscriptionMode = false
	model.observedSequence = 0

	first := completionEventChain{interactionID: "first", runID: "run-a", prompt: "first", answer: "Done."}
	events, sequence := first.events(nil)
	second := completionEventChain{
		interactionID: "second", runID: "run-b", prompt: "second", answer: "Done.", sequence: sequence,
	}
	events, _ = second.events(events)

	model.reduceStreamBatch(streamItemsFor(events...))

	require.NoError(t, model.streamErr)
	require.Len(t, model.completionMarkers, 2)
	assert.Equal(t, "first", model.completionMarkers[0].interactionID)
	assert.Equal(t, 2, model.completionMarkers[0].afterMessages,
		"the first marker stays after the first answer")
	assert.Equal(t, "second", model.completionMarkers[1].interactionID)
	assert.Equal(t, 4, model.completionMarkers[1].afterMessages,
		"the second marker follows the second answer")
}

// TestCompletionMarkerFollowsTheAnswerAcrossADurableTranscriptReplacement pins
// the ask_user shape: the Runtime publishes the durable tool result after the
// interaction completed, so the transcript is replaced with the result inserted
// ahead of the answer and without candidate identities.
func TestCompletionMarkerFollowsTheAnswerAcrossADurableTranscriptReplacement(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	call := ai.Assistant(ai.ToolCallPart{ID: "call-1", Name: "ask_user", Args: ai.JSON(`{}`)})
	answer := ai.AssistantText("final answer")
	model.state.Transcript = []ai.Message{ai.UserText("ask"), call, answer}
	model.state.MessageCandidates = []coding.CandidateIdentity{{}, {}, assistantCandidate("run-a")}
	completeInteraction(model, "first", "run-a")
	require.Len(t, model.completionMarkers, 1)
	require.Equal(t, 3, model.completionMarkers[0].afterMessages)

	// The durable path replaces the transcript and leaves the candidate array at
	// its old length, so the run identity is unusable and the digest takes over.
	model.state.Transcript = []ai.Message{
		ai.UserText("ask"),
		call,
		ai.ToolResultText("call-1", "ask_user", "ok"),
		answer,
	}
	model.rebaseCompletionMarkers()

	assert.Equal(t, 4, model.completionMarkers[0].afterMessages)
	rendered := renderTimeline(model.timelineBlocks(), model.markdown, 80, themeDark, true)
	answerAt := strings.Index(rendered, "final answer")
	markerAt := strings.Index(rendered, completionGlyph+" ")
	require.NotEqual(t, -1, answerAt)
	require.NotEqual(t, -1, markerAt)
	assert.Less(t, answerAt, markerAt, "the completion row stays below the answer it belongs to")
}

// TestCompletionMarkerDroppedWhenItsAnchorLeavesTheWindow pins the trim contract:
// a marker whose message the window released must not be re-pointed at whatever
// now occupies that index.
func TestCompletionMarkerDroppedWhenItsAnchorLeavesTheWindow(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.Transcript = []ai.Message{
		ai.UserText("a"), ai.AssistantText("answer a"),
		ai.UserText("b"), ai.AssistantText("answer b"),
	}
	model.state.MessageCandidates = []coding.CandidateIdentity{
		{}, assistantCandidate("run-a"), {}, assistantCandidate("run-b"),
	}
	completeInteraction(model, "a", "run-a")
	require.Len(t, model.completionMarkers, 1)

	// The window releases the first two messages: the marker's answer is gone.
	model.state.Transcript = model.state.Transcript[2:]
	model.state.MessageCandidates = model.state.MessageCandidates[2:]
	model.rebaseCompletionMarkersForWindow(2)

	assert.Empty(t, model.completionMarkers, "a marker cannot survive the message it follows")
}

// TestInlineCompletionMarkerWaitsForItsOwnDelta pins the inline contract: native
// history is append-only, so a marker whose answer is still held back must not be
// written above it.
func TestInlineCompletionMarkerWaitsForItsOwnDelta(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.Transcript = []ai.Message{
		ai.UserText("a"), ai.AssistantText("answer a"),
		ai.UserText("b"), ai.AssistantText("answer b"),
	}
	model.state.MessageCandidates = []coding.CandidateIdentity{
		{}, assistantCandidate("run-a"), {}, assistantCandidate("run-b"),
	}
	completeInteraction(model, "a", "run-a")
	require.Len(t, model.completionMarkers, 1)

	// The delta is held at the first answer, so the marker's own answer is not in
	// it yet.
	model.scrollback.messages = 0
	ready, waiting := model.completionMarkersForDelta(1)
	assert.Empty(t, ready, "an answer outside the delta is not covered")
	require.Len(t, waiting, 1)

	// The delta now carries both answers: the marker follows the first one.
	ready, waiting = model.completionMarkersForDelta(2)
	require.Len(t, ready, 1)
	assert.Empty(t, waiting)
	assert.Equal(t, 2, ready[0].afterMessages, "the marker is rebased onto the delta")
}

// TestInlineCompletionMarkerPrintsOnceAcrossDeltas pins exactly-once delivery:
// the marker is committed with the delta that carried it and never repeats.
func TestInlineCompletionMarkerPrintsOnceAcrossDeltas(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.Transcript = []ai.Message{ai.UserText("a"), ai.AssistantText("answer a")}
	model.state.MessageCandidates = []coding.CandidateIdentity{{}, assistantCandidate("run-a")}
	completeInteraction(model, "a", "run-a")

	model.scrollback.messages = 0
	ready, _ := model.completionMarkersForDelta(2)
	require.Len(t, ready, 1)
	for _, marker := range ready {
		model.markCompletionCommitted(marker)
	}

	ready, waiting := model.completionMarkersForDelta(2)
	assert.Empty(t, ready)
	assert.Empty(t, waiting)
}

// TestCompletionMarkerOrderThroughPerEventDelivery drives the same leading
// snapshot one event per frame, which is what the subscription bridge hands the
// model when the queue holds one record. The frame boundary, the adoption of the
// Runtime projection and the per-event bookkeeping are all exercised together.
func TestCompletionMarkerOrderThroughPerEventDelivery(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Sequence = 20
	state.Transcript = []ai.Message{
		ai.UserText("first"), ai.AssistantText("Done."),
		ai.UserText("second"), ai.AssistantText("Done."),
	}
	state.MessageCandidates = []coding.CandidateIdentity{
		{}, assistantCandidate("run-a"), {}, assistantCandidate("run-b"),
	}

	controller := runtimeStateController{stubController: stubController{state: state}}
	model := readyModelWithController(t, controller, true)
	model.subscriptionMode = true
	model.runtimeState = true
	model.observedSequence = 16

	chain := completionEventChain{interactionID: "first", runID: "run-a", prompt: "first", answer: "Done.", sequence: 12}
	events, _ := chain.events(nil)
	require.Len(t, events, 8)

	// The assistant commit names the run, then the interaction completes. The
	// Runtime snapshot was already at sequence 20 for both.
	for _, event := range events[4:] {
		model.reduceObservedEvent(event)
	}

	require.NoError(t, model.streamErr)
	require.Len(t, model.completionMarkers, 1)
	assert.Equal(t, 2, model.completionMarkers[0].afterMessages,
		"the completion row follows its own answer through the streaming path")
}

// durableQuestionRefresh builds the actual reducer transition that inserts a
// deferred ask_user result, rather than supplying pre-aligned test candidates.
func durableQuestionRefresh(t *testing.T) coding.State {
	t.Helper()

	chain := completionEventChain{
		interactionID: "old", runID: "run-old", prompt: "old question", answer: "Done.",
	}
	events, _ := chain.events(nil)
	call := ai.Assistant(ai.ToolCallPart{ID: "question", Name: "ask_user", Args: ai.JSON(`{}`)})
	callEvent := events[4]
	callEvent.Payload = coding.MessageCommitted{Message: call}
	events = append(events[:4], append([]coding.Event{callEvent}, events[4:]...)...)
	for index := range events {
		events[index].Sequence = uint64(index + 1)
	}
	state, err := coding.ReduceBatch(readyState(), events)
	require.NoError(t, err)
	require.Len(t, state.Transcript, 3)

	replacement := []ai.Message{
		state.Transcript[0], call, ai.ToolResultText("question", "ask_user", "ok"), state.Transcript[2],
	}
	refresh := coding.Event{
		Schema: coding.EventSchema, Sequence: state.Sequence + 1, Time: completionOrderTime,
		SessionID: state.SessionID, Type: coding.EventSessionTreeChanged,
		Payload: coding.SessionTreeChanged{
			Tree: coding.SessionTree{SessionID: state.SessionID}, Transcript: replacement,
		},
	}
	state, err = coding.Reduce(state, refresh)
	require.NoError(t, err)
	require.Len(t, state.MessageCandidates, len(state.Transcript))
	require.Equal(t, assistantCandidate("run-old"), state.MessageCandidates[3])

	return state
}

func TestCompletionOrderAfterDurableInsertion(t *testing.T) {
	t.Parallel()

	for _, runtimeSnapshot := range []bool{false, true} {
		name := "reduced batch"
		if runtimeSnapshot {
			name = "leading runtime snapshot"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			state := durableQuestionRefresh(t)
			observed := state.Sequence
			first := completionEventChain{
				interactionID: "first", runID: "run-a", prompt: "repeat", answer: "Done.", sequence: observed,
			}
			firstEvents, sequence := first.events(nil)
			second := completionEventChain{
				interactionID: "second", runID: "run-b", prompt: "repeat", answer: "Done.", sequence: sequence,
			}
			allEvents, _ := second.events(firstEvents)

			model := readyModel(t, true)
			if runtimeSnapshot {
				snapshot, err := coding.ReduceBatch(state, allEvents)
				require.NoError(t, err)
				var controller runtimeStateController
				controller.state = snapshot
				model = fullscreenModel(t, controller, true)
				model.runtimeState = true
			}
			model.state = state
			model.observedSequence = observed
			model.reduceStreamBatch(streamItemsFor(allEvents...))

			require.NoError(t, model.streamErr)
			require.Len(t, model.completionMarkers, 2)
			assert.Equal(t, 6, model.completionMarkers[0].afterMessages)
			assert.Equal(t, 8, model.completionMarkers[1].afterMessages)
			model.rebaseCompletionMarkers()
			assert.Equal(t, 6, model.completionMarkers[0].afterMessages,
				"rebasing must not move a marker to a later identical answer")
			rendered := renderTimeline(model.timelineBlocks(), model.markdown, 80, themeDark, true)
			parts := strings.Split(rendered, completionGlyph+" ")
			require.Len(t, parts, 3)
			assert.Equal(t, 2, strings.Count(parts[0], "Done."), "first marker follows only the old and first answers")
			assert.Equal(t, 1, strings.Count(parts[1], "Done."), "second marker follows the next answer")

			// The inline pager must wait for the first answer, write its marker
			// at that boundary, then emit only the second marker in the next delta.
			model.scrollback.messages = 4
			ready, waiting := model.completionMarkersForDelta(5)
			assert.Empty(t, ready)
			require.Len(t, waiting, 2)
			ready, waiting = model.completionMarkersForDelta(6)
			require.Len(t, ready, 1)
			require.Len(t, waiting, 1)
			assert.Equal(t, "first", ready[0].interactionID)
			assert.Equal(t, 2, ready[0].afterMessages)
			model.markCompletionCommitted(ready[0])
			model.scrollback.messages = 6
			ready, waiting = model.completionMarkersForDelta(8)
			require.Len(t, ready, 1)
			assert.Empty(t, waiting)
			assert.Equal(t, "second", ready[0].interactionID)
			assert.Equal(t, 2, ready[0].afterMessages)
		})
	}
}
