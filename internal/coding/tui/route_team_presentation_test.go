package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTeamRouteActivityNamesEveryOperation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		operation teamRouteOperation
		title     string
	}{
		{teamRouteOperationNone, "Team operation in progress"},
		{teamRouteOperationGenerate, "Designing Team proposal"},
		{teamRouteOperationRevise, "Revising Team proposal"},
		{teamRouteOperationConfirm, "Admitting Coding Team"},
		{teamRouteOperationRead, "Refreshing Team state"},
		{teamRouteOperationControl, "Applying Team control"},
		{teamRouteOperationDecline, "Discarding Team proposal"},
		{teamRouteOperationDiscoverRecovery, "Checking Team recovery"},
		{teamRouteOperationResumeRecovery, "Resuming Coding Team"},
		{teamRouteOperationLoadIntegration, "Loading Integration state"},
		{teamRouteOperationPrepareIntegration, "Preparing Integration preview"},
		{teamRouteOperationApplyIntegration, "Applying Integration"},
		{teamRouteOperationRejectIntegration, "Rejecting Integration preview"},
		{teamRouteOperationRecoverIntegration, "Recovering Integration"},
		{teamRouteOperationCleanup, "Cleaning Team resources"},
	}

	for _, test := range tests {
		t.Run(test.title, func(t *testing.T) {
			t.Parallel()

			activity := teamRouteActivity(test.operation, false)
			assert.Equal(t, test.title, activity.title)
			assert.NotEmpty(t, activity.detail)
			assert.Contains(t, activity.hint, "Esc")

			cancelling := teamRouteActivity(test.operation, true)
			assert.Contains(t, cancelling.title, "Cancelling")
			assert.NotEqual(t, "Cancelling…", cancelling.title)
		})
	}
}

func TestTeamRouteGenerationRetainsObjectiveAndPendingIdentity(t *testing.T) {
	t.Parallel()

	controller := newTeamRouteTestController(readyState())
	model := readyModelWithController(t, controller, true)
	command := model.activateTeamRoute("audit frontend and backend contract mismatches")
	require.NotNil(t, command)

	assert.True(t, model.route.loading)
	assert.Equal(t, teamRouteOperationGenerate, model.route.team.pending)
	content := model.View().Content
	assert.Contains(t, content, "  ─ Coding Team ─")
	assert.Contains(t, content, "audit frontend and backend contract mismatches")
	assert.Contains(t, content, "  ✻ Designing Team proposal")
	assert.Contains(t, content, "    The constrained Lead is designing up to 3 Workers and a task DAG.")
	assert.Contains(t, content, "  Esc cancel generation")
	assert.NotContains(t, content, "Working…")
	assert.NotContains(t, content, "W1")
	assert.NotContains(t, content, "Workers ·")

	driveModelCommands(t, model, command)
	assert.False(t, model.route.loading)
	assert.Equal(t, teamRouteOperationNone, model.route.team.pending)
}

func TestTeamRouteGenerationUsesSemanticStylesAndAnimatedClock(t *testing.T) {
	t.Parallel()

	controller := newTeamRouteTestController(readyState())
	model := readyModelWithController(t, controller, false)
	command := model.activateTeamRoute("audit frontend and backend contract mismatches")
	require.NotNil(t, command)

	content := model.View().Content
	assert.Contains(t, ansi.Strip(content), "  ─ Coding Team ─")
	assert.GreaterOrEqual(t, strings.Count(content, "\x1b["), 6)

	before := model.activity.spinner.View()
	_, tick := model.Update(activityTickMsg{})
	require.NotNil(t, tick)
	assert.NotEqual(t, before, model.activity.spinner.View())

	driveModelCommands(t, model, command)
	_, stopped := model.Update(activityTickMsg{})
	assert.Nil(t, stopped)

	model.Update(tea.WindowSizeMsg{Width: 8, Height: 8})
	rail, _ := model.teamRouteRail("a description that cannot fit")
	assert.LessOrEqual(t, ansi.StringWidth(rail), 8)
}

