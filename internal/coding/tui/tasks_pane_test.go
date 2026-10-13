//nolint:wsl_v5 // The pane's fixtures and assertions read as one contract table.
package tui

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/background"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tasksPaneNow anchors the pane's elapsed column so a test states the text it
// expects instead of racing the wall clock.
var tasksPaneNow = time.Date(2026, time.August, 2, 10, 0, 0, 0, time.UTC)

func liveTasksChild(id, task string) coding.SubagentState {
	return coding.SubagentState{
		ChildSessionID: id, Role: subagent.RoleExplore,
		State: subagent.StateRunning, TaskPreview: task, Model: "openai/test",
	}
}

func tasksPaneModel(t *testing.T) (*Model, *overlayController) {
	t.Helper()

	controller := newOverlayController(readyState())
	model := readyModelWithController(t, controller, true)

	return model, controller
}

func TestTasksPaneOpensAndClosesOnRunningCountTransitions(t *testing.T) {
	t.Parallel()

	model, _ := tasksPaneModel(t)
	model.state.Subagents = []coding.SubagentState{liveTasksChild("child-1", "fix the login bug")}

	model.syncTasksPane(tasksPaneNow)
	assert.True(t, model.tasksPane.visible, "the first running child opens the pane")
	assert.True(t, model.tasksPane.openedByAuto)

	content := model.View().Content
	assert.Contains(t, content, "▾ Subagents 1")
	assert.Contains(t, content, "fix the login bug")
	assert.Contains(t, content, tasksPaneInspectLabel)
	assert.Contains(t, content, tasksPaneKillLabel, "a running row affords the kill control")

	model.state.Subagents[0].State = subagent.StateSucceeded
	model.state.Subagents[0].DurationMillis = 4_000
	model.syncTasksPane(tasksPaneNow)
	assert.False(t, model.tasksPane.visible, "the last child stopping closes the pane")
	assert.NotContains(t, model.View().Content, "Subagents 1")
	assert.NotContains(t, model.View().Content, "fix the login bug")
}

func TestTasksPaneStaysOpenWhileAChildKeepsRunning(t *testing.T) {
	t.Parallel()

	model, _ := tasksPaneModel(t)
	model.state.Subagents = []coding.SubagentState{
		liveTasksChild("child-1", "first child"),
		liveTasksChild("child-2", "second child"),
	}

	model.syncTasksPane(tasksPaneNow)
	require.True(t, model.tasksPane.visible)

	model.state.Subagents[0].State = subagent.StateSucceeded
	model.syncTasksPane(tasksPaneNow)
	assert.True(t, model.tasksPane.visible, "one child stopping leaves the pane up")
	assert.Contains(t, model.View().Content, "▾ Subagents 1")
	assert.NotContains(t, model.View().Content, "first child")
}

func TestTasksPaneShowDoneListsFinishedRowsRunningFirst(t *testing.T) {
	t.Parallel()

	model, _ := tasksPaneModel(t)
	finished := liveTasksChild("child-1", "finished child")
	finished.State = subagent.StateSucceeded
	finished.DurationMillis = 23_000
	model.state.Subagents = []coding.SubagentState{
		finished,
		liveTasksChild("child-2", "running child"),
	}

	model.syncTasksPane(tasksPaneNow)
	// Finished rows are hidden until the reader asks for them.
	assert.NotContains(t, model.View().Content, "finished child")

	model.tasksPane.showDone = true
	content := model.View().Content
	assert.Contains(t, content, "▾ Subagents 2")
	assert.Less(t, strings.Index(content, "running child"), strings.Index(content, "finished child"),
		"a running row sorts above a finished one")

	model.syncTasksPane(tasksPaneNow)
	assert.True(t, model.tasksPane.visible, "show_done keeps the pane up with nothing running")
}

// TestTasksPaneWithNothingToListPaintsNothing pins the empty pane: no note, no
// band row, no pointer rows, and no key ownership, so the composer keeps the
// frame's keys instead of an invisible pane swallowing them.
func TestTasksPaneWithNothingToListPaintsNothing(t *testing.T) {
	t.Parallel()

	model, _ := tasksPaneModel(t)
	model.revealTasksPane()
	require.Empty(t, model.tasksPaneRows())
	require.True(t, model.tasksPane.visible)

	assert.Zero(t, model.tasksPaneCapacity(), "an empty pane has no capacity")
	assert.Empty(t, model.tasksPaneBand(tasksPaneNow), "the band contributes no row")
	assert.False(t, model.tasksPaneHit.painted, "the pane owns no pointer row")
	assert.False(t, model.tasksPaneFocused(), "an empty pane owns no key")

	model.toggleTasksPane()
	assert.True(t, model.tasksPane.focused, "the chord still focuses the pane")
	assert.False(t, model.tasksPaneFocused(), "focus alone does not take the keys")

	model.Update(key("x"))
	assert.Equal(t, "x", model.composer.Value(), "the composer keeps the frame's keys")
}

