package tui

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/attachment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComposerInsertPasteThresholds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		content   string
		collapsed bool
	}{
		{name: "one thousand characters", content: strings.Repeat("界", 1_000)},
		{name: "one thousand and one characters", content: strings.Repeat("界", 1_001), collapsed: true},
		{name: "eight lines", content: strings.Repeat("line\n", 7) + "line"},
		{name: "nine lines", content: strings.Repeat("line\r\n", 8) + "line", collapsed: true},
		{name: "nine lines with carriage returns", content: strings.Repeat("line\r", 8) + "line", collapsed: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			composer := newTestComposer()
			collapsed, err := composer.InsertPaste(test.content)
			require.NoError(t, err)
			assert.Equal(t, test.collapsed, collapsed)

			assembled, err := composer.Assemble()
			require.NoError(t, err)
			assert.Equal(t, test.content, assembled)
			if test.collapsed {
				assert.Len(t, composer.elements, 1)
				assert.NotEqual(t, test.content, composer.Value())
			} else {
				assert.Empty(t, composer.elements)
				assert.Equal(t, test.content, composer.Value())
			}
		})
	}
}

func TestComposerPasteExpansionIsLossless(t *testing.T) {
	t.Parallel()

	content := "  leading\r\n" + strings.Repeat("中间\n", 9) + "trailing  \n"
	composer := newTestComposer()
	composer.InsertString("before\n")
	collapsed, err := composer.InsertPaste(content)
	require.NoError(t, err)
	require.True(t, collapsed)
	composer.InsertString("\nafter")

	assembled, err := composer.Assemble()
	require.NoError(t, err)
	assert.Equal(t, "before\n"+content+"\nafter", assembled)
}

func TestComposerPasteLabelSkipsLiteralCollision(t *testing.T) {
	t.Parallel()

	content := strings.Repeat("x", 1_001)
	composer := newTestComposer()
	composer.SetValue("[Pasted text #1 · 1001 chars · 1 lines]")
	composer.CursorEnd()

	collapsed, err := composer.InsertPaste(content)
	require.NoError(t, err)
	require.True(t, collapsed)
	require.Len(t, composer.elements, 1)
	assert.Contains(t, composer.elements[0].label, "#2")

	assembled, err := composer.Assemble()
	require.NoError(t, err)
	assert.Equal(t, "[Pasted text #1 · 1001 chars · 1 lines]"+content, assembled)
}

func TestComposerInsertPasteRejectsMalformedAndOversizedInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
	}{
		{name: "invalid utf8", content: string([]byte{0xff})},
		{name: "nul", content: strings.Repeat("x", 1_001) + "\x00"},
		{name: "control", content: strings.Repeat("x", 1_001) + "\x01"},
		{name: "over prompt limit", content: strings.Repeat("x", coding.MaxPromptTextBytes+1)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			composer := newTestComposer()
			before := composer.Snapshot()
			collapsed, err := composer.InsertPaste(test.content)
			require.Error(t, err)
			assert.False(t, collapsed)
			assert.Equal(t, before, composer.Snapshot())
		})
	}
}

func TestComposerProtectedPasteIsAtomic(t *testing.T) {
	t.Parallel()

	composer := newTestComposer()
	_, err := composer.InsertPaste(strings.Repeat("payload", 200))
	require.NoError(t, err)
	require.Len(t, composer.elements, 1)
	label := composer.elements[0].label

	composer, _ = composer.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	assert.Empty(t, composer.Value())
	assert.Empty(t, composer.elements)

	_, err = composer.InsertPaste(strings.Repeat("payload", 200))
	require.NoError(t, err)
	activeLabel := composer.elements[0].label
	composer.SetCursorColumn(3)
	composer, _ = composer.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	assert.NotContains(
		t,
		[]int{1, 2, 3},
		composerCursorByte(composer.Value(), composer.Line(), composer.Column()),
	)

	composer.Model.SetCursorColumn(3)
	composer, _ = composer.Update(tea.KeyPressMsg{Text: "x"})
	assert.Equal(t, 1, strings.Count(composer.Value(), activeLabel))
	assert.Equal(t, activeLabel, composer.elements[0].label)
	assert.Contains(t, composer.Value(), label[:min(len(label), 8)])
}

