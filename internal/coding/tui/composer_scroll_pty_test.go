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

const composerScrollPTYEnv = "PIPS_TUI_COMPOSER_SCROLL_PTY_HELPER"

// TestPTYComposerWheelScrollsATallDraft is the end-to-end guard for the reported
// defect: a draft taller than its box could only be scrolled down, because the
// editor's view follows its caret and every update put it back. The draft here is
// built the way a user builds one — a paste and then newlines — and the frame is read
// back to see the window move, in both directions.
func TestPTYComposerWheelScrollsATallDraft(t *testing.T) {
	if os.Getenv(composerScrollPTYEnv) == "1" {
		runComposerScrollHelper(t)

		return
	}
	t.Parallel()

	harness := newPTYHarness(t, composerScrollPTYEnv, "TestPTYComposerWheelScrollsATallDraft", 44, 100)

	require.Eventually(t, func() bool {
		return containsAll(harness.frame(100, 44), "Test Model")
	}, 30*time.Second, 10*time.Millisecond, "helper output: %s", harness.output.String())

	// Eight lines arrive as one paste, which is the most a paste inserts verbatim; the
	// rest are typed as newlines, so the draft ends up taller than the eight-row box.
	harness.write("\x1b[200~" + strings.Join(composerScrollLines(0, 8), "\n") + "\x1b[201~")
	for index := 8; index < 12; index++ {
		harness.write("\x0a" + composerScrollLine(index))
	}

	require.Eventually(t, func() bool {
		frame := harness.frame(100, 44)

		return containsAll(frame, composerScrollLine(11)) && !strings.Contains(frame, composerScrollLine(0))
	}, 30*time.Second, 10*time.Millisecond, "helper output: %s", harness.output.String())

	row := composerBoxRow(t, harness.frame(100, 44))
	require.Positive(t, row, "the frame draws the Composer box")

	// Up: the window follows the caret to the top of the draft.
	for range 5 {
		harness.write(sgrMouse(64, 4, row, false))
	}
	require.Eventually(t, func() bool {
		return containsAll(harness.frame(100, 44), composerScrollLine(0))
	}, 30*time.Second, 10*time.Millisecond, "helper output: %s", harness.output.String())

	scrolled := harness.frame(100, 44)
	assert.Contains(t, scrolled, composerScrollLine(0), "the top of the draft is readable")
	assert.NotContains(t, scrolled, composerScrollLine(11), "and the window moved off the bottom")
	t.Logf("scrolled frame:\n%s", scrolled)

	// Down: it comes back, so the wheel is not a one-way gesture.
	for range 5 {
		harness.write(sgrMouse(65, 4, row, false))
	}
	require.Eventually(t, func() bool {
		return containsAll(harness.frame(100, 44), composerScrollLine(11))
	}, 30*time.Second, 10*time.Millisecond, "helper output: %s", harness.output.String())

	harness.write("\x0bquit\r")
	result := harness.waitForExit()
	require.NotNil(t, result)
	require.Equal(t, 0, result.ExitCode(), "helper output: %s", harness.output.String())
}

// composerScrollLines are the pasted lines, numbered so the frame says which part of
// the draft is on screen.
func composerScrollLines(from, to int) []string {
	lines := make([]string, 0, to-from)
	for index := from; index < to; index++ {
		lines = append(lines, composerScrollLine(index))
	}

	return lines
}

func composerScrollLine(index int) string {
	return fmt.Sprintf("DRAFT-%02d", index)
}

// composerBoxRow is the one-based frame row of the Composer's top border, which is
// where a wheel notch has to land to belong to the draft.
func composerBoxRow(t *testing.T, frame string) int {
	t.Helper()

	for index, line := range strings.Split(frame, "\n") {
		if strings.Contains(line, "┌") {
			return index + 1
		}
	}

	return 0
}

func runComposerScrollHelper(t *testing.T) {
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
