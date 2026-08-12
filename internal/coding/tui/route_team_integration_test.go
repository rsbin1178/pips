//nolint:wsl_v5 // Route decisions and exact Controller assertions stay adjacent.
package tui

import (
	"context"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent/team"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/teamintegration"
	"github.com/rsbin1178/pips/internal/coding/teamstate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTeamRouteRecoveryRequiresExplicitRetryDecision(t *testing.T) {
	t.Parallel()

	controller := newTeamRouteTestController(readyState())
	controller.recoveries = []coding.TeamRecoveryCandidate{{
		TeamID: "private-team-id", ResourceRevision: 9,
		ResourceState: teamstate.StateInterrupted, TeamStatus: team.StatusActive,
		Disposition: coding.TeamRecoveryResume,
		Attempts: []coding.TeamAttemptRecoveryCandidate{{
			TaskID: "private-task", AttemptID: "private-attempt", RetryWork: true,
		}},
		Diagnostics: []coding.TeamRecoveryDiagnostic{{Code: "interrupted_work"}},
	}}
	controller.view.TeamID = "private-team-id"
	for index := range controller.view.Attempts {
		controller.view.Attempts[index].Target.TeamID = "private-team-id"
	}
	model := readyModelWithController(t, controller, true)
	driveModelCommands(t, model, model.openTeamRoute(""))

	require.Equal(t, teamRouteRecovery, model.route.team.stage)
	content := model.View().Content
	assert.Contains(t, content, "Discovery is read-only")
	assert.NotContains(t, content, "private-team-id")
	assert.NotContains(t, content, "private-task")

	model.Update(key("enter"))
	require.Equal(t, teamRouteRecoveryConfirmation, model.route.team.stage)
	model.Update(key("down"))
	assert.True(t, model.route.team.retryWork)
	_, command := model.Update(key("enter"))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	require.Len(t, controller.resumes, 1)
	assert.Equal(t, team.ID("private-team-id"), controller.resumes[0].teamID)
	assert.Equal(t, team.Revision(9), controller.resumes[0].decision.ExpectedResourceRevision)
	assert.True(t, controller.resumes[0].decision.RetryInterruptedWork)
	assert.Equal(t, routeNone, model.route.kind)
	assert.Contains(t, model.View().Content, "Team · 1 Workers")
}

func TestSessionResumeAutomaticallyOpensReadOnlyTeamRecoveryReview(t *testing.T) {
	t.Parallel()

	controller := newTeamRouteTestController(readyState())
	controller.recoveries = []coding.TeamRecoveryCandidate{{
		TeamID: "private-team-id", ResourceRevision: 9,
		ResourceState: teamstate.StateInterrupted, TeamStatus: team.StatusActive,
		Disposition: coding.TeamRecoveryResume,
	}}
	model := readyModelWithController(t, controller, true)
	_, command := model.Update(controlResultMsg{operation: operationResume})
	driveModelCommands(t, model, command)

	require.Equal(t, routeTeam, model.route.kind)
	require.Equal(t, teamRouteRecovery, model.route.team.stage)
	assert.Contains(t, model.View().Content, "Discovery is read-only")
	assert.NotContains(t, model.View().Content, "private-team-id")
	assert.Empty(t, controller.resumes)
}

