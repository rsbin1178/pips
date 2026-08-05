package coding

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/teamstate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestObserveTeamWorkerUsesExactLiveRuntimeAndIndependentGap(t *testing.T) {
	t.Parallel()

	model := newBlockingRuntimeModel()
	runtime := openGitTestRuntime(t, model, nil)
	reference := admitInspectionTestTeam(t, runtime)
	target, owner := waitForLiveInspectionWorker(t, runtime, reference.TeamID)

	observation, err := runtime.ObserveTeamWorker(t.Context(), target)
	require.NoError(t, err)
	require.NotNil(t, observation.Subscription)
	assert.NotEqual(t, runtime.Snapshot().SessionID, observation.State.SessionID)
	assert.Equal(t, owner.childSessionID, observation.State.SessionID)

	parent, err := runtime.ObserveEvents()
	require.NoError(t, err)
	require.NotNil(t, parent.Subscription)
	t.Cleanup(parent.Subscription.Close)
	assert.NotContains(t, parent.Children, observation.State.SessionID)

	_, err = runtime.InspectTeamWorkerState(t.Context(), target)
	require.ErrorIs(t, err, ErrRuntimeBusy)

	stale := target
	stale.OwnerGeneration++
	_, err = runtime.ObserveTeamWorker(t.Context(), stale)
	require.ErrorIs(t, err, ErrTeamWorkerStale)

	owner.mu.Lock()
	workerRuntime := owner.runtime
	owner.mu.Unlock()
	require.NotNil(t, workerRuntime)

	for index := 0; index <= defaultEventSubscriberCapacity; index++ {
		workerRuntime.recordDiagnostic(t.Context(), IntegrationDiagnostic{
			Component: "worker_inspection_test", Code: "gap", Message: "bounded diagnostic",
		})
	}

	require.ErrorIs(t, observation.Subscription.Err(), ErrEventGap)
	observation.Subscription.Close()
}

func TestInspectTeamWorkerStateRebuildsTerminalSessionAndReleasesHandle(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(runtimeTextResponse("terminal Worker response"))
	runtime := openGitTestRuntime(t, model, nil)
	reference := admitInspectionTestTeam(t, runtime)
	target, childSessionID := waitForTerminalInspectionWorker(t, runtime, reference.TeamID)
	requestsBefore := len(model.Requests())

	state, err := runtime.InspectTeamWorkerState(t.Context(), target)
	require.NoError(t, err)
	assert.Equal(t, childSessionID, state.SessionID)
	assert.True(t, state.SessionOpen)
	assert.Equal(t, PhaseIdle, state.Phase)
	assert.NotEmpty(t, state.Transcript)
	assert.Len(t, model.Requests(), requestsBefore, "inspection must not invoke the model")

	state.Transcript[0] = ai.UserText("mutated")
	again, err := runtime.InspectTeamWorkerState(t.Context(), target)
	require.NoError(t, err, "the first inspection must release the Session handle")
	assert.NotEqual(t, state.Transcript, again.Transcript)
	assert.Len(t, model.Requests(), requestsBefore)

	_, err = runtime.ObserveTeamWorker(t.Context(), target)
	require.ErrorIs(t, err, ErrRuntimeBusy)

	stale := target
	stale.MemberID = "different-worker"
	_, err = runtime.InspectTeamWorkerState(t.Context(), stale)
	require.ErrorIs(t, err, ErrTeamWorkerStale)
}

func TestObserveTeamWorkerLiveToTerminalRaceIsClassified(t *testing.T) {
	t.Parallel()

	model := newReleasableTeamWorkerModel()
	runtime := openGitTestRuntime(t, model, nil)
	reference := admitInspectionTestTeam(t, runtime)
	target, _ := waitForLiveInspectionWorker(t, runtime, reference.TeamID)

	result := make(chan error, 1)

	go func() {
		for {
			observation, err := runtime.ObserveTeamWorker(t.Context(), target)
			if err != nil {
				result <- err

				return
			}

			observation.Subscription.Close()
		}
	}()

	close(model.release)

	terminalTarget, _ := waitForTerminalInspectionWorker(t, runtime, reference.TeamID)
	require.Equal(t, target, terminalTarget)

	select {
	case err := <-result:
		assert.True(
			t,
			errors.Is(err, ErrRuntimeBusy) || errors.Is(err, ErrTeamWorkerStale),
			err,
		)
	case <-time.After(30 * time.Second):
		t.Fatal("live Worker observer did not converge after owner termination")
	}

	_, err := runtime.InspectTeamWorkerState(t.Context(), terminalTarget)
	require.NoError(t, err)
}