func TestTeamRouteObjectiveInputInteractionRemainsUnchanged(t *testing.T) {
	t.Parallel()

	controller := newTeamRouteTestController(readyState())
	model := readyModelWithController(t, controller, true)
	driveModelCommands(t, model, model.openTeamRoute(""))
	require.Equal(t, teamRouteObjective, model.route.team.stage)

	content, inputY := model.teamObjectiveContent()
	assert.Equal(t, -1, inputY)
	assert.True(t, strings.HasPrefix(content, strings.Join([]string{
		"Coding Team",
		"",
		"Describe one outcome. A constrained Lead will propose up to three Workers and a task DAG.",
		"",
	}, "\n")))
	assert.Contains(t, content, "Enter generate proposal · Esc close")
	require.NotNil(t, model.View().Cursor)

	model.Update(tea.KeyPressMsg{Text: "ship one outcome"})
	_, command := model.Update(key(keyEnter))
	require.NotNil(t, command)
	assert.Equal(t, "ship one outcome", model.route.team.objective)
	assert.Equal(t, teamRouteOperationGenerate, model.route.team.pending)
}

func TestTeamRouteInputMatchesAgentComposerAndTracksScrolledCursor(t *testing.T) {
	t.Parallel()

	model := readyModelWithController(t, newTeamRouteTestController(readyState()), true)
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 7})
	driveModelCommands(t, model, model.openTeamRoute(""))
	model.Update(tea.KeyPressMsg{Text: "cursor"})

	for _, stage := range []teamRouteStage{
		teamRouteObjective,
		teamRouteRevision,
		teamRouteControlInput,
	} {
		model.route.team.stage = stage
		view := model.View()
		assert.Contains(t, ansi.Strip(view.Content), "│ "+inputArrow+" cursor")
	}

	model.route.team.stage = teamRouteObjective
	model.route.offset = 2

	view := model.View()
	plain := ansi.Strip(view.Content)
	lines := strings.Split(plain, "\n")
	inputLine := lineContaining(lines, "│ "+inputArrow+" cursor")
	require.NotEqual(t, -1, inputLine)
	require.NotNil(t, view.Cursor)
	assert.Equal(t, inputLine, view.Cursor.Y)
	assert.Equal(t, ansi.StringWidth("│ "+inputArrow+" cursor"), view.Cursor.X)
	assert.Equal(t, "┌"+strings.Repeat("─", model.width-2)+"┐", lines[inputLine-1])

	model.route.offset = 0
	model.Update(tea.WindowSizeMsg{Width: 20, Height: 7})
	view = model.View()
	plain = ansi.Strip(view.Content)
	lines = strings.Split(plain, "\n")
	inputLine = lineContaining(lines, inputArrow+" cursor")
	require.NotEqual(t, -1, inputLine)
	require.NotNil(t, view.Cursor)
	assert.Equal(t, inputLine, view.Cursor.Y)
	assert.Equal(t, ansi.StringWidth(inputArrow+" cursor"), view.Cursor.X)
	assert.NotContains(t, plain, "┌")
}

func TestTeamRouteLoadingPresentationRetainsReviewedContext(t *testing.T) {
	t.Parallel()

	model := readyModelWithController(t, newTeamRouteTestController(readyState()), true)
	tests := []struct {
		operation teamRouteOperation
		title     string
	}{
		{teamRouteOperationConfirm, "Admitting Coding Team"},
		{teamRouteOperationRead, "Refreshing Team state"},
		{teamRouteOperationControl, "Applying Team control"},
		{teamRouteOperationDiscoverRecovery, "Checking Team recovery"},
		{teamRouteOperationResumeRecovery, "Resuming Coding Team"},
		{teamRouteOperationLoadIntegration, "Loading Integration state"},
		{teamRouteOperationPrepareIntegration, "Preparing Integration preview"},
		{teamRouteOperationApplyIntegration, "Applying Integration"},
		{teamRouteOperationRejectIntegration, "Rejecting Integration preview"},
		{teamRouteOperationRecoverIntegration, "Recovering Integration"},
		{teamRouteOperationCleanup, "Cleaning Team resources"},
	}

	for _, test := range tests {
		t.Run(test.title, func(t *testing.T) {
			t.Parallel()

			content := model.teamRouteLoadingContent("reviewed Team context", &teamRouteState{
				pending: test.operation,
			})
			assert.Contains(t, content, "reviewed Team context")
			assert.Contains(t, content, test.title)
			assert.NotContains(t, content, "Working…")
		})
	}

	cancelling := model.teamRouteLoadingContent("ignored", &teamRouteState{
		pending: teamRouteOperationRevise, cancelRequested: true,
		objective: "retain the reviewed objective",
	})
	assert.Contains(t, cancelling, "  ✻ Cancelling proposal revision")
	assert.Contains(t, cancelling, "retain the reviewed objective")
	assert.NotContains(t, cancelling, "Cancelling…")
}

