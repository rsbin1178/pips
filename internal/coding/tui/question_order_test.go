package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/question"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const questionAnswerBody = "the resumed answer"

// questionOrderFixture builds the exact live shape of an ask_user pause: the
// model's tool call is committed, the resumed answer is committed, and the
// Runtime has not published the durable tool result yet, so the answered card
// is the optimistic preview.
func questionOrderFixture(t *testing.T) (coding.State, question.Request) {
	t.Helper()

	request, err := question.NewRequest("q-order", "ask-order", question.Spec{
		Questions: []question.Question{{
			Header: "Framework", Question: "Which framework?",
			Options: []question.Option{
				{Label: "React", Description: "Component ecosystem"},
				{Label: "Vue", Description: "Progressive framework"},
			},
		}},
	})
	require.NoError(t, err)

	arguments, err := json.Marshal(question.Spec{Questions: request.Questions})
	require.NoError(t, err)

	state := readyState()
	state.Transcript = []ai.Message{
		ai.UserText("ask a question"),
		ai.Assistant(ai.ToolCallPart{
			ID: request.ToolCallID, Name: question.ToolName, Args: arguments,
		}),
	}
	state.MessageCandidates = []coding.CandidateIdentity{
		{}, {RunID: "run-1", Turn: 1},
	}

	return state, request
}

func questionOrderResolution(request question.Request) question.Resolution {
	return question.Resolution{
		RequestID:    request.ID,
		SchemaDigest: request.SchemaDigest,
		Answers:      []question.Answer{{Selections: []string{"React"}}},
	}
}

func questionBlockIndex(blocks []timelineBlock, body string) (int, int) {
	card, answer := -1, -1

	for index, block := range blocks {
		switch {
		case block.kind == blockQuestion:
			card = index
		case block.kind == blockAssistant && strings.Contains(block.body, body):
			answer = index
		}
	}

	return card, answer
}

// TestQuestionPreviewStaysAboveACommittedAnswerInFullscreen pins the streaming
// frame that used to invert the conversation: the resumed answer commits before
// the Runtime publishes the durable ask_user result, so the answered preview
// shares a position with that answer. The frame cache must insert it into the
// committed prefix instead of appending it after the prefix.
func TestQuestionPreviewStaysAboveACommittedAnswerInFullscreen(t *testing.T) {
	t.Parallel()

	state, request := questionOrderFixture(t)
	model := fullscreenModel(t, stubController{state: state}, true)
	model.state = state
	// The preview belongs at position 3, where the durable tool result will land.
	model.previewQuestionOutcome(request, questionOrderResolution(request), false)

	// The resumed answer commits before the result is published.
	model.state.Transcript = append(model.state.Transcript, ai.Assistant(ai.Text(questionAnswerBody)))
	model.state.MessageCandidates = append(
		model.state.MessageCandidates,
		coding.CandidateIdentity{RunID: "run-1", Turn: 2},
	)
	model.rerenderTranscript(true)
	// A second frame reuses the cached prefix; the store must not keep the
	// preview record and append it again.
	model.rerenderTranscript(true)

	cached := viewportProjectionBlocks(model)
	require.Equal(t, model.transcriptBlocks(), cached,
		"the incremental frame is field-for-field equal to the pure projection")

	card, answer := questionBlockIndex(cached, questionAnswerBody)
	require.NotEqual(t, -1, card, "the answered card is projected")
	require.NotEqual(t, -1, answer, "the resumed answer is projected")
	assert.Less(t, card, answer, "the question stays above the answer that follows it")

	content := ansi.Strip(model.View().Content)
	assert.Equal(t, 1, strings.Count(content, "User answered the question"),
		"the preview is rendered exactly once across frames")
	cardAt := strings.Index(content, "User answered the question")
	answerAt := strings.Index(content, questionAnswerBody)

	require.NotEqual(t, -1, cardAt)
	require.NotEqual(t, -1, answerAt)
	assert.Less(t, cardAt, answerAt, "the painted frame keeps the same order")
}

