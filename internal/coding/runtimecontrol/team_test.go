package runtimecontrol

import (
	"context"
	"testing"

	"github.com/rsbin1178/pips/agent/team"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/question"
	"github.com/rsbin1178/pips/internal/coding/teamintegration"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestControllerTeamMethodsDelegateAndReturnDefensiveValues(t *testing.T) {
	t.Parallel()

	target := coding.TeamWorkerTarget{
		TeamID: "team-1", MemberID: "worker-1", TaskID: "task-1",
		AttemptID: "attempt-1", OwnerGeneration: 4,
	}
	runtime := &teamFakeRuntime{
		fakeRuntime: &fakeRuntime{state: coding.State{
			SessionID: "parent-session", SessionOpen: true,
			Mode: coding.ModeAgent, Phase: coding.PhaseIdle,
		}},
		proposal: coding.TeamProposal{
			ID: "proposal-1",
			Request: coding.TeamProposalRequest{
				Objective: "objective",
				Workers:   []coding.TeamWorkerSpec{{Name: "Worker", Role: "role"}},
				Tasks: []coding.TeamTaskSpec{{
					ID: "task-1", Title: "task", AssignedWorker: "Worker",
					Dependencies: []string{"dependency"},
				}},
			},
		},
		view: coding.TeamView{
			TeamID: "team-1",
			Tasks: []coding.TeamTaskView{{
				ID: "task-1", DependencyIDs: []team.TaskID{"dependency"},
			}},
		},
		workerState: coding.State{
			SessionID: "worker-session", SessionOpen: true,
			Transcript: []ai.Message{ai.UserText("original")},
		},
		recovery: []coding.TeamRecoveryCandidate{{
			TeamID: "team-1", ResourceRevision: 8,
			Attempts:    []coding.TeamAttemptRecoveryCandidate{{TaskID: "task-1"}},
			Diagnostics: []coding.TeamRecoveryDiagnostic{{Code: "review"}},
		}},
		integrationPreview: coding.TeamIntegrationPreview{
			ID: "integration-1", AttemptIDs: []team.AttemptID{"attempt-1"},
			Manifest:      teamintegration.Manifest{Entries: []teamintegration.ManifestEntry{{Path: "a.go"}}},
			ApprovalToken: "token-1",
		},
		integrationRecoveries: []coding.TeamIntegrationRecovery{{
			ID: "integration-1", State: "applying", Base: 1,
		}},
	}
	controller := &Controller{runtime: runtime, closeDone: make(chan struct{})}

	firstProposal, err := controller.GenerateTeamProposal(t.Context(), coding.TeamProposalPrompt{
		Objective: "objective",
	})
	require.NoError(t, err)

	firstProposal.Request.Tasks[0].Dependencies[0] = "mutated"
	secondProposal, err := controller.GenerateTeamProposal(t.Context(), coding.TeamProposalPrompt{
		Objective: "objective",
	})
	require.NoError(t, err)
	assert.Equal(t, "dependency", secondProposal.Request.Tasks[0].Dependencies[0])

	firstView, err := controller.ReadTeam(t.Context(), coding.TeamReadRequest{TeamID: "team-1"})
	require.NoError(t, err)

	firstView.Tasks[0].DependencyIDs[0] = "mutated"
	secondView, err := controller.ReadTeam(t.Context(), coding.TeamReadRequest{TeamID: "team-1"})
	require.NoError(t, err)
	assert.Equal(t, team.TaskID("dependency"), secondView.Tasks[0].DependencyIDs[0])

	firstState, err := controller.InspectTeamWorkerState(t.Context(), target)
	require.NoError(t, err)

	firstState.Transcript[0] = ai.UserText("mutated")
	secondState, err := controller.InspectTeamWorkerState(t.Context(), target)
	require.NoError(t, err)
	assert.NotEqual(t, firstState.Transcript, secondState.Transcript)

	resolution := question.Resolution{
		RequestID: "question-1", SchemaDigest: "digest",
		Answers: []question.Answer{{Selections: []string{"original"}}},
	}
	_, err = controller.ResolveTeamWorkerQuestion(t.Context(), target, resolution)
	require.NoError(t, err)
	assert.Equal(t, "original", resolution.Answers[0].Selections[0])
	assert.Equal(t, "changed by fake", runtime.questionResolution.Answers[0].Selections[0])

	observation, err := controller.ObserveTeamWorker(t.Context(), target)
	require.NoError(t, err)
	assert.Equal(t, "worker-session", observation.State.SessionID)
	assert.Equal(t, target, runtime.target)

	firstRecovery, err := controller.DiscoverTeamRecovery(t.Context())
	require.NoError(t, err)

	firstRecovery[0].Attempts[0].TaskID = "mutated"
	firstRecovery[0].Diagnostics[0].Code = "mutated"
	secondRecovery, err := controller.DiscoverTeamRecovery(t.Context())
	require.NoError(t, err)
	assert.Equal(t, team.TaskID("task-1"), secondRecovery[0].Attempts[0].TaskID)
	assert.Equal(t, "review", secondRecovery[0].Diagnostics[0].Code)

	request := coding.TeamIntegrationRequest{TaskIDs: []team.TaskID{"task-1"}}
	firstPreview, err := controller.PrepareTeamIntegration(t.Context(), request)
	require.NoError(t, err)

	firstPreview.AttemptIDs[0] = "mutated"
	firstPreview.Manifest.Entries[0].Path = "mutated"
	secondPreview, err := controller.PrepareTeamIntegration(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, team.AttemptID("attempt-1"), secondPreview.AttemptIDs[0])
	assert.Equal(t, "a.go", secondPreview.Manifest.Entries[0].Path)
	assert.Equal(t, team.TaskID("task-1"), request.TaskIDs[0])

	_, err = controller.ResumeTeam(t.Context(), "team-1", coding.TeamResumeDecision{
		ExpectedResourceRevision: 8, RetryInterruptedWork: true,
	})
	require.NoError(t, err)
	assert.Equal(t, team.ID("team-1"), runtime.resumeTeamID)
	assert.True(t, runtime.resumeDecision.RetryInterruptedWork)

	_, err = controller.ApplyTeamIntegration(t.Context(), coding.TeamIntegrationApproval{
		ID: "integration-1", Token: "token-1",
	})
	require.NoError(t, err)
	require.NoError(t, controller.RejectTeamIntegration(t.Context(), coding.TeamIntegrationApproval{
		ID: "integration-2", Token: "token-2",
	}))
	recoveries, err := controller.TeamIntegrationRecoveries(t.Context())
	require.NoError(t, err)
	require.Len(t, recoveries, 1)
	recoveries[0].State = "mutated"
	again, err := controller.TeamIntegrationRecoveries(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "applying", again[0].State)
	_, err = controller.RecoverTeamIntegration(t.Context(), coding.TeamIntegrationRecoveryRequest{
		ID: "integration-1", Action: teamintegration.RecoveryComplete,
	})
	require.NoError(t, err)
	assert.Equal(t, teamintegration.RecoveryComplete, runtime.integrationRecovery.Action)

	cleanupRequest := coding.TeamCleanupRequest{
		TeamID: "team-1", ExpectedResourceRevision: 12, CloseWithoutIntegration: true,
	}
	cleanup, err := controller.CleanupTeam(t.Context(), cleanupRequest)
	require.NoError(t, err)
	assert.Equal(t, team.ID("team-1"), cleanup.TeamID)
	assert.Equal(t, cleanupRequest, runtime.cleanupRequest)
}

func TestControllerTeamOperationLeaseBlocksReplacement(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	runtime := &blockingTeamFakeRuntime{
		fakeRuntime: &fakeRuntime{state: coding.State{
			SessionID: "parent-session", SessionOpen: true,
			Mode: coding.ModeAgent, Phase: coding.PhaseIdle,
		}},
		started: started,
		release: release,
	}
	controller := &Controller{runtime: runtime, closeDone: make(chan struct{})}
	result := make(chan error, 1)

	go func() {
		_, err := controller.GenerateTeamProposal(t.Context(), coding.TeamProposalPrompt{
			Objective: "objective",
		})
		result <- err
	}()

	<-started

	_, err := controller.beginReplacement(t.Context())
	require.ErrorIs(t, err, ErrBusy)

	close(release)
	require.NoError(t, <-result)
	controller.mu.Lock()
	assert.Zero(t, controller.active)
	controller.mu.Unlock()
}

type teamFakeRuntime struct {
	*fakeRuntime
	proposal              coding.TeamProposal
	view                  coding.TeamView
	workerState           coding.State
	target                coding.TeamWorkerTarget
	questionResolution    question.Resolution
	recovery              []coding.TeamRecoveryCandidate
	resumeTeamID          team.ID
	resumeDecision        coding.TeamResumeDecision
	integrationPreview    coding.TeamIntegrationPreview
	integrationRecoveries []coding.TeamIntegrationRecovery
	integrationRequest    coding.TeamIntegrationRequest
	integrationApproval   coding.TeamIntegrationApproval
	integrationRejection  coding.TeamIntegrationApproval
	integrationRecovery   coding.TeamIntegrationRecoveryRequest
	cleanupRequest        coding.TeamCleanupRequest
}

func (r *teamFakeRuntime) GenerateTeamProposal(
	context.Context,
	coding.TeamProposalPrompt,
) (coding.TeamProposal, error) {
	return r.proposal, nil
}

func (r *teamFakeRuntime) ReadTeam(
	context.Context,
	coding.TeamReadRequest,
) (coding.TeamView, error) {
	return r.view, nil
}

func (r *teamFakeRuntime) ResolveTeamWorkerQuestion(
	_ context.Context,
	target coding.TeamWorkerTarget,
	resolution question.Resolution,
) (coding.TeamControlReference, error) {
	r.target = target
	resolution.Answers[0].Selections[0] = "changed by fake"
	r.questionResolution = resolution

	return coding.TeamControlReference{}, nil
}

func (r *teamFakeRuntime) ObserveTeamWorker(
	_ context.Context,
	target coding.TeamWorkerTarget,
) (coding.EventObservation, error) {
	r.target = target

	return coding.EventObservation{State: r.workerState}, nil
}

func (r *teamFakeRuntime) InspectTeamWorkerState(
	context.Context,
	coding.TeamWorkerTarget,
) (coding.State, error) {
	return r.workerState, nil
}

func (r *teamFakeRuntime) DiscoverTeamRecovery(
	context.Context,
) ([]coding.TeamRecoveryCandidate, error) {
	return cloneTeamRecoveryCandidates(r.recovery), nil
}

func (r *teamFakeRuntime) ResumeTeam(
	_ context.Context,
	teamID team.ID,
	decision coding.TeamResumeDecision,
) (coding.TeamReference, error) {
	r.resumeTeamID = teamID
	r.resumeDecision = decision

	return coding.TeamReference{TeamID: teamID}, nil
}

func (r *teamFakeRuntime) PrepareTeamIntegration(
	_ context.Context,
	request coding.TeamIntegrationRequest,
) (coding.TeamIntegrationPreview, error) {
	r.integrationRequest = request
	if len(request.TaskIDs) > 0 {
		request.TaskIDs[0] = "changed by fake"
	}

	return cloneTeamIntegrationPreview(r.integrationPreview), nil
}

func (r *teamFakeRuntime) ApplyTeamIntegration(
	_ context.Context,
	approval coding.TeamIntegrationApproval,
) (coding.TeamIntegrationResult, error) {
	r.integrationApproval = approval

	return coding.TeamIntegrationResult{ID: approval.ID, State: "applied"}, nil
}

func (r *teamFakeRuntime) RejectTeamIntegration(
	_ context.Context,
	approval coding.TeamIntegrationApproval,
) error {
	r.integrationRejection = approval

	return nil
}

func (r *teamFakeRuntime) TeamIntegrationRecoveries(
	context.Context,
) ([]coding.TeamIntegrationRecovery, error) {
	return append([]coding.TeamIntegrationRecovery(nil), r.integrationRecoveries...), nil
}

func (r *teamFakeRuntime) RecoverTeamIntegration(
	_ context.Context,
	request coding.TeamIntegrationRecoveryRequest,
) (coding.TeamIntegrationResult, error) {
	r.integrationRecovery = request

	return coding.TeamIntegrationResult{ID: request.ID, State: string(request.Action)}, nil
}

func (r *teamFakeRuntime) CleanupTeam(
	_ context.Context,
	request coding.TeamCleanupRequest,
) (coding.TeamCleanupResult, error) {
	r.cleanupRequest = request

	return coding.TeamCleanupResult{TeamID: request.TeamID}, nil
}

type blockingTeamFakeRuntime struct {
	*fakeRuntime
	started chan struct{}
	release chan struct{}
}

func (r *blockingTeamFakeRuntime) GenerateTeamProposal(
	context.Context,
	coding.TeamProposalPrompt,
) (coding.TeamProposal, error) {
	close(r.started)
	<-r.release

	return coding.TeamProposal{}, nil
}

var (
	_ runtimeInstance = (*teamFakeRuntime)(nil)
	_ runtimeInstance = (*blockingTeamFakeRuntime)(nil)
)
