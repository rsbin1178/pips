package tui

import (
	"context"
	"iter"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/question"
	"github.com/rsbin1178/pips/internal/coding/runtimecontrol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStructuredQuestionPromptResolvesExactSelection(t *testing.T) {
	t.Parallel()

	request := testQuestionRequest(t)
	controller := &questionPromptController{stubController: stubController{
		state: questionPromptStateSnapshot(request),
	}}
	model := readyModelWithController(t, controller, true)

	require.Equal(t, promptQuestion, model.prompt.kind)
	view := model.View().Content
	assert.Contains(t, view, "[□ Framework]")
	assert.Contains(t, view, "React")
	assert.Contains(t, view, "Component ecosystem")
	assert.Contains(t, view, "Chat about this")
	assert.Nil(t, model.View().Cursor)

	model.Update(key("enter"))
	assert.Equal(t, 1, model.prompt.question.tab)
	_, command := model.Update(key("enter"))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	require.Len(t, controller.resolutions, 1)
	assert.Equal(t, request.ID, controller.resolutions[0].RequestID)
	assert.Equal(t, request.SchemaDigest, controller.resolutions[0].SchemaDigest)
	require.Len(t, controller.resolutions[0].Answers, 1)
	assert.Equal(t, []string{"React"}, controller.resolutions[0].Answers[0].Selections)
	assert.Equal(t, promptNone, model.prompt.kind)
	assert.True(t, model.composer.Focused())
}

func TestMixedQuestionPromptEditsTextAndSubmits(t *testing.T) {
	t.Parallel()

	request, err := question.NewRequest("q-mixed", "call-mixed", question.Spec{
		Questions: []question.Question{
			{
				Header: "Framework", Question: "Which framework?",
				Kind: question.KindChoice,
				Options: []question.Option{
					{Label: "React", Description: "Components"},
					{Label: "Vue", Description: "Progressive"},
				},
			},
			{
				Header: "Risk", Question: "Which risk should be verified first?",
				Kind: question.KindText,
			},
			{
				Header: "Environment", Question: "Where should it run?",
				Kind: question.KindChoice,
				Options: []question.Option{
					{Label: "Local", Description: "Developer machine"},
					{Label: "CI", Description: "Pipeline"},
				},
			},
		},
	})
	require.NoError(t, err)

	controller := &questionPromptController{stubController: stubController{
		state: questionPromptStateSnapshot(request),
	}}
	model := readyModelWithController(t, controller, true)
	require.Equal(t, promptQuestion, model.prompt.kind)
	require.Equal(t, 0, model.prompt.question.tab)

	// Select the first choice; the next tab is kind text and opens its editor.
	model.Update(key("enter"))
	assert.Equal(t, 1, model.prompt.question.tab)
	assert.Equal(t, questionEditCustom, model.prompt.question.editing)
	assert.Contains(t, ansi.Strip(model.View().Content), "Which risk should be verified first?")

	model.prompt.question.editor.SetValue("支付重试")
	model.Update(key("enter"))
	assert.Equal(t, 2, model.prompt.question.tab)
	assert.Equal(t, questionEditNone, model.prompt.question.editing)

	// Select the last choice, then submit from the review tab.
	model.Update(key("enter"))
	assert.Equal(t, 3, model.prompt.question.tab)
	_, command := model.Update(key("enter"))
	driveModelCommands(t, model, command)

	require.Len(t, controller.resolutions, 1)
	answers := controller.resolutions[0].Answers
	require.Len(t, answers, 3)
	assert.Equal(t, []string{"React"}, answers[0].Selections)
	assert.Equal(t, "支付重试", answers[1].Custom)
	assert.Empty(t, answers[1].Selections)
	assert.Equal(t, []string{"Local"}, answers[2].Selections)
}