func TestTeamProposalPresentationShowsResponsiveWorkerTaskDAG(t *testing.T) {
	t.Parallel()

	controller := newTeamRouteTestController(readyState())
	controller.proposal = presentationTeamProposal()
	model := readyModelWithController(t, controller, true)
	command := model.activateTeamRoute(controller.proposal.Request.Objective)
	require.NotNil(t, command)
	driveModelCommands(t, model, command)
	require.Equal(t, teamRouteProposal, model.route.team.stage)

	model.Update(tea.WindowSizeMsg{Width: 100, Height: 80})
	wide := model.View().Content
	assert.Contains(t, wide, "  ─ Coding Team ─ Review proposal")
	assert.Contains(t, wide, "Workers · 2")
	assert.Contains(t, wide, "  ○ W1  Contract explorer")
	assert.Contains(t, wide, "    └ read-only boundary audit")
	assert.Contains(t, wide, "Task DAG · 3")
	assert.Contains(t, wide, "  ○ T3  Reconcile mismatches")
	assert.Contains(t, wide, "    └ Integration reviewer · after T1, T2")
	assert.NotContains(t, wide, "task-contracts")
	assert.NotContains(t, wide, "\x1b[")

	model.Update(tea.WindowSizeMsg{Width: 36, Height: 80})
	narrow := model.View().Content
	assert.Contains(t, narrow, "○ T3  Reconcile mismatches")
	assert.Contains(t, narrow, "after T1, T2")
	assert.Contains(t, narrow, "  ─ Coding Team")

	for line := range strings.SplitSeq(narrow, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), 36)
	}
}

func TestActiveTeamPresentationShowsHierarchyAndTruthfulStates(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Teams = []coding.TeamLifecycleState{{TeamLifecycle: coding.TeamLifecycle{
		TeamID: "team-1", State: coding.TeamLifecycleRunning,
	}}}
	controller := newTeamRouteTestController(state)
	view := controller.viewSnapshot()
	view.Tasks = append(view.Tasks, coding.TeamTaskView{
		ID: "task-2", Title: "Review integration", AssignedMemberID: "lead",
		Status: team.TaskStatusPending,
	})
	controller.setView(view)
	model := readyModelWithController(t, controller, true)
	driveModelCommands(t, model, model.openTeamRoute(""))
	model.Update(tea.WindowSizeMsg{Width: 100, Height: 80})

	content := model.View().Content
	assert.Contains(t, content, "Team · 1 Workers · Tasks 0/2")
	assert.Contains(t, content, "› ✻ Builder  Working")
	assert.NotContains(t, content, "Tasks · 2")

	model.Update(key(keyCtrlT))
	content = model.View().Content
	assert.Contains(t, content, "Tasks · 2")
	assert.Contains(t, content, "› ✻ T1  Build Team route")
	assert.Contains(t, content, "Running")
	assert.Contains(t, content, "  ○ T2  Review integration")
	assert.Contains(t, content, "Pending")
	assert.NotContains(t, content, "team-1")
	assert.NotContains(t, content, "attempt-1")
}

func presentationTeamProposal() coding.TeamProposal {
	return coding.TeamProposal{
		ID: "proposal-presentation",
		Request: coding.TeamProposalRequest{
			Objective: "audit frontend and backend contract mismatches",
			Workers: []coding.TeamWorkerSpec{
				{Name: "Contract explorer", Role: "read-only boundary audit"},
				{Name: "Integration reviewer", Role: "reconcile findings"},
			},
			Tasks: []coding.TeamTaskSpec{
				{
					ID: "task-contracts", Title: "Audit frontend contracts",
					Description:    "Trace every request and response boundary.",
					AssignedWorker: "Contract explorer",
				},
				{
					ID: "task-runtime", Title: "Audit backend contracts",
					AssignedWorker: "Contract explorer",
				},
				{
					ID: "task-reconcile", Title: "Reconcile mismatches",
					Description:    strings.Repeat("bounded detail ", 80),
					AssignedWorker: "Integration reviewer",
					Dependencies:   []string{"task-contracts", "task-runtime"},
				},
			},
		},
		ExpiresAt: time.Now().Add(time.Minute).UTC(),
	}
}
