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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const helpPanelPTYEnv = "PIPS_TUI_HELP_PANEL_PTY_HELPER"

// TestPTYHelpPanelLeavesNothingBehindOnExit is the regression test for a real
// report: opening /help, closing it with Esc and then quitting left the panel's
// rows in the terminal's scrollback. /status did not, because it composed its
// frame through presentationView and the help panel did not — a raw tea.NewView
// leaves view.AltScreen false, so the frame was painted into the main buffer.
//
// The assertion is about the terminal's own state after the helper exits, not
// about the byte stream: a panel's bytes are legitimately written while the
// alternate screen is up, and the bug is that they survive leaving it.
func TestPTYHelpPanelLeavesNothingBehindOnExit(t *testing.T) {
	const testName = "TestPTYHelpPanelLeavesNothingBehindOnExit"

	if os.Getenv(helpPanelPTYEnv) == "1" {
		runHelpPanelHelper(t)

		return
	}
	t.Parallel()

	harness := newPTYHarness(t, helpPanelPTYEnv, testName, 44, 100)

	require.Eventually(t, func() bool {
		return containsAll(harness.frame(100, 44), "Test Model")
	}, 30*time.Second, 10*time.Millisecond, "helper output: %s", harness.output.String())

	harness.write("\x18")
	require.Eventually(t, func() bool {
		return containsAll(harness.frame(100, 44), "Shortcuts", "Esc to close")
	}, 30*time.Second, 10*time.Millisecond, "helper output: %s", harness.output.String())

	harness.write("\x1b")
	require.Eventually(t, func() bool {
		return !strings.Contains(harness.frame(100, 44), "Esc to close")
	}, 30*time.Second, 10*time.Millisecond, "helper output: %s", harness.output.String())

	harness.write("\x0bquit\r")
	result := harness.waitForExit()
	require.NotNil(t, result)
	require.Equal(t, 0, result.ExitCode(), "helper output: %s", harness.output.String())

	// After exit the terminal shows its own main screen. The panel was closed
	// before quitting, so none of it may be there.
	main := harness.frame(100, 44)
	assert.NotContains(t, main, "Esc to close",
		"the help panel survived the alternate screen; main screen was:\n%s", main)
	assert.NotContains(t, main, "select all", "the help panel survived the alternate screen")
}

// runHelpPanelHelper is the child process: a real TUI on a real terminal, pinned to
// the shipped default presentation so the test measures what a user gets.
func runHelpPanelHelper(t *testing.T) {
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
			_, writeErr := fmt.Fprintf(os.Stdout, ";HELP_PTY_EXIT=%s", info.SessionID)

			return writeErr
		},
	})
	if err != nil {
		_, _ = os.Stderr.WriteString(err.Error())
		os.Exit(1)
	}

	os.Exit(0)
}