func TestComposerHistoryNavigationAndLiveDraft(t *testing.T) {
	t.Parallel()

	composer := newTestComposer()
	for _, value := range []string{"first", "second", "second", "third"} {
		composer.SetValue(value)
		require.NoError(t, composer.RecordHistory(composer.Snapshot()))
	}
	require.Len(t, composer.history, 3)

	composer.SetValue("live draft")
	require.True(t, composer.HistoryUp())
	assert.Equal(t, "third", composer.Value())
	require.True(t, composer.HistoryUp())
	assert.Equal(t, "second", composer.Value())
	require.True(t, composer.HistoryDown())
	assert.Equal(t, "third", composer.Value())
	require.True(t, composer.HistoryDown())
	assert.Equal(t, "live draft", composer.Value())
	assert.False(t, composer.HistoryDown())

	require.True(t, composer.HistoryUp())
	composer, _ = composer.Update(tea.KeyPressMsg{Text: "!"})
	assert.False(t, composer.isBrowsingHistory)
	assert.Equal(t, "third!", composer.Value())
}

func TestComposerHistoryEvictsByCountAndPayloadBudget(t *testing.T) {
	t.Parallel()

	composer := newTestComposer()
	for index := range maximumComposerHistoryEntries + 1 {
		composer.SetValue(strings.Repeat("x", index+1))
		require.NoError(t, composer.RecordHistory(composer.Snapshot()))
	}
	assert.Len(t, composer.history, maximumComposerHistoryEntries)
	assert.Equal(t, "xx", composer.history[0].display)

	composer = newTestComposer()
	payload := strings.Repeat("界", (coding.MaxPromptTextBytes-8)/utf8.RuneLen('界'))
	for index := range 17 {
		composer.Reset()
		_, err := composer.InsertPaste(payload + string(rune('a'+index)))
		require.NoError(t, err)
		require.NoError(t, composer.RecordHistory(composer.Snapshot()))
	}
	assert.LessOrEqual(t, composer.historyBytes, maximumComposerHistoryBytes)
	assert.Less(t, len(composer.history), 17)
}

func TestReadyComposerCollapsesPasteAndSubmitsExactPayload(t *testing.T) {
	t.Parallel()

	controller := &interactionController{stubController: stubController{state: readyState()}}
	model := readyModelWithController(t, controller, true)
	content := "  leading\r\n" + strings.Repeat("middle\n", 9) + "trailing  \n"

	model.Update(tea.PasteMsg{Content: content})
	require.Len(t, model.composer.elements, 1)
	assert.Contains(t, model.composer.Value(), "[Pasted text #1")
	assert.NotContains(t, model.composer.Value(), "middle")

	_, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, command)
	driveModelCommands(t, model, command)
	require.Len(t, controller.prompts, 1)
	require.Len(t, controller.prompts[0].Parts, 1)
	part, ok := controller.prompts[0].Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Equal(t, content, part.Text)
	assert.Empty(t, model.composer.Value())
	assert.Len(t, model.composer.history, 1)
}

func TestReadyComposerRejectsBadPasteWithoutDraftLoss(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.composer.SetValue("keep this")
	before := model.composer.Snapshot()

	model.Update(tea.PasteMsg{Content: strings.Repeat("x", 1_001) + "\x00"})
	assert.Equal(t, before, model.composer.Snapshot())
	require.ErrorIs(t, model.streamErr, errComposerInvalidPaste)
}

func TestReadyComposerHistoryRespectsMultilineBoundaries(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.composer.SetValue("remembered")
	require.NoError(t, model.composer.RecordHistory(model.composer.Snapshot()))
	model.composer.SetValue("first\nsecond")
	require.Equal(t, 1, model.composer.Line())

	model.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	assert.Equal(t, "first\nsecond", model.composer.Value())
	assert.Equal(t, 0, model.composer.Line())

	model.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	assert.Equal(t, "remembered", model.composer.Value())
	model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	assert.Equal(t, "first\nsecond", model.composer.Value())
}

