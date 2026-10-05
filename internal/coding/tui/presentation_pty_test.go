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

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/creack/pty"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPTYInlineModeKeepsNativeHistory is the AC10 regression: the inline
// compatibility path must still avoid the alternate screen, keep the shell's own
// history, and restore terminal modes.
//
// The fullscreen counterpart (alternate screen entered, transcript restored on
// exit) is deferred with the viewport: with the alternate screen active, the
// current native-write pipeline paints nothing, so a fullscreen PTY run today
// would assert an empty conversation. This test is the guard that the inline
// default is genuinely unchanged until that lands.
func TestPTYInlineModeKeepsNativeHistory(t *testing.T) {
	if os.Getenv("PIPS_TUI_INLINE_PTY_HELPER") == "1" {
		_, _ = os.Stdout.WriteString("SHELL-HISTORY\n")
		err := Run(context.Background(), Options{
			Input: os.Stdin, Output: os.Stdout, Environment: os.Environ(),
			Workspace: t.TempDir(), Trusted: true, NoColor: true,
			PinPresentation: true,
			Screen:          ScreenInline,
			AltScreen:       AltScreenAuto,
			Bootstrap: func(context.Context, bool) (Controller, error) {
				return stubController{state: readyState()}, nil
			},
		})
		if err != nil {
			_, _ = os.Stderr.WriteString(err.Error())
			os.Exit(1)
		}
		os.Exit(0)
	}
	t.Parallel()
	executable, err := os.Executable()
	require.NoError(t, err)
	master, slave, err := pty.Open()
	require.NoError(t, err)
	t.Cleanup(func() { _ = slave.Close(); _ = master.Close() })
	require.NoError(t, pty.Setsize(master, &pty.Winsize{Rows: 12, Cols: 60}))
	before, err := term.GetState(master.Fd())
	require.NoError(t, err)
	process, err := os.StartProcess(executable,
		[]string{executable, "-test.run=^TestPTYInlineModeKeepsNativeHistory$"}, &os.ProcAttr{
			Env: append(os.Environ(), "PIPS_TUI_INLINE_PTY_HELPER=1",
				"TERM=xterm-256color", "NO_COLOR=1"),
			Files: []*os.File{slave, slave, slave},
			Sys:   &syscall.SysProcAttr{Setsid: true, Setctty: true},
		})
	require.NoError(t, err)
	t.Cleanup(func() { _ = process.Kill() })
	var output synchronizedBuffer
	readDone := make(chan struct{})
	go func() { _, _ = io.Copy(&output, master); close(readDone) }()

	var capture string
	require.Eventually(t, func() bool {
		capture = output.String()
		if !strings.Contains(capture, "idle") {
			return false
		}
		e := newScreenReplay(60, 12)
		_, writeErr := e.Write([]byte(capture))
		ready := writeErr == nil
		_ = e.Close()

		return ready
	}, 30*time.Second, 10*time.Millisecond)

	e := newScreenReplay(60, 12)
	_, err = e.Write([]byte(capture))
	require.NoError(t, err)
	assert.False(t, e.IsAltScreen(), "inline mode must stay on the main screen")
	assert.Contains(t, stableScreenHistory(e), "SHELL-HISTORY", "native history is retained")
	require.NoError(t, e.Close())
	assert.NotContains(t, capture, "\x1b[?1049h", "inline mode must not enter the alternate screen")
	assert.NotContains(t, capture, "\x1b[3J", "inline mode must never clear scrollback")

	_, err = master.Write([]byte("\x0bquit\r"))
	require.NoError(t, err)
	finished := make(chan *os.ProcessState, 1)
	go func() { state, _ := process.Wait(); finished <- state }()
	var result *os.ProcessState
	require.Eventually(t, func() bool {
		select {
		case result = <-finished:
			return true
		default:
			return false
		}
	}, 30*time.Second, 10*time.Millisecond)
	require.NotNil(t, result)
	require.Equal(t, 0, result.ExitCode(), "helper output: %s", output.String())
	after, err := term.GetState(master.Fd())
	require.NoError(t, err)
	assert.True(t, reflect.DeepEqual(before, after), "terminal modes restored")
	_ = slave.Close()
	_ = master.Close()
	<-readDone
}