// TestTasksPanePaintsNothingOnceEveryRowHasStopped pins the state a reader lands
// in after the last child stops while the pane is focused: the finished row is
// hidden, so the pane paints nothing at all rather than a note about itself.
func TestTasksPanePaintsNothingOnceEveryRowHasStopped(t *testing.T) {
	t.Parallel()

	model, _ := tasksPaneModel(t)
	model.state.Subagents = []coding.SubagentState{liveTasksChild("child-1", "a running child")}
	model.syncTasksPane(tasksPaneNow)
	model.toggleTasksPane()
	require.True(t, model.tasksPane.focused)

	model.state.Subagents[0].State = subagent.StateSucceeded
	model.syncTasksPane(tasksPaneNow)
	require.True(t, model.tasksPane.visible, "a focused pane is not closed under the reader")
	require.Empty(t, model.tasksPaneRows(), "the finished row stays hidden until asked for")

	band, hits := model.tasksPanePaint(tasksPaneNow, model.tasksPaneCapacity())
	assert.Empty(t, band, "an empty pane paints no note")
	assert.Empty(t, hits, "and no row a click could resolve")
	assert.False(t, model.tasksPaneHit.painted)

	model.Update(key("x"))
	assert.Equal(t, "x", model.composer.Value(), "the composer gets the keys back")
}

// TestTasksPaneDoesNotTakeTheKeysBackOnItsOwn pins the focus rule across an empty
// spell: the focus is dropped with the rows, so a child starting while the reader
// types cannot silently take a keystroke meant for the composer.
func TestTasksPaneDoesNotTakeTheKeysBackOnItsOwn(t *testing.T) {
	t.Parallel()

	model, _ := tasksPaneModel(t)
	model.state.Subagents = []coding.SubagentState{liveTasksChild("child-1", "first child")}
	model.syncTasksPane(tasksPaneNow)
	model.toggleTasksPane()
	require.True(t, model.tasksPaneFocused(), "the focused pane owns the keys while a child runs")

	// The child stops and its row is hidden, so the pane has nothing to list.
	model.state.Subagents[0].State = subagent.StateSucceeded
	model.syncTasksPane(tasksPaneNow)
	require.False(t, model.tasksPaneFocused(), "the empty pane gives the keys back")

	// A new child starts while the reader is typing.
	model.state.Subagents = []coding.SubagentState{liveTasksChild("child-2", "second child")}
	model.syncTasksPane(tasksPaneNow)
	require.True(t, model.tasksPane.visible, "the running child reveals the pane")
	assert.False(t, model.tasksPaneFocused(), "the refilled pane does not take the keys back")

	model.Update(key("x"))
	assert.Equal(t, "x", model.composer.Value(), "the keystroke stays with the composer")
}

// TestTasksPaneRevealTurnsFinishedRowsOnWhenNothingRuns pins the reader's "show me
// everything" gesture: the chord that reveals the pane turns the finished rows on
// when nothing is running, so revealing it shows the finished children and stopped
// watches instead of an empty strip.
func TestTasksPaneRevealTurnsFinishedRowsOnWhenNothingRuns(t *testing.T) {
	t.Parallel()

	model, _ := tasksPaneModel(t)
	finished := liveTasksChild("child-1", "finished child")
	finished.State = subagent.StateSucceeded
	finished.DurationMillis = 4_000
	model.state.Subagents = []coding.SubagentState{finished}
	model.state.BackgroundTasks = []coding.BackgroundTaskLifecycle{{
		ID: "monitor-1", Kind: string(background.KindMonitor),
		Description: "stopped watch", Completed: true,
		StartedAt: tasksPaneNow.Add(-9 * time.Second), EndedAt: tasksPaneNow,
	}}

	model.syncTasksPane(tasksPaneNow)
	require.False(t, model.tasksPane.visible, "nothing running leaves the pane hidden")
	require.Empty(t, model.tasksPaneRows())

	model.Update(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})
	require.True(t, model.tasksPane.showDone, "the chord turns the finished rows on")

	content := model.View().Content
	assert.Contains(t, content, "▾ Subagents 1")
	assert.Contains(t, content, "finished child")
	assert.Contains(t, content, "▾ Watchers 1")
	assert.Contains(t, content, "stopped watch")
}

func TestTasksPaneRowMetaCarriesElapsedActivityAndModel(t *testing.T) {
	t.Parallel()

	model, _ := tasksPaneModel(t)
	child := liveTasksChild("child-1", "audit the pane")
	child.Activity = subagent.ActivitySummary{
		Action: subagent.ActivityActionRead, Target: "model.go",
	}
	model.state.Subagents = []coding.SubagentState{child}
	model.syncTasksPane(tasksPaneNow)
	model.tasksPane.seen["child-1"] = tasksPaneNow.Add(-90 * time.Second)

	band, _ := model.tasksPanePaint(tasksPaneNow, model.tasksPaneCapacity())
	assert.Contains(t, band, "1m 30s")
	assert.Contains(t, band, "Read model.go")
	assert.Contains(t, band, "openai/test")
	assert.Contains(t, band, "▾ Subagents 1")
}

func TestTasksPaneHidesBelowTheMinimumViewHeight(t *testing.T) {
	t.Parallel()

	model, _ := tasksPaneModel(t)
	model.state.Subagents = []coding.SubagentState{liveTasksChild("child-1", "fix the login bug")}
	model.syncTasksPane(tasksPaneNow)
	require.True(t, model.tasksPane.visible)

	model.Update(tea.WindowSizeMsg{Width: defaultWidth, Height: tasksPaneMinViewHeight - 1})
	assert.NotContains(t, model.View().Content, "fix the login bug")
	assert.Empty(t, model.tasksPaneBand(tasksPaneNow))
}