func TestReadyComposerHistoryRespectsWrappedRowBoundary(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.composer.SetValue("remembered")
	require.NoError(t, model.composer.RecordHistory(model.composer.Snapshot()))
	model.composer.SetWidth(8)
	live := strings.Repeat("wrapped ", 8)
	model.composer.SetValue(live)
	model.composer.CursorEnd()
	require.Greater(t, model.composer.LineInfo().Height, 1)

	for steps := 0; !model.composer.AtFirstVisualRow(); steps++ {
		require.Less(t, steps, 100)
		model.Update(tea.KeyPressMsg{Code: tea.KeyUp})
		assert.Equal(t, live, model.composer.Value())
	}
	model.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	assert.Equal(t, "remembered", model.composer.Value())
}

func TestReadyPasteFollowsRouteInputPrecedence(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.activateSessionPicker("draft")
	model.Update(tea.PasteMsg{Content: "session query"})

	assert.Equal(t, "session query", model.route.search.Value())
	assert.Empty(t, model.composer.Value())
}

func TestReadyQueueFailureRestoresProtectedSnapshot(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Phase = coding.PhaseRunning
	state.Interaction.Active = true
	controller := &rejectingQueueController{
		interactionController: interactionController{
			stubController: stubController{state: state},
		},
		err: errors.New("queue refused"),
	}
	model := readyModelWithController(t, controller, true)
	content := strings.Repeat("payload\n", 9)
	model.Update(tea.PasteMsg{Content: content})
	before := model.composer.Snapshot()

	_, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, command)
	model.Update(command())
	assert.Equal(t, before, model.composer.Snapshot())
	assert.ErrorIs(t, model.streamErr, controller.err)
	assert.Empty(t, model.composer.history)
}

func TestReadyTemporaryInputSurfacesRestoreProtectedSnapshot(t *testing.T) {
	t.Parallel()

	model := readyModelWithController(t, newOverlayController(readyState()), true)
	_, err := model.composer.InsertPaste(strings.Repeat("payload\n", 9))
	require.NoError(t, err)
	model.composer.InsertString(" @notes")
	fileStart := strings.LastIndex(model.composer.Value(), "@notes")
	require.NoError(t, model.composer.InsertFile(
		fileStart,
		fileStart+len("@notes"),
		attachment.Reference{Path: "notes.txt", Kind: attachment.KindText},
	))
	model.composer.InsertString(" ")
	require.NoError(t, model.composer.InsertImage(
		normalizedComposerImage(t, clipboardImageName, 0),
	))
	original := model.composer.Snapshot()

	model.openCommandPicker()
	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, original, model.composer.Snapshot())

	model.activateSessionPickerSnapshot(original)
	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, original, model.composer.Snapshot())

	model.activateSkillsRouteSnapshot(original)
	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, original, model.composer.Snapshot())

	model.openAgentsRoute()
	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, original, model.composer.Snapshot())

	model.openTeamRoute("")
	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, original, model.composer.Snapshot())

	model.openTreeRoute(true)
	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, original, model.composer.Snapshot())

	detail := newToolDetailView(timelineBlock{kind: blockTool, tools: []toolActivity{{
		id: "tool-1", name: "read", class: toolClassExplore,
	}}})
	model.openToolDetailRoute(detail)
	require.Equal(t, routeToolDetail, model.route.kind)
	model.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	assert.Equal(t, original, model.composer.Snapshot())

	model.composer.CursorEnd()
	model.Update(tea.KeyPressMsg{Text: "$"})
	require.Equal(t, pickerSkill, model.picker.kind)
	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, original, model.composer.Snapshot())

	model.openModelPicker()
	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, original, model.composer.Snapshot())

	model.openCommandPicker()
	model.state.Approval = approvalReviewState().Approval
	model.syncApprovalPrompt()
	assert.Equal(t, original, model.composer.Snapshot())
	model.state.Approval = coding.ApprovalState{}
	model.syncApprovalPrompt()

	model.openCommandPicker()

	questionState := questionPromptStateSnapshot(testQuestionRequest(t))
	model.state.Question = questionState.Question
	model.syncApprovalPrompt()
	assert.Equal(t, original, model.composer.Snapshot())
}

type rejectingQueueController struct {
	interactionController
	err error
}

func (c *rejectingQueueController) Steer(messages ...ai.Message) error {
	c.steered = append(c.steered, messages...)

	return c.err
}

func newTestComposer() composerState {
	editor := textarea.New()
	editor.SetVirtualCursor(false)
	editor.SetWidth(80)
	editor.MaxContentHeight = 200
	editor.Focus()

	return newComposerState(editor)
}