func TestMixedQuestionPromptTabSkipsTextQuestion(t *testing.T) {
	t.Parallel()

	request, err := question.NewRequest("q-mixed-tab", "call-mixed-tab", question.Spec{
		Questions: []question.Question{
			{
				Header: "Risk", Question: "Which risk should be verified first?",
				Kind: question.KindText,
			},
			{
				Header: "Environment", Question: "Where should it run?",
				Kind: question.KindChoice,
				Options: []question.Option{
					{Label: "Local", Description: "Developer machine"},
					{Label: "CI", Description: "Pipeline"},
				},
			},
		},
	})
	require.NoError(t, err)

	controller := &questionPromptController{stubController: stubController{
		state: questionPromptStateSnapshot(request),
	}}
	model := readyModelWithController(t, controller, true)
	require.Equal(t, questionEditCustom, model.prompt.question.editing)

	model.prompt.question.editor.SetValue("flaky checkout")
	model.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	assert.Equal(t, 1, model.prompt.question.tab)
	assert.Equal(t, questionEditNone, model.prompt.question.editing)
	assert.Equal(t, "flaky checkout", model.prompt.question.custom[0])

	// Submit with the text answer retained and the choice still unanswered.
	model.Update(key("enter"))
	_, command := model.Update(key("enter"))
	driveModelCommands(t, model, command)

	require.Len(t, controller.resolutions, 1)
	assert.Equal(t, "flaky checkout", controller.resolutions[0].Answers[0].Custom)
	assert.Equal(t, []string{"Local"}, controller.resolutions[0].Answers[1].Selections)
}

func TestFreeformQuestionPromptSubmitsDirectChatResponse(t *testing.T) {
	t.Parallel()

	request, err := question.NewFreeformRequest(
		"q-freeform", "call-freeform", "Which authentication constraint is missing?",
	)
	require.NoError(t, err)

	controller := &questionPromptController{stubController: stubController{
		state: questionPromptStateSnapshot(request),
	}}
	model := readyModelWithController(t, controller, true)

	require.Equal(t, questionEditChat, model.prompt.question.editing)
	view := model.View().Content
	assert.Contains(t, view, "▌", "freeform shares the question band's accent gutter")
	assert.Contains(t, view, "←  [□ Input required]")
	assert.Contains(t, view, "Which authentication constraint is missing?")
	model.prompt.question.editor.SetValue("Use the school's SSO provider.")
	_, command := model.Update(key("enter"))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	require.Len(t, controller.resolutions, 1)
	assert.Equal(t, "Use the school's SSO provider.", controller.resolutions[0].Chat)
	assert.Empty(t, controller.resolutions[0].Answers)
}

func TestFreeformQuestionPromptRejectsOnEscape(t *testing.T) {
	t.Parallel()

	request, err := question.NewFreeformRequest(
		"q-freeform-cancel", "call-freeform-cancel", "Which constraint is missing?",
	)
	require.NoError(t, err)

	controller := &questionPromptController{stubController: stubController{
		state: questionPromptStateSnapshot(request),
	}}
	model := readyModelWithController(t, controller, true)

	_, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	require.Len(t, controller.rejections, 1)
	assert.Equal(t, request.ID, controller.rejections[0].requestID)
	assert.Empty(t, controller.resolutions)
}

func TestStructuredQuestionPromptRejectsWithoutSynthesizingAnswer(t *testing.T) {
	t.Parallel()

	request := testQuestionRequest(t)
	controller := &questionPromptController{stubController: stubController{
		state: questionPromptStateSnapshot(request),
	}}
	model := readyModelWithController(t, controller, true)

	_, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	require.Len(t, controller.rejections, 1)
	assert.Equal(t, request.ID, controller.rejections[0].requestID)
	assert.Equal(t, request.SchemaDigest, controller.rejections[0].schemaDigest)
	assert.Empty(t, controller.resolutions)
}