// TestPTYFullscreenOwnsTheScreenAndRestoresIt is the AC10 counterpart of the
// inline test: the managed layout enters the alternate screen, shows every entry
// of a long history without any terminal write, and restores the terminal on exit.
func TestPTYFullscreenOwnsTheScreenAndRestoresIt(t *testing.T) {
	if os.Getenv("PIPS_TUI_FULLSCREEN_PTY_HELPER") == "1" {
		state := readyState()
		for _, marker := range historyMarkers(80) {
			state.Transcript = append(state.Transcript, ai.UserText(marker))
		}
		_, _ = os.Stdout.WriteString("SHELL-HISTORY\n")
		err := Run(context.Background(), Options{
			Input: os.Stdin, Output: os.Stdout, Environment: os.Environ(),
			Workspace: t.TempDir(), Trusted: true, NoColor: true,
			PinPresentation: true, Screen: ScreenFullscreen, AltScreen: AltScreenAlways,
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
	executable, err := os.Executable()
	require.NoError(t, err)
	master, slave, err := pty.Open()
	require.NoError(t, err)
	t.Cleanup(func() { _ = slave.Close(); _ = master.Close() })
	require.NoError(t, pty.Setsize(master, &pty.Winsize{Rows: 10, Cols: 40}))
	before, err := term.GetState(master.Fd())
	require.NoError(t, err)
	process, err := os.StartProcess(executable,
		[]string{executable, "-test.run=^TestPTYFullscreenOwnsTheScreenAndRestoresIt$"}, &os.ProcAttr{
			Env: append(os.Environ(), "PIPS_TUI_FULLSCREEN_PTY_HELPER=1",
				"TERM=xterm-256color", "NO_COLOR=1"),
			Files: []*os.File{slave, slave, slave},
			Sys:   &syscall.SysProcAttr{Setsid: true, Setctty: true},
		})
	require.NoError(t, err)
	t.Cleanup(func() { _ = process.Kill() })
	var output synchronizedBuffer
	readDone := make(chan struct{})
	go func() { _, _ = io.Copy(&output, master); close(readDone) }()

	// The newest marker is visible while following the tail; no repair input.
	var capture string
	require.Eventually(t, func() bool {
		capture = output.String()
		if !strings.Contains(capture, "HISTORY-079") {
			return false
		}
		e := newScreenReplay(40, 10)
		_, writeErr := e.Write([]byte(capture))
		screen := e.String()
		ok := writeErr == nil && strings.Contains(screen, "idle") && e.IsAltScreen()
		_ = e.Close()

		return ok
	}, 30*time.Second, 10*time.Millisecond)

	e := newScreenReplay(40, 10)
	_, err = e.Write([]byte(capture))
	require.NoError(t, err)
	assert.True(t, e.IsAltScreen(), "fullscreen must own the alternate screen")
	frame := e.String()
	rows := strings.Split(frame, "\n")
	require.LessOrEqual(t, len(rows), 10, "the frame never exceeds the window: %q", frame)
	assert.Contains(t, ansi.Strip(frame), "HISTORY-079", "the tail is visible")
	assert.Contains(t, ansi.Strip(frame), "idle", "the status band is inside the frame")
	require.NoError(t, e.Close())
	// Under the alternate screen the terminal's own history must not receive the
	// conversation, and scrollback must never be cleared.
	assert.NotContains(t, capture, "\x1b[3J")
	assert.NotContains(t, capture, "HISTORY-000\r\n", "entries are painted, not printed")

	_, err = master.Write([]byte("\x0bquit\r"))
	require.NoError(t, err)
	finished := make(chan *os.ProcessState, 1)
	go func() { state, _ := process.Wait(); finished <- state }()
	var result *os.ProcessState
	require.Eventually(t, func() bool {
		select {
		case result = <-finished:
			return true
		default:
			return false
		}
	}, 30*time.Second, 10*time.Millisecond)
	require.NotNil(t, result)
	require.Equal(t, 0, result.ExitCode(), "helper output: %s", output.String())
	after, err := term.GetState(master.Fd())
	require.NoError(t, err)
	assert.True(t, reflect.DeepEqual(before, after), "terminal modes restored")
	// The shipped exit_output is resume-hint: quitting must not hand the
	// conversation back to the main screen. Comparing marker counts catches a
	// transcript handoff without depending on CR/LF details.
	assert.Equal(t,
		strings.Count(capture, "HISTORY-000"),
		strings.Count(output.String(), "HISTORY-000"),
		"the default exit_output must not write the conversation back",
	)
	_ = slave.Close()
	_ = master.Close()
	<-readDone
}
