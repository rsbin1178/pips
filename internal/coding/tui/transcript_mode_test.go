//nolint:wsl_v5 // Transaction transitions and their screen assertions stay adjacent.
package tui

import (
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ctrlO() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl}
}

// TestTranscriptModeHandsTheConversationToTheTerminal is the AC11 core: entering
// the escape hatch leaves the alternate screen, releases the mouse, and writes
// every record of the managed conversation to the terminal exactly once.
func TestTranscriptModeHandsTheConversationToTheTerminal(t *testing.T) {
	t.Parallel()

	state := readyState()
	for index := range 25 {
		state.Transcript = append(state.Transcript, ai.UserText(fmt.Sprintf("ROW-%03d", index)))
	}
	model := fullscreenModel(t, stubController{state: state}, true)
	model.Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	model.rerenderTranscript(true)

	_, command := model.Update(ctrlO())
	require.True(t, model.transcriptMode.active, "the escape hatch opened")

	view := model.View()
	assert.False(t, view.AltScreen, "the terminal's main screen owns the frame")
	assert.Equal(t, tea.MouseModeNone, view.MouseMode, "the terminal gets the mouse back")
	assert.Contains(t, ansi.Strip(view.Content), "Esc returns")

	printed := driveModelCommandsCapture(t, model, command)
	previous := -1
	for index := range 25 {
		marker := fmt.Sprintf("ROW-%03d", index)
		assert.Equal(t, 1, strings.Count(printed, marker), "printed %q once in %q", marker, printed)

		position := strings.Index(printed, marker)
		require.Greater(t, position, previous, "records keep their order")
		previous = position
	}
}

// TestTranscriptPagesAreBounded pins the page contract: no single write exceeds
// transcriptPageRows, every row is handed over exactly once, and a page whose
// transaction ended is dropped.
func TestTranscriptPagesAreBounded(t *testing.T) {
	t.Parallel()

	model := fullscreenModel(t, stubController{state: readyState()}, true)
	rows := make([]string, transcriptPageRows*2+50)
	for index := range rows {
		rows[index] = fmt.Sprintf("ROW-%04d", index)
	}
	model.transcriptMode = transcriptModeState{active: true, sequence: 3, rows: rows}

	seen := 0
	for {
		command := model.transcriptPageCmd()
		if command == nil {
			break
		}

		body, ok := firstPrintBody(t, command)
		require.True(t, ok, "a page is a Println command")

		lines := strings.Split(body, "\n")
		assert.LessOrEqual(t, len(lines), transcriptPageRows)
		require.Equal(t, fmt.Sprintf("ROW-%04d", seen), lines[0], "pages are contiguous")
		seen += len(lines)
	}
	assert.Equal(t, len(rows), seen, "every row is written")

	model.transcriptMode = transcriptModeState{sequence: 4}
	assert.Nil(t, model.updateTranscriptPage(transcriptPageMsg{sequence: 3}),
		"a page from an abandoned transaction is dropped")
}

// TestTranscriptModeRestoresTheReadingPosition asserts that entering and leaving
// the escape hatch is a detour: the paused reader comes back to the same record,
// and following resumes when it was following.
func TestTranscriptModeRestoresTheReadingPosition(t *testing.T) {
	t.Parallel()

	state := readyState()
	for index := range 60 {
		state.Transcript = append(state.Transcript, ai.UserText(fmt.Sprintf("ROW-%03d", index)))
	}
	model := fullscreenModel(t, stubController{state: state}, true)
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	model.rerenderTranscript(true)

	model.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	model.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	require.False(t, model.transcriptScroll.follow, "scrolling up pauses following")
	anchorID := model.transcript.recordIDAt(model.transcriptScroll.offset)
	require.NotEmpty(t, anchorID)

	_, command := model.Update(ctrlO())
	require.True(t, model.transcriptMode.active)

	// New output and a resize arrive while the user reads the transcript.
	model.state.Transcript = append(model.state.Transcript, ai.UserText("ROW-LATE"))
	model.Update(tea.WindowSizeMsg{Width: 52, Height: 14})

	model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	assert.False(t, model.transcriptMode.active, "Esc returns to the viewport")
	assert.False(t, model.transcriptScroll.follow, "a paused reader stays paused")
	assert.Equal(t, anchorID, model.transcript.recordIDAt(model.transcriptScroll.offset))
	assert.True(t, model.View().AltScreen, "the viewport owns the alternate screen again")
	// The abandoned page must not resurrect the transaction.
	assert.Nil(t, model.updateTranscriptPage(transcriptPageMsg{}))
	_ = command

	// A following reader is put back at the bottom.
	model.transcriptScroll.gotoBottom()
	_, _ = model.Update(ctrlO())
	require.True(t, model.transcriptMode.active)
	model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	assert.True(t, model.transcriptScroll.follow)
	assert.Contains(t, ansi.Strip(model.View().Content), "ROW-LATE")
}

// TestTranscriptModeIsInertInline asserts the inline compatibility mode is
// untouched: the terminal already holds the conversation, so the binding is a
// no-op rather than a second copy of the tail.
func TestTranscriptModeIsInertInline(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	_, command := model.Update(ctrlO())
	assert.False(t, model.transcriptMode.active)
	assert.Nil(t, command)
	assert.False(t, model.View().AltScreen)
	assert.Contains(t, model.historyHelpLine(), "terminal owns conversation history",
		"inline help still names the terminal as the owner")
}

