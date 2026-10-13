//go:build darwin || linux

//nolint:wsl_v5 // PTY launch, frame reads and teardown follow physical order.
package tui

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const tasksPanePTYEnv = "PIPS_TUI_TASKS_PANE_PTY_HELPER"

// The helper's two finished children and the conversation rows the wheel test
// measures the transcript against.
const (
	tasksPanePTYFirstTask  = "PTY-FIRST-TASK"
	tasksPanePTYSecondTask = "PTY-SECOND-TASK"
	tasksPanePTYRows       = 40
)

// TestPTYTasksPaneEmptyToggleAndWheel drives the pane on a real terminal: the
// chord reveals it with the finished rows because nothing is running, a wheel
// notch over it moves its own selection while the conversation stays put, and the
// real Ctrl+H byte empties it again so the composer gets the frame's keys back.
func TestPTYTasksPaneEmptyToggleAndWheel(t *testing.T) {
	const testName = "TestPTYTasksPaneEmptyToggleAndWheel"
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

	// Nothing is running, so the pane is hidden: it paints no header, no note, and
	// the conversation keeps the whole frame.
	frame := harness.frame(cols, rows)
	require.NotContains(t, frame, "Subagents", "a hidden pane paints nothing")
	require.NotContains(t, frame, "No running tasks", "the empty-state note is gone")
	require.Positive(t, highestTranscriptRow(frame), "the fixture conversation is on screen")

	// Ctrl+B reveals it with the finished rows, because nothing is running.
	harness.write("\x02")
	require.Eventually(t, func() bool {
		return containsAll(harness.frame(cols, rows),
			tasksPaneHeaderExpanded+" Subagents 2", tasksPanePTYFirstTask, tasksPanePTYSecondTask)
	}, 30*time.Second, 10*time.Millisecond, "helper output: %s", harness.output.String())
	requireInsideWidth(t, harness, cols, rows)

	selected := frameLineWith(harness, cols, rows, tasksPanePTYFirstTask)
	require.True(t, strings.HasPrefix(selected, "› "),
		"the focused pane marks its first row:\n%s", harness.frame(cols, rows))

	// A wheel notch inside the pane moves the pane's own selection, and the
	// conversation underneath never moves.
	before := highestTranscriptRow(harness.frame(cols, rows))
	row := frameRowOfText(t, harness, cols, rows, tasksPanePTYFirstTask)
	harness.write(sgrMouse(65, 4, row+1, false))
	require.Eventually(t, func() bool {
		return strings.HasPrefix(frameLineWith(harness, cols, rows, tasksPanePTYSecondTask), "› ")
	}, 30*time.Second, 10*time.Millisecond, "the notch moved the pane:\n%s", harness.frame(cols, rows))
	assert.Equal(t, before, highestTranscriptRow(harness.frame(cols, rows)),
		"a notch over the pane never scrolls the conversation")

	// The real Ctrl+H byte turns the finished rows off. Nothing is running, so the
	// pane has nothing to list: it paints nothing and the composer takes the keys.
	harness.write("\x08")
	require.Eventually(t, func() bool {
		return !strings.Contains(harness.frame(cols, rows), "Subagents")
	}, 30*time.Second, 10*time.Millisecond, "helper output: %s", harness.output.String())
	require.NotContains(t, harness.frame(cols, rows), "No running tasks", "no note replaces the rows")

	harness.write("x")
	require.Eventually(t, func() bool {
		return strings.Contains(harness.frame(cols, rows), "❯ x")
	}, 30*time.Second, 10*time.Millisecond, "the composer kept the frame's keys:\n%s",
		harness.frame(cols, rows))

	// The other spelling of the same toggle — the byte a modern Backspace sends —
	// brings the rows back and takes them away again.
	harness.write("\x02")
	require.Eventually(t, func() bool {
		return strings.Contains(harness.frame(cols, rows), tasksPaneHeaderExpanded+" Subagents 2")
	}, 30*time.Second, 10*time.Millisecond, "helper output: %s", harness.output.String())

	harness.write("\x7f")
	require.Eventually(t, func() bool {
		return !strings.Contains(harness.frame(cols, rows), "Subagents")
	}, 30*time.Second, 10*time.Millisecond, "helper output: %s", harness.output.String())

	harness.write("\x0bquit\r")
	result := harness.waitForExit()
	require.NotNil(t, result)
	require.Equal(t, 0, result.ExitCode(), "helper output: %s", harness.output.String())
}

// tasksPanePTYRowPattern matches one of the fixture conversation's own rows.
var tasksPanePTYRowPattern = regexp.MustCompile(`ROW-(\d{2})`)

// highestTranscriptRow reads the newest conversation row on screen, so a test can
// tell a scrolled transcript from one that never moved.
func highestTranscriptRow(frame string) int {
	highest := -1
	for _, match := range tasksPanePTYRowPattern.FindAllStringSubmatch(frame, -1) {
		value, err := strconv.Atoi(match[1])
		if err == nil && value > highest {
			highest = value
		}
	}

	return highest
}

// frameRowOfText returns the frame row carrying needle.
func frameRowOfText(t *testing.T, h *ptyHarness, cols, rows int, needle string) int {
	t.Helper()

	for index, line := range strings.Split(h.frame(cols, rows), "\n") {
		if strings.Contains(line, needle) {
			return index
		}
	}
	t.Fatalf("%q is not on screen:\n%s", needle, h.frame(cols, rows))

	return -1
}

// tasksPanePTYController serves a conversation with two finished children, so the
// pane has rows to reveal while nothing is running.
type tasksPanePTYController struct {
	stubController
}

func (c *tasksPanePTYController) ObserveEvents() (coding.EventObservation, error) {
	return coding.EventObservation{
		State: c.state.Clone(), Subscription: &coding.EventSubscription{},
	}, nil
}

// runTasksPaneHelper is the child process: a real TUI on a real terminal, pinned
// to the shipped default presentation so the test measures what a user gets.
func runTasksPaneHelper(t *testing.T) {
	t.Helper()

	controller, ok := reasoningController(t, reasoningModelConfig(), nil).(stubController)
	require.True(t, ok)

	seeded := &tasksPanePTYController{stubController: controller}
	for index := range tasksPanePTYRows {
		seeded.state.Transcript = append(
			seeded.state.Transcript,
			ai.UserText(fmt.Sprintf("ROW-%02d", index)),
		)
	}
	for _, task := range []string{tasksPanePTYFirstTask, tasksPanePTYSecondTask} {
		seeded.state.Subagents = append(seeded.state.Subagents, coding.SubagentState{
			ChildSessionID: "child-" + task, Role: subagent.RoleExplore,
			State: subagent.StateSucceeded, TaskPreview: task, DurationMillis: 4_000,
		})
	}

	err := Run(context.Background(), Options{
		Input: os.Stdin, Output: os.Stdout, Environment: os.Environ(),
		Workspace: t.TempDir(), Trusted: true, NoColor: true,
		PinPresentation: true, Screen: ScreenFullscreen, AltScreen: AltScreenAlways,
		MouseReporting: true,
		Bootstrap: func(context.Context, bool) (Controller, error) {
			return seeded, nil
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