func TestStructuredQuestionPromptSupportsMultiSelectPreviewAndCustomAnswer(t *testing.T) {
	t.Parallel()

	request, err := question.NewRequest("q-multi", "call-multi", question.Spec{
		Questions: []question.Question{{
			Header: "Scope", Question: "Which areas should change?", Multiple: true,
			Options: []question.Option{
				{
					Label: "Runtime", Description: "Core runtime",
					Preview: "### Runtime preview\n\n- event flow",
				},
				{Label: "Tests", Description: "Focused coverage"},
			},
		}},
	})
	require.NoError(t, err)

	controller := &questionPromptController{stubController: stubController{
		state: questionPromptStateSnapshot(request),
	}}
	model := readyModelWithController(t, controller, true)
	assert.Contains(t, model.View().Content, "Runtime preview")

	model.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	model.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	model.Update(key("enter"))
	_, command := model.Update(key("enter"))
	driveModelCommands(t, model, command)

	require.Len(t, controller.resolutions, 1)
	assert.Equal(
		t,
		[]string{"Runtime", "Tests"},
		controller.resolutions[0].Answers[0].Selections,
	)

	request = testQuestionRequest(t)
	controller = &questionPromptController{stubController: stubController{
		state: questionPromptStateSnapshot(request),
	}}
	model = readyModelWithController(t, controller, true)
	model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	model.Update(key("enter"))
	require.Equal(t, questionEditCustom, model.prompt.question.editing)
	model.prompt.question.editor.SetValue("Svelte")
	model.Update(key("enter"))
	_, command = model.Update(key("enter"))
	driveModelCommands(t, model, command)

	require.Len(t, controller.resolutions, 1)
	assert.Equal(t, "Svelte", controller.resolutions[0].Answers[0].Custom)
}

func TestStructuredQuestionPromptSupportsChatAboutThis(t *testing.T) {
	t.Parallel()

	request := testQuestionRequest(t)
	controller := &questionPromptController{stubController: stubController{
		state: questionPromptStateSnapshot(request),
	}}

	model := readyModelWithController(t, controller, true)
	for range 3 {
		model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}

	model.Update(key("enter"))
	require.Equal(t, questionEditChat, model.prompt.question.editing)
	model.prompt.question.editor.SetValue("Compare the maintenance trade-offs first.")
	_, command := model.Update(key("enter"))
	driveModelCommands(t, model, command)

	require.Len(t, controller.resolutions, 1)
	assert.Equal(
		t,
		"Compare the maintenance trade-offs first.",
		controller.resolutions[0].Chat,
	)
	assert.Empty(t, controller.resolutions[0].Answers)
}

func TestQuestionEditorCursorStaysInThePromptBand(t *testing.T) {
	t.Parallel()

	request := testQuestionRequest(t)
	controller := &questionPromptController{stubController: stubController{
		state: questionPromptStateSnapshot(request),
	}}
	model := readyModelWithController(t, controller, true)

	model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	model.Update(key("enter"))
	require.Equal(t, questionEditCustom, model.prompt.question.editing)

	view := model.View()
	require.NotNil(t, view.Cursor)
	row := promptRowIndex(t, view.Content, "Type a custom answer")
	assert.Equal(t, row, view.Cursor.Y, "the caret belongs on the editor row")
	assert.NotEqual(t, model.frameHit.composerTop, view.Cursor.Y, "the caret must not fall onto the Composer")
	rowContent := strings.Split(view.Content, "\n")[row]
	prefixEnd := strings.Index(rowContent, "Type a custom answer")
	require.NotEqual(t, -1, prefixEnd)
	assert.Equal(t, ansi.StringWidth(rowContent[:prefixEnd]), view.Cursor.X)
}

func TestQuestionPreviewDropsDetailBeforeChoices(t *testing.T) {
	t.Parallel()

	request, err := question.NewRequest("q-preview", "call-preview", question.Spec{
		Questions: []question.Question{{
			Header: "Framework", Question: "Which framework should be used?",
			Options: []question.Option{
				{
					Label: "React", Description: "Component ecosystem",
					Preview: "### React\n\n- hooks\n- ecosystem\n- server components\n- suspence",
				},
				{Label: "Vue", Description: "Progressive framework"},
				{Label: "Svelte", Description: "Compiled UI"},
				{Label: "Solid", Description: "Fine-grained reactivity"},
			},
		}},
	})
	require.NoError(t, err)

	controller := &questionPromptController{stubController: stubController{
		state: questionPromptStateSnapshot(request),
	}}
	model := readyModelWithController(t, controller, true)

	content := model.View().Content
	for _, label := range []string{"React", "Vue", "Svelte", "Solid", "Type something", "Chat about this"} {
		assert.Contains(t, content, label)
	}
}