func admitInspectionTestTeam(t *testing.T, runtime *Runtime) TeamReference {
	t.Helper()

	proposal, err := runtime.ProposeTeam(t.Context(), validTeamProposalRequest())
	require.NoError(t, err)
	reference, err := runtime.ConfirmTeam(t.Context(), TeamConfirmation{
		ProposalID: proposal.ID, Admission: TeamAdmissionClean,
	})
	require.NoError(t, err)

	return reference
}

func waitForLiveInspectionWorker(
	t *testing.T,
	runtime *Runtime,
	teamID team.ID,
) (TeamWorkerTarget, *attemptOwner) {
	t.Helper()

	var (
		target TeamWorkerTarget
		owner  *attemptOwner
	)

	require.Eventually(t, func() bool {
		runtime.team.mu.Lock()
		for _, slot := range runtime.team.owners {
			owner, _ = slot.owner.(*attemptOwner)
			if owner != nil {
				break
			}
		}
		runtime.team.mu.Unlock()

		if owner == nil {
			return false
		}

		owner.mu.Lock()
		workerRuntime := owner.runtime
		owner.mu.Unlock()

		if workerRuntime == nil {
			return false
		}

		resources, err := runtime.team.state.Load(t.Context(), teamID)
		if err != nil {
			return false
		}

		for _, value := range resources.Attempts {
			if value.AttemptID != owner.candidate.key.attemptID ||
				value.State != teamstate.AttemptRunning ||
				value.Worktree.LeaseGeneration == 0 {
				continue
			}

			target = TeamWorkerTarget{
				TeamID: teamID, MemberID: value.MemberID,
				TaskID: value.TaskID, AttemptID: value.AttemptID,
				OwnerGeneration: value.Worktree.LeaseGeneration,
			}

			return true
		}

		return false
	}, 5*time.Second, 10*time.Millisecond)

	return target, owner
}

func waitForTerminalInspectionWorker(
	t *testing.T,
	runtime *Runtime,
	teamID team.ID,
) (TeamWorkerTarget, string) {
	t.Helper()

	var (
		target         TeamWorkerTarget
		childSessionID string
	)

	require.Eventually(t, func() bool {
		resources, err := runtime.team.state.Load(t.Context(), teamID)
		if err != nil || len(resources.Attempts) != 1 {
			return false
		}

		value := resources.Attempts[0]
		if value.State != teamstate.AttemptTerminal ||
			value.Worktree.LeaseGeneration == 0 || value.Session.SessionID == "" {
			return false
		}

		key := attemptKey{teamID: teamID, taskID: value.TaskID, attemptID: value.AttemptID}

		runtime.team.mu.Lock()
		_, live := runtime.team.owners[key]
		runtime.team.mu.Unlock()

		if live {
			return false
		}

		target = TeamWorkerTarget{
			TeamID: teamID, MemberID: value.MemberID,
			TaskID: value.TaskID, AttemptID: value.AttemptID,
			OwnerGeneration: value.Worktree.LeaseGeneration,
		}
		childSessionID = value.Session.SessionID

		return true
	}, 10*time.Second, 10*time.Millisecond)

	return target, childSessionID
}

type releasableTeamWorkerModel struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newReleasableTeamWorkerModel() *releasableTeamWorkerModel {
	return &releasableTeamWorkerModel{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (m *releasableTeamWorkerModel) Generate(
	ctx context.Context,
	_ ai.Request,
) (*ai.Response, error) {
	m.once.Do(func() { close(m.started) })

	select {
	case <-m.release:
		return runtimeTextResponse("released Worker response"), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *releasableTeamWorkerModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		response, err := m.Generate(ctx, request)
		if err != nil {
			yield(ai.StreamEvent{}, err)

			return
		}

		for _, event := range runtimeResponseEvents(response) {
			if !yield(event, nil) {
				return
			}
		}
	}
}

func (*releasableTeamWorkerModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (*releasableTeamWorkerModel) ModelID() string       { return "runtime-test" }
func (*releasableTeamWorkerModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

var _ ai.LanguageModel = (*releasableTeamWorkerModel)(nil)
