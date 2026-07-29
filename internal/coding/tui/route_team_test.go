//nolint:wsl_v5 // Route transitions and their observable assertions stay adjacent.
package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/teamstate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTeamCommandPreservesBoundedObjectiveTail(t *testing.T) {
	t.Parallel()

	controller := newTeamRouteTestController(readyState())
	model := readyModelWithController(t, controller, true)
	model.openCommandPicker()
	model.Update(tea.KeyPressMsg{Text: "team inspect the runtime ownership boundary"})

	filtered := model.filteredCommands()
	require.Len(t, filtered, 1)
	assert.Equal(t, commandTeam, filtered[0].name)
	assert.Contains(t, model.composer.Value(), "inspect the runtime ownership boundary")

	_, command := model.Update(key(keyEnter))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	assert.Equal(t, routeTeam, model.route.kind)
	require.NotNil(t, model.route.team.proposal)
	assert.Equal(t, "inspect the runtime ownership boundary", controller.generatedObjective())

	other := readyModelWithController(t, newTeamRouteTestController(readyState()), true)
	other.openCommandPicker()
	other.picker.query = "resume unexpected"
	_, command = other.executeCommand(commandNamed(t, "resume"))
	assert.Nil(t, command)
	require.Error(t, other.picker.err)

	bounded := readyModelWithController(t, newTeamRouteTestController(readyState()), true)
	bounded.openCommandPicker()
	bounded.picker.query = commandTeam + " " + strings.Repeat("x", maximumCommandArgumentTailBytes+1)
	_, command = bounded.executeCommand(commandNamed(t, commandTeam))
	assert.Nil(t, command)
	require.Error(t, bounded.picker.err)
}

func TestTeamRouteNoArgumentInputIsAgentModeOnlyAndNoColorSafe(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Mode = coding.ModePlan
	controller := newTeamRouteTestController(state)
	model := readyModelWithController(t, controller, true)
	model.Update(tea.WindowSizeMsg{Width: 28, Height: 9})

	driveModelCommands(t, model, model.openTeamRoute(""))
	assert.Equal(t, teamRouteObjective, model.route.team.stage)
	require.NotNil(t, model.View().Cursor)
	assert.Contains(t, model.View().Content, "Plan Mode is read-only")
	assert.NotContains(t, model.View().Content, "\x1b[")
	for line := range strings.SplitSeq(model.View().Content, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), 28)
	}

	model.Update(tea.KeyPressMsg{Text: "build the feature"})
	_, command := model.Update(key(keyEnter))
	assert.Nil(t, command)
	require.Error(t, model.route.err)
	assert.Empty(t, controller.generatedObjective())

	model.Update(tea.WindowSizeMsg{Width: 18, Height: 7})
	assert.NotContains(t, model.View().Content, "\x1b[")
}

func TestTeamRouteHidesPrivateRuntimeErrorDetail(t *testing.T) {
	t.Parallel()

	controller := newTeamRouteTestController(readyState())
	controller.generate = func(context.Context, coding.TeamProposalPrompt) (coding.TeamProposal, error) {
		return coding.TeamProposal{}, fmt.Errorf(
			"%w: inspect /root/private/worktree/internal-ref",
			coding.ErrTeamAdmission,
		)
	}
	model := readyModelWithController(t, controller, true)
	driveModelCommands(t, model, model.openTeamRoute("inspect error disclosure"))

	content := model.View().Content
	assert.Contains(t, content, "review Runtime diagnostics")
	assert.NotContains(t, content, "/root/private")
	assert.NotContains(t, content, "internal-ref")
}