func TestTeamRouteIntegrationPreviewApplyAndRejectUseExactToken(t *testing.T) {
	t.Parallel()

	controller := newTeamIntegrationRouteController()
	model := readyModelWithController(t, controller, true)
	driveModelCommands(t, model, model.openTeamRoute(""))

	_, load := model.Update(key("g"))
	require.NotNil(t, load)
	driveModelCommands(t, model, load)
	require.Equal(t, teamRouteIntegration, model.route.team.stage)
	assert.Contains(t, model.View().Content, "[x] Build Team route")

	_, prepare := model.Update(key("p"))
	require.NotNil(t, prepare)
	driveModelCommands(t, model, prepare)
	require.Equal(t, teamRouteIntegrationPreview, model.route.team.stage)
	require.Len(t, controller.integrationPrepares, 1)
	assert.Empty(t, controller.integrationPrepares[0].TaskIDs)
	content := model.View().Content
	assert.Contains(t, content, "1 added · 1 changed · 0 deleted")
	for _, private := range []string{
		"integration-private", "token-private", "secret/path.go", "digest-private",
	} {
		assert.NotContains(t, content, private)
	}

	model.Update(key("enter"))
	require.Equal(t, teamRouteIntegrationConfirmation, model.route.team.stage)
	_, apply := model.Update(key("enter"))
	require.NotNil(t, apply)
	driveModelCommands(t, model, apply)
	require.Len(t, controller.integrationApplies, 1)
	assert.Equal(t, "integration-private", controller.integrationApplies[0].ID)
	assert.Equal(t, "token-private", controller.integrationApplies[0].Token)
	assert.Empty(t, controller.controlsSnapshot())

	_, prepare = model.Update(key("p"))
	require.NotNil(t, prepare)
	driveModelCommands(t, model, prepare)
	model.Update(key("x"))
	_, reject := model.Update(key("enter"))
	require.NotNil(t, reject)
	driveModelCommands(t, model, reject)
	require.Len(t, controller.integrationRejects, 1)
	assert.Equal(t, "integration-private", controller.integrationRejects[0].ID)
	assert.Equal(t, "token-private", controller.integrationRejects[0].Token)
}

func TestTeamRouteIntegrationRecoveryUsesOnlyManagerActions(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		key    string
		action teamintegration.RecoveryAction
	}{
		{name: "complete", key: "c", action: teamintegration.RecoveryComplete},
		{name: "rollback", key: "b", action: teamintegration.RecoveryRollback},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			controller := newTeamIntegrationRouteController()
			controller.integrationRecoveries = []coding.TeamIntegrationRecovery{{
				ID: "recovery-private", State: "applying", Base: 1, Target: 2,
				JournalHash: "journal-private",
			}}
			model := readyModelWithController(t, controller, true)
			driveModelCommands(t, model, model.openTeamRoute(""))
			_, load := model.Update(key("g"))
			driveModelCommands(t, model, load)

			content := model.View().Content
			assert.Contains(t, content, "Interrupted apply journals")
			assert.NotContains(t, content, "recovery-private")
			assert.NotContains(t, content, "journal-private")
			model.Update(key(test.key))
			_, recoveryCommand := model.Update(key("enter"))
			require.NotNil(t, recoveryCommand)
			driveModelCommands(t, model, recoveryCommand)

			require.Len(t, controller.integrationRecovery, 1)
			assert.Equal(t, "recovery-private", controller.integrationRecovery[0].ID)
			assert.Equal(t, test.action, controller.integrationRecovery[0].Action)
		})
	}
}

func TestTeamRouteCleanupBindsExactRevisionAndSeparatesCloseWithoutIntegration(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name                    string
		state                   teamstate.State
		closeWithoutIntegration bool
		label                   string
	}{
		{
			name: "close without Integration", state: teamstate.StateWorkComplete,
			closeWithoutIntegration: true, label: "Close without Integration",
		},
		{
			name: "clean after Integration", state: teamstate.StateIntegrated,
			closeWithoutIntegration: false, label: "after the applied Integration",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			controller := newTeamIntegrationRouteController()
			controller.view.ResourceRevision = 17
			controller.view.ResourceState = test.state
			controller.view.Cleanup = coding.TeamCleanupView{Class: teamstate.CleanupEligible, Eligible: 2}
			model := readyModelWithController(t, controller, true)
			driveModelCommands(t, model, model.openTeamRoute(""))
			_, load := model.Update(key("g"))
			driveModelCommands(t, model, load)

			assert.Contains(t, model.View().Content, "2 eligible")
			model.Update(key("d"))
			require.Equal(t, teamRouteIntegrationConfirmation, model.route.team.stage)
			assert.Contains(t, model.View().Content, test.label)
			_, cleanup := model.Update(key("enter"))
			require.NotNil(t, cleanup)
			driveModelCommands(t, model, cleanup)

			require.Len(t, controller.cleanups, 1)
			assert.Equal(t, team.ID("team-1"), controller.cleanups[0].TeamID)
			assert.Equal(t, team.Revision(17), controller.cleanups[0].ExpectedResourceRevision)
			assert.Equal(t, test.closeWithoutIntegration, controller.cleanups[0].CloseWithoutIntegration)
			assert.Equal(t, routeNone, model.route.kind)
		})
	}
}

