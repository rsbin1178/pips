//nolint:wsl_v5 // Test transitions and assertions are deliberately adjacent.
package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartupSizeBootstrapRendezvous(t *testing.T) {
	t.Parallel()
	for _, sizeFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("sizeFirst=%v", sizeFirst), func(t *testing.T) {
			t.Parallel()
			state := readyState()
			state.Transcript = []ai.Message{ai.UserText("STARTUP-HISTORY")}
			controller := stubController{state: state}
			model := newModel(t.Context(), Options{NoColor: true})
			_, invalid := model.Update(tea.WindowSizeMsg{})
			require.Nil(t, invalid)
			var command tea.Cmd
			if sizeFirst {
				_, command = model.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
				require.Nil(t, command)
				_, command = model.Update(bootstrapResult{controller: controller})
			} else {
				_, command = model.Update(bootstrapResult{controller: controller})
				require.Nil(t, command)
				assert.Zero(t, model.scrollback.messages)
				assert.False(t, model.bannerPrinted)
				assert.NotContains(t, model.View().Content, "STARTUP-HISTORY")
				_, command = model.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
			}
			printed := driveModelCommandsCapture(t, model, command)
			assert.Equal(t, 1, strings.Count(printed, "✻ Pips"))
			assert.Equal(t, 1, strings.Count(printed, "STARTUP-HISTORY"))
			for _, size := range []tea.WindowSizeMsg{{Width: 80, Height: 24}, {}, {Width: 40, Height: 10}} {
				_, command = model.Update(size)
				assert.Empty(t, modelCommandOutput(model, command))
			}
		})
	}
}

func TestScrollbackFIFOExactCompletionAndContinuation(t *testing.T) {
	t.Parallel()
	model := readyModel(t, true)
	model.scrollbackOutput = false
	first := model.printScrollback("FIRST")
	firstID := model.presentation.writeSequence
	second := model.printScrollback("SECOND")
	secondID := model.presentation.writeSequence
	third := model.printScrollback("THIRD")
	thirdID := model.presentation.writeSequence
	require.Nil(t, second)
	require.Nil(t, third)
	var continued atomic.Bool
	model.afterScrollback(nil, func() tea.Msg { continued.Store(true); return nil })
	model.openToolDetailRoute(toolDetailView{title: "route"})
	assert.Equal(t, routeNone, model.route.kind)
	assert.False(t, continued.Load())
	// Neither a queued-but-undispatched ID nor an unknown one may retire head.
	assert.Nil(t, model.finishScrollbackWrite(secondID))
	assert.Nil(t, model.finishScrollbackWrite(thirdID+1))
	require.Len(t, model.presentation.writes, 3)
	assert.Equal(t, "FIRST", commandOutput(first))
	next := model.finishScrollbackWrite(firstID)
	assert.Nil(t, model.finishScrollbackWrite(firstID))
	assert.Equal(t, routeNone, model.route.kind)
	printed := modelCommandOutput(model, next)
	assert.Equal(t, "\nSECOND\n\nTHIRD", printed)
	assert.True(t, continued.Load())
	assert.Equal(t, routeToolDetail, model.route.kind)
	assert.Empty(t, model.presentation.writes)
	assert.Nil(t, model.finishScrollbackWrite(thirdID))
}

func TestScrollbackReplacementPreservesOutputAndRestartsSubscription(t *testing.T) {
	t.Parallel()
	for _, operation := range []controlOperation{operationResume, operationNew, operationFork} {
		t.Run(string(operation), func(t *testing.T) {
			t.Parallel()
			state := readyState()
			state.Transcript = []ai.Message{ai.UserText("NEW-SESSION-HISTORY")}
			model := readyModelWithController(t, stubController{state: state}, true)
			first := model.printScrollback("OLD-SESSION-ACCEPTED")
			oldID := model.presentation.writeSequence
			var stale atomic.Bool
			model.afterScrollback(nil, func() tea.Msg { stale.Store(true); return nil })
			_, command := model.Update(controlResultMsg{operation: operation})
			require.Nil(t, command, "replacement must queue behind the delayed first write")
			require.Len(t, model.presentation.writes, 2)
			assert.Greater(t, model.presentation.writeSequence, oldID)
			printed := driveModelCommandsCapture(t, model, first)
			assert.Less(t, strings.Index(printed, "OLD-SESSION-ACCEPTED"), strings.Index(printed, "NEW-SESSION-HISTORY"))
			assert.False(t, stale.Load(), "old subscription continuation was invalidated")
			assert.Empty(t, model.presentation.writes)
		})
	}
}

// This promotes the research reproduction to a before-Quit screen assertion.
// Prepare at 80x24, execute at 40x10, with a delayed first logical write and a
// queued second write. No frame timer, fake input, or closing repaint is used.
func TestScrollbackPreparedResizeScreen(t *testing.T) {
	t.Parallel()
	for _, fps := range []int{1, 30, 60, 120} {
		for _, noColor := range []bool{false, true} {
			t.Run(fmt.Sprintf("fps%d/noColor=%v", fps, noColor), func(t *testing.T) {
				t.Parallel()
				model := readyModel(t, noColor)
				model.composer.Focus()
				model.scrollbackOutput = false
				lines := historyMarkers(40)
				first := model.printScrollback(strings.Join(lines, "\n"))
				require.Nil(t, model.printScrollback("SECOND-TRANSACTION"))
				model.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
				probe := &stableScreenProbe{Model: model, initial: first}
				capture := runStableScreenProgram(t, probe, fps)
				e := replayStableScreen(t, capture, 40, 10)
				assertStableScreen(t, e, append(lines, "SECOND-TRANSACTION"))
			})
		}
	}
}

