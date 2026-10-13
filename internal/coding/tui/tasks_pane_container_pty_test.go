//go:build darwin || linux

//nolint:wsl_v5 // PTY launch, frame reads and teardown follow physical order.
package tui

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const tasksPaneRowsPTYEnv = "PIPS_TUI_TASKS_PANE_ROWS_PTY_HELPER"

// TestPTYTasksPaneDrawsTheRowsBelowTheStatusLine drives a real terminal with a
// running child and a running watch. The pane opens itself for them, and what the
// test measures is the frame a user sees: the pane is the last band, below the
// status line, inside the width, and every section it names has one of its rows
// under the header.
func TestPTYTasksPaneDrawsTheRowsBelowTheStatusLine(t *testing.T) {
	const testName = "TestPTYTasksPaneDrawsTheRowsBelowTheStatusLine"
	const rows, cols = 30, 100

	if os.Getenv(tasksPaneRowsPTYEnv) == "1" {
		runTasksPaneRowsHelper(t)

		return
	}
	t.Parallel()

	harness := newPTYHarness(t, tasksPaneRowsPTYEnv, testName, rows, cols)

	var frame string
	require.Eventually(t, func() bool {
		frame = harness.frame(cols, rows)

		return strings.Contains(frame, "▾ Subagents 1") &&
			strings.Contains(frame, "▾ Watchers 1")
	}, 30*time.Second, 10*time.Millisecond, "helper output: %s", harness.output.String())

	requireInsideWidth(t, harness, cols, rows)

	lines := strings.Split(frame, "\n")
	indexOf := func(value string) int {
		return slices.IndexFunc(lines, func(line string) bool {
			return strings.Contains(line, value)
		})
	}

	subagents := indexOf("▾ Subagents 1")
	require.NotEqual(t, -1, subagents, "the pane is painted:\n%s", frame)
	require.Greater(t, subagents, 1)
	assert.Contains(t, lines[subagents-1], "Test Model", "the status line stays above the pane")

	// Every section the pane names has one of its rows under its header, and the
	// terminal's own diffing is why this reads the frame's order rather than a
	// fixed offset.
	watchers := indexOf("▾ Watchers 1")
	child := indexOf("fix the login bug")
	watch := indexOf(backgroundTaskTag + " deploy log")
	require.NotEqual(t, -1, watchers)
	require.NotEqual(t, -1, child)
	require.NotEqual(t, -1, watch)
	assert.Greater(t, child, subagents, "the child's row follows its header")
	assert.Greater(t, watch, watchers, "the watch's row follows its header")
	assert.Greater(t, watchers, child, "the sections keep their display order")

	harness.write("\x0bquit\r")
	result := harness.waitForExit()
	require.NotNil(t, result)
	require.Equal(t, 0, result.ExitCode(), "helper output: %s", harness.output.String())
}

// tasksPaneRowsController serves one seeded state, so the helper's terminal shows
// rows without a live Runtime behind it.
type tasksPaneRowsController struct {
	stubController
}

func (c *tasksPaneRowsController) ObserveEvents() (coding.EventObservation, error) {
	return coding.EventObservation{
		State: c.state.Clone(), Subscription: &coding.EventSubscription{},
	}, nil
}

// runTasksPaneRowsHelper is the child process: a real TUI on a real terminal with
// one running child and one running watch.
func runTasksPaneRowsHelper(t *testing.T) {
	t.Helper()

	controller, ok := reasoningController(t, reasoningModelConfig(), nil).(stubController)
	require.True(t, ok)

	seeded := &tasksPaneRowsController{stubController: controller}
	seeded.state.Subagents = []coding.SubagentState{liveTasksChild("child-1", "fix the login bug")}
	seeded.state.BackgroundTasks = []coding.BackgroundTaskLifecycle{{
		ID: "monitor-1", Kind: string(background.KindMonitor),
		Description: "deploy log", StartedAt: time.Now().Add(-12 * time.Second),
	}}

	err := Run(context.Background(), Options{
		Input: os.Stdin, Output: os.Stdout, Environment: os.Environ(),
		Workspace: t.TempDir(), Trusted: true, NoColor: true,
		PinPresentation: true, Screen: ScreenFullscreen, AltScreen: AltScreenAlways,
		MouseReporting: true,
		Bootstrap: func(context.Context, bool) (Controller, error) {
			return seeded, nil
		},
		OnExit: func(info ExitInfo) error {
			_, writeErr := fmt.Fprintf(os.Stdout, ";TASKS_PANE_ROWS_PTY_EXIT=%s", info.SessionID)

			return writeErr
		},
	})
	if err != nil {
		_, _ = os.Stderr.WriteString(err.Error())
		os.Exit(1)
	}

	os.Exit(0)
}