func TestFreeformQuestionCursorStaysInThePromptBand(t *testing.T) {
	t.Parallel()

	request, err := question.NewFreeformRequest(
		"q-freeform-cursor", "call-freeform-cursor", "Which authentication constraint is missing?",
	)
	require.NoError(t, err)

	controller := &questionPromptController{stubController: stubController{
		state: questionPromptStateSnapshot(request),
	}}
	model := readyModelWithController(t, controller, true)

	view := model.View()
	require.NotNil(t, view.Cursor)
	row := promptRowIndex(t, view.Content, "Type the missing preference or constraint")
	assert.Equal(t, row, view.Cursor.Y)
	assert.NotEqual(t, model.frameHit.composerTop, view.Cursor.Y)

	rowContent := strings.Split(view.Content, "\n")[row]
	prefixEnd := strings.Index(rowContent, "Type the missing preference or constraint")
	require.NotEqual(t, -1, prefixEnd)
	assert.Equal(t, ansi.StringWidth(rowContent[:prefixEnd]), view.Cursor.X)
}

func TestQuestionCustomAnswerAcceptsNewlines(t *testing.T) {
	t.Parallel()

	request := testQuestionRequest(t)
	controller := &questionPromptController{stubController: stubController{
		state: questionPromptStateSnapshot(request),
	}}
	model := readyModelWithController(t, controller, true)

	for range 2 {
		model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}

	model.Update(key("enter"))
	require.Equal(t, questionEditCustom, model.prompt.question.editing)

	model.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	model.Update(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
	model.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})

	assert.Equal(t, "a\nb", model.prompt.question.editor.Value())
}

func TestQuestionPromptShowsBatchProgress(t *testing.T) {
	t.Parallel()

	request := testQuestionRequest(t)
	request.QueueIndex, request.QueueSize = 2, 3
	model := readyModelWithController(t, stubController{
		state: questionPromptStateSnapshot(request),
	}, true)
	model.syncApprovalPrompt()
	require.Equal(t, promptQuestion, model.prompt.kind)

	view, _ := model.promptView()
	assert.Contains(t, ansi.Strip(view), "Question 2/3")
}

func TestQuestionPromptHidesProgressForSingleRequest(t *testing.T) {
	t.Parallel()

	request := testQuestionRequest(t)
	model := readyModelWithController(t, stubController{
		state: questionPromptStateSnapshot(request),
	}, true)
	model.syncApprovalPrompt()
	require.Equal(t, promptQuestion, model.prompt.kind)

	view, _ := model.promptView()
	assert.NotContains(t, ansi.Strip(view), "Question 1/1")
}

func TestQuestionPreviewRendersBeforeTheNextStreamingAnswer(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		rejected bool
	}{
		{name: "answered"},
		{name: "canceled", rejected: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			request := testQuestionRequest(t)
			model := readyModel(t, true)
			resolution := question.Resolution{
				RequestID: request.ID, SchemaDigest: request.SchemaDigest,
				Answers: []question.Answer{{Selections: []string{"React"}}},
			}
			model.previewQuestionOutcome(request, resolution, testCase.rejected)
			model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{
				Kind: ai.StreamTextDelta, Text: "next answer",
			})

			blocks := model.timelineBlocks()
			previewIndex, draftIndex := -1, -1

			for index, block := range blocks {
				switch block.kind {
				case blockQuestion:
					previewIndex = index
				case blockDraft:
					draftIndex = index
				default:
				}
			}

			require.NotEqual(t, -1, previewIndex, "the question card is projected")
			require.NotEqual(t, -1, draftIndex, "the next answer is streaming")
			assert.Less(t, previewIndex, draftIndex,
				"the question card stays above the answer that follows it")
		})
	}
}