func TestLongResumeScreenWithoutRepairInput(t *testing.T) {
	t.Parallel()
	for _, fps := range []int{1, 60} {
		t.Run(fmt.Sprintf("fps%d", fps), func(t *testing.T) {
			t.Parallel()
			state := readyState()
			for _, marker := range historyMarkers(40) {
				state.Transcript = append(state.Transcript, ai.UserText(marker))
			}
			model := readyModelWithController(t, stubController{state: state}, true)
			model.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
			model.route = newSessionPickerState("", model.theme, true)
			model.route.controlling = true
			model.route.loading = true
			model.setLayout()
			probe := &stableScreenProbe{Model: model, initial: func() tea.Msg {
				return controlResultMsg{operation: operationResume}
			}}
			capture := runStableScreenProgram(t, probe, fps)
			assertStableScreen(t, replayStableScreen(t, capture, 40, 10), historyMarkers(40))
		})
	}
}

type stableScreenProbe struct {
	*Model
	initial tea.Cmd
	done    atomic.Bool
}

func (p *stableScreenProbe) Init() tea.Cmd { return p.initial }

func (p *stableScreenProbe) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	_, command := p.Model.Update(message)
	if _, ok := message.(scrollbackWriteDoneMsg); ok && !p.presentation.hasScrollbackWrites() {
		p.done.Store(true)
	}
	return p, command
}

func historyMarkers(count int) []string {
	lines := make([]string, count)
	for index := range lines {
		lines[index] = fmt.Sprintf("HISTORY-%03d", index)
	}
	return lines
}

func runStableScreenProgram(t *testing.T, probe *stableScreenProbe, fps int) string {
	t.Helper()
	var output synchronizedBuffer
	program := tea.NewProgram(probe, tea.WithInput(nil), tea.WithOutput(&output),
		tea.WithEnvironment([]string{"TERM=xterm-256color", "TERM_PROGRAM=Apple_Terminal"}),
		tea.WithWindowSize(40, 10), tea.WithFPS(fps), tea.WithoutSignalHandler())
	finished := make(chan error, 1)
	go func() { _, err := program.Run(); finished <- err }()
	t.Cleanup(program.Kill)
	require.Eventually(t, probe.done.Load, 30*time.Second, 10*time.Millisecond)
	capture := output.String() // completion is after synchronous renderer restoration
	program.Quit()
	require.NoError(t, <-finished)
	if directory := os.Getenv("PIPS_TUI_CAPTURE_DIR"); directory != "" {
		require.NoError(t, os.MkdirAll(directory, 0o700)) //nolint:gosec // Test operator explicitly selects the capture directory.
		name := strings.ReplaceAll(t.Name(), "/", "_") + ".ansi"
		require.NoError(t, os.WriteFile(filepath.Join(directory, name), []byte(capture), 0o600)) //nolint:gosec // Synthetic replay artifact in the operator-selected directory.
	}
	return capture
}

func replayStableScreen(t *testing.T, capture string, width, height int) *vt.Emulator {
	t.Helper()
	e := newScreenReplay(width, height)
	t.Cleanup(func() { _ = e.Close() })
	_, err := e.Write([]byte("SHELL-HISTORY\r\n" + capture))
	require.NoError(t, err)
	assert.NotContains(t, capture, "\x1b[3J")
	assert.False(t, e.IsAltScreen())
	return e
}

func stableScreenHistory(e *vt.Emulator) string {
	var history strings.Builder
	for _, line := range e.Scrollback().Lines() {
		for _, cell := range line {
			history.WriteString(cell.Content)
		}
		history.WriteByte('\n')
	}
	history.WriteString(e.String())
	return history.String()
}

func assertStableScreen(t *testing.T, e *vt.Emulator, markers []string) {
	t.Helper()
	history := stableScreenHistory(e)
	previous := -1
	for _, marker := range markers {
		assert.Equal(t, 1, strings.Count(history, marker), "marker %s in %q", marker, history)
		at := strings.Index(history, marker)
		assert.Greater(t, at, previous, "marker order %s", marker)
		previous = at
	}
	assert.Contains(t, history, "SHELL-HISTORY")
	screen := ansi.Strip(e.String())
	assert.Contains(t, screen, "│ ❯", "bordered composer before Quit")
	assert.Contains(t, screen, "idle", "status before Quit")
	position := e.CursorPosition()
	rows := strings.Split(screen, "\n")
	require.Less(t, position.Y, len(rows))
	assert.Contains(t, rows[position.Y], "│ ❯", "cursor must be on the editor")
	arrow := strings.Index(rows[position.Y], "❯")
	require.GreaterOrEqual(t, arrow, 0)
	assert.Equal(t, ansi.StringWidth(rows[position.Y][:arrow])+2, position.X)
}
