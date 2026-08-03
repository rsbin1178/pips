package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTeamPanelKeepsLeadComposerAndUsesExplicitFocus(t *testing.T) {
	t.Parallel()

	controller := newTeamWorkerRouteController()
	model := readyModelWithController(t, controller, true)
	model.Update(tea.WindowSizeMsg{Width: 72, Height: 20})

	view := controller.viewSnapshot()
	model.storeTeamProjectionView(view)
	model.state.Teams = []coding.TeamLifecycleState{{TeamLifecycle: coding.TeamLifecycle{
		TeamID: view.TeamID, State: coding.TeamLifecycleRunning,
	}}}

	content := ansi.Strip(model.View().Content)
	composer := strings.Index(content, inputArrow)
	statusLine := strings.Index(content, ansi.Strip(model.statusLineView()))
	panel := strings.Index(content, "Team · 1 Workers · Tasks 0/1")

	require.GreaterOrEqual(t, composer, 0)
	require.Greater(t, statusLine, composer)
	require.Greater(t, panel, statusLine)
	require.NotNil(t, model.View().Cursor)
	assert.Contains(t, content, "Builder")
	assert.NotContains(t, content, "Tasks · 1")

	model.Update(key(keyTab))
	assert.True(t, model.teamPanel.isFocused)
	assert.Nil(t, model.View().Cursor)
	assert.Contains(t, ansi.Strip(model.View().Content), "› ✻ Builder")

	model.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	assert.True(t, model.teamPanel.isPeeking)
	assert.Contains(t, ansi.Strip(model.View().Content), "Worker peek")
	assert.Contains(t, ansi.Strip(model.View().Content), "Task · Build Team route")

	model.Update(key(keyCtrlT))
	assert.True(t, model.teamPanel.showTasks)
	assert.True(t, model.teamPanel.isTaskFocused)
	assert.Contains(t, ansi.Strip(model.View().Content), "Tasks · 1")
	assert.Contains(t, ansi.Strip(model.View().Content), "› ✻ T1")

	model.Update(key(keyEscape))
	assert.False(t, model.teamPanel.showTasks)
	assert.True(t, model.teamPanel.isFocused)
	model.Update(key(keyEscape))
	assert.False(t, model.teamPanel.isFocused)
	require.NotNil(t, model.View().Cursor)
}

func TestTeamPanelPrioritizesNeedsInputAndOpensExactWorker(t *testing.T) {
	t.Parallel()

	controller := newTeamWorkerRouteController()
	model := readyModelWithController(t, controller, true)

	view := controller.viewSnapshot()
	view.Members = append(view.Members, coding.TeamMemberView{
		ID: "worker-2", Name: "Reviewer", Role: "review", Status: team.MemberStatusActive,
	})
	view.Tasks = append(view.Tasks, coding.TeamTaskView{
		ID: "task-2", Title: "Review Team route",
		AssignedMemberID: "worker-2", Status: team.TaskStatusRunning,
	})
	blocked := view.Attempts[0]
	blocked.Target.MemberID = "worker-2"
	blocked.Target.TaskID = "task-2"
	blocked.Target.AttemptID = "attempt-2"
	blocked.ChildSessionID = "worker-session-2"
	blocked.LifecycleState = coding.TeamLifecyclePaused
	blocked.Activity = coding.TeamActivityAwaitingQuestion
	view.Attempts = append(view.Attempts, blocked)
	model.storeTeamProjectionView(view)
	model.state.Teams = []coding.TeamLifecycleState{{TeamLifecycle: coding.TeamLifecycle{
		TeamID: view.TeamID, State: coding.TeamLifecycleRunning,
	}}}

	workers := teamPanelWorkers(view)
	require.Len(t, workers, 2)
	assert.Equal(t, "Reviewer", workers[0].member.Name)

	content := ansi.Strip(model.teamPanelView())
	assert.Less(t, strings.Index(content, "Reviewer"), strings.Index(content, "Builder"))
	assert.Contains(t, content, "Needs input · question")

	model.storeTeamProjectionView(controller.viewSnapshot())
	model.Update(key(keyTab))
	_, command := model.Update(key(keyEnter))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)
	assert.Equal(t, routeChild, model.route.kind)
	assert.Equal(t, childTeamWorker, model.route.childKind)
	assert.Equal(t, team.AttemptID("attempt-1"), model.route.childSummary.worker.Target.AttemptID)
}

