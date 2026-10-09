//nolint:wsl_v5 // Each guard drives the indicator and states its outcome next to it.
package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// turnIndicatorModel is a fullscreen viewport holding three long turns, so the
// indicator has several turns to walk back through.
func turnIndicatorModel(t *testing.T) *Model {
	t.Helper()

	state := readyState()
	state.Transcript = []ai.Message{
		ai.UserText("TURN-ONE"), ai.AssistantText(strings.Repeat("one answer\n\n", 12)),
		ai.UserText("TURN-TWO"), ai.AssistantText(strings.Repeat("two answer\n\n", 12)),
		ai.UserText("TURN-THREE"), ai.AssistantText(strings.Repeat("three answer\n\n", 12)),
	}

	model := fullscreenModel(t, stubController{state: state}, true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	model.rerenderTranscript(true)
	_ = model.View()

	return model
}

// turnIndicatorClick clicks the arrow's own cell and returns its command.
func turnIndicatorClick(t *testing.T, model *Model) tea.Cmd {
	t.Helper()

	_, left, _, shown := model.turnIndicator()
	require.True(t, shown, "the arrow has to be on the frame")

	command := model.handleMouse(tea.MouseClickMsg{X: left, Y: 0, Button: tea.MouseLeft})
	require.NotNil(t, command)

	return command
}

// TestTurnIndicatorShowsWhileAnythingIsAbove pins the marker's reach: it reports that
// the conversation continues above the window rather than that one particular turn is
// cut off, so it is there for every turn and only leaves at the beginning.
func TestTurnIndicatorShowsWhileAnythingIsAbove(t *testing.T) {
	t.Parallel()

	model := turnIndicatorModel(t)
	require.Positive(t, model.transcriptScroll.offset, "the fixture is at the bottom of a long conversation")
	require.True(t, model.turnIndicatorShown())
	require.True(t, model.frameHit.turnIndicator, "the frame recorded the indicator")

	rows := strings.Split(model.View().Content, "\n")
	_, left, right, shown := model.turnIndicator()
	require.True(t, shown)
	cell := ansi.Strip(ansi.TruncateLeft(rows[0], left, ""))
	assert.True(t, strings.HasPrefix(cell, turnIndicatorIcon),
		"the indicator is centred in the first row, found %q", cell)
	assert.Equal(t, model.width, ansi.StringWidth(rows[0]), "the row keeps the frame width")
	assert.Equal(t, 1, right-left)

	// The beginning of the conversation is the one place it leaves.
	model.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	require.Equal(t, 0, model.transcriptScroll.offset)
	assert.False(t, model.turnIndicatorShown())
	_ = model.View()
	assert.False(t, model.frameHit.turnIndicator)
	assert.NotContains(t, strings.Split(model.View().Content, "\n")[0], turnIndicatorIcon)
}

// TestTurnIndicatorWalksBackOneTurnAtATime pins the pointer half: each click puts the
// newest prompt above the window at its top, so repeated clicks walk back through the
// conversation, and the arrow stays until the window reaches the beginning.
func TestTurnIndicatorWalksBackOneTurnAtATime(t *testing.T) {
	t.Parallel()

	model := turnIndicatorModel(t)

	for _, want := range []string{"TURN-THREE", "TURN-TWO", "TURN-ONE"} {
		before := model.transcriptScroll.offset
		row, ok := model.transcript.rowOfPreviousUserTurn(before)
		require.True(t, ok, "a turn sits above offset %d", before)

		turnIndicatorClick(t, model)
		assert.Equal(t, row, model.transcriptScroll.offset, "the window moved to %s", want)
		assert.Contains(t, strings.Split(model.View().Content, "\n")[0], want,
			"the prompt the reader walked back to is the first row")

		_ = model.View()
		assert.True(t, model.frameHit.turnIndicator,
			"the arrow stays for the next turn the reader may walk back to")
	}

	// The last click reaches the beginning of the conversation, where there is no
	// user entry left to walk back to.
	row, ok := model.transcript.rowOfPreviousUserTurn(model.transcriptScroll.offset)
	assert.False(t, ok)
	turnIndicatorClick(t, model)
	assert.Equal(t, 0, row, "the fallback is the beginning of the conversation")
	require.Equal(t, 0, model.transcriptScroll.offset)

	_ = model.View()
	assert.False(t, model.frameHit.turnIndicator, "and there the arrow is gone")
	assert.False(t, model.turnIndicatorShown())
}

// TestTurnIndicatorIgnoresOtherCells pins the hit test: only the cells the frame
// painted the arrow in answer, so a click beside it is still the transcript's.
func TestTurnIndicatorIgnoresOtherCells(t *testing.T) {
	t.Parallel()

	model := turnIndicatorModel(t)
	_, left, right, shown := model.turnIndicator()
	require.True(t, shown)
	offset := model.transcriptScroll.offset

	for _, click := range []tea.MouseMsg{
		tea.MouseClickMsg{X: left - 1, Y: 0, Button: tea.MouseLeft},
		tea.MouseClickMsg{X: right, Y: 0, Button: tea.MouseLeft},
		tea.MouseClickMsg{X: left, Y: 1, Button: tea.MouseLeft},
		tea.MouseClickMsg{X: left, Y: 0, Button: tea.MouseRight},
	} {
		_, handled := model.turnIndicatorMouse(click)
		assert.False(t, handled, "%v is not the arrow's", click)
	}
	assert.Equal(t, offset, model.transcriptScroll.offset, "the window did not move")
}

// TestTurnIndicatorIsNotAnsweredWithoutTheFrame pins the discipline the band also
// follows: a click resolves against the frame the reader saw, so an arrow the last
// painted frame did not draw answers nothing.
func TestTurnIndicatorIsNotAnsweredWithoutTheFrame(t *testing.T) {
	t.Parallel()

	model := turnIndicatorModel(t)
	_, left, _, shown := model.turnIndicator()
	require.True(t, shown)

	// Compose a frame at the beginning, then click where the arrow used to be.
	model.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	_ = model.View()
	require.False(t, model.frameHit.turnIndicator)

	_, handled := model.turnIndicatorMouse(tea.MouseClickMsg{X: left, Y: 0, Button: tea.MouseLeft})
	assert.False(t, handled)
	assert.Equal(t, 0, model.transcriptScroll.offset, "nothing moved")
}

// TestTurnIndicatorNeedsSomethingAbove keeps the arrow off a conversation that fits
// the window, where there is nothing above to walk back to.
func TestTurnIndicatorNeedsSomethingAbove(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Transcript = []ai.Message{ai.UserText("ONLY"), ai.AssistantText("a short reply")}

	model := fullscreenModel(t, stubController{state: state}, true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	model.rerenderTranscript(true)
	_ = model.View()

	require.Equal(t, 0, model.transcriptScroll.offset)
	assert.False(t, model.turnIndicatorShown())
	assert.False(t, model.frameHit.turnIndicator)
}
