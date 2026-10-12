//nolint:wsl_v5 // The pane's fixtures and assertions read as one contract table.
package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
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

func TestTasksPaneEmptyStateNamesTheToggle(t *testing.T) {
	t.Parallel()

	model, _ := tasksPaneModel(t)
	model.revealTasksPane()

	assert.Contains(t, model.View().Content, tasksPaneEmptyRunning)

	model.tasksPane.showDone = true
	assert.Contains(t, model.View().Content, tasksPaneEmptyAll)
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

	t.Run("h toggles the finished rows", func(t *testing.T) {
		t.Parallel()

		model, _ := tasksPaneModel(t)
		model.state.Subagents = []coding.SubagentState{liveTasksChild("child-1", "fix the login bug")}
		model.syncTasksPane(tasksPaneNow)
		model.toggleTasksPane()

		model.Update(key("h"))
		assert.True(t, model.tasksPane.showDone)
		model.Update(key("h"))
		assert.False(t, model.tasksPane.showDone)
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
		Button: tea.MouseLeft, X: 2, Y: model.tasksPaneHit.top + 1,
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
	assert.Equal(t, painted, model.tasksPaneHit.top, "the hit map matches the painted row")

	// A click on the child row selects it; a click in the transcript above does
	// not reach the pane.
	model.handleMouse(tea.MouseClickMsg{Button: tea.MouseLeft, X: 2, Y: painted + 1})
	assert.True(t, model.tasksPane.focused, "a click on the painted pane row selects")
	assert.Empty(t, model.composer.Value(), "the click never reaches the composer")
}