func TestMultipleQuestionPreviewsStayInConversationOrder(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	canceled := testQuestionRequest(t)
	canceled.ToolCallID = "z-call"
	answered, err := question.NewFreeformRequest("q-second", "a-call", "Say something")
	require.NoError(t, err)

	model.state.Transcript = []ai.Message{
		ai.UserText("test both"),
		ai.Assistant(ai.ToolCallPart{
			ID: "z-call", Name: "ask_user", Args: ai.JSON(`{"questions":[]}`),
		}),
	}
	model.previewQuestionOutcome(canceled, question.Resolution{
		RequestID: canceled.ID, SchemaDigest: canceled.SchemaDigest,
	}, true)

	model.state.Transcript = append(model.state.Transcript, ai.Assistant(
		ai.Text("ask_user canceled; now ask_user_text"),
		ai.ToolCallPart{
			ID: "a-call", Name: "ask_user_text", Args: ai.JSON(`{"question":"Say something"}`),
		},
	))
	model.previewQuestionOutcome(answered, question.Resolution{
		RequestID: answered.ID, SchemaDigest: answered.SchemaDigest, Chat: "hello world",
	}, false)
	model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{
		Kind: ai.StreamTextDelta, Text: "next answer",
	})

	blocks := model.timelineBlocks()
	indexOf := func(match func(timelineBlock) bool) int {
		for index, block := range blocks {
			if match(block) {
				return index
			}
		}

		return -1
	}
	canceledAt := indexOf(func(block timelineBlock) bool {
		return block.kind == blockQuestion && block.title == questionCanceledTitle
	})
	textAt := indexOf(func(block timelineBlock) bool {
		return block.kind == blockAssistant && strings.Contains(block.body, "ask_user canceled")
	})
	answeredAt := indexOf(func(block timelineBlock) bool {
		return block.kind == blockQuestion && block.title == questionAnsweredSingleTitle
	})
	draftAt := indexOf(func(block timelineBlock) bool { return block.kind == blockDraft })

	require.NotEqual(t, -1, canceledAt)
	require.NotEqual(t, -1, textAt)
	require.NotEqual(t, -1, answeredAt)
	require.NotEqual(t, -1, draftAt)
	assert.Less(t, canceledAt, textAt, "the first question stays before the message that followed it")
	assert.Less(t, textAt, answeredAt, "the second question stays after that message")
	assert.Less(t, answeredAt, draftAt, "both cards stay above the next answer")
}

func TestQuestionPreviewRendersAboveTheStreamedAnswerInFullscreen(t *testing.T) {
	t.Parallel()

	request := testQuestionRequest(t)
	model := fullscreenModel(t, stubController{state: readyState()}, true)
	model.previewQuestionOutcome(request, question.Resolution{
		RequestID: request.ID, SchemaDigest: request.SchemaDigest,
		Answers: []question.Answer{{Selections: []string{"React"}}},
	}, false)
	model.state.Draft = coding.NewStreamDraft(coding.MessageDelta{
		Kind: ai.StreamTextDelta, Text: "next answer",
	})
	model.rerenderTranscript(true)

	content := ansi.Strip(model.View().Content)
	cardAt := strings.Index(content, "User answered the question")
	answerAt := strings.Index(content, "next answer")

	require.NotEqual(t, -1, cardAt)
	require.NotEqual(t, -1, answerAt)
	assert.Less(t, cardAt, answerAt, "the card is painted above the next answer")
}

func TestQuestionPromptMouseSelectsSingleOption(t *testing.T) {
	t.Parallel()

	request := testQuestionRequest(t)
	controller := &questionPromptController{stubController: stubController{
		state: questionPromptStateSnapshot(request),
	}}
	model := fullscreenModel(t, controller, true)

	row := promptRowIndex(t, model.View().Content, "1. ○ React")
	model.handleMouse(tea.MouseClickMsg{Button: tea.MouseLeft, X: 8, Y: row})

	require.Equal(t, 1, model.prompt.question.tab, "a single choice advances to Submit")
	require.Len(t, model.prompt.question.selected, 1)
	assert.True(t, model.prompt.question.selected[0][0])

	_, command := model.Update(key("enter"))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	require.Len(t, controller.resolutions, 1)
	assert.Equal(t, []string{"React"}, controller.resolutions[0].Answers[0].Selections)
}