// TestQuestionPreviewOrderSurvivesTheDurableResult pins the frame after the
// preview is replaced by the durable tool result. The preview shifted the store's
// records, so the next frame must rebuild from that index instead of reusing the
// old order.
func TestQuestionPreviewOrderSurvivesTheDurableResult(t *testing.T) {
	t.Parallel()

	state, request := questionOrderFixture(t)
	model := fullscreenModel(t, stubController{state: state}, true)
	model.state = state
	model.previewQuestionOutcome(request, questionOrderResolution(request), false)
	model.state.Transcript = append(model.state.Transcript, ai.Assistant(ai.Text(questionAnswerBody)))
	model.state.MessageCandidates = append(
		model.state.MessageCandidates,
		coding.CandidateIdentity{RunID: "run-1", Turn: 2},
	)
	model.rerenderTranscript(true)

	// The Runtime publishes the durable tool result and replaces the transcript
	// through session.tree_changed at interaction end.
	model.questionPreviews = nil
	model.state.Transcript = []ai.Message{
		ai.UserText("ask a question"),
		ai.Assistant(ai.ToolCallPart{
			ID: request.ToolCallID, Name: question.ToolName,
			Args: mustQuestionArguments(t, request),
		}),
		ai.ToolResultText(request.ToolCallID, question.ToolName,
			`{"answers":[{"selections":["React"]}]}`),
		ai.Assistant(ai.Text(questionAnswerBody)),
	}
	model.state.MessageCandidates = append(
		model.state.MessageCandidates,
		coding.CandidateIdentity{},
	)
	model.rerenderTranscript(true)

	cached := viewportProjectionBlocks(model)
	require.Equal(t, model.transcriptBlocks(), cached,
		"the incremental frame is field-for-field equal after the durable result")
	card, answer := questionBlockIndex(cached, questionAnswerBody)
	require.NotEqual(t, -1, card)
	require.NotEqual(t, -1, answer)
	assert.Less(t, card, answer)

	content := ansi.Strip(model.View().Content)
	cardAt := strings.Index(content, "User answered the question")
	answerAt := strings.Index(content, questionAnswerBody)

	require.NotEqual(t, -1, cardAt)
	require.NotEqual(t, -1, answerAt)
	assert.Less(t, cardAt, answerAt, "the store rebuilt the records the preview shifted")
}

// TestQuestionPreviewRemovalRestoresTheCommittedFrame pins the other side of the
// same cache seam: a preview can be dropped without a transcript change (the
// prompt reopens because its resolution did not land). The store may not keep
// the preview record and lose the answer that was rendered after it.
func TestQuestionPreviewRemovalRestoresTheCommittedFrame(t *testing.T) {
	t.Parallel()

	state, request := questionOrderFixture(t)
	model := fullscreenModel(t, stubController{state: state}, true)
	model.state = state
	model.previewQuestionOutcome(request, questionOrderResolution(request), false)
	model.state.Transcript = append(model.state.Transcript, ai.Assistant(ai.Text(questionAnswerBody)))
	model.state.MessageCandidates = append(
		model.state.MessageCandidates,
		coding.CandidateIdentity{RunID: "run-1", Turn: 2},
	)
	model.rerenderTranscript(true)

	// The prompt reopens for the same call and drops the optimistic card; the
	// committed transcript is unchanged.
	model.questionPreviews = nil
	model.rerenderTranscript(true)

	content := ansi.Strip(model.View().Content)
	assert.NotContains(t, content, "User answered the question",
		"the dropped preview leaves no record behind")
	assert.Equal(t, 1, strings.Count(content, questionAnswerBody),
		"the answer stays visible exactly once")
	assert.Equal(t, model.transcriptBlocks(), viewportProjectionBlocks(model))
}

