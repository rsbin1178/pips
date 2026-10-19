//go:build darwin || linux

//nolint:wsl_v5 // PTY launch, key sequences and frame reads follow physical order.
package tui

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pagesPTYEnv  = "PIPS_TUI_CHILD_WATCH_PAGE_PTY_HELPER"
	pagesPTYTest = "TestPTYChildAndWatchPagesRenderAsContainers"

	childTailPTYEnv  = "PIPS_TUI_CHILD_PAGE_TAIL_PTY_HELPER"
	childTailPTYTest = "TestPTYChildPageOpensOnItsNewestRow"

	// childTailPreview names the child in the Agent list, and childTailOldest and
	// childTailNewest are the ends of its transcript.
	childTailPreview = "Inspect the long projection"
	childTailOldest  = "CHILD-ROW-000"
	childTailNewest  = "CHILD-ROW-039"
)

// TestPTYChildPageOpensOnItsNewestRow drives the child page on a real terminal with
// a transcript longer than the page. What the reader sees when the page opens is
// its newest row, not the beginning of the child's transcript.
func TestPTYChildPageOpensOnItsNewestRow(t *testing.T) {
	if os.Getenv(childTailPTYEnv) == "1" {
		runRouteClickHelper(t, func(controller *overlayController) {
			controller.agents = []subagent.Summary{{
				ChildSessionID: "child-1", Role: subagent.RoleExplore,
				State: subagent.StateRunning, TaskPreview: childTailPreview,
			}}
			controller.agentDetail = subagent.Detail{
				Summary:    controller.agents[0],
				Transcript: childTailTranscript(),
			}
		})

		return
	}
	t.Parallel()

	const cols, rows = 100, 30

	harness := routeClickHarness(t, childTailPTYEnv, childTailPTYTest, rows, cols)
	openRoutePTY(t, harness, cols, rows, "/agents", "Enter inspect")
	require.Eventually(t, func() bool {
		return strings.Contains(harness.frame(cols, rows), "› "+childTailPreview)
	}, 30*time.Second, 10*time.Millisecond, "the child's run row paints:\n%s", harness.frame(cols, rows))

	harness.write("\r")
	require.Eventually(t, func() bool {
		return containsAll(harness.frame(cols, rows),
			childTailNewest, "explore subagent", subagentHintClose)
	}, 30*time.Second, 10*time.Millisecond, "the child page opens:\n%s", harness.frame(cols, rows))

	assert.NotContains(t, harness.frame(cols, rows), childTailOldest,
		"the page opens on the tail, not at the beginning of the transcript")
	requireInsideWidth(t, harness, cols, rows)

	// Escape leaves the page for the conversation, which is what the footer
	// advertises; the command picker only answers once the route gave the keys back.
	harness.write("\x1b")
	require.Eventually(t, func() bool {
		return !strings.Contains(harness.frame(cols, rows), childTailNewest)
	}, 30*time.Second, 10*time.Millisecond, "Escape leaves the page:\n%s", harness.frame(cols, rows))

	harness.write("\x0bquit\r")
	result := harness.waitForExit()
	require.NotNil(t, result)
	require.Equal(t, 0, result.ExitCode(), "helper output: %s", harness.output.String())
}

// childTailTranscript builds a child transcript longer than any page, so opening it
// can only show its tail.
func childTailTranscript() []ai.Message {
	messages := make([]ai.Message, 0, 40)
	for index := range 40 {
		messages = append(messages, ai.AssistantText(fmt.Sprintf("CHILD-ROW-%03d", index)))
	}

	return messages
}