func TestQuestionPromptMouseSubmitsFromTheReviewTab(t *testing.T) {
	t.Parallel()

	request := testQuestionRequest(t)
	controller := &questionPromptController{stubController: stubController{
		state: questionPromptStateSnapshot(request),
	}}
	model := fullscreenModel(t, controller, true)

	row := promptRowIndex(t, model.View().Content, "1. ○ React")
	model.handleMouse(tea.MouseClickMsg{Button: tea.MouseLeft, X: 8, Y: row})
	require.Equal(t, len(request.Questions), model.prompt.question.tab)

	model.View()

	submitRow, submitX, submitStart := -1, -1, -1

	for index, hitRow := range model.frameHit.question.rows {
		for _, segment := range hitRow.segments {
			if segment.index == len(request.Questions) {
				submitRow = model.frameHit.question.top + index
				submitX = (segment.start + segment.end) / 2
				submitStart = segment.start
			}
		}
	}

	require.NotEqual(t, -1, submitRow)

	tabRow := ansi.Strip(strings.Split(model.View().Content, "\n")[submitRow])
	labelStart := strings.Index(tabRow, "[✓ Submit]")
	require.NotEqual(t, -1, labelStart)
	assert.Equal(t, ansi.StringWidth(tabRow[:labelStart]), submitStart,
		"the pointer map must address the Submit label that was painted")

	command := model.handleMouse(tea.MouseClickMsg{Button: tea.MouseLeft, X: submitX, Y: submitRow})
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	require.Len(t, controller.resolutions, 1)
	assert.Equal(t, []string{"React"}, controller.resolutions[0].Answers[0].Selections)
}

func TestQuestionPromptMouseTogglesMultipleOptions(t *testing.T) {
	t.Parallel()

	request, err := question.NewRequest("q-mouse", "call-mouse", question.Spec{
		Questions: []question.Question{{
			Header: "Scope", Question: "Which areas should change?", Multiple: true,
			Options: []question.Option{
				{Label: "Runtime", Description: "Core runtime"},
				{Label: "Tests", Description: "Focused coverage"},
			},
		}},
	})
	require.NoError(t, err)

	controller := &questionPromptController{stubController: stubController{
		state: questionPromptStateSnapshot(request),
	}}
	model := fullscreenModel(t, controller, true)

	row := promptRowIndex(t, model.View().Content, "1. [ ] Runtime")
	model.handleMouse(tea.MouseClickMsg{Button: tea.MouseLeft, X: 8, Y: row})
	assert.True(t, model.prompt.question.selected[0][0])
	assert.Equal(t, 0, model.prompt.question.tab, "toggling keeps the question open")

	row = promptRowIndex(t, model.View().Content, "1. [✓] Runtime")
	model.handleMouse(tea.MouseClickMsg{Button: tea.MouseLeft, X: 8, Y: row})
	assert.False(t, model.prompt.question.selected[0][0])
}

func TestQuestionPromptMouseOpensCustomAnswerEditor(t *testing.T) {
	t.Parallel()

	request := testQuestionRequest(t)
	controller := &questionPromptController{stubController: stubController{
		state: questionPromptStateSnapshot(request),
	}}
	model := fullscreenModel(t, controller, true)

	row := promptRowIndex(t, model.View().Content, "Type something")
	model.handleMouse(tea.MouseClickMsg{Button: tea.MouseLeft, X: 8, Y: row})

	assert.Equal(t, questionEditCustom, model.prompt.question.editing)
	assert.True(t, model.prompt.question.editor.Focused())
}

func TestQuestionSubmitReturnsToTheFirstUnansweredQuestion(t *testing.T) {
	t.Parallel()

	request := testTwoQuestionRequest(t)
	controller := &questionPromptController{stubController: stubController{
		state: questionPromptStateSnapshot(request),
	}}
	model := readyModelWithController(t, controller, true)

	model.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	model.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	require.Equal(t, len(request.Questions), model.prompt.question.tab)

	model.Update(key("enter"))
	assert.Equal(t, 0, model.prompt.question.tab)
	require.Error(t, model.prompt.question.err)
	assert.Contains(t, model.prompt.question.err.Error(), "Framework")
}

func promptRowIndex(t *testing.T, content, needle string) int {
	t.Helper()

	for index, row := range strings.Split(content, "\n") {
		if strings.Contains(ansi.Strip(row), needle) {
			return index
		}
	}

	require.FailNow(t, "prompt row not found", needle)

	return -1
}

func TestShiftTabTogglesProcessMode(t *testing.T) {
	t.Parallel()

	controller := &modePromptController{stubController: stubController{state: readyState()}}
	model := readyModelWithController(t, controller, true)

	_, command := model.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	require.NotNil(t, command)
	message := command()
	_, next := model.Update(message)
	driveModelCommands(t, model, next)

	assert.Equal(t, coding.ModePlan, controller.state.Mode)
	assert.Equal(t, coding.ModePlan, model.state.Mode)
	assert.Contains(t, model.statusLine(), "Plan mode")
	assert.NotContains(t, model.View().Content, "Planning:")
	assert.NotContains(t, model.View().Content, ".pips/plans/")
	assert.True(t, controller.overridden)
}

