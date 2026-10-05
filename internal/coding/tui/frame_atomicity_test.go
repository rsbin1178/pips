//nolint:wsl_v5 // Capability injection and the balance assertions stay adjacent.
package tui

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// atomicFrameProbe is a Program model that reports when it has been rendered at
// least once, so the capability report is sent into a running program.
type atomicFrameProbe struct {
	*Model
	seen atomic.Bool
}

func (p *atomicFrameProbe) Init() tea.Cmd { return nil }

func (p *atomicFrameProbe) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	_, command := p.Model.Update(message)
	p.seen.Store(true)

	return p, command
}

func (p *atomicFrameProbe) finished() bool { return p.seen.Load() }

// TestFrameUpdatesUseSynchronizedOutputOnceTheTerminalSupportsIt is the phase-1
// half of the frame-atomicity decision: the model emits no escape sequences, the
// resolved renderer owns the boundary, and every frame it wraps is closed again.
//
// The capability report is injected rather than parsed from the wire: the harness
// has no terminal to answer the mode query, and the report is exactly the message
// a real terminal's reply produces.
func TestFrameUpdatesUseSynchronizedOutputOnceTheTerminalSupportsIt(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Transcript = []ai.Message{ai.UserText("ATOMIC-FRAME")}
	model := fullscreenModel(t, stubController{state: state}, true)
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	model.rerenderTranscript(true)

	probe := &atomicFrameProbe{Model: model}
	var output synchronizedBuffer
	// No TERM_PROGRAM and no SSH_TTY is the condition under which the renderer
	// queries mode 2026 at startup.
	program := tea.NewProgram(probe, tea.WithInput(nil), tea.WithOutput(&output),
		tea.WithEnvironment([]string{"TERM=xterm-256color"}),
		tea.WithWindowSize(40, 10), tea.WithFPS(60), tea.WithoutSignalHandler())
	finished := make(chan error, 1)
	go func() { _, err := program.Run(); finished <- err }()
	t.Cleanup(program.Kill)
	require.Eventually(t, probe.finished, 30*time.Second, 10*time.Millisecond)

	program.Send(tea.ModeReportMsg{
		Mode:  ansi.ModeSynchronizedOutput,
		Value: ansi.ModeReset,
	})
	// A scroll changes the frame, which is what gives the renderer something to
	// wrap; a frame identical to the last one is skipped entirely.
	program.Send(tea.KeyPressMsg{Code: tea.KeyPgUp})

	require.Eventually(t, func() bool {
		return strings.Contains(output.String(), ansi.SetModeSynchronizedOutput)
	}, 30*time.Second, 10*time.Millisecond)

	program.Quit()
	require.NoError(t, <-finished)

	capture := output.String()
	opens := strings.Count(capture, ansi.SetModeSynchronizedOutput)
	closes := strings.Count(capture, ansi.ResetModeSynchronizedOutput)
	assert.Positive(t, opens, "at least one frame is wrapped")
	assert.Equal(t, opens, closes, "every open block is closed in %q", capture)

	// Each open must be closed before the next one starts, so the terminal is
	// never left inside an open block between frames.
	index := 0
	for {
		open := strings.Index(capture[index:], ansi.SetModeSynchronizedOutput)
		if open < 0 {
			break
		}

		open += index
		closed := strings.Index(capture[open:], ansi.ResetModeSynchronizedOutput)
		require.Positive(t, closed, "an open block is closed before the next one")

		index = open + len(ansi.SetModeSynchronizedOutput)
	}
}
