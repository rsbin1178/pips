//go:build darwin || linux

//nolint:wsl_v5 // PTY launch, frame reads and teardown follow physical order.
package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const tasksPanePTYEnv = "PIPS_TUI_TASKS_PANE_PTY_HELPER"

// TestPTYTasksPaneRevealsFocusesAndCloses drives the pane on a real terminal:
// the chord reveals it, the focused pane owns its own keys, and Escape gives the
// composer and the reading keys back.
func TestPTYTasksPaneRevealsFocusesAndCloses(t *testing.T) {
	const testName = "TestPTYTasksPaneRevealsFocusesAndCloses"
	const rows, cols = 30, 100

	if os.Getenv(tasksPanePTYEnv) == "1" {
		runTasksPaneHelper(t)

		return
	}
	t.Parallel()

	harness := newPTYHarness(t, tasksPanePTYEnv, testName, rows, cols)

	require.Eventually(t, func() bool {
		return strings.Contains(harness.frame(cols, rows), "Test Model")
	}, 30*time.Second, 10*time.Millisecond, "helper output: %s", harness.output.String())

	frame := harness.frame(cols, rows)
	require.NotContains(t, frame, tasksPaneEmptyRunning, "the pane starts hidden")

	// Ctrl+B reveals and focuses the pane. No child is running, so it shows the
	// state that names the toggle.
	harness.write("\x02")
	require.Eventually(t, func() bool {
		return strings.Contains(harness.frame(cols, rows), tasksPaneEmptyRunning)
	}, 30*time.Second, 10*time.Millisecond, "helper output: %s", harness.output.String())
	requireInsideWidth(t, harness, cols, rows)

	// The focused pane owns `h`.
	harness.write("h")
	require.Eventually(t, func() bool {
		return strings.Contains(harness.frame(cols, rows), tasksPaneEmptyAll)
	}, 30*time.Second, 10*time.Millisecond, "helper output: %s", harness.output.String())

	// Tab unfocuses it but leaves the rows on screen, so the next keystroke
	// belongs to the composer again. Tab rather than Escape because a bare Escape
	// byte and the next keystroke would arrive as one Alt sequence.
	harness.write("\t")
	harness.write("x")
	require.Eventually(t, func() bool {
		frame := harness.frame(cols, rows)

		return strings.Contains(frame, "❯ x") && strings.Contains(frame, tasksPaneEmptyAll)
	}, 30*time.Second, 10*time.Millisecond, "helper output: %s", harness.output.String())

	requireInsideWidth(t, harness, cols, rows)

	harness.write("\x0bquit\r")
	result := harness.waitForExit()
	require.NotNil(t, result)
	require.Equal(t, 0, result.ExitCode(), "helper output: %s", harness.output.String())
}

// runTasksPaneHelper is the child process: a real TUI on a real terminal, pinned
// to the shipped default presentation so the test measures what a user gets.
func runTasksPaneHelper(t *testing.T) {
	t.Helper()

	controller, ok := reasoningController(t, reasoningModelConfig(), nil).(stubController)
	require.True(t, ok)

	err := Run(context.Background(), Options{
		Input: os.Stdin, Output: os.Stdout, Environment: os.Environ(),
		Workspace: t.TempDir(), Trusted: true, NoColor: true,
		PinPresentation: true, Screen: ScreenFullscreen, AltScreen: AltScreenAlways,
		MouseReporting: true,
		Bootstrap: func(context.Context, bool) (Controller, error) {
			return controller, nil
		},
		OnExit: func(info ExitInfo) error {
			_, writeErr := fmt.Fprintf(os.Stdout, ";TASKS_PANE_PTY_EXIT=%s", info.SessionID)

			return writeErr
		},
	})
	if err != nil {
		_, _ = os.Stderr.WriteString(err.Error())
		os.Exit(1)
	}

	os.Exit(0)
}