func TestTeamRouteRevisionCancelAndDirtyAdmissionDefaultStop(t *testing.T) {
	t.Parallel()

	controller := newTeamRouteTestController(readyState())
	controller.proposal.Dirty = true
	controller.revised = controller.proposal.Clone()
	controller.revised.ID = "proposal-2"
	controller.revised.Request.Tasks[0].Title = "Revised task"
	model := readyModelWithController(t, controller, true)
	driveModelCommands(t, model, model.openTeamRoute("deliver Team UI"))

	model.Update(key("r"))
	model.Update(tea.KeyPressMsg{Text: "split the implementation and tests"})
	_, command := model.Update(key(keyEnter))
	driveModelCommands(t, model, command)
	require.NotNil(t, model.route.team.proposal)
	assert.Equal(t, "proposal-2", model.route.team.proposal.ID)
	proposalID, feedback := controller.lastRevision()
	assert.Equal(t, "proposal-1", proposalID)
	assert.Equal(t, "split the implementation and tests", feedback)

	model.Update(key(keyEnter))
	assert.Equal(t, teamRouteConfirmation, model.route.team.stage)
	assert.Zero(t, model.route.cursor, "dirty admission must default to stopping")
	_, command = model.Update(key(keyEnter))
	assert.Nil(t, command)
	assert.Equal(t, teamRouteProposal, model.route.team.stage)
	assert.Empty(t, controller.confirmationsSnapshot())

	model.Update(key(keyEnter))
	model.Update(key(keyDown))
	_, command = model.Update(key(keyEnter))
	driveModelCommands(t, model, command)
	require.Len(t, controller.confirmationsSnapshot(), 1)
	assert.Equal(t, coding.TeamAdmissionHEADOnly, controller.confirmationsSnapshot()[0].Admission)
	assert.Equal(t, teamRouteActive, model.route.team.stage)

	cancelController := newTeamRouteTestController(readyState())
	cancelModel := readyModelWithController(t, cancelController, true)
	driveModelCommands(t, cancelModel, cancelModel.openTeamRoute("cancel this preview"))
	_, command = cancelModel.Update(key("c"))
	driveModelCommands(t, cancelModel, command)
	assert.Equal(t, routeNone, cancelModel.route.kind)
	assert.Equal(t, []string{"proposal-1"}, cancelController.declinesSnapshot())
}

func TestTeamRouteProposalCancellationIsSingleFlightAndCleansLateSuccess(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	controller := newTeamRouteTestController(readyState())
	controller.generate = func(context.Context, coding.TeamProposalPrompt) (coding.TeamProposal, error) {
		close(started)
		<-release

		return controller.proposal.Clone(), nil
	}
	model := readyModelWithController(t, controller, true)
	command := model.openTeamRoute("inspect cancellation")
	require.NotNil(t, command)
	result := make(chan tea.Msg, 1)
	go func() { result <- command() }()
	<-started

	_, duplicate := model.Update(key(keyEnter))
	assert.Nil(t, duplicate)
	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	close(release)
	_, cleanup := model.Update(<-result)
	driveModelCommands(t, model, cleanup)

	assert.Equal(t, teamRouteObjective, model.route.team.stage)
	assert.Nil(t, model.route.team.proposal)
	assert.Equal(t, []string{"proposal-1"}, controller.declinesSnapshot())
}

func TestActiveTeamRouteSubmitsEveryExactControlAndRefreshesByEvent(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Teams = []coding.TeamLifecycleState{{TeamLifecycle: coding.TeamLifecycle{
		TeamID: "team-1", State: coding.TeamLifecycleAdmitted,
	}}}
	controller := newTeamRouteTestController(state)
	model := readyModelWithController(t, controller, true)
	driveModelCommands(t, model, model.openTeamRoute(""))
	require.Equal(t, teamRouteActive, model.route.team.stage)

	submitTeamRouteTextControl(t, model, "m", "check the ownership edge")
	submitTeamRouteTextControl(t, model, "f", "then run the focused test")
	submitTeamRouteConfirmedControl(t, model, "i")
	submitTeamRouteConfirmedControl(t, model, "x")

	failed := controller.viewSnapshot()
	failed.Tasks[0].Status = team.TaskStatusFailed
	failed.Attempts[0].DomainState = team.AttemptStatusFailed
	controller.setView(failed)
	view := failed.Clone()
	model.route.team.view = &view
	submitTeamRouteConfirmedControl(t, model, "r")
	submitTeamRouteConfirmedControl(t, model, "C")

	controls := controller.controlsSnapshot()
	require.Len(t, controls, 6)
	for _, index := range []int{0, 1, 2} {
		assert.Equal(t, team.MemberID("worker-1"), controls[index].MemberID)
		assert.Equal(t, team.TaskID("task-1"), controls[index].TaskID)
		assert.Equal(t, team.AttemptID("attempt-1"), controls[index].ExpectedAttemptID)
		assert.Equal(t, uint64(7), controls[index].OwnerGeneration)
	}
	assert.Equal(t, "check the ownership edge", controls[0].Text)
	assert.Equal(t, "then run the focused test", controls[1].Text)
	assert.Empty(t, controls[3].MemberID)
	assert.Empty(t, controls[4].MemberID)
	assert.Empty(t, controls[5].TaskID)

	updated := controller.viewSnapshot()
	updated.Objective = "refreshed objective"
	controller.setView(updated)
	refresh := model.invalidateTeamRoute(coding.Event{Payload: coding.TeamControlLifecycle{
		TeamID: "team-1", Revision: 12, CommandID: "control-event",
		Action: coding.TeamControlMessage, State: coding.TeamControlApplied,
	}})
	require.NotNil(t, refresh)
	driveModelCommands(t, model, refresh)
	assert.Equal(t, "refreshed objective", model.route.team.view.Objective)
	require.Len(t, model.route.team.view.Controls, 1)
	assert.Equal(t, coding.TeamControlApplied, model.route.team.view.Controls[0].State)

	late := model.invalidateTeamRoute(coding.Event{Payload: coding.TeamLifecycle{
		TeamID: "team-1", State: coding.TeamLifecycleRunning,
	}})
	require.NotNil(t, late)
	message := late()
	model.closeRouteToParent()
	model.Update(message)
	assert.Equal(t, routeNone, model.route.kind)
}

