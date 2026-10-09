package tui

import (
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dragAcross drags from the first marker's row to the second one's and returns
// the release command, so every guard below starts from the same real gesture.
func dragAcross(t *testing.T, model *Model, from, to string) tea.Cmd {
	t.Helper()

	first := paintedRow(t, model, from)
	second := paintedRow(t, model, to)
	require.Less(t, first, second)

	model.handleMouse(tea.MouseClickMsg{X: 0, Y: first, Button: tea.MouseLeft})
	model.handleMouse(tea.MouseMotionMsg{X: 40, Y: second, Button: tea.MouseLeft})

	command := model.handleMouse(tea.MouseReleaseMsg{X: 40, Y: second, Button: tea.MouseLeft})
	require.NotNil(t, command)

	return command
}

// TestDragSelectionNeverReachesTheTextSaver is the load-bearing half of the
// auto-copy contract: a spy saver is installed and the drag must not call it,
// while the clipboard write must be requested.
func TestDragSelectionNeverReachesTheTextSaver(t *testing.T) {
	t.Parallel()

	model, calls := selectionModel(t, true, "MARKER-A", "MARKER-B")

	written := &[]string{}
	model.options.ClipboardWriter = func(content string) error {
		*written = append(*written, content)

		return nil
	}

	messages := runCommandTree(t, dragAcross(t, model, "MARKER-A", "MARKER-B"))

	assert.Empty(t, *calls, "the drag path never reaches the text saver")
	assert.Equal(t, []string{"❯ MARKER-A\n\n❯ MARKER-B"}, *written,
		"the injected clipboard writer is the one asked to write")
	assert.Empty(t, clipboardTexts(messages),
		"an injected writer replaces the Program's OSC 52 request")
	assert.Empty(t, model.copiedNotice, "no confirmation before the outcome arrives")
}

// TestDragSelectionClearsTheHighlightOnASuccessfulCopy pins the visible half: the
// confirmation clears the selection, so no highlight is left behind once the text
// is on the clipboard.
func TestDragSelectionClearsTheHighlightOnASuccessfulCopy(t *testing.T) {
	t.Parallel()

	model, _ := selectionModel(t, false, "MARKER-A", "MARKER-B")
	command := dragAcross(t, model, "MARKER-A", "MARKER-B")
	require.True(t, model.selection.visible)

	messages := runCommandTree(t, command)
	result, ok := findClipboardResult(messages)
	require.True(t, ok)
	require.NoError(t, result.err)

	_, notice := model.Update(result)
	assert.NotNil(t, notice)
	assert.False(t, model.selection.visible, "a confirmed copy leaves no highlight")
	assert.Equal(t, "copied 3 lines", model.copiedNotice)
	assert.Contains(t, ansi.Strip(model.reservedBand()), "copied 3 lines")
	assert.NotContains(t, ansi.Strip(model.View().Content), "\x1b[7m",
		"the painted frame carries no selection highlight")
}

// TestDragSelectionKeepsTheHighlightWhenTheCopyFails pins the other half: a copy
// that could not be made leaves the addressed text standing and says so.
func TestDragSelectionKeepsTheHighlightWhenTheCopyFails(t *testing.T) {
	t.Parallel()

	model, calls := selectionModel(t, false, "MARKER-A", "MARKER-B")
	model.options.ClipboardWriter = func(string) error { return errors.New("clipboard unavailable") }

	messages := runCommandTree(t, dragAcross(t, model, "MARKER-A", "MARKER-B"))
	result, ok := findClipboardResult(messages)
	require.True(t, ok)
	require.Error(t, result.err)
	assert.Empty(t, *calls)

	_, notice := model.Update(result)
	assert.NotNil(t, notice)
	assert.True(t, model.selection.visible, "a failed copy keeps what it addressed")
	assert.Empty(t, model.copiedNotice, "a failed copy shows no confirmation")
	assert.True(t, model.statusNoticeErr)
	assert.Contains(t, ansi.Strip(model.statusLine()), "could not copy")
}

// TestDragSelectionWithNothingAddressedKeepsTheSelection covers the other failure
// the gesture can report: a drag that covered only the renderer's padding. The
// notice is set before the command returns, so there is no timer to wait out.
func TestDragSelectionWithNothingAddressedKeepsTheSelection(t *testing.T) {
	t.Parallel()

	model, calls := selectionModel(t, true, "MARKER-A")
	row := paintedRow(t, model, "MARKER-A")

	model.handleMouse(tea.MouseClickMsg{X: 38, Y: row, Button: tea.MouseLeft})
	model.handleMouse(tea.MouseMotionMsg{X: 39, Y: row, Button: tea.MouseLeft})
	command := model.handleMouse(tea.MouseReleaseMsg{X: 39, Y: row, Button: tea.MouseLeft})
	require.NotNil(t, command)

	assert.Empty(t, *calls)
	assert.True(t, model.selection.visible)
	assert.True(t, model.statusNoticeErr)
	assert.Contains(t, ansi.Strip(model.statusLine()), "nothing selected to copy")
	assert.Empty(t, model.copiedNotice)
}

// TestExplicitCopiesStillReachTheTextSaver is the regression guard for the paths
// the drag gesture must not change: /copy, a path copy and /export keep their
// saver, and a bare /copy keeps the Program's own clipboard request.
func TestExplicitCopiesStillReachTheTextSaver(t *testing.T) {
	t.Parallel()

	model, calls := copyModel(t, ai.AssistantText("REPLY"))

	assert.Equal(t, []string{"REPLY"}, clipboardTexts(runCommandTree(t, model.copyAssistant(1, ""))))
	assert.Empty(t, clipboardTexts(runCommandTree(t, model.copyAssistant(1, "/out/reply.txt"))),
		"an explicit destination is the whole delivery")
	assert.Empty(t, clipboardTexts(runCommandTree(t, model.exportConversation(""))))

	require.Len(t, *calls, 3)
	assert.Equal(t, TextKindCopy, (*calls)[0].Kind)
	assert.Empty(t, (*calls)[0].Path, "a bare /copy asks the saver for its default destination")
	assert.Equal(t, "/out/reply.txt", (*calls)[1].Path)
	assert.Equal(t, TextKindExport, (*calls)[2].Kind)
}

// TestInjectedClipboardWriterReplacesEveryRequest pins the option's scope: a
// caller that owns another route to the clipboard replaces the OSC 52 request for
// every copy, not only for the drag gesture, so the two cannot disagree about
// where the text went.
func TestInjectedClipboardWriterReplacesEveryRequest(t *testing.T) {
	t.Parallel()

	model, _ := copyModel(t, ai.AssistantText("REPLY"))

	written := &[]string{}
	model.options.ClipboardWriter = func(content string) error {
		*written = append(*written, content)

		return nil
	}

	messages := runCommandTree(t, model.copyAssistant(1, ""))
	assert.Empty(t, clipboardTexts(messages), "the injected writer replaces the OSC 52 request")
	assert.Equal(t, []string{"REPLY"}, *written, "/copy uses the caller's route too")

	// A failing writer is reported for the explicit path as well, and leaves the
	// drag gesture's own state alone.
	model.options.ClipboardWriter = func(string) error { return errors.New("clipboard unavailable") }
	messages = runCommandTree(t, model.copyAssistant(1, ""))
	result, ok := findClipboardResult(messages)
	require.True(t, ok)
	require.Error(t, result.err)
	assert.False(t, result.selection, "an explicit copy is not the drag gesture")
	_, command := model.Update(result)
	assert.NotNil(t, command)
	assert.Contains(t, ansi.Strip(model.statusLine()), "could not copy")
}

// TestCopiedNoticeExpiresOnItsOwn keeps the band's confirmation transient and
// independent of the status line's notice.
func TestCopiedNoticeExpiresOnItsOwn(t *testing.T) {
	t.Parallel()

	model, _ := selectionModel(t, true, "MARKER-A", "MARKER-B")
	messages := runCommandTree(t, dragAcross(t, model, "MARKER-A", "MARKER-B"))
	result, ok := findClipboardResult(messages)
	require.True(t, ok)

	_, expiry := model.Update(result)
	require.NotNil(t, expiry)
	require.Equal(t, "copied 3 lines", model.copiedNotice)

	model.Update(copiedNoticeExpiredMsg{generation: model.copiedNoticeSeq})
	assert.Empty(t, model.copiedNotice)
	assert.Empty(t, ansi.Strip(model.reservedBand()))
}