func TestModeControlWaitsForSubscribedEvent(t *testing.T) {
	t.Parallel()

	controller := &modePromptController{stubController: stubController{state: readyState()}}
	model := readyModelWithController(t, controller, true)
	model.subscriptionMode = true

	_, command := model.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	require.NotNil(t, command)
	_, _ = model.Update(command())

	assert.Equal(t, uint64(0), model.state.Sequence)
	assert.Equal(t, coding.ModeAgent, model.state.Mode)

	model.reduceObservedEvent(coding.Event{
		Schema: coding.EventSchema, Sequence: controller.state.Sequence,
		Time: time.Unix(1, 0).UTC(), SessionID: controller.state.SessionID,
		Type: coding.EventModeChanged, Payload: coding.ModeChanged{Mode: coding.ModePlan},
	})

	require.NoError(t, model.streamErr)
	assert.Equal(t, controller.state.Sequence, model.state.Sequence)
	assert.Equal(t, coding.ModePlan, model.state.Mode)
}

func testQuestionRequest(t *testing.T) question.Request {
	t.Helper()

	request, err := question.NewRequest("q-test", "call-test", question.Spec{
		Questions: []question.Question{{
			Header: "Framework", Question: "Which framework should be used?",
			Options: []question.Option{
				{Label: "React", Description: "Component ecosystem"},
				{Label: "Vue", Description: "Progressive framework"},
			},
		}},
	})
	require.NoError(t, err)

	return request
}

func testTwoQuestionRequest(t *testing.T) question.Request {
	t.Helper()

	request, err := question.NewRequest("q-two", "call-two", question.Spec{
		Questions: []question.Question{
			{
				Header: "Framework", Question: "Which framework should be used?",
				Options: []question.Option{
					{Label: "React", Description: "Component ecosystem"},
					{Label: "Vue", Description: "Progressive framework"},
				},
			},
			{
				Header: "Database", Question: "Which database should store the data?",
				Options: []question.Option{
					{Label: "SQLite", Description: "Embedded storage"},
					{Label: "Postgres", Description: "Server database"},
				},
			},
		},
	})
	require.NoError(t, err)

	return request
}

func questionPromptStateSnapshot(request question.Request) coding.State {
	state := readyState()
	state.Phase = coding.PhasePaused
	state.Interaction = coding.InteractionState{
		ID: "interaction-1", Active: true, Mode: coding.ModeAgent,
	}
	state.Question.Required = &request

	return state
}

type questionRejection struct {
	requestID    string
	schemaDigest string
}

type questionPromptController struct {
	stubController
	resolutions []question.Resolution
	rejections  []questionRejection
}

func (c *questionPromptController) ResolveQuestion(
	_ context.Context,
	resolution question.Resolution,
) iter.Seq2[coding.Event, error] {
	c.resolutions = append(c.resolutions, question.CloneResolution(resolution))
	c.finishQuestion()

	return emptyEventSequence()
}

func (c *questionPromptController) RejectQuestion(
	_ context.Context,
	requestID string,
	schemaDigest string,
) iter.Seq2[coding.Event, error] {
	c.rejections = append(c.rejections, questionRejection{
		requestID: requestID, schemaDigest: schemaDigest,
	})
	c.finishQuestion()

	return emptyEventSequence()
}

func (c *questionPromptController) finishQuestion() {
	c.state.Question.Required = nil
	c.state.Phase = coding.PhaseIdle
	c.state.Interaction.Active = false
}

func emptyEventSequence() iter.Seq2[coding.Event, error] {
	return func(func(coding.Event, error) bool) {}
}

type modePromptController struct {
	stubController
	overridden bool
}

func (c *modePromptController) Mode() runtimecontrol.ModeState {
	return runtimecontrol.ModeState{
		Current: c.state.Mode, Configured: coding.ModeAgent, Overridden: c.overridden,
	}
}

func (c *modePromptController) SetMode(_ context.Context, mode coding.OperatingMode) error {
	c.state.Mode = mode
	c.state.Sequence++
	c.overridden = mode != coding.ModeAgent

	return nil
}