func TestTeamPanelPreservesExactTaskAndTeamControls(t *testing.T) {
	t.Parallel()

	controller := newTeamWorkerRouteController()
	model := readyModelWithController(t, controller, true)

	view := controller.viewSnapshot()
	model.storeTeamProjectionView(view)
	model.state.Teams = []coding.TeamLifecycleState{{TeamLifecycle: coding.TeamLifecycle{
		TeamID: view.TeamID, State: coding.TeamLifecycleRunning,
	}}}

	model.Update(key(keyCtrlT))
	model.Update(key("x"))
	assert.True(t, model.teamPanel.isConfirming)
	_, command := model.Update(key(keyEnter))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	failed := view.Clone()
	failed.Tasks[0].Status = team.TaskStatusFailed
	failed.Attempts[0].DomainState = team.AttemptStatusFailed
	failed.Attempts[0].LifecycleState = coding.TeamLifecycleFailed
	controller.setView(failed)
	model.storeTeamProjectionView(failed)
	model.Update(key("r"))
	assert.True(t, model.teamPanel.isConfirming)
	_, command = model.Update(key(keyEnter))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	model.Update(key("C"))
	assert.True(t, model.teamPanel.isConfirming)
	_, command = model.Update(key(keyEnter))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	controls := controller.controlsSnapshot()
	require.Len(t, controls, 3)
	assert.Equal(t, coding.TeamControlCancelTask, controls[0].Action)
	assert.Equal(t, team.TaskID("task-1"), controls[0].TaskID)
	assert.Equal(t, coding.TeamControlRetryTask, controls[1].Action)
	assert.Equal(t, team.TaskID("task-1"), controls[1].TaskID)
	assert.Equal(t, coding.TeamControlCancelTeam, controls[2].Action)
	assert.Empty(t, controls[2].TaskID)
}

func TestTeamPanelControlsStayReadOnlyInPlanMode(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Mode = coding.ModePlan
	controller := newTeamRouteTestController(state)
	view := controller.viewSnapshot()
	view.Attempts[0].ChildSessionID = "worker-session-1"
	model := readyModelWithController(t, controller, true)
	model.storeTeamProjectionView(view)
	model.state.Teams = []coding.TeamLifecycleState{{TeamLifecycle: coding.TeamLifecycle{
		TeamID: view.TeamID, State: coding.TeamLifecycleRunning,
	}}}

	model.Update(key(keyTab))
	_, command := model.Update(key("x"))
	assert.Nil(t, command)
	assert.Contains(t, ansi.Strip(model.teamPanelView()), "read-only in Plan Mode")
	assert.Empty(t, controller.controlsSnapshot())
}

func TestTeamPanelInterruptDoesNotRequireInspectableChildSession(t *testing.T) {
	t.Parallel()

	controller := newTeamRouteTestController(readyState())
	model := readyModelWithController(t, controller, true)
	view := controller.viewSnapshot()
	require.Empty(t, view.Attempts[0].ChildSessionID)
	model.storeTeamProjectionView(view)
	model.state.Teams = []coding.TeamLifecycleState{{TeamLifecycle: coding.TeamLifecycle{
		TeamID: view.TeamID, State: coding.TeamLifecycleRunning,
	}}}

	model.Update(key(keyTab))
	_, command := model.Update(key("x"))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	controls := controller.controlsSnapshot()
	require.Len(t, controls, 1)
	assert.Equal(t, coding.TeamControlInterruptAttempt, controls[0].Action)
	assert.Equal(t, view.Attempts[0].Target.AttemptID, controls[0].ExpectedAttemptID)
	assert.Equal(t, view.Attempts[0].Target.OwnerGeneration, controls[0].OwnerGeneration)
}

func TestTeamPanelCapturingStateWinsOverRunningDomainState(t *testing.T) {
	t.Parallel()

	attempt := testTeamRouteView().Attempts[0]
	attempt.LifecycleState = coding.TeamLifecycleCapturing
	worker := teamPanelWorker{attempt: attempt, hasAttempt: true}

	_, label, _ := teamPanelWorkerState(worker)
	assert.Equal(t, "Capturing result", label)
}

func TestTeamPanelTaskWindowKeepsSelectedOverflowTaskVisible(t *testing.T) {
	t.Parallel()

	controller := newTeamWorkerRouteController()
	model := readyModelWithController(t, controller, true)

	view := controller.viewSnapshot()
	for index := 2; index <= 10; index++ {
		view.Tasks = append(view.Tasks, coding.TeamTaskView{
			ID:    team.TaskID(fmt.Sprintf("task-%d", index)),
			Title: fmt.Sprintf("Task %d", index), Status: team.TaskStatusPending,
		})
	}

	model.teamPanel = teamPanelState{
		isFocused: true, showTasks: true, isTaskFocused: true, taskCursor: 6,
	}

	viewContent := ansi.Strip(strings.Join(model.teamPanelTaskLines(view), "\n"))
	assert.Contains(t, viewContent, "› ○ T7  Task 7")
	assert.NotContains(t, viewContent, "T1  Build Team route")
	assert.Contains(t, viewContent, "earlier tasks")
	assert.Contains(t, viewContent, "later tasks")
}

func TestTeamPanelNarrowNoColorGeometryAndCursorOwnership(t *testing.T) {
	t.Parallel()

	controller := newTeamWorkerRouteController()
	model := readyModelWithController(t, controller, true)
	model.options.NoColor = true
	view := controller.viewSnapshot()
	model.storeTeamProjectionView(view)
	model.state.Teams = []coding.TeamLifecycleState{{TeamLifecycle: coding.TeamLifecycle{
		TeamID: view.TeamID, State: coding.TeamLifecycleRunning,
	}}}
	model.Update(tea.WindowSizeMsg{Width: 30, Height: 20})
	model.Update(key(keyTab))

	content := model.View().Content
	assert.Nil(t, model.View().Cursor)
	assert.NotContains(t, content, "\x1b[")

	for line := range strings.SplitSeq(content, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), 30)
	}
}