func TestTasksPaneCapacityCapsTheStrip(t *testing.T) {
	t.Parallel()

	model, _ := tasksPaneModel(t)
	children := make([]coding.SubagentState, 0, tasksPaneMaxRows+4)
	for index := range tasksPaneMaxRows + 4 {
		children = append(children, liveTasksChild(
			"child-"+string(rune('a'+index)), "child task",
		))
	}
	model.state.Subagents = children
	model.syncTasksPane(tasksPaneNow)

	capacity := model.tasksPaneCapacity()
	require.Positive(t, capacity)
	require.LessOrEqual(t, capacity, tasksPaneMaxRows)

	band, hits := model.tasksPanePaint(tasksPaneNow, capacity)
	assert.Len(t, strings.Split(band, "\n"), capacity)
	assert.Len(t, hits, capacity)
	assert.Equal(t, 1, strings.Count(band, "Subagents"), "the header is painted once")
}

func TestTasksPaneKeysKillAndInspect(t *testing.T) {
	t.Parallel()

	t.Run("Ctrl+B focuses the pane and Tab leaves it", func(t *testing.T) {
		t.Parallel()

		model, _ := tasksPaneModel(t)
		model.state.Subagents = []coding.SubagentState{liveTasksChild("child-1", "fix the login bug")}
		model.syncTasksPane(tasksPaneNow)

		model.Update(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})
		assert.True(t, model.tasksPane.focused)
		assert.False(t, model.tasksPane.showDone,
			"a running child keeps the finished rows hidden")
		assert.Contains(t, model.View().Content, "› ", "a focused pane marks its selection")

		model.Update(key("tab"))
		assert.False(t, model.tasksPane.focused)
	})

	t.Run("x kills the selected running child", func(t *testing.T) {
		t.Parallel()

		model, controller := tasksPaneModel(t)
		model.state.Subagents = []coding.SubagentState{liveTasksChild("child-1", "fix the login bug")}
		model.syncTasksPane(tasksPaneNow)
		model.toggleTasksPane()

		_, command := model.Update(key("x"))
		require.NotNil(t, command)
		driveModelCommands(t, model, command)
		assert.Equal(t, []string{"child-1"}, controller.agentCanceled)
	})

	t.Run("x does nothing on a finished row", func(t *testing.T) {
		t.Parallel()

		model, controller := tasksPaneModel(t)
		finished := liveTasksChild("child-1", "finished child")
		finished.State = subagent.StateSucceeded
		model.state.Subagents = []coding.SubagentState{finished}
		model.tasksPane.showDone = true
		model.revealTasksPane()
		model.toggleTasksPane()

		_, command := model.Update(key("x"))
		assert.Nil(t, command)
		assert.Empty(t, controller.agentCanceled)
	})

	t.Run("Enter inspects and Ctrl+F inspects too", func(t *testing.T) {
		t.Parallel()

		for _, open := range []tea.KeyPressMsg{key("enter"), {Code: 'f', Mod: tea.ModCtrl}} {
			model, controller := tasksPaneModel(t)
			model.state.Subagents = []coding.SubagentState{liveTasksChild("child-1", "fix the login bug")}
			model.syncTasksPane(tasksPaneNow)
			model.toggleTasksPane()

			_, command := model.Update(open)
			require.NotNil(t, command)
			driveModelCommands(t, model, command)
			assert.Equal(t, routeSubagent, model.route.kind)
			assert.Equal(t, []string{"child-1"}, controller.agentInspections)
		}
	})

	t.Run("Ctrl+H toggles the finished rows and a bare h does not", func(t *testing.T) {
		t.Parallel()

		model, _ := tasksPaneModel(t)
		model.state.Subagents = []coding.SubagentState{liveTasksChild("child-1", "fix the login bug")}
		model.syncTasksPane(tasksPaneNow)
		model.toggleTasksPane()

		model.Update(tea.KeyPressMsg{Code: 'h', Mod: tea.ModCtrl})
		assert.True(t, model.tasksPane.showDone)
		model.Update(tea.KeyPressMsg{Code: 'h', Mod: tea.ModCtrl})
		assert.False(t, model.tasksPane.showDone)

		// The other spelling a terminal can send for the same key: the byte a
		// legacy terminal sends for Backspace is Ctrl+H's own code, so the pane
		// answers it too.
		model.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
		assert.True(t, model.tasksPane.showDone)
		model.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
		assert.False(t, model.tasksPane.showDone)

		model.Update(key("h"))
		assert.False(t, model.tasksPane.showDone, "a bare h is no longer the toggle")
	})

	t.Run("the selection moves and clamps", func(t *testing.T) {
		t.Parallel()

		model, _ := tasksPaneModel(t)
		model.state.Subagents = []coding.SubagentState{
			liveTasksChild("child-1", "aaa"),
			liveTasksChild("child-2", "bbb"),
		}
		model.syncTasksPane(tasksPaneNow)
		model.toggleTasksPane()

		model.Update(key("j"))
		assert.Equal(t, 1, model.tasksPane.cursor)
		model.Update(key("j"))
		assert.Equal(t, 1, model.tasksPane.cursor, "the cursor clamps at the last row")
		model.Update(key("k"))
		model.Update(key("k"))
		assert.Equal(t, 0, model.tasksPane.cursor)
	})

	t.Run("an unfocused pane leaves the composer alone", func(t *testing.T) {
		t.Parallel()

		model, controller := tasksPaneModel(t)
		model.state.Subagents = []coding.SubagentState{liveTasksChild("child-1", "fix the login bug")}
		model.syncTasksPane(tasksPaneNow)
		require.True(t, model.tasksPane.visible)
		require.False(t, model.tasksPane.focused)

		model.Update(key("x"))
		assert.Empty(t, controller.agentCanceled, "x is the composer's while the pane is unfocused")
		assert.Equal(t, "x", model.composer.Value())
	})
}