// TestQuestionPreviewPrecedesTheCommittedAnswerInline pins the native-history
// projection: the preview position is absolute, while an inline delta's blocks
// are window-relative. A preview appended after the delta would print the answer
// before its question card and the terminal would keep that order for good.
func TestQuestionPreviewPrecedesTheCommittedAnswerInline(t *testing.T) {
	t.Parallel()

	state, request := questionOrderFixture(t)
	model := readyModel(t, true)
	model.state = state
	model.scrollback.messages = len(state.Transcript)

	model.previewQuestionOutcome(request, questionOrderResolution(request), false)
	model.state.Transcript = append(model.state.Transcript, ai.Assistant(ai.Text(questionAnswerBody)))
	model.state.MessageCandidates = append(
		model.state.MessageCandidates,
		coding.CandidateIdentity{RunID: "run-1", Turn: 2},
	)

	active := model.activeTimelineBlocks()
	card, answer := questionBlockIndex(active, questionAnswerBody)
	require.NotEqual(t, -1, card, "the live tail carries the preview")
	require.NotEqual(t, -1, answer, "the live tail carries the answer")
	assert.Less(t, card, answer, "the tail keeps the question above the answer")

	stable := model.takeStableTimelineBlocks()
	card, answer = questionBlockIndex(stable, questionAnswerBody)
	require.NotEqual(t, -1, card, "the stable delta prints the preview")
	require.NotEqual(t, -1, answer, "the stable delta prints the answer")
	assert.Less(t, card, answer, "native history receives the question first")
	assert.Empty(t, model.questionPreviews, "the printed preview is consumed once")
}

func mustQuestionArguments(t *testing.T, request question.Request) ai.JSON {
	t.Helper()

	arguments, err := json.Marshal(question.Spec{Questions: request.Questions})
	require.NoError(t, err)

	return arguments
}

// TestQuestionPreviewFrameOrderMatchesThePureProjection drives a full Update
// cycle through a window size change so the composed frame, not just the block
// list, is checked against the reference projection.
func TestQuestionPreviewFrameOrderMatchesThePureProjection(t *testing.T) {
	t.Parallel()

	state, request := questionOrderFixture(t)
	model := fullscreenModel(t, stubController{state: state}, true)
	model.state = state
	model.previewQuestionOutcome(request, questionOrderResolution(request), false)
	model.state.Transcript = append(model.state.Transcript, ai.Assistant(ai.Text(questionAnswerBody)))
	model.state.MessageCandidates = append(
		model.state.MessageCandidates,
		coding.CandidateIdentity{RunID: "run-1", Turn: 2},
	)

	model.Update(tea.WindowSizeMsg{Width: 72, Height: 30})

	cached := viewportProjectionBlocks(model)
	require.Equal(t, model.transcriptBlocks(), cached)
	card, answer := questionBlockIndex(cached, questionAnswerBody)
	require.NotEqual(t, -1, card)
	require.NotEqual(t, -1, answer)
	assert.Less(t, card, answer)
}

// streamedAnswerDelta is one parent-Session answer delta for the ordering
// fixture's resumed turn.
func streamedAnswerDelta(state coding.State, sequence uint64, text string) coding.Event {
	return coding.Event{
		Schema: coding.EventSchema, Sequence: sequence, Time: time.Now().UTC(),
		SessionID: state.SessionID, InteractionID: "interaction-1", RunID: "run-1",
		Type:    coding.EventMessageDelta,
		Payload: coding.MessageDelta{Kind: ai.StreamTextDelta, Text: text},
	}
}

// streamedAnswerCommit commits the resumed answer under the same run.
func streamedAnswerCommit(state coding.State, sequence uint64) coding.Event {
	return coding.Event{
		Schema: coding.EventSchema, Sequence: sequence, Time: time.Now().UTC(),
		SessionID: state.SessionID, InteractionID: "interaction-1", RunID: "run-1",
		Type:    coding.EventMessageCommitted,
		Payload: coding.MessageCommitted{Message: ai.Assistant(ai.Text(questionAnswerBody))},
	}
}

// previewOrderIndex locates the answered card and the answer it belongs to. The
// answer is a draft block while it streams and an assistant block once it
// commits, so both kinds count.
func previewOrderIndex(blocks []timelineBlock, body string) (int, int) {
	card, answer := -1, -1

	for index, block := range blocks {
		switch {
		case block.kind == blockQuestion && card < 0:
			card = index
		case (block.kind == blockAssistant || block.kind == blockDraft) &&
			strings.Contains(block.body, body) && answer < 0:
			answer = index
		}
	}

	return card, answer
}

