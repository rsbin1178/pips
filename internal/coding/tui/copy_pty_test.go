//go:build darwin || linux

//nolint:wsl_v5 // PTY lifecycle setup stays sequential and auditable.
package tui

import (
	"context"
	"encoding/base64"
	"io"
	"os"
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

// copyPTYMode is the presentation one PTY run exercises.
type copyPTYMode struct {
	name     string
	screen   ScreenMode
	alt      AltScreenPolicy
	altUsage bool
}

var copyPTYModes = []copyPTYMode{
	{name: "fullscreen", screen: ScreenFullscreen, alt: AltScreenAlways, altUsage: true},
	{name: "inline", screen: ScreenInline, alt: AltScreenNever, altUsage: false},
}

func copyPTYModeByName(name string) (copyPTYMode, bool) {
	for _, mode := range copyPTYModes {
		if mode.name == name {
			return mode, true
		}
	}

	return copyPTYMode{}, false
}

// TestPTYCopyWritesOSC52AndTheFallbackFile is the end-to-end guard for Ctrl+X in
// both presentation modes: the OSC 52 request must reach the terminal through the
// Program writer, and the fallback file must exist because that delivery cannot be
// confirmed.
func TestPTYCopyWritesOSC52AndTheFallbackFile(t *testing.T) {
	if os.Getenv("PIPS_TUI_COPY_PTY_HELPER") == "1" {
		mode, ok := copyPTYModeByName(os.Getenv("PIPS_TUI_COPY_PTY_SCREEN"))
		if !ok {
			_, _ = os.Stderr.WriteString("unknown copy PTY screen mode")
			os.Exit(2)
		}

		state := readyState()
		state.Transcript = []ai.Message{
			ai.UserText("QUESTION"),
			ai.AssistantText("COPY-ME-PLEASE"),
		}
		_, _ = os.Stdout.WriteString("SHELL-HISTORY\n")
		err := Run(context.Background(), Options{
			Input: os.Stdin, Output: os.Stdout, Environment: os.Environ(),
			Workspace: t.TempDir(), Trusted: true, NoColor: true,
			PinPresentation: true, Screen: mode.screen, AltScreen: mode.alt,
			Bootstrap: func(context.Context, bool) (Controller, error) {
				return stubController{state: state}, nil
			},
			SaveText: func(_ context.Context, request TextSaveRequest) (string, error) {
				path := os.Getenv("PIPS_TUI_COPY_FILE")
				//nolint:gosec // The helper writes only to the path the parent test exported.
				if err := os.WriteFile(path, []byte(request.Content), 0o600); err != nil {
					return "", err
				}

				return path, nil
			},
		})
		if err != nil {
			_, _ = os.Stderr.WriteString(err.Error())
			os.Exit(1)
		}

		os.Exit(0)
	}
	t.Parallel()

	for _, mode := range copyPTYModes {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()

			runCopyPTY(t, mode)
		})
	}
}

func runCopyPTY(t *testing.T, mode copyPTYMode) {
	t.Helper()

	executable, err := os.Executable()
	require.NoError(t, err)

	copyPath := t.TempDir() + "/copy.txt"
	master, slave, err := pty.Open()
	require.NoError(t, err)
	t.Cleanup(func() { _ = slave.Close(); _ = master.Close() })
	require.NoError(t, pty.Setsize(master, &pty.Winsize{Rows: 24, Cols: 100}))
	before, err := term.GetState(master.Fd())
	require.NoError(t, err)

	process, err := os.StartProcess(executable,
		[]string{executable, "-test.run=^TestPTYCopyWritesOSC52AndTheFallbackFile$"},
		&os.ProcAttr{
			Env: append(os.Environ(), "PIPS_TUI_COPY_PTY_HELPER=1",
				"PIPS_TUI_COPY_PTY_SCREEN="+mode.name,
				"PIPS_TUI_COPY_FILE="+copyPath,
				"TERM=xterm-256color", "NO_COLOR=1"),
			Files: []*os.File{slave, slave, slave},
			Sys:   &syscall.SysProcAttr{Setsid: true, Setctty: true},
		})
	require.NoError(t, err)
	t.Cleanup(func() { _ = process.Kill() })

	var output synchronizedBuffer

	readDone := make(chan struct{})
	go func() { _, _ = io.Copy(&output, master); close(readDone) }()

	require.Eventually(t, func() bool {
		capture := output.String()
		if !strings.Contains(capture, "COPY-ME-PLEASE") {
			return false
		}

		e := newScreenReplay(100, 24)
		_, writeErr := e.Write([]byte(capture))
		ready := writeErr == nil && strings.Contains(e.String(), "idle")
		if mode.altUsage {
			ready = ready && e.IsAltScreen()
		}
		_ = e.Close()

		return ready
	}, 30*time.Second, 10*time.Millisecond)

	// Ctrl+X copies the newest assistant reply.
	_, err = master.Write([]byte{0x18})
	require.NoError(t, err)

	payload := "\x1b]52;c;" +
		base64.StdEncoding.EncodeToString([]byte("COPY-ME-PLEASE")) + "\x07"
	require.Eventually(t, func() bool {
		return strings.Contains(output.String(), payload)
	}, 30*time.Second, 10*time.Millisecond)

	require.Eventually(t, func() bool {
		return strings.Contains(output.String(), "copied to clipboard")
	}, 30*time.Second, 10*time.Millisecond)

	// The fallback file is the delivery the terminal cannot confirm, so it must
	// exist with the copied text even though OSC 52 was also emitted.
	data, err := os.ReadFile(copyPath) //nolint:gosec // The test controls the temporary path.
	require.NoError(t, err)
	assert.Equal(t, "COPY-ME-PLEASE", string(data))

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
	assert.Equal(t, before, after, "terminal modes restored")
	_ = slave.Close()
	_ = master.Close()
	<-readDone
}
