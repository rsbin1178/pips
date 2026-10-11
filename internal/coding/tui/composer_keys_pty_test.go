//go:build darwin || linux

//nolint:wsl_v5 // PTY launch, frame reads and teardown follow physical order.
package tui

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const composerKeysPTYEnv = "PIPS_TUI_COMPOSER_KEYS_PTY_HELPER"

// Ctrl+A and Ctrl+X are the two keys this work adds to the Composer, so they are
// read back from a real terminal frame rather than from the update that handled
// them. The frame is tall enough to hold the whole shortcut list, so the bindings
// are asserted where a reader would see them.
func TestPTYComposerSelectAllAndShortcutList(t *testing.T) {
	if os.Getenv(composerKeysPTYEnv) == "1" {
		runComposerKeysHelper(t)

		return
	}
	t.Parallel()

	harness := newPTYHarness(t, composerKeysPTYEnv, "TestPTYComposerSelectAllAndShortcutList", 44, 100)

	require.Eventually(t, func() bool {
		return containsAll(harness.frame(100, 44), "Test Model")
	}, 30*time.Second, 10*time.Millisecond, "helper output: %s", harness.output.String())

	harness.write("a whole draft")

	// Ctrl+A selects the draft and says what the keys do to it.
	harness.write("\x01")
	require.Eventually(t, func() bool {
		return containsAll(harness.frame(100, 44), "all selected")
	}, 30*time.Second, 10*time.Millisecond, "helper output: %s", harness.output.String())

	frame := harness.frame(100, 44)
	assert.Contains(t, frame, "a whole draft", "the draft is kept")
	assert.Contains(t, frame, "all selected")
	t.Logf("selection frame:\n%s", frame)

	// Ctrl+X opens the shortcut panel, which lists the keys this work moved.
	harness.write("\x18")
	require.Eventually(t, func() bool {
		return containsAll(harness.frame(100, 44), "Shortcuts", "select all")
	}, 30*time.Second, 10*time.Millisecond, "helper output: %s", harness.output.String())

	panel := harness.frame(100, 44)
	assert.Contains(t, panel, "ctrl+a")
	assert.Contains(t, panel, "select all")
	assert.Contains(t, panel, "copy reply")
	assert.Contains(t, panel, "Esc to close")
	t.Logf("help panel frame:\n%s", panel)

	// Escape gives the Composer back, and the panel's own footer advertises it.
	harness.write("\x1b")
	require.Eventually(t, func() bool {
		return containsAll(harness.frame(100, 44), "Test Model", "a whole draft")
	}, 30*time.Second, 10*time.Millisecond, "helper output: %s", harness.output.String())

	harness.write("\x0bquit\r")
	result := harness.waitForExit()
	require.NotNil(t, result)
	require.Equal(t, 0, result.ExitCode(), "helper output: %s", harness.output.String())
}

func runComposerKeysHelper(t *testing.T) {
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
	})
	if err != nil {
		_, _ = os.Stderr.WriteString(err.Error())
		os.Exit(1)
	}

	os.Exit(0)
}