func TestTasksPaneListsWatchesWithTheirTwoActions(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.backgroundInspection = coding.BackgroundTaskInspection{
		Task: background.Snapshot{
			ID: "monitor-1", Kind: background.KindMonitor,
			Description: "deploy log", Command: "tail -f deploy.log",
			Start: tasksPaneNow.Add(-12 * time.Second),
		},
		Output: "line one\nline two\n",
	}
	model := readyModelWithController(t, controller, true)
	model.state.BackgroundTasks = []coding.BackgroundTaskLifecycle{{
		ID: "monitor-1", Kind: string(background.KindMonitor),
		Description: "deploy log", Command: "tail -f deploy.log",
		StartedAt: tasksPaneNow.Add(-12 * time.Second),
	}}

	model.syncTasksPane(tasksPaneNow)
	require.True(t, model.tasksPane.visible, "a running watch opens the pane")

	content := model.View().Content
	assert.Contains(t, content, "▾ Watchers 1")
	assert.Contains(t, content, backgroundTaskTag+" deploy log")
	assert.Contains(t, content, tasksPaneKillLabel, "a running watch affords the kill control")
	assert.NotContains(t, content, "▾ Subagents")

	rows := model.tasksPaneRows()
	require.Len(t, rows, 1)
	assert.Equal(t, tasksPaneWatchers, tasksPaneGroupOf(rows[0]))
	assert.Equal(t, "12s", model.tasksPaneRowMeta(rows[0], tasksPaneNow),
		"a watch shows its own elapsed time")

	model.toggleTasksPane()

	// x stops the watch through the control plane the model's stop Tool uses.
	_, command := model.Update(key("x"))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)
	assert.Equal(t, []string{"monitor-1"}, controller.backgroundKilled)

	// Enter opens the bounded detail view, because a watch has no transcript.
	_, command = model.Update(key("enter"))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)
	assert.Equal(t, []string{"monitor-1"}, controller.backgroundInspected)
	assert.Equal(t, routeToolDetail, model.route.kind)
	require.NotNil(t, model.route.toolDetail)
	assert.Equal(t, "Watch details", model.route.toolDetail.title)

	detail := model.toolDetailRouteContent()
	assert.Contains(t, detail, "deploy log")
	assert.Contains(t, detail, "running")
	// The page carries the watch's own command the way the shell detail surface
	// carries a script, so the reader can see what is being watched.
	assert.Contains(t, detail, "$")
	assert.Contains(t, detail, "tail -f deploy.log")
	assert.Contains(t, detail, "line one")
}

// TestWatchPageFollowsARunningTask pins the background-task viewer's mechanism:
// while the task runs the open page re-reads its output and stays on the tail,
// and once the task stops both the re-read and the clock stop with it.
func TestWatchPageFollowsARunningTask(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.backgroundInspection = coding.BackgroundTaskInspection{
		Task: background.Snapshot{
			ID: "monitor-1", Kind: background.KindMonitor,
			Description: "deploy log", Command: "tail -f deploy.log",
			Start: tasksPaneNow.Add(-12 * time.Second),
		},
		Output: "line one\n",
	}
	model := readyModelWithController(t, controller, true)
	driveModelCommands(t, model, model.inspectBackgroundTask("monitor-1"))
	require.Equal(t, routeToolDetail, model.route.kind)
	require.True(t, model.watchPageActivityVisible(), "a running watch keeps the clock")
	assert.Contains(t, model.toolDetailRouteContent(), "line one")

	// The activity clock drives the re-read, so one tick asks for the next one.
	_, command := model.Update(activityTickMsg{})
	require.NotNil(t, command)
	assert.True(t, model.route.watchFollowPending, "the tick asked for the next read")

	// The next line the watch prints lands on the page the reader already has.
	controller.backgroundInspection.Output = "line one\nline two\n"
	for _, message := range commandMessages(t, command) {
		model.Update(message)
	}
	assert.Contains(t, model.toolDetailRouteContent(), "line two")
	assert.False(t, model.route.watchFollowPending)

	// A finished watch stops following, so the clock stops with it.
	controller.backgroundInspection.Task.Completed = true
	driveModelCommands(t, model, model.followOpenWatchPage())
	assert.False(t, model.watchPageActivityVisible(), "a finished watch stops the clock")
	assert.Nil(t, model.followOpenWatchPage())
	assert.Contains(t, model.toolDetailRouteContent(), "exited (code 0)")
}