// TestPTYChildAndWatchPagesRenderAsContainers drives a real terminal through
// both detail pages. What it measures is the frame a user sees: the child's own
// transcript and the watch's own stream fill the width inside the conversation's
// container, with no border and no close control of their own, and Escape is the
// key that leaves them.
func TestPTYChildAndWatchPagesRenderAsContainers(t *testing.T) {
	if os.Getenv(pagesPTYEnv) == "1" {
		runPagesPTYHelper(t)

		return
	}
	t.Parallel()

	const cols, rows = 100, 30

	harness := routeClickHarness(t, pagesPTYEnv, pagesPTYTest, rows, cols)
	require.Eventually(t, func() bool {
		return containsAll(harness.frame(cols, rows), "▾ Subagents 1", "▾ Watchers 1")
	}, 30*time.Second, 10*time.Millisecond, "helper output: %s", harness.output.String())

	// ── The child page ────────────────────────────────────────────────────
	// The hint row is what tells the Agent list apart from the tasks-pane row
	// that already carries the child's task text.
	openRoutePTY(t, harness, cols, rows, "/agents", "Enter inspect")
	require.Eventually(t, func() bool {
		return strings.Contains(harness.frame(cols, rows), "› "+childTaskPreview)
	}, 30*time.Second, 10*time.Millisecond, "the child's run row paints:\n%s", harness.frame(cols, rows))

	harness.write("\r")
	require.Eventually(t, func() bool {
		return containsAll(harness.frame(cols, rows),
			"❯ "+childTaskPreview+".",
			"I will inspect the projection.",
			subagentHintClose,
		)
	}, 30*time.Second, 10*time.Millisecond, "the child page paints:\n%s", harness.frame(cols, rows))

	child := harness.frame(cols, rows)
	assert.NotContains(t, child, "[✗]", "the child page carries no close control")
	assert.NotContains(t, child, "┌", "the child page is not a bordered window")
	assert.NotContains(t, child, "└", "the child page is not a bordered window")
	assert.Contains(t, child, "explore subagent", "the footer names the child")
	requireInsideWidth(t, harness, cols, rows)

	// Escape leaves the page for the conversation, which is what the footer
	// advertises. The Composer's own box is the visible proof the frame is back
	// to the conversation rather than to the Agent list.
	harness.write("\x1b")
	require.Eventually(t, func() bool {
		return strings.Contains(harness.frame(cols, rows), "┌")
	}, 30*time.Second, 10*time.Millisecond, "the conversation returns:\n%s", harness.frame(cols, rows))

	// ── The watch page ────────────────────────────────────────────────────
	harness.write("\x02")   // Ctrl+B focuses the tasks pane.
	harness.write("\x1b[B") // Down moves from the child's row to the watch's.
	harness.write("\r")
	require.Eventually(t, func() bool {
		return containsAll(harness.frame(cols, rows),
			"Watch details",
			"$",
			"tail -f deploy.log",
			"WATCH-LINE-",
		)
	}, 30*time.Second, 10*time.Millisecond, "the watch page paints:\n%s", harness.frame(cols, rows))

	// The page follows the live stream: a later read's line reaches the frame
	// while the task is still running.
	first := watchLineNumber(harness.frame(cols, rows))
	require.Positive(t, first, "the page shows one read:\n%s", harness.frame(cols, rows))
	require.Eventually(t, func() bool {
		return watchLineNumber(harness.frame(cols, rows)) > first
	}, 30*time.Second, 10*time.Millisecond, "the page follows the stream:\n%s", harness.frame(cols, rows))

	watch := harness.frame(cols, rows)
	assert.NotContains(t, watch, "[✗]", "the watch page carries no close control")
	assert.NotContains(t, watch, "┌", "the watch page is not a bordered window")
	requireInsideWidth(t, harness, cols, rows)

	harness.write("\x1b")
	require.Eventually(t, func() bool {
		return !strings.Contains(harness.frame(cols, rows), "Watch details")
	}, 30*time.Second, 10*time.Millisecond, "Escape leaves the watch page:\n%s", harness.frame(cols, rows))

	// Closing the watch page releases the tasks-pane focus with the route, so
	// the command picker answers without another Esc.
	assert.NotContains(t, harness.frame(cols, rows), "› ")

	harness.write("\x0bquit\r")
	result := harness.waitForExit()
	require.NotNil(t, result)
	require.Equal(t, 0, result.ExitCode(), "helper output: %s", harness.output.String())
}

const childTaskPreview = "Inspect the projection"

// watchLinePattern matches one read of the test watch's growing output.
var watchLinePattern = regexp.MustCompile(`WATCH-LINE-(\d{3})`)

// watchLineNumber reads the highest read number the frame carries, so the test
// can tell a re-read from the snapshot the page opened with.
func watchLineNumber(frame string) int {
	highest := 0
	for _, match := range watchLinePattern.FindAllStringSubmatch(frame, -1) {
		value, err := strconv.Atoi(match[1])
		if err == nil && value > highest {
			highest = value
		}
	}

	return highest
}

// watchPagePTYController serves a watch whose output grows on every read, so the
// test can prove the open page follows a live stream and not just a snapshot
// taken when it opened.
type watchPagePTYController struct {
	*overlayController
	reads atomic.Int64
}

// ObserveEvents serves the seeded state once, which is what opens the tasks pane
// the test reaches the watch through.
func (c *watchPagePTYController) ObserveEvents() (coding.EventObservation, error) {
	return coding.EventObservation{
		State: c.state.Clone(), Subscription: &coding.EventSubscription{},
	}, nil
}

func (c *watchPagePTYController) InspectBackgroundTask(
	_ context.Context,
	_ string,
) (coding.BackgroundTaskInspection, error) {
	read := c.reads.Add(1)

	inspection := c.backgroundInspection
	inspection.Output = "deploy starting\n" +
		fmt.Sprintf("WATCH-LINE-%03d\n", read)

	return inspection, nil
}

// runPagesPTYHelper is the child process: a real TUI on a real terminal with one
// child and one watch to open.
func runPagesPTYHelper(t *testing.T) {
	t.Helper()

	controller := &watchPagePTYController{overlayController: newOverlayController(readyState())}
	controller.agents = []subagent.Summary{{
		ChildSessionID: "child-1", Role: subagent.RoleExplore,
		State: subagent.StateRunning, TaskPreview: childTaskPreview,
	}}
	controller.agentDetail = subagent.Detail{
		Summary: controller.agents[0],
		Transcript: []ai.Message{
			ai.UserText(childTaskPreview + "."),
			ai.AssistantText("I will inspect the projection."),
		},
	}
	controller.state.Subagents = []coding.SubagentState{{
		ChildSessionID: "child-1", Role: subagent.RoleExplore,
		State: subagent.StateRunning, TaskPreview: childTaskPreview,
	}}
	controller.state.BackgroundTasks = []coding.BackgroundTaskLifecycle{{
		ID: "monitor-1", Kind: string(background.KindMonitor),
		Description: "deploy log", Command: "tail -f deploy.log",
		StartedAt: time.Now().Add(-12 * time.Second),
	}}
	controller.backgroundInspection = coding.BackgroundTaskInspection{
		Task: background.Snapshot{
			ID: "monitor-1", Kind: background.KindMonitor,
			Description: "deploy log", Command: "tail -f deploy.log",
			Start: time.Now().Add(-12 * time.Second),
		},
	}

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
