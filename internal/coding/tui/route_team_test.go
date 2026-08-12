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
	"github.com/rsbin1178/pips/agent/team"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/teamstate"
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

func TestTeamRouteExplainsRepositoryPrerequisite(t *testing.T) {
	t.Parallel()

	controller := newTeamRouteTestController(readyState())
	controller.generate = func(context.Context, coding.TeamProposalPrompt) (coding.TeamProposal, error) {
		return coding.TeamProposal{}, fmt.Errorf(
			"%w: %w",
			coding.ErrTeamAdmission,
			coding.ErrTeamRepositoryRequired,
		)
	}
	model := readyModelWithController(t, controller, true)
	driveModelCommands(t, model, model.openTeamRoute("inspect frontend and backend bugs"))

	content := model.View().Content
	assert.Contains(t, content, "Git repository root with at")
	assert.Contains(t, content, "least one commit")
	assert.Contains(t, content, "Create the initial commit, then retry")
	assert.NotContains(t, content, "review Runtime diagnostics")
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
	assert.Equal(t, routeNone, model.route.kind)
	assert.Contains(t, ansi.Strip(model.View().Content), "Team · 1 Workers")

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
	batch, ok := command().(tea.BatchMsg)
	require.True(t, ok)
	require.NotEmpty(t, batch)
	result := make(chan tea.Msg, 1)
	go func() { result <- batch[0]() }()
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
	controlErr            error
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
	if c.controlErr != nil {
		return coding.TeamControlReference{}, c.controlErr
	}

	return coding.TeamControlReference{
		CommandID: team.CommandID("control-1"), TeamID: request.TeamID,
		Action: request.Action, CreatedAt: time.Now().UTC(),
	}, nil
}

func TestTeamRouteComposerExpandsPasteAndKeepsInlineFilePickerOwnership(t *testing.T) {
	t.Parallel()

	controller := newTeamRouteTestController(readyState())
	model := readyModelWithController(t, controller, true)
	driveModelCommands(t, model, model.openTeamRoute(""))

	pasted := strings.Repeat("full requirement line\n", 9)
	placeholder, err := model.composer.InsertPaste(pasted)
	require.NoError(t, err)
	assert.True(t, placeholder)
	assert.Contains(t, model.composer.Value(), "Pasted text")

	_, command := model.Update(key(keyEnter))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)
	assert.Equal(t, strings.TrimSpace(pasted), controller.generatedObjective())

	other := readyModelWithController(t, newTeamRouteTestController(readyState()), true)
	driveModelCommands(t, other, other.openTeamRoute(""))
	_, command = other.Update(tea.KeyPressMsg{Text: "@"})
	require.NotNil(t, command)
	assert.Equal(t, pickerFile, other.picker.kind)
	assert.Equal(t, routeTeam, other.route.kind)
	assert.Contains(t, other.View().Content, "Loading Workspace files")
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