// TestWatchPageFollowsOnlyWhileTheReaderHoldsTheTail pins the follow rule: a
// reader who scrolled up keeps their place while new lines arrive, and coming
// back to the tail follows again.
func TestWatchPageFollowsOnlyWhileTheReaderHoldsTheTail(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.backgroundInspection = coding.BackgroundTaskInspection{
		Task: background.Snapshot{
			ID: "monitor-1", Kind: background.KindMonitor, Description: "deploy log",
			Start: tasksPaneNow,
		},
		Output: strings.Repeat("line\n", 200),
	}
	model := readyModelWithController(t, controller, true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 12})
	driveModelCommands(t, model, model.inspectBackgroundTask("monitor-1"))
	require.Equal(t, routeToolDetail, model.route.kind)
	maximum := model.toolDetailMaximumOffset()
	require.Positive(t, maximum, "the page overflows its body")
	assert.Equal(t, maximum, model.route.offset, "a running watch opens on its tail")

	model.route.offset = 0
	driveModelCommands(t, model, model.followOpenWatchPage())
	assert.Equal(t, 0, model.route.offset, "a reader above the tail keeps their place")

	model.route.offset = model.toolDetailMaximumOffset()
	controller.backgroundInspection.Output = strings.Repeat("line\n", 220)
	driveModelCommands(t, model, model.followOpenWatchPage())
	assert.Equal(t, model.toolDetailMaximumOffset(), model.route.offset,
		"a reader holding the tail keeps following it")
}

// TestWatchFollowDoesNotReopenAClosedPage keeps a read that lands after the
// reader left from reopening the route behind them.
func TestWatchFollowDoesNotReopenAClosedPage(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.backgroundInspection = coding.BackgroundTaskInspection{
		Task: background.Snapshot{
			ID: "monitor-1", Kind: background.KindMonitor, Description: "deploy log",
			Start: tasksPaneNow.Add(-12 * time.Second),
		},
	}
	model := readyModelWithController(t, controller, true)
	driveModelCommands(t, model, model.inspectBackgroundTask("monitor-1"))
	require.Equal(t, routeToolDetail, model.route.kind)

	follow := model.followOpenWatchPage()
	require.NotNil(t, follow)
	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Equal(t, routeNone, model.route.kind)

	driveModelCommands(t, model, follow)
	assert.Equal(t, routeNone, model.route.kind, "the stale read does not reopen the page")
}

// TestTasksPaneHidesAFinishedWatchUnlessAsked checks the show-done rule applies
// to a watch the same way it does to a child.
func TestTasksPaneHidesAFinishedWatchUnlessAsked(t *testing.T) {
	t.Parallel()

	model, _ := tasksPaneModel(t)
	model.state.BackgroundTasks = []coding.BackgroundTaskLifecycle{{
		ID: "monitor-1", Kind: string(background.KindMonitor),
		Description: "finished watch", Completed: true,
		StartedAt: tasksPaneNow.Add(-5 * time.Second),
		EndedAt:   tasksPaneNow,
	}}

	model.syncTasksPane(tasksPaneNow)
	assert.Empty(t, model.tasksPaneRows(), "a finished watch is hidden until the reader asks for it")

	model.tasksPane.showDone = true
	rows := model.tasksPaneRows()
	require.Len(t, rows, 1)
	assert.Equal(t, "monitor-1", rows[0].taskID)
	assert.False(t, rows[0].running)
	assert.Equal(t, tasksPaneWatchers, tasksPaneGroupOf(rows[0]))
	assert.Equal(t, "5s", model.tasksPaneRowMeta(rows[0], tasksPaneNow))
}

// TestTasksPaneSyncsWithALiveWatchFrame drives the whole TUI path for a watch: a
// background.started event arrives on the session stream, the reducer projects
// it into the state the pane reads, and the pane paints the Watchers row without
// the test seeding that state by hand.
func TestTasksPaneSyncsWithALiveWatchFrame(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Sequence = 3
	state.Phase = coding.PhaseRunning
	state.Interaction = coding.InteractionState{ID: "interaction-1", Active: true}
	state.Runs = []coding.RunState{{ID: "run-1", Active: true, TurnOpen: true, Turn: 1}}

	model := readyModelWithController(t, stubController{state: state}, true)
	model.runtimeState = false
	model.observedSequence = 3

	model.reduceStreamBatch([]streamItem{{event: coding.Event{
		Schema: coding.EventSchema, Sequence: 4, Time: time.Unix(1, 0).UTC(),
		SessionID: "session-1",
		Type:      coding.EventBackgroundStarted,
		Payload: coding.BackgroundTaskLifecycle{
			ID: "monitor-1", Kind: string(background.KindMonitor),
			Description: "deploy log", Command: "tail -f deploy.log",
			StartedAt: time.Unix(1, 0).UTC(),
		},
	}}})

	require.NoError(t, model.streamErr)
	require.Len(t, model.state.BackgroundTasks, 1, "the reducer projects the watch into the pane's state")
	assert.Equal(t, "monitor-1", model.state.BackgroundTasks[0].ID)

	// The pane reads that projection on the next frame.
	model.syncTasksPane(tasksPaneNow)
	assert.True(t, model.tasksPane.visible, "a live watch opens the pane")

	content := model.View().Content
	assert.Contains(t, content, "▾ Watchers 1")
	assert.Contains(t, content, backgroundTaskTag+" deploy log")
}

func TestTasksPaneSyncsWithALiveChildFrame(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Sequence = 4
	state.Phase = coding.PhaseRunning
	state.Interaction = coding.InteractionState{ID: "interaction-1", Active: true}
	state.Runs = []coding.RunState{{ID: "run-1", Active: true, TurnOpen: true, Turn: 1}}
	state.Subagents = []coding.SubagentState{liveTasksChild("child-1", "fix the login bug")}

	model := readyModelWithController(t, runtimeStateController{stubController{state: state}}, true)
	model.runtimeState = true
	model.observedSequence = 3

	// The frame the Runtime already reduced is what the pane observes.
	model.reduceStreamBatch([]streamItem{{event: coding.Event{
		Schema: coding.EventSchema, Sequence: 4, Time: time.Unix(1, 0).UTC(),
		SessionID: "session-1", InteractionID: "interaction-1", RunID: "run-1",
		Type:    coding.EventSubagentStarted,
		Payload: coding.SubagentLifecycle{ChildSessionID: "child-1", State: subagent.StateRunning},
	}}})

	require.NoError(t, model.streamErr)
	assert.True(t, model.tasksPane.visible, "a frame carrying a live child opens the pane")
	assert.Contains(t, model.View().Content, "fix the login bug")
}

