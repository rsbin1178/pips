//go:build darwin || linux

//nolint:wsl_v5 // PTY setup, observation and teardown follow physical order.
package tui

import (
	"context"
	"io"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptyHarness runs one helper test binary in a real PTY and replays what it wrote
// through the vt emulator, so the assertions are about the terminal's own state
// (alternate screen, scrollback, modes) rather than about a captured byte string.
type ptyHarness struct {
	t      *testing.T
	master *os.File
	slave  *os.File
	output *synchronizedBuffer
	before *term.State
	exited chan struct{}
	state  *os.ProcessState
}

func newPTYHarness(t *testing.T, envVar, testName string, rows, cols uint16, extraEnv ...string) *ptyHarness {
	t.Helper()

	executable, err := os.Executable()
	require.NoError(t, err)
	master, slave, err := pty.Open()
	require.NoError(t, err)
	require.NoError(t, pty.Setsize(master, &pty.Winsize{Rows: rows, Cols: cols}))
	before, err := term.GetState(master.Fd())
	require.NoError(t, err)

	environment := append(os.Environ(), envVar+"=1",
		"TERM=xterm-256color", "NO_COLOR=1")
	environment = append(environment, extraEnv...)
	environment = harnessEnvironment(environment)

	process, err := os.StartProcess(executable,
		[]string{executable, "-test.run=^" + testName + "$"}, &os.ProcAttr{
			Env:   environment,
			Files: []*os.File{slave, slave, slave},
			Sys:   &syscall.SysProcAttr{Setsid: true, Setctty: true},
		})
	require.NoError(t, err)

	harness := &ptyHarness{
		t:      t,
		master: master,
		slave:  slave,
		output: &synchronizedBuffer{},
		before: before,
		exited: make(chan struct{}),
	}
	readDone := make(chan struct{})
	go func() { _, _ = io.Copy(harness.output, master); close(readDone) }()
	go func() { harness.state, _ = process.Wait(); close(harness.exited) }()
	t.Cleanup(func() {
		_ = process.Kill()
		_ = slave.Close()
		_ = master.Close()
		<-readDone

		// A failure is only diagnosable with the helper's own output, and the
		// buffer is complete once the reader has drained.
		if t.Failed() {
			t.Logf("helper output:\n%s", harness.output.String())
		}
	})

	return harness
}

// write sends raw input bytes to the helper's terminal.
func (h *ptyHarness) write(value string) {
	h.t.Helper()

	_, err := h.master.Write([]byte(value))
	require.NoError(h.t, err)
}

// screenMatches replays everything written so far and reports whether the
// terminal is in the requested state.
func (h *ptyHarness) screenMatches(width, height int, check func(*vt.Emulator) bool) bool {
	e := newScreenReplay(width, height)
	defer func() { _ = e.Close() }()

	if _, err := e.Write([]byte(h.output.String())); err != nil {
		return false
	}

	return check(e)
}

// harnessEnvironment collapses duplicate variables so the last entry for a key
// wins, and drops a key whose winning override is empty.
//
// The helper is a real process: a pinned NO_COLOR=1 reaches libraries that read
// the process environment directly, whatever Options the test hands the program,
// so an extraEnv override has to be able to clear it.
func harnessEnvironment(entries []string) []string {
	last := make(map[string]int, len(entries))

	for index, entry := range entries {
		if key, _, ok := strings.Cut(entry, "="); ok {
			last[key] = index
		}
	}

	values := make([]string, 0, len(entries))

	for index, entry := range entries {
		key, _, _ := strings.Cut(entry, "=")
		if last[key] != index {
			continue
		}

		if entry == key+"=" {
			continue
		}

		values = append(values, entry)
	}

	return values
}

func (h *ptyHarness) waitForExit() *os.ProcessState {
	h.t.Helper()

	select {
	case <-h.exited:
		return h.state
	case <-time.After(30 * time.Second):
		h.t.Fatal("the helper did not exit")

		return nil
	}
}

// assertRestored compares the terminal modes with the state before the run.
func (h *ptyHarness) assertRestored() {
	h.t.Helper()

	after, err := term.GetState(h.master.Fd())
	require.NoError(h.t, err)
	assert.True(h.t, reflect.DeepEqual(h.before, after), "terminal modes restored")
}

// TestPTYTranscriptModeHandsHistoryToTheTerminal is the AC11 end-to-end proof:
// Ctrl+O leaves the alternate screen, the whole managed conversation appears in
// the terminal's own scrollback exactly once and in order, the compact frame
// stays inside the window, and Esc re-enters the viewport.
func TestPTYTranscriptModeHandsHistoryToTheTerminal(t *testing.T) {
	const testName = "TestPTYTranscriptModeHandsHistoryToTheTerminal"
	if os.Getenv("PIPS_TUI_TRANSCRIPT_PTY_HELPER") == "1" {
		state := readyState()
		for _, marker := range historyMarkers(30) {
			state.Transcript = append(state.Transcript, ai.UserText(marker))
		}
		_, _ = os.Stdout.WriteString("SHELL-HISTORY\n")
		err := Run(context.Background(), Options{
			Input: os.Stdin, Output: os.Stdout, Environment: os.Environ(),
			Workspace: t.TempDir(), Trusted: true, NoColor: true,
			PinPresentation: true, Screen: ScreenFullscreen, AltScreen: AltScreenAlways,
			// The in-session mode is asserted on its own; the exit handoff has its
			// own test.
			ExitOutput: config.ExitOutputResumeHint,
			Bootstrap: func(context.Context, bool) (Controller, error) {
				return stubController{state: state}, nil
			},
		})
		if err != nil {
			_, _ = os.Stderr.WriteString(err.Error())
			os.Exit(1)
		}
		os.Exit(0)
	}
	t.Parallel()

	h := newPTYHarness(t, "PIPS_TUI_TRANSCRIPT_PTY_HELPER", testName, 12, 60)

	require.Eventually(t, func() bool {
		return h.screenMatches(60, 12, func(e *vt.Emulator) bool {
			return e.IsAltScreen() && strings.Contains(e.String(), "HISTORY-029")
		})
	}, 30*time.Second, 10*time.Millisecond)

	h.write("\x0f") // Ctrl+O
	require.Eventually(t, func() bool {
		return h.screenMatches(60, 12, func(e *vt.Emulator) bool {
			return !e.IsAltScreen() &&
				strings.Contains(stableScreenHistory(e), "HISTORY-000") &&
				strings.Contains(e.String(), "Esc returns")
		})
	}, 30*time.Second, 10*time.Millisecond)

	e := newScreenReplay(60, 12)
	_, err := e.Write([]byte(h.output.String()))
	require.NoError(t, err)
	assert.False(t, e.IsAltScreen(), "the escape hatch left the alternate screen")
	history := stableScreenHistory(e)
	previous := -1
	for _, marker := range historyMarkers(30) {
		assert.Equal(t, 1, strings.Count(history, marker), "marker %s in %q", marker, history)

		position := strings.Index(history, marker)
		require.Greater(t, position, previous, "markers keep their order")
		previous = position
	}
	frame := e.String()
	assert.LessOrEqual(t, len(strings.Split(frame, "\n")), 12, "the live frame fits the window")
	require.NoError(t, e.Close())
	assert.NotContains(t, h.output.String(), "\x1b[3J", "scrollback is never cleared")

	h.write("\x1b") // Esc
	require.Eventually(t, func() bool {
		return h.screenMatches(60, 12, func(e *vt.Emulator) bool { return e.IsAltScreen() })
	}, 30*time.Second, 10*time.Millisecond)

	h.write("\x0bquit\r")
	result := h.waitForExit()
	require.NotNil(t, result)
	require.Equal(t, 0, result.ExitCode(), "helper output: %s", h.output.String())
	h.assertRestored()
}

// TestPTYExitHandoffWritesTheConversationToTheMainScreen is the exit half of
// AC11: quitting a fullscreen session hands the whole conversation to the main
// screen after terminal restoration, so the terminal's wheel and selection can
// read what the viewport owned.
func TestPTYExitHandoffWritesTheConversationToTheMainScreen(t *testing.T) {
	const testName = "TestPTYExitHandoffWritesTheConversationToTheMainScreen"
	if os.Getenv("PIPS_TUI_EXIT_TRANSCRIPT_PTY_HELPER") == "1" {
		state := readyState()
		for _, marker := range historyMarkers(20) {
			state.Transcript = append(state.Transcript, ai.UserText(marker))
		}
		_, _ = os.Stdout.WriteString("SHELL-HISTORY\n")
		err := Run(context.Background(), Options{
			Input: os.Stdin, Output: os.Stdout, Environment: os.Environ(),
			Workspace: t.TempDir(), Trusted: true, NoColor: true,
			PinPresentation: true, Screen: ScreenFullscreen, AltScreen: AltScreenAlways,
			ExitOutput: config.ExitOutputTranscript,
			Bootstrap: func(context.Context, bool) (Controller, error) {
				return stubController{state: state}, nil
			},
		})
		if err != nil {
			_, _ = os.Stderr.WriteString(err.Error())
			os.Exit(1)
		}
		os.Exit(0)
	}
	t.Parallel()

	h := newPTYHarness(t, "PIPS_TUI_EXIT_TRANSCRIPT_PTY_HELPER", testName, 12, 50)

	require.Eventually(t, func() bool {
		return h.screenMatches(50, 12, func(e *vt.Emulator) bool {
			return e.IsAltScreen() && strings.Contains(e.String(), "HISTORY-019")
		})
	}, 30*time.Second, 10*time.Millisecond)

	h.write("\x0bquit\r")
	result := h.waitForExit()
	require.NotNil(t, result)
	require.Equal(t, 0, result.ExitCode(), "helper output: %s", h.output.String())

	// The handoff runs after terminal restoration, so it is only observable once
	// the process has finished.
	require.Eventually(t, func() bool {
		return h.screenMatches(50, 12, func(e *vt.Emulator) bool {
			history := stableScreenHistory(e)

			return !e.IsAltScreen() &&
				strings.Contains(history, "SHELL-HISTORY") &&
				strings.Contains(history, "HISTORY-000") &&
				strings.Contains(history, "HISTORY-019")
		})
	}, 30*time.Second, 10*time.Millisecond)

	e := newScreenReplay(50, 12)
	_, err := e.Write([]byte(h.output.String()))
	require.NoError(t, err)
	history := stableScreenHistory(e)
	previous := -1
	for _, marker := range historyMarkers(20) {
		assert.Equal(t, 1, strings.Count(history, marker), "marker %s in %q", marker, history)

		position := strings.Index(history, marker)
		require.Greater(t, position, previous, "markers keep their order")
		previous = position
	}
	require.NoError(t, e.Close())
	assert.NotContains(t, h.output.String(), "\x1b[3J", "scrollback is never cleared")
	h.assertRestored()
}