func TestActiveTeamRouteIsReadOnlyInPlanMode(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Mode = coding.ModePlan
	state.Teams = []coding.TeamLifecycleState{{TeamLifecycle: coding.TeamLifecycle{
		TeamID: "team-1", State: coding.TeamLifecycleRunning,
	}}}
	controller := newTeamRouteTestController(state)
	model := readyModelWithController(t, controller, true)
	driveModelCommands(t, model, model.openTeamRoute(""))

	_, command := model.Update(key("m"))
	assert.Nil(t, command)
	require.Error(t, model.route.err)
	assert.Contains(t, model.View().Content, "Plan Mode")
	assert.Empty(t, controller.controlsSnapshot())
}

func submitTeamRouteTextControl(t *testing.T, model *Model, keyValue, text string) {
	t.Helper()

	model.Update(key(keyValue))
	require.Equal(t, teamRouteControlInput, model.route.team.stage)
	model.Update(tea.KeyPressMsg{Text: text})
	_, command := model.Update(key(keyEnter))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)
	require.Equal(t, teamRouteActive, model.route.team.stage)
}

func submitTeamRouteConfirmedControl(t *testing.T, model *Model, keyValue string) {
	t.Helper()

	model.Update(key(keyValue))
	require.Equal(t, teamRouteControlConfirmation, model.route.team.stage)
	_, command := model.Update(key(keyEnter))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)
	require.Equal(t, teamRouteActive, model.route.team.stage)
}

func commandNamed(t *testing.T, name string) commandDescriptor {
	t.Helper()

	for _, command := range commands {
		if command.name == name {
			return command
		}
	}

	t.Fatalf("command %q not found", name)

	return commandDescriptor{}
}

type teamRouteRevisionCall struct {
	proposalID string
	feedback   string
}

type teamRouteResumeCall struct {
	teamID   team.ID
	decision coding.TeamResumeDecision
}

type teamRouteTestController struct {
	*overlayController

	mu                    sync.Mutex
	proposal              coding.TeamProposal
	revised               coding.TeamProposal
	view                  coding.TeamView
	generate              func(context.Context, coding.TeamProposalPrompt) (coding.TeamProposal, error)
	objectives            []string
	revisions             []teamRouteRevisionCall
	confirmations         []coding.TeamConfirmation
	declines              []string
	controls              []coding.TeamControlRequest
	reads                 []coding.TeamReadRequest
	resumes               []teamRouteResumeCall
	integrationPreview    coding.TeamIntegrationPreview
	integrationRecoveries []coding.TeamIntegrationRecovery
	integrationPrepares   []coding.TeamIntegrationRequest
	integrationApplies    []coding.TeamIntegrationApproval
	integrationRejects    []coding.TeamIntegrationApproval
	integrationRecovery   []coding.TeamIntegrationRecoveryRequest
	cleanups              []coding.TeamCleanupRequest
}

func newTeamRouteTestController(state coding.State) *teamRouteTestController {
	proposal := testTeamRouteProposal()

	return &teamRouteTestController{
		overlayController: newOverlayController(state),
		proposal:          proposal,
		revised:           proposal.Clone(),
		view:              testTeamRouteView(),
	}
}

func (c *teamRouteTestController) GenerateTeamProposal(
	ctx context.Context,
	prompt coding.TeamProposalPrompt,
) (coding.TeamProposal, error) {
	c.mu.Lock()
	c.objectives = append(c.objectives, prompt.Objective)
	generate := c.generate
	proposal := c.proposal.Clone()
	c.mu.Unlock()
	if generate != nil {
		return generate(ctx, prompt)
	}

	return proposal, nil
}

