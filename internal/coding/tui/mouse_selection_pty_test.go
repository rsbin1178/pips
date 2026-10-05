//go:build darwin || linux

//nolint:wsl_v5 // PTY setup, gesture replay and teardown follow physical order.
package tui

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/stretchr/testify/require"
)

// sgrMouse builds one SGR (mode 1006) mouse report. Coordinates are one-based, as
// a terminal reports them; button 32 is a drag with the left button held.
func sgrMouse(button, column, row int, release bool) string {
	final := "M"
	if release {
		final = "m"
	}

	return fmt.Sprintf("\x1b[<%d;%d;%d%s", button, column, row, final)
}

// TestPTYDragSelectCopiesTheRows is the end-to-end proof of the gesture: a real
// terminal sends press, drag and release, and the addressed rows reach both the
// OSC 52 clipboard request and the fallback file.
func TestPTYDragSelectCopiesTheRows(t *testing.T) {
	const testName = "TestPTYDragSelectCopiesTheRows"
	if os.Getenv("PIPS_TUI_SELECTION_PTY_HELPER") == "1" {
		state := readyState()
		state.Transcript = []ai.Message{
			ai.UserText("COPY-ROW-ALPHA"),
			ai.UserText("COPY-ROW-BETA"),
		}
		err := Run(context.Background(), Options{
			Input: os.Stdin, Output: os.Stdout, Environment: os.Environ(),
			Workspace: t.TempDir(), Trusted: true, NoColor: true,
			PinPresentation: true, Screen: ScreenFullscreen, AltScreen: AltScreenAlways,
			MouseReporting: true,
			ExitOutput:     config.ExitOutputResumeHint,
			Bootstrap: func(context.Context, bool) (Controller, error) {
				return stubController{state: state}, nil
			},
			SaveText: func(_ context.Context, request TextSaveRequest) (string, error) {
				path := os.Getenv("PIPS_TUI_SELECTION_FILE")
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

	copyPath := filepath.Join(t.TempDir(), "selection.txt")
	h := newPTYHarness(t, "PIPS_TUI_SELECTION_PTY_HELPER", testName, 24, 60,
		"PIPS_TUI_SELECTION_FILE="+copyPath)

	require.Eventually(t, func() bool {
		return h.screenMatches(60, 24, func(e *vt.Emulator) bool {
			frame := e.String()

			return e.IsAltScreen() &&
				strings.Contains(frame, "COPY-ROW-ALPHA") &&
				strings.Contains(frame, "COPY-ROW-BETA")
		})
	}, 30*time.Second, 10*time.Millisecond)

	frame := strings.Split(h.frame(60, 24), "\n")
	first, second := -1, -1

	for index, line := range frame {
		if strings.Contains(line, "COPY-ROW-ALPHA") {
			first = index
		}

		if strings.Contains(line, "COPY-ROW-BETA") {
			second = index
		}
	}

	require.GreaterOrEqual(t, first, 0, "the first row is on screen:\n%s", h.frame(60, 24))
	require.Greater(t, second, first)

	// A terminal reports the gesture as press, motion, release, all in one-based
	// screen coordinates.
	h.write(sgrMouse(0, 1, first+1, false))
	h.write(sgrMouse(32, 60, second+1, false))
	h.write(sgrMouse(0, 60, second+1, true))

	expected := "❯ COPY-ROW-ALPHA\n\n❯ COPY-ROW-BETA"
	payload := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(expected)) + "\x07"

	require.Eventually(t, func() bool {
		return strings.Contains(h.output.String(), payload)
	}, 30*time.Second, 10*time.Millisecond, "helper output: %s", h.output.String())

	require.Eventually(t, func() bool {
		written, err := os.ReadFile(copyPath)

		return err == nil && string(written) == expected
	}, 30*time.Second, 10*time.Millisecond)

	h.write("\x0bquit\r")
	result := h.waitForExit()
	require.NotNil(t, result)
	require.Equal(t, 0, result.ExitCode(), "helper output: %s", h.output.String())
	h.assertRestored()
}