// TestQuestionPreviewOrderHoldsAcrossStreamedFrames drives task A's fixture
// through the real subscription path instead of assigning the model's state: the
// resumed answer arrives as streamed deltas and then commits, and every
// intermediate frame has to keep the answered card above the answer — in the
// cached frame, in the live tail, and in what reaches native history.
func TestQuestionPreviewOrderHoldsAcrossStreamedFrames(t *testing.T) {
	t.Parallel()

	for _, fullscreen := range []bool{true, false} {
		t.Run(map[bool]string{true: "fullscreen", false: "inline"}[fullscreen], func(t *testing.T) {
			t.Parallel()

			state, request := questionOrderFixture(t)
			state.Phase = coding.PhaseRunning
			state.Interaction = coding.InteractionState{ID: "interaction-1", Active: true}
			state.Runs = []coding.RunState{{ID: "run-1", Active: true, TurnOpen: true, Turn: 1}}

			controller := stubController{state: state}

			var model *Model
			if fullscreen {
				model = fullscreenModel(t, controller, true)
			} else {
				model = readyModelWithController(t, controller, true)
			}

			model.state = state
			model.subscription = &subscriptionBridge{}
			model.Update(tea.WindowSizeMsg{Width: 72, Height: 30})

			// The user answers: the optimistic card lands at the durable result's
			// position while the Runtime resumes the interaction.
			model.previewQuestionOutcome(request, questionOrderResolution(request), false)

			assertOrder := func(stage string, blocks []timelineBlock, body string) {
				t.Helper()

				card, answer := previewOrderIndex(blocks, body)
				require.NotEqual(t, -1, card, "%s: the answered card is projected", stage)
				require.NotEqual(t, -1, answer, "%s: the answer is projected", stage)
				assert.Less(t, card, answer, "%s: the card stays above the answer", stage)
			}

			// Inline mode hands the stable delta to the terminal as it is produced,
			// so the order that matters there is the order of the printed rows.
			assertPrintedOrder := func(stage, printed, body string, answerIsStable bool) {
				t.Helper()

				cardAt := strings.Index(printed, "User answered the question")
				require.NotEqual(t, -1, cardAt, "%s: native history receives the answered card", stage)

				// A live answer stays in the mutable tail, so only the rows the
				// stream promoted have reached the terminal. Whatever did reach it
				// has to come after the card.
				answerAt := strings.Index(printed, body)
				if answerAt < 0 {
					assert.False(t, answerIsStable, "%s: native history receives the answer", stage)

					return
				}

				assert.Less(t, cardAt, answerAt, "%s: the card precedes the answer in native history", stage)
			}

			sequence := uint64(0)
			streamed := ""
			printed := ""

			// deliver feeds one event and acknowledges the native writes it
			// produced, without running the subscription wait: the stub bridge has
			// no producer, so a driven wait would report a closed subscription.
			deliver := func(event coding.Event) {
				t.Helper()

				_, command := model.Update(subscriptionEventMsg{
					bridge: model.subscription, ok: true,
					record: coding.EventRecord{Event: event},
				})
				require.NoError(t, model.streamErr)
				_, tick := model.Update(renderTickMsg{})

				if fullscreen {
					return
				}

				printed += strings.Join(modelCommandOutputs(model, tea.Batch(command, tick)), "\n")
			}

			for _, chunk := range []string{"the resumed ", "answer"} {
				sequence++
				streamed += chunk
				deliver(streamedAnswerDelta(state, sequence, chunk))

				body := strings.TrimSpace(streamed)

				if fullscreen {
					blocks := model.transcriptBlocks()
					require.Equal(t, blocks, viewportProjectionBlocks(model),
						"a streamed frame is field-for-field equal to the pure projection")
					assertOrder("delta", blocks, body)

					continue
				}

				assertPrintedOrder("delta", printed, body, false)
			}

			sequence++
			deliver(streamedAnswerCommit(state, sequence))

			if fullscreen {
				blocks := model.transcriptBlocks()
				require.Equal(t, blocks, viewportProjectionBlocks(model),
					"the commit frame is field-for-field equal to the pure projection")
				assertOrder("commit", blocks, questionAnswerBody)

				return
			}

			assertPrintedOrder("commit", printed, questionAnswerBody, true)
			assert.Contains(t, ansi.Strip(printed), questionAnswerBody,
				"native history receives the committed answer")
		})
	}
}