func TestTasksPaneResetsAcrossASessionChange(t *testing.T) {
	t.Parallel()

	model, _ := tasksPaneModel(t)
	model.state.Subagents = []coding.SubagentState{liveTasksChild("child-1", "fix the login bug")}
	model.syncTasksPane(tasksPaneNow)
	require.True(t, model.tasksPane.visible)

	model.resetTasksPane()
	assert.False(t, model.tasksPane.visible, "a new session starts with the pane hidden")
	assert.Empty(t, model.tasksPane.seen, "the previous session's child records are dropped")
	assert.Zero(t, model.tasksPane.prevRunning, "a stale count cannot suppress the next auto-open")

	// The next session's first child opens the pane again.
	model.state.Subagents = []coding.SubagentState{liveTasksChild("child-2", "another task")}
	model.syncTasksPane(tasksPaneNow)
	assert.True(t, model.tasksPane.visible)
}

func TestTasksPaneClickSelectsKillsAndInspects(t *testing.T) {
	t.Parallel()

	// The pointer only reaches the pane where the frame asks for mouse reporting,
	// which is the fullscreen presentation.
	controller := newOverlayController(readyState())
	model := fullscreenModel(t, controller, true)
	model.state.Subagents = []coding.SubagentState{
		liveTasksChild("child-1", "aaa"),
		liveTasksChild("child-2", "bbb"),
	}
	model.syncTasksPane(tasksPaneNow)
	// Compose the frame so the pane records its painted rows.
	require.Contains(t, model.View().Content, "aaa")
	require.True(t, model.tasksPaneHit.painted)
	require.Len(t, model.tasksPaneHit.rows, 3, "one header plus two rows")

	header := model.tasksPaneHit.rows[0]
	assert.True(t, header.header)

	first := model.tasksPaneHit.rows[1]
	require.Equal(t, 0, first.index)

	// A click on the row body selects it without activating anything.
	command := model.handleMouse(tea.MouseClickMsg{
		Button: tea.MouseLeft, X: 1, Y: model.tasksPaneHit.top + 1,
	})
	assert.NotNil(t, command)
	assert.Equal(t, 0, model.tasksPane.cursor)
	assert.True(t, model.tasksPane.focused)
	assert.Empty(t, controller.agentCanceled)

	// The kill control cancels the selected running child.
	require.GreaterOrEqual(t, first.killStart, 0)
	command = model.handleMouse(tea.MouseClickMsg{
		Button: tea.MouseLeft, X: first.killStart, Y: model.tasksPaneHit.top + 1,
	})
	require.NotNil(t, command)
	driveModelCommands(t, model, command)
	assert.Equal(t, []string{"child-1"}, controller.agentCanceled)

	// The inspect control opens the child's transcript.
	command = model.handleMouse(tea.MouseClickMsg{
		Button: tea.MouseLeft, X: first.inspectStart, Y: model.tasksPaneHit.top + 1,
	})
	require.NotNil(t, command)
	driveModelCommands(t, model, command)
	assert.Equal(t, routeSubagent, model.route.kind)
}

// TestTasksPaneLeavesTheTranscriptDragToTheTranscript pins the pane's pointer
// scope: it owns only the rows it painted, so a drag over the conversation still
// extends the selection while the pane is on screen.
func TestTasksPaneLeavesTheTranscriptDragToTheTranscript(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Transcript = append(state.Transcript, ai.UserText("MARKER-A"), ai.UserText("MARKER-B"))
	state.Subagents = []coding.SubagentState{liveTasksChild("child-1", "a running child")}

	model := fullscreenModel(t, stubController{state: state}, true)
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 24})
	model.syncTasksPane(tasksPaneNow)
	model.rerenderTranscript(true)
	model.View()
	require.True(t, model.tasksPaneHit.painted, "the running child paints the pane")

	calls := &[]TextSaveRequest{}
	model.options.SaveText = captureSaver(calls)

	first := paintedRow(t, model, "MARKER-A")
	second := paintedRow(t, model, "MARKER-B")
	require.Less(t, first, second)

	model.handleMouse(tea.MouseClickMsg{X: 0, Y: first, Button: tea.MouseLeft})
	require.True(t, model.selection.dragging, "the press arms the transcript drag")
	model.handleMouse(tea.MouseMotionMsg{X: 40, Y: second, Button: tea.MouseLeft})
	command := model.handleMouse(tea.MouseReleaseMsg{X: 40, Y: second, Button: tea.MouseLeft})
	require.NotNil(t, command, "the release finishes the drag")

	messages := runCommandTree(t, command)
	assert.Equal(t, []string{"❯ MARKER-A\n\n❯ MARKER-B"}, clipboardTexts(messages))
	assert.Empty(t, *calls, "the drag writes the clipboard alone")
}