func (c *teamRouteTestController) ReviseTeamProposal(
	_ context.Context,
	proposalID string,
	feedback string,
) (coding.TeamProposal, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.revisions = append(c.revisions, teamRouteRevisionCall{
		proposalID: proposalID, feedback: feedback,
	})

	return c.revised.Clone(), nil
}

func (c *teamRouteTestController) DeclineTeam(_ context.Context, proposalID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.declines = append(c.declines, proposalID)

	return nil
}

func (c *teamRouteTestController) ConfirmTeam(
	_ context.Context,
	confirmation coding.TeamConfirmation,
) (coding.TeamReference, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.confirmations = append(c.confirmations, confirmation)

	return coding.TeamReference{
		TeamID: c.view.TeamID, Admission: confirmation.Admission,
		LeadMember: c.view.LeadMemberID,
	}, nil
}

func (c *teamRouteTestController) ReadTeam(
	_ context.Context,
	request coding.TeamReadRequest,
) (coding.TeamView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads = append(c.reads, request)

	return c.view.Clone(), nil
}

func (c *teamRouteTestController) SubmitTeamControl(
	_ context.Context,
	request coding.TeamControlRequest,
) (coding.TeamControlReference, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.controls = append(c.controls, request)

	return coding.TeamControlReference{
		CommandID: team.CommandID("control-1"), TeamID: request.TeamID,
		Action: request.Action, CreatedAt: time.Now().UTC(),
	}, nil
}

func (c *teamRouteTestController) generatedObjective() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.objectives) == 0 {
		return ""
	}

	return c.objectives[len(c.objectives)-1]
}

func (c *teamRouteTestController) lastRevision() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.revisions) == 0 {
		return "", ""
	}
	value := c.revisions[len(c.revisions)-1]

	return value.proposalID, value.feedback
}

func (c *teamRouteTestController) confirmationsSnapshot() []coding.TeamConfirmation {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]coding.TeamConfirmation(nil), c.confirmations...)
}

func (c *teamRouteTestController) declinesSnapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]string(nil), c.declines...)
}

func (c *teamRouteTestController) controlsSnapshot() []coding.TeamControlRequest {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]coding.TeamControlRequest(nil), c.controls...)
}

func (c *teamRouteTestController) viewSnapshot() coding.TeamView {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.view.Clone()
}

func (c *teamRouteTestController) setView(view coding.TeamView) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.view = view.Clone()
}

func testTeamRouteProposal() coding.TeamProposal {
	return coding.TeamProposal{
		ID: "proposal-1",
		Request: coding.TeamProposalRequest{
			Objective: "deliver Team UI",
			Workers:   []coding.TeamWorkerSpec{{Name: "Builder", Role: "implementation"}},
			Tasks: []coding.TeamTaskSpec{{
				ID: "task-1", Title: "Build Team route", AssignedWorker: "Builder",
			}},
		},
		ExpiresAt: time.Now().Add(time.Minute).UTC(),
	}
}

func testTeamRouteView() coding.TeamView {
	return coding.TeamView{
		TeamID: "team-1", LeadMemberID: "lead",
		Revision: 5, ChangeCursor: 5, ResourceRevision: 4,
		Status: team.StatusActive, ResourceState: teamstate.StateActive,
		Objective: "deliver Team UI",
		Members: []coding.TeamMemberView{
			{ID: "lead", Name: "Lead", Role: "coordination", Status: team.MemberStatusActive},
			{ID: "worker-1", Name: "Builder", Role: "implementation", Status: team.MemberStatusActive},
		},
		Tasks: []coding.TeamTaskView{{
			ID: "task-1", Title: "Build Team route",
			AssignedMemberID: "worker-1", Status: team.TaskStatusRunning,
		}},
		Attempts: []coding.TeamAttemptView{{
			Target: coding.TeamWorkerTarget{
				TeamID: "team-1", MemberID: "worker-1", TaskID: "task-1",
				AttemptID: "attempt-1", OwnerGeneration: 7,
			},
			Number: 1, StartedAt: time.Now().Add(-time.Second).UTC(),
			DomainState:    team.AttemptStatusRunning,
			ResourceState:  teamstate.AttemptRunning,
			LifecycleState: coding.TeamLifecycleRunning,
			Activity:       coding.TeamActivityWorking,
		}},
	}
}