// TestHistoryHelpNamesTheOwningSurface keeps the help text honest about which
// surface scrolls the conversation.
func TestHistoryHelpNamesTheOwningSurface(t *testing.T) {
	t.Parallel()

	inline := readyModel(t, true)
	assert.Contains(t, inline.historyHelpLine(), "terminal owns conversation history")

	fullscreen := fullscreenModel(t, stubController{state: readyState()}, true)
	assert.Contains(t, fullscreen.historyHelpLine(), "Ctrl+O")
	assert.Contains(t, renderActionHelp(defaultActions, contextIdle), keyCtrlO+" — transcript",
		"the binding is discoverable in the help listing")
}

// TestTranscriptExitRowsOnlyForTheFullscreenTranscript pins which exits hand the
// conversation back to the terminal.
func TestTranscriptExitRowsOnlyForTheFullscreenTranscript(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Transcript = []ai.Message{ai.UserText("EXIT-ROW")}

	newFullscreen := func(t *testing.T, exitOutput string) *Model {
		t.Helper()

		model := fullscreenModel(t, stubController{state: state}, true)
		model.options.ExitOutput = exitOutput
		model.rerenderTranscript(true)
		_ = model.View()

		return model
	}

	t.Run("default exit leaves only the resume hint", func(t *testing.T) {
		t.Parallel()
		model := newFullscreen(t, "")
		assert.Equal(t, config.ExitOutputResumeHint, model.presentationSnapshot.exit)
		assert.Nil(t, model.transcriptExitRows())
	})

	t.Run("explicit transcript is handed over", func(t *testing.T) {
		t.Parallel()
		model := newFullscreen(t, config.ExitOutputTranscript)
		assert.Equal(t, config.ExitOutputTranscript, model.presentationSnapshot.exit)
		assert.Contains(t, strings.Join(model.transcriptExitRows(), "\n"), "EXIT-ROW")
	})

	t.Run("resume-hint leaves the screen alone", func(t *testing.T) {
		t.Parallel()
		model := newFullscreen(t, config.ExitOutputResumeHint)
		assert.Equal(t, config.ExitOutputResumeHint, model.presentationSnapshot.exit)
		assert.Nil(t, model.transcriptExitRows())
	})

	t.Run("inline is already handed over", func(t *testing.T) {
		t.Parallel()
		model := readyModel(t, true)
		_ = model.View()
		assert.Nil(t, model.transcriptExitRows())
	})

	t.Run("an open escape hatch already printed the rows", func(t *testing.T) {
		t.Parallel()
		model := newFullscreen(t, "")
		_, _ = model.Update(ctrlO())
		require.True(t, model.transcriptMode.active)
		assert.Nil(t, model.transcriptExitRows())
	})

	t.Run("a used escape hatch is not repeated on exit", func(t *testing.T) {
		t.Parallel()
		model := newFullscreen(t, "")
		_, _ = model.Update(ctrlO())
		require.True(t, model.transcriptMode.active)
		model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
		require.False(t, model.transcriptMode.active)
		assert.True(t, model.transcriptHanded)
		assert.Nil(t, model.transcriptExitRows())
	})
}

// transcriptScreenProbe enters the escape hatch from Init and reports when the
// paged write has finished, so the replay capture holds a settled frame.
type transcriptScreenProbe struct {
	*Model
	enter tea.Cmd
	done  atomic.Bool
}

func (p *transcriptScreenProbe) Init() tea.Cmd { return p.enter }

func (p *transcriptScreenProbe) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	_, command := p.Model.Update(message)
	if _, ok := message.(transcriptPageMsg); ok && !p.transcriptModePending() {
		p.done.Store(true)
	}

	return p, command
}

func (p *transcriptScreenProbe) finished() bool { return p.done.Load() }

// TestTranscriptModeScreenReplay drives the escape hatch through a real Program
// and asserts the running screen: the alternate screen is gone, the whole
// conversation sits in the terminal's own history exactly once and in order, and
// the live frame stays inside the window.
func TestTranscriptModeScreenReplay(t *testing.T) {
	t.Parallel()

	state := readyState()
	for _, marker := range historyMarkers(40) {
		state.Transcript = append(state.Transcript, ai.UserText(marker))
	}
	model := fullscreenModel(t, stubController{state: state}, true)
	model.rerenderTranscript(true)

	probe := &transcriptScreenProbe{Model: model, enter: func() tea.Msg { return ctrlO() }}
	capture := runScreenProgram(t, probe, 60)

	e := replayStableScreen(t, capture, 40, 10)
	assert.False(t, e.IsAltScreen(), "the escape hatch left the alternate screen")

	history := stableScreenHistory(e)
	previous := -1
	for _, marker := range historyMarkers(40) {
		assert.Equal(t, 1, strings.Count(history, marker), "marker %s in %q", marker, history)

		position := strings.Index(history, marker)
		require.Greater(t, position, previous, "markers keep their order")
		previous = position
	}

	frame := e.String()
	assert.Contains(t, ansi.Strip(frame), "Esc returns", "the way back stays visible")
	assert.LessOrEqual(t, len(strings.Split(frame, "\n")), 10, "the live frame fits the window")
}

// firstPrintBody unwraps a sequenced command to the body of its first Println.
func firstPrintBody(t *testing.T, command tea.Cmd) (string, bool) {
	t.Helper()

	message := command()
	value := reflect.ValueOf(message)
	if !value.IsValid() || value.Type().PkgPath() != "charm.land/bubbletea/v2" {
		return "", false
	}
	if value.Type().Name() == "sequenceMsg" && value.Len() > 0 {
		first, ok := value.Index(0).Interface().(tea.Cmd)
		require.True(t, ok)

		return firstPrintBody(t, first)
	}
	if value.Type().Name() == "printLineMessage" {
		return value.FieldByName("messageBody").String(), true
	}

	return "", false
}