// TestTasksPaneHitMapTracksThePaintedRow pins the pane's pointer map against the
// frame it painted. With transcript content above the strip the pane is not at
// frame row zero, so a hit map that ignored the transcript's height would aim a
// click at the wrong row.
func TestTasksPaneHitMapTracksThePaintedRow(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Transcript = append(state.Transcript, ai.UserText("HELLO-WORLD"))
	state.Subagents = []coding.SubagentState{liveTasksChild("child-1", "a running child")}

	model := fullscreenModel(t, stubController{state: state}, true)
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 24})
	model.syncTasksPane(tasksPaneNow)
	model.rerenderTranscript(true)
	content := ansi.Strip(model.View().Content)

	painted := -1
	for index, line := range strings.Split(content, "\n") {
		if strings.Contains(line, "Subagents 1") {
			painted = index

			break
		}
	}
	require.GreaterOrEqual(t, painted, 0, "the pane is painted:\n%s", content)
	assert.Equal(t, painted, model.tasksPaneHit.top, "the hit map matches the painted header row")

	// A click on the child row selects it; a click in the transcript above does
	// not reach the pane.
	model.handleMouse(tea.MouseClickMsg{Button: tea.MouseLeft, X: 1, Y: painted + 1})
	assert.True(t, model.tasksPane.focused, "a click on the painted pane row selects")
	assert.Empty(t, model.composer.Value(), "the click never reaches the composer")
}

// TestTasksPaneSitsBelowTheStatusLine pins the pane's place in the frame: it is
// the last band, under the status line and under the Composer.
func TestTasksPaneSitsBelowTheStatusLine(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	model := fullscreenModel(t, controller, true)
	model.state.Subagents = []coding.SubagentState{liveTasksChild("child-1", "fix the login bug")}
	model.syncTasksPane(tasksPaneNow)

	content := ansi.Strip(model.View().Content)
	lines := strings.Split(content, "\n")
	require.True(t, model.tasksPaneHit.painted)

	pane := model.tasksPaneHit.top
	require.Greater(t, pane, 1)
	assert.Contains(t, lines[pane], tasksPaneHeaderExpanded+" Subagents 1",
		"the pane's first row is a section header")

	// The status line is the row right above the pane, and the Composer stays
	// above both.
	status := pane - 1
	assert.Contains(t, lines[status], "workspace", "the status line stays above the container")
	assert.Less(t, model.frameHit.composerTop, status, "the Composer stays above the status line")
}

// TestTasksPaneKeepsAHeaderOverItsOwnRows pins the container's floor: every
// section it names shows at least one of its rows, and a section whose rows do
// not all fit says how many it hides. A header counting rows that are not painted
// is the failure this test exists to catch.
func TestTasksPaneKeepsAHeaderOverItsOwnRows(t *testing.T) {
	t.Parallel()

	model, _ := tasksPaneModel(t)
	children := make([]coding.SubagentState, 0, tasksPaneMaxRows+1)

	for index := range tasksPaneMaxRows + 1 {
		children = append(children, liveTasksChild(
			"child-"+strconv.Itoa(index), "child task "+strconv.Itoa(index),
		))
	}

	model.state.Subagents = children
	model.state.BackgroundTasks = []coding.BackgroundTaskLifecycle{{
		ID: "monitor-1", Kind: string(background.KindMonitor),
		Description: "deploy log", StartedAt: time.Now().Add(-12 * time.Second),
	}}
	model.Update(tea.WindowSizeMsg{Width: defaultWidth, Height: 24})
	model.syncTasksPane(tasksPaneNow)

	content := ansi.Strip(model.View().Content)
	lines := strings.Split(content, "\n")
	assert.Contains(t, content, "▾ Subagents "+strconv.Itoa(tasksPaneMaxRows+1))
	assert.Contains(t, content, "▾ Watchers 1", "a squeezed container still names its sections")
	assert.Contains(t, content, tasksPaneMorePrefix, "a squeezed section says how many rows it hides")

	subagents := slices.IndexFunc(lines, func(line string) bool {
		return strings.Contains(line, "▾ Subagents "+strconv.Itoa(tasksPaneMaxRows+1))
	})
	require.NotEqual(t, -1, subagents)
	assert.Contains(t, lines[subagents+1], "child task", "the header's own row follows it")

	watchers := slices.IndexFunc(lines, func(line string) bool {
		return strings.Contains(line, "▾ Watchers 1")
	})
	require.NotEqual(t, -1, watchers)
	assert.Contains(t, lines[watchers+1], backgroundTaskTag, "the watch's own row follows its header")
}

// TestTasksPaneScrollsALongSectionToKeepTheCursorVisible pins the other half of
// the floor: a section with more rows than the container can hold scrolls to the
// row the reader is on instead of dropping it.
func TestTasksPaneScrollsALongSectionToKeepTheCursorVisible(t *testing.T) {
	t.Parallel()

	model, _ := tasksPaneModel(t)
	children := make([]coding.SubagentState, 0, tasksPaneMaxRows+1)

	for index := range tasksPaneMaxRows + 1 {
		children = append(children, liveTasksChild(
			"child-"+strconv.Itoa(index), "child task "+strconv.Itoa(index),
		))
	}

	model.state.Subagents = children
	model.syncTasksPane(tasksPaneNow)
	model.tasksPane.focused = true
	model.tasksPane.cursor = len(children) - 1

	interior, _ := model.tasksPanePaint(tasksPaneNow, model.tasksPaneCapacity())
	assert.Contains(t, interior, "child task "+strconv.Itoa(len(children)-1),
		"the row the reader is on stays painted")
	assert.Contains(t, interior, tasksPaneMorePrefix, "the rows it hides are counted")
}