func TestTeamRouteCleanupRequiresPendingPreviewRejectionFirst(t *testing.T) {
	t.Parallel()

	controller := newTeamIntegrationRouteController()
	model := readyModelWithController(t, controller, true)
	driveModelCommands(t, model, model.openTeamRoute(""))
	_, load := model.Update(key("g"))
	driveModelCommands(t, model, load)
	_, prepare := model.Update(key("p"))
	driveModelCommands(t, model, prepare)
	model.Update(key(keyEscape))
	require.Equal(t, teamRouteIntegration, model.route.team.stage)

	_, cleanup := model.Update(key("d"))
	assert.Nil(t, cleanup)
	require.Error(t, model.route.err)
	assert.Contains(t, model.View().Content, "reject the pending Integration preview")
	assert.Empty(t, controller.cleanups)
}

func newTeamIntegrationRouteController() *teamRouteTestController {
	state := readyState()
	state.Teams = []coding.TeamLifecycleState{{TeamLifecycle: coding.TeamLifecycle{
		TeamID: "team-1", State: coding.TeamLifecycleRunning,
	}}}
	controller := newTeamRouteTestController(state)
	controller.view.Tasks[0].Status = team.TaskStatusCompleted
	controller.view.Attempts[0].DomainState = team.AttemptStatusCompleted
	controller.view.Attempts[0].LifecycleState = coding.TeamLifecycleCompleted
	controller.view.Attempts[0].Activity = ""
	controller.integrationPreview = coding.TeamIntegrationPreview{
		ID: "integration-private", AttemptIDs: []team.AttemptID{"attempt-1"},
		Manifest: teamintegration.Manifest{
			Entries: []teamintegration.ManifestEntry{{Path: "secret/path.go"}},
			Added:   1, Changed: 1, Digest: "digest-private",
		},
		Verification:  teamintegration.Verification{Status: teamintegration.VerificationNotRun},
		ApprovalToken: "token-private", ExpiresAt: time.Now().Add(time.Minute).UTC(),
		ProducesUnstagedChanges: true,
	}

	return controller
}

func (c *teamRouteTestController) ResumeTeam(
	_ context.Context,
	teamID team.ID,
	decision coding.TeamResumeDecision,
) (coding.TeamReference, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resumes = append(c.resumes, teamRouteResumeCall{teamID: teamID, decision: decision})

	return coding.TeamReference{TeamID: teamID, LeadMember: c.view.LeadMemberID}, nil
}

func (c *teamRouteTestController) PrepareTeamIntegration(
	_ context.Context,
	request coding.TeamIntegrationRequest,
) (coding.TeamIntegrationPreview, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	request.TaskIDs = append([]team.TaskID(nil), request.TaskIDs...)
	c.integrationPrepares = append(c.integrationPrepares, request)

	return cloneTeamRouteIntegrationPreview(c.integrationPreview), nil
}

func (c *teamRouteTestController) ApplyTeamIntegration(
	_ context.Context,
	approval coding.TeamIntegrationApproval,
) (coding.TeamIntegrationResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.integrationApplies = append(c.integrationApplies, approval)

	return coding.TeamIntegrationResult{ID: approval.ID, State: "applied", Files: 1}, nil
}

func (c *teamRouteTestController) RejectTeamIntegration(
	_ context.Context,
	approval coding.TeamIntegrationApproval,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.integrationRejects = append(c.integrationRejects, approval)

	return nil
}

func (c *teamRouteTestController) TeamIntegrationRecoveries(
	context.Context,
) ([]coding.TeamIntegrationRecovery, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]coding.TeamIntegrationRecovery(nil), c.integrationRecoveries...), nil
}

func (c *teamRouteTestController) RecoverTeamIntegration(
	_ context.Context,
	request coding.TeamIntegrationRecoveryRequest,
) (coding.TeamIntegrationResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.integrationRecovery = append(c.integrationRecovery, request)
	c.integrationRecoveries = nil

	return coding.TeamIntegrationResult{ID: request.ID, State: string(request.Action)}, nil
}

func (c *teamRouteTestController) CleanupTeam(
	_ context.Context,
	request coding.TeamCleanupRequest,
) (coding.TeamCleanupResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cleanups = append(c.cleanups, request)

	return coding.TeamCleanupResult{
		TeamID: request.TeamID, State: teamstate.StateClosedWithoutIntegration, Cleaned: 2,
	}, nil
}
