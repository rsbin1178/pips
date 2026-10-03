//go:build darwin || linux

//nolint:wsl_v5 // PTY setup, observation, and teardown follow physical order.
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
	"github.com/creack/pty"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPTYLongHistoryVisibleBeforeInput(t *testing.T) {
	if os.Getenv("PIPS_TUI_LONG_PTY_HELPER") == "1" {
		state := readyState()
		for _, marker := range historyMarkers(80) {
			state.Transcript = append(state.Transcript, ai.UserText(marker))
		}
		_, _ = os.Stdout.WriteString("SHELL-HISTORY\n")
		err := Run(context.Background(), Options{
			Input: os.Stdin, Output: os.Stdout, Environment: os.Environ(),
			Workspace: t.TempDir(), Trusted: true, NoColor: true,
			Bootstrap: func(context.Context, bool) (Controller, error) {
				return &longHistoryPTYController{stubController: stubController{state: state}}, nil
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
		[]string{executable, "-test.run=^TestPTYLongHistoryVisibleBeforeInput$"}, &os.ProcAttr{
			Env:   append(os.Environ(), "PIPS_TUI_LONG_PTY_HELPER=1", "TERM=xterm-256color", "NO_COLOR=1"),
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
		if !strings.Contains(capture, "HISTORY-079") {
			return false
		}
		e := newScreenReplay(40, 10)
		_, writeErr := e.Write([]byte(capture))
		screen := e.String()
		position := e.CursorPosition()
		rows := strings.Split(screen, "\n")
		ready := writeErr == nil && strings.Contains(screen, "idle") &&
			position.Y >= 0 && position.Y < len(rows) &&
			strings.Contains(rows[position.Y], "│ ❯")
		_ = e.Close()
		return ready
	}, 30*time.Second, 10*time.Millisecond)
	// No key, repair prompt, resize, or Quit has been sent yet.
	e := newScreenReplay(40, 10)
	_, err = e.Write([]byte(capture))
	require.NoError(t, err)
	assertStableScreen(t, e, historyMarkers(80))
	require.NoError(t, e.Close())
	assert.NotContains(t, capture, "\x1b[?1049h")
	assert.NotContains(t, capture, "\x1b[3J")
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
	require.Eventually(t, func() bool {
		return strings.Contains(output.String(), "LONG_CONTROLLER_CLOSED")
	}, 30*time.Second, 10*time.Millisecond)
	value := output.String()
	assert.Greater(t, strings.Index(value, "LONG_CONTROLLER_CLOSED"), strings.LastIndex(value, resetTerminalInteraction))
	_ = slave.Close()
	_ = master.Close()
	<-readDone
}

type longHistoryPTYController struct{ stubController }

func (c *longHistoryPTYController) Close(context.Context) error {
	_, err := os.Stdout.WriteString("LONG_CONTROLLER_CLOSED")
	return err
}