// TestTasksPaneWheelMovesThePaneAndNotTheTranscript pins the pointer scope of the
// notch: a wheel over the pane's own rows moves the pane's selection through its
// key path, while the conversation underneath stays where it was.
func TestTasksPaneWheelMovesThePaneAndNotTheTranscript(t *testing.T) {
	t.Parallel()

	state := readyState()
	for index := range 40 {
		state.Transcript = append(state.Transcript, ai.UserText("ROW-"+strconv.Itoa(index)))
	}
	state.Subagents = []coding.SubagentState{
		liveTasksChild("child-1", "first child"),
		liveTasksChild("child-2", "second child"),
	}

	model := fullscreenModel(t, stubController{state: state}, true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 24})
	model.syncTasksPane(tasksPaneNow)
	model.rerenderTranscript(true)
	model.View()
	require.True(t, model.tasksPaneHit.painted, "the running children paint the pane")

	transcript := model.transcriptScroll.offset
	require.Equal(t, 0, model.tasksPane.cursor)

	model.Update(tea.MouseWheelMsg{
		Button: tea.MouseWheelDown, X: 4, Y: model.tasksPaneHit.top + 1,
	})
	assert.Equal(t, 1, model.tasksPane.cursor, "the notch moved the pane's selection")
	assert.Equal(t, transcript, model.transcriptScroll.offset, "the transcript stayed put")
	assert.False(t, model.tasksPane.focused, "the notch does not take the frame's keys")

	// A notch above the pane is still the transcript's.
	model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp, X: 4, Y: 0})
	assert.Equal(t, 1, model.tasksPane.cursor, "the pane kept its selection")
	assert.Equal(t, transcript-wheelLinesDefault, model.transcriptScroll.offset,
		"a notch outside the pane still reads the conversation")
}

// TestTasksPaneWheelClampsAtTheEnds pins that the notch moves the selection the way
// the pane's own keys do, so a wheel cannot drift from the keyboard.
func TestTasksPaneWheelClampsAtTheEnds(t *testing.T) {
	t.Parallel()

	state := readyState()
	for index := range 40 {
		state.Transcript = append(state.Transcript, ai.UserText("ROW-"+strconv.Itoa(index)))
	}
	state.Subagents = []coding.SubagentState{
		liveTasksChild("child-1", "first child"),
		liveTasksChild("child-2", "second child"),
	}

	model := fullscreenModel(t, stubController{state: state}, true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 24})
	model.syncTasksPane(tasksPaneNow)
	model.rerenderTranscript(true)
	model.View()

	for range 100 {
		model.Update(tea.MouseWheelMsg{
			Button: tea.MouseWheelDown, X: 4, Y: model.tasksPaneHit.top + 1,
		})
	}
	assert.Equal(t, 1, model.tasksPane.cursor, "the selection clamps at the last row")

	for range 100 {
		model.Update(tea.MouseWheelMsg{
			Button: tea.MouseWheelUp, X: 4, Y: model.tasksPaneHit.top + 1,
		})
	}
	assert.Equal(t, 0, model.tasksPane.cursor, "and at the first")
}

// TestTasksPaneNeverPaintsMoreThanTheCap pins the tightened strip at every frame
// height: the pane is the frame's compact band, and the conversation keeps the rest.
func TestTasksPaneNeverPaintsMoreThanTheCap(t *testing.T) {
	t.Parallel()

	model, _ := tasksPaneModel(t)
	children := make([]coding.SubagentState, 0, tasksPaneMaxRows+4)
	for index := range tasksPaneMaxRows + 4 {
		children = append(children, liveTasksChild(
			"child-"+strconv.Itoa(index), "child task "+strconv.Itoa(index),
		))
	}
	model.state.Subagents = children
	model.syncTasksPane(tasksPaneNow)

	for _, height := range []int{tasksPaneMinViewHeight, 20, 24, 60} {
		model.Update(tea.WindowSizeMsg{Width: defaultWidth, Height: height})

		capacity := model.tasksPaneCapacity()
		require.LessOrEqual(t, capacity, tasksPaneMaxRows, "height %d", height)
		require.LessOrEqual(t, capacity, max(1, int(float64(height)*tasksPaneMaxHeightFraction)),
			"height %d", height)

		band, hits := model.tasksPanePaint(tasksPaneNow, capacity)
		assert.Len(t, strings.Split(band, "\n"), capacity, "height %d", height)
		assert.Len(t, hits, capacity, "height %d", height)
	}
}

// TestTasksPaneCollapsedSectionKeepsOnlyItsHeader pins the collapse control: the
// section keeps its count and gives up its rows.
func TestTasksPaneCollapsedSectionKeepsOnlyItsHeader(t *testing.T) {
	t.Parallel()

	model, _ := tasksPaneModel(t)
	model.state.Subagents = []coding.SubagentState{liveTasksChild("child-1", "fix the login bug")}
	model.syncTasksPane(tasksPaneNow)
	model.tasksPane.collapsed[tasksPaneSubagents] = true

	interior, _ := model.tasksPanePaint(tasksPaneNow, model.tasksPaneCapacity())
	assert.Contains(t, interior, "▸ Subagents 1")
	assert.NotContains(t, interior, "fix the login bug")
}
