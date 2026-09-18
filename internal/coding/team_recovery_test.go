//nolint:wsl_v5 // Recovery fixtures keep crash-window setup and evidence adjacent.
package coding

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent/continuation"
	"github.com/rsbin1178/pips/agent/team"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/execution/gitcontrol"
	"github.com/rsbin1178/pips/internal/coding/teamcontrol"
	"github.com/rsbin1178/pips/internal/coding/teamstate"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTeamDiscoveryOnSessionResumeIsReadOnly(t *testing.T) {
	t.Parallel()

	model := newRecoveryBlockingModel()
	first := openGitTestRuntime(t, model, nil)
	collectRuntimeEvents(t, first.Prompt(t.Context(), ai.UserText("prepare Team recovery test")))
	proposal, err := first.ProposeTeam(t.Context(), validTeamProposalRequest())
	require.NoError(t, err)
	reference, err := first.ConfirmTeam(t.Context(), TeamConfirmation{
		ProposalID: proposal.ID,
		Admission:  TeamAdmissionClean,
	})
	require.NoError(t, err)
	select {
	case <-model.blocked.started:
	case <-time.After(30 * time.Second):
		t.Fatal("Team Worker did not start")
	}

	require.NoError(t, first.team.close(context.Background()))
	continuationStore, err := continuation.NewJSONLStore(first.paths.TeamContinuationsDir())
	require.NoError(t, err)
	resources, err := first.team.state.Load(t.Context(), reference.TeamID)
	require.NoError(t, err)
	require.Len(t, resources.Attempts, 1)
	before, err := continuationStore.Load(t.Context(), resources.Attempts[0].ContinuationID)
	require.NoError(t, err)

	sessionID := first.handle.Metadata().ID
	workspaceRoot := first.workspace.Root()
	configValue := first.config.Clone()
	pathsValue := first.paths
	options := first.opts
	resolved := first.resolved.Clone()
	abruptRuntimeStop(t, first)

	workerSpace, err := workspace.Open(workspaceRoot)
	require.NoError(t, err)
	resumed, err := Open(t.Context(), OpenOptions{
		Workspace: workerSpace, Trusted: true, Config: configValue,
		Paths: pathsValue, Session: SessionTarget{ID: sessionID},
		Model: model, Resolved: resolved, Execution: options,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = resumed.Close(context.Background()) })

	candidates, err := resumed.TeamRecoveryCandidates()
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	assert.Equal(t, reference.TeamID, candidates[0].TeamID)
	assert.Equal(t, TeamRecoveryResume, candidates[0].Disposition)
	require.Len(t, candidates[0].Attempts, 1)
	assert.Equal(t, before.Execution.Status, candidates[0].Attempts[0].ContinuationState)

	after, err := continuationStore.Load(t.Context(), resources.Attempts[0].ContinuationID)
	require.NoError(t, err)
	assert.Equal(t, before.Execution.Revision, after.Execution.Revision)
	assert.Equal(t, before.Transition, after.Transition)
	assert.Nil(t, resumed.team)
}

type recoveryBlockingModel struct {
	mu        sync.Mutex
	calls     int
	lead      *runtimeModel
	blocked   *blockingRuntimeModel
	recovered *runtimeModel
}

func newRecoveryBlockingModel() *recoveryBlockingModel {
	return &recoveryBlockingModel{
		lead:      newRuntimeModel(runtimeTextResponse("ready")),
		blocked:   newBlockingRuntimeModel(),
		recovered: newRuntimeModel(runtimeTextResponse("recovered Worker complete")),
	}
}

func (m *recoveryBlockingModel) Generate(ctx context.Context, request ai.Request) (*ai.Response, error) {
	call := m.next()
	if call == 1 {
		return m.lead.Generate(ctx, request)
	}
	if call == 2 {
		return m.blocked.Generate(ctx, request)
	}

	return m.recovered.Generate(ctx, request)
}

func (m *recoveryBlockingModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
	call := m.next()
	if call == 1 {
		return m.lead.Stream(ctx, request)
	}
	if call == 2 {
		return m.blocked.Stream(ctx, request)
	}

	return m.recovered.Stream(ctx, request)
}

func (m *recoveryBlockingModel) next() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++

	return m.calls
}

func (*recoveryBlockingModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (*recoveryBlockingModel) ModelID() string       { return "runtime-test" }
func (*recoveryBlockingModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

func TestTeamDiscoveryCandidatesAreDefensivelyCloned(t *testing.T) {
	t.Parallel()

	runtime := openTestRuntime(t, newRuntimeModel())
	runtime.teamRecovery = []TeamRecoveryCandidate{{
		TeamID: "team-one", Disposition: TeamRecoveryBlockedIdentity,
		Diagnostics: []TeamRecoveryDiagnostic{{Code: "parent_identity_mismatch"}},
	}}

	candidates, err := runtime.TeamRecoveryCandidates()
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	candidates[0].Diagnostics[0].Code = "mutated"

	again, err := runtime.TeamRecoveryCandidates()
	require.NoError(t, err)
	assert.Equal(t, "parent_identity_mismatch", again[0].Diagnostics[0].Code)
}

func TestParentRecoveryIdentityRejectsReplacement(t *testing.T) {
	t.Parallel()

	runtime := openGitTestRuntime(t, newRuntimeModel(), nil)
	identity := runtime.workspace.Identity()
	snapshot := teamstate.Snapshot{
		Parent: teamstate.ParentResource{
			SessionID:   runtime.handle.Metadata().ID,
			WorkspaceID: identity.Key(),
			Workspace: teamstate.FileIdentity{
				Path: identity.Path(), Device: identity.Device(), Inode: identity.Inode() + 1,
			},
		},
	}

	assert.False(t, runtime.parentRecoveryIdentityMatches(t.Context(), snapshot))
}

func TestTeamResumeReconcilesTerminalChildWithNewLeaseGeneration(t *testing.T) {
	t.Parallel()

	model := newRecoveryBlockingModel()
	first := openGitTestRuntime(t, model, nil)
	collectRuntimeEvents(t, first.Prompt(t.Context(), ai.UserText("prepare explicit Team recovery")))
	proposal, err := first.ProposeTeam(t.Context(), validTeamProposalRequest())
	require.NoError(t, err)
	reference, err := first.ConfirmTeam(t.Context(), TeamConfirmation{
		ProposalID: proposal.ID,
		Admission:  TeamAdmissionClean,
	})
	require.NoError(t, err)
	select {
	case <-model.blocked.started:
	case <-time.After(30 * time.Second):
		t.Fatal("Team Worker did not start")
	}
	require.NoError(t, first.team.close(context.Background()))

	before, err := first.team.state.Load(t.Context(), reference.TeamID)
	require.NoError(t, err)
	require.Len(t, before.Attempts, 1)
	oldGeneration := before.Attempts[0].Worktree.LeaseGeneration
	sessionID := first.handle.Metadata().ID
	workspaceRoot := first.workspace.Root()
	configValue := first.config.Clone()
	pathsValue := first.paths
	options := first.opts
	resolved := first.resolved.Clone()
	abruptRuntimeStop(t, first)

	workerSpace, err := workspace.Open(workspaceRoot)
	require.NoError(t, err)
	resumed, err := Open(t.Context(), OpenOptions{
		Workspace: workerSpace, Trusted: true, Config: configValue,
		Paths: pathsValue, Session: SessionTarget{ID: sessionID},
		Model: model, Resolved: resolved, Execution: options,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = resumed.Close(context.Background()) })
	candidates, err := resumed.TeamRecoveryCandidates()
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Equal(t, TeamRecoveryResume, candidates[0].Disposition)

	beforeStale, err := resumed.teamStateSnapshot(t.Context(), reference.TeamID)
	require.NoError(t, err)
	_, err = resumed.ResumeTeam(t.Context(), reference.TeamID, TeamResumeDecision{
		ExpectedResourceRevision: candidates[0].ResourceRevision + 1,
		RetryInterruptedWork:     true,
	})
	require.ErrorIs(t, err, ErrTeamRecoveryStale)
	afterStale, err := resumed.teamStateSnapshot(t.Context(), reference.TeamID)
	require.NoError(t, err)
	assert.Equal(t, beforeStale.Revision, afterStale.Revision)
	assert.Equal(t, beforeStale.Attempts, afterStale.Attempts)

	resumedReference, err := resumed.ResumeTeam(t.Context(), reference.TeamID, TeamResumeDecision{
		ExpectedResourceRevision: candidates[0].ResourceRevision,
		RetryInterruptedWork:     true,
	})
	require.NoError(t, err)
	assert.Equal(t, reference.TeamID, resumedReference.TeamID)
	completed := assert.Eventually(t, func() bool {
		aggregate, getErr := resumed.team.engine.Get(t.Context(), reference.TeamID)

		return getErr == nil && len(aggregate.Tasks) == 1 &&
			aggregate.Tasks[0].Status == team.TaskStatusFailed
	}, 30*time.Second, 20*time.Millisecond)
	if !completed {
		aggregate, _ := resumed.team.engine.Get(t.Context(), reference.TeamID)
		resources, _ := resumed.team.state.Load(t.Context(), reference.TeamID)
		store, _ := continuation.NewJSONLStore(resumed.paths.TeamContinuationsDir())
		record, _ := store.Load(t.Context(), before.Attempts[0].ContinuationID)
		resumed.team.mu.Lock()
		closeErr := resumed.team.closeErr
		ownerCount := len(resumed.team.owners)
		resumed.team.mu.Unlock()
		t.Fatalf(
			"recovered Team did not complete: team=%+v resources=%+v continuation=%+v owners=%d err=%v",
			aggregate,
			resources,
			record.Execution,
			ownerCount,
			closeErr,
		)
	}

	var after teamstate.Snapshot
	require.Eventually(t, func() bool {
		var loadErr error
		after, loadErr = resumed.team.state.Load(t.Context(), reference.TeamID)

		return loadErr == nil && len(after.Attempts) == 1 &&
			after.Attempts[0].State == teamstate.AttemptTerminal
	}, 30*time.Second, 10*time.Millisecond)
	assert.Greater(t, after.Attempts[0].Worktree.LeaseGeneration, oldGeneration)
}

func TestTeamResumeReusesRecoverableJoinAttemptAfterDependencyRefIsRestored(t *testing.T) {
	t.Parallel()

	model := newStaleDependencyRecoveryModel()
	first := openGitTestRuntime(t, model, nil)
	collectRuntimeEvents(t, first.Prompt(t.Context(), ai.UserText("prepare Team dependency recovery")))
	proposal, err := first.ProposeTeam(t.Context(), TeamProposalRequest{
		Objective: "Recover the same join Attempt after stale dependency evidence is repaired.",
		Workers: []TeamWorkerSpec{
			{Name: "First", Role: "Create first.txt."},
			{Name: "Second", Role: "Create second.txt."},
			{Name: "Join", Role: "Create join.txt from both results."},
		},
		Tasks: []TeamTaskSpec{
			{ID: "first", Title: "Create first.txt", AssignedWorker: "First"},
			{ID: "second", Title: "Create second.txt", AssignedWorker: "Second"},
			{
				ID: "join", Title: "Create join.txt", AssignedWorker: "Join",
				Dependencies: []string{"first", "second"},
			},
		},
	})
	require.NoError(t, err)
	reference, err := first.ConfirmTeam(t.Context(), TeamConfirmation{
		ProposalID: proposal.ID, Admission: TeamAdmissionClean,
	})
	require.NoError(t, err)
	select {
	case <-model.secondWaiting:
	case <-time.After(15 * time.Second):
		t.Fatal("second dependency did not reach the controlled capture boundary")
	}

	var firstAttempt teamstate.AttemptResource
	require.Eventually(t, func() bool {
		resources, loadErr := first.team.state.Load(t.Context(), reference.TeamID)
		if loadErr != nil {
			return false
		}
		for _, attempt := range resources.Attempts {
			if attempt.TaskID == "first" && attempt.Worktree.ResultRef != "" &&
				attempt.Worktree.ResultCommitOID != "" {
				firstAttempt = attempt

				return true
			}
		}

		return false
	}, 30*time.Second, 20*time.Millisecond)

	runner, err := gitcontrol.New(first.opts.GitPath, gitcontrol.DefaultLimits())
	require.NoError(t, err)
	repository, err := runner.InspectRepository(t.Context(), first.workspace.Root())
	require.NoError(t, err)
	require.NoError(t, runner.UpdateRefs(
		t.Context(), first.workspace.Root(), repository.ObjectFormat,
		"test stale dependency result",
		[]gitcontrol.RefUpdate{{
			Ref:    firstAttempt.Worktree.ResultRef,
			NewOID: repository.HeadOID, OldOID: firstAttempt.Worktree.ResultCommitOID,
		}},
	))
	close(model.releaseSecond)

	var recoverable teamstate.AttemptResource
	require.Eventually(t, func() bool {
		aggregate, getErr := first.team.engine.Get(t.Context(), reference.TeamID)
		if getErr != nil {
			return false
		}
		joinTask, found := taskByID(aggregate.Tasks, "join")
		if !found || joinTask.Status != team.TaskStatusRunning || len(joinTask.Attempts) != 1 {
			return false
		}
		resources, loadErr := first.team.state.Load(t.Context(), reference.TeamID)
		if loadErr != nil {
			return false
		}
		for _, attempt := range resources.Attempts {
			if attempt.TaskID == "join" && attempt.State == teamstate.AttemptRecoverable {
				recoverable = attempt

				return first.team.ownerCount() == 0
			}
		}

		return false
	}, 30*time.Second, 20*time.Millisecond)
	assert.Empty(t, recoverable.Base.OID)
	assert.Empty(t, recoverable.Worktree.ID)
	assert.Empty(t, recoverable.Session.SessionID)
	assert.Zero(t, model.callsFor("join.txt"))

	require.ErrorIs(t, first.team.close(context.Background()), errDependencyBaseUnavailable)
	sessionID := first.handle.Metadata().ID
	workspaceRoot := first.workspace.Root()
	configValue := first.config.Clone()
	pathsValue := first.paths
	options := first.opts
	resolved := first.resolved.Clone()
	abruptRuntimeStop(t, first)

	workerSpace, err := workspace.Open(workspaceRoot)
	require.NoError(t, err)
	resumed, err := Open(t.Context(), OpenOptions{
		Workspace: workerSpace, Trusted: true, Config: configValue,
		Paths: pathsValue, Session: SessionTarget{ID: sessionID},
		Model: model, Resolved: resolved, Execution: options,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = resumed.Close(context.Background()) })
	candidates, err := resumed.TeamRecoveryCandidates()
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Equal(t, TeamRecoveryResume, candidates[0].Disposition)

	require.NoError(t, runner.UpdateRefs(
		t.Context(), workspaceRoot, repository.ObjectFormat,
		"restore dependency result",
		[]gitcontrol.RefUpdate{{
			Ref:    firstAttempt.Worktree.ResultRef,
			NewOID: firstAttempt.Worktree.ResultCommitOID, OldOID: repository.HeadOID,
		}},
	))
	_, err = resumed.ResumeTeam(t.Context(), reference.TeamID, TeamResumeDecision{
		ExpectedResourceRevision: candidates[0].ResourceRevision,
	})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		aggregate, getErr := resumed.team.engine.Get(t.Context(), reference.TeamID)
		if getErr != nil {
			return false
		}
		joinTask, found := taskByID(aggregate.Tasks, "join")

		return found && joinTask.Status == team.TaskStatusCompleted &&
			len(joinTask.Attempts) == 1 && joinTask.Attempts[0].ID == recoverable.AttemptID &&
			resumed.team.ownerCount() == 0
	}, 30*time.Second, 20*time.Millisecond)
	resources, err := resumed.team.state.Load(t.Context(), reference.TeamID)
	require.NoError(t, err)
	for _, attempt := range resources.Attempts {
		if attempt.AttemptID == recoverable.AttemptID {
			assert.NotEmpty(t, attempt.Base.OwnedRef)
			assert.Equal(t, attempt.Base.OID, attempt.Worktree.BaseOID)
		}
	}
	assert.Equal(t, 2, model.callsFor("join.txt"))
	_, err = os.Lstat(filepath.Join(workspaceRoot, "join.txt"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

type staleDependencyRecoveryModel struct {
	mu            sync.Mutex
	calls         map[string]int
	secondWaiting chan struct{}
	releaseSecond chan struct{}
	waitOnce      sync.Once
}

func newStaleDependencyRecoveryModel() *staleDependencyRecoveryModel {
	return &staleDependencyRecoveryModel{
		calls: make(map[string]int, 3), secondWaiting: make(chan struct{}),
		releaseSecond: make(chan struct{}),
	}
}

func (m *staleDependencyRecoveryModel) Generate(
	ctx context.Context,
	request ai.Request,
) (*ai.Response, error) {
	path := ""
	for _, candidate := range []string{"first.txt", "second.txt", "join.txt"} {
		if requestContainsText(request, candidate) {
			path = candidate
			break
		}
	}
	if path == "" {
		if requestContainsText(request, "prepare Team dependency recovery") {
			return runtimeTextResponse("Team dependency recovery session is ready."), nil
		}

		return nil, errors.New("stale dependency model could not identify the task")
	}
	m.mu.Lock()
	m.calls[path]++
	call := m.calls[path]
	m.mu.Unlock()
	if path == "second.txt" && call == 2 {
		m.waitOnce.Do(func() { close(m.secondWaiting) })
		select {
		case <-m.releaseSecond:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if call == 1 {
		arguments, err := json.Marshal(struct {
			Patch string `json:"patch"`
		}{Patch: "*** Begin Patch\n*** Add File: " + path +
			"\n+captured by " + path + "\n*** End Patch"})
		if err != nil {
			return nil, err
		}

		return runtimeToolResponse("write-"+path, "apply_patch", string(arguments)), nil
	}
	if call == 2 {
		return runtimeTextResponse("Worker result is ready for capture."), nil
	}

	return nil, errors.New("stale dependency model script exhausted")
}

func (m *staleDependencyRecoveryModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
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

func (*staleDependencyRecoveryModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (*staleDependencyRecoveryModel) ModelID() string       { return "stale-dependency-test" }
func (*staleDependencyRecoveryModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

func (m *staleDependencyRecoveryModel) callsFor(path string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.calls[strings.TrimSpace(path)]
}

func (r *Runtime) teamStateSnapshot(
	ctx context.Context,
	id team.ID,
) (teamstate.Snapshot, error) {
	store, err := teamstate.New(r.paths.TeamResourcesDir(), teamstate.Limits{})
	if err != nil {
		return teamstate.Snapshot{}, err
	}

	return store.Load(ctx, id)
}

func TestTeamRecoveryRetriesOnlyExplicitlyAuthorizedInterruptedWork(t *testing.T) {
	t.Parallel()

	store, err := continuation.NewJSONLStore(t.TempDir())
	require.NoError(t, err)
	engine, err := continuation.New(store)
	require.NoError(t, err)
	execution, err := engine.Create(t.Context(), continuation.CreateRequest{
		ID: "continuation-one",
		Target: continuation.Target{
			Kind: attemptTargetKind,
			ID:   "attempt-one",
		},
		Worker: attemptWorkerRef, Controller: team.DefaultAttemptControllerRef(),
		Limits: continuation.Limits{MaxAttempts: 1},
	})
	require.NoError(t, err)
	interrupted, err := engine.Advance(t.Context(), execution.ID, execution.Revision, continuation.Handlers{
		WorkerRef:     attemptWorkerRef,
		Worker:        canceledContinuationWorker{},
		ControllerRef: team.DefaultAttemptControllerRef(),
		Controller:    team.CompleteAfterWork{},
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, continuation.StatusInterrupted, interrupted.Status)

	aggregate := team.Team{
		ID: "team-one", Status: team.StatusActive, LeadMemberID: "lead",
		Members: []team.Member{
			{ID: "lead", Status: team.MemberStatusActive},
			{ID: "worker", Status: team.MemberStatusActive, CapabilityProfileRef: "profile"},
		},
		Tasks: []team.Task{{
			ID: "task-one", Status: team.TaskStatusRunning,
			AssignedMemberID: "worker", ClaimedMemberID: "worker",
			Attempts: []team.Attempt{{
				ID: "attempt-one", Status: team.AttemptStatusRunning,
				MemberID: "worker", ContinuationID: execution.ID,
			}},
		}},
	}
	snapshot := teamstate.Snapshot{
		TeamID: aggregate.ID,
		Members: []teamstate.MemberResource{
			{MemberID: "lead", CapabilityProfileFingerprint: "profile"},
			{MemberID: "worker", CapabilityProfileFingerprint: "profile"},
		},
	}
	_, err = prepareRecoveredCandidates(
		t.Context(), aggregate, snapshot, store, engine, TeamResumeDecision{},
	)
	require.ErrorIs(t, err, ErrTeamRecoveryDecisionRequired)

	candidates, err := prepareRecoveredCandidates(
		t.Context(),
		aggregate,
		snapshot,
		store,
		engine,
		TeamResumeDecision{RetryInterruptedWork: true},
	)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	retried, err := store.Load(t.Context(), execution.ID)
	require.NoError(t, err)
	assert.Equal(t, continuation.StatusReady, retried.Execution.Status)
	assert.Equal(t, continuation.CauseRetryWork, retried.Transition.Cause)
}

func TestTeamRecoveryResumesPausedContinuationWithoutReplayingWork(t *testing.T) {
	t.Parallel()

	store, err := continuation.NewJSONLStore(t.TempDir())
	require.NoError(t, err)
	engine, err := continuation.New(store)
	require.NoError(t, err)
	execution, err := engine.Create(t.Context(), continuation.CreateRequest{
		ID:     "continuation-paused",
		Target: continuation.Target{Kind: attemptTargetKind, ID: "attempt-paused"},
		Worker: attemptWorkerRef, Controller: team.DefaultAttemptControllerRef(),
		Limits: continuation.Limits{MaxAttempts: 1},
	})
	require.NoError(t, err)
	execution, err = engine.Pause(t.Context(), execution.ID, execution.Revision, "test pause")
	require.NoError(t, err)
	require.Equal(t, continuation.StatusPaused, execution.Status)
	require.NotNil(t, execution.Suspension)
	require.False(t, execution.Suspension.RetryRequired)

	aggregate, snapshot := recoveryTestAttemptState(
		execution.ID,
		"attempt-paused",
	)
	candidates, err := prepareRecoveredCandidates(
		t.Context(), aggregate, snapshot, store, engine, TeamResumeDecision{},
	)
	require.NoError(t, err)
	require.Len(t, candidates, 1)

	resumed, err := store.Load(t.Context(), execution.ID)
	require.NoError(t, err)
	assert.Equal(t, continuation.StatusReady, resumed.Execution.Status)
	assert.Equal(t, continuation.CauseResume, resumed.Transition.Cause)
	assert.Zero(t, resumed.Execution.Accounting.Attempts)
}

func TestTeamRecoveryCrashWindowPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		resource      teamstate.State
		execution     *continuation.Execution
		explicitRetry bool
	}{
		{name: "create", resource: teamstate.StateAdmitted},
		{
			name: "start", resource: teamstate.StateProvisioning,
			execution: &continuation.Execution{Status: continuation.StatusReady, Phase: continuation.PhaseWork},
		},
		{
			name: "work", resource: teamstate.StateActive, explicitRetry: true,
			execution: &continuation.Execution{Status: continuation.StatusRunning, Phase: continuation.PhaseWork},
		},
		{
			name: "pause", resource: teamstate.StateActive, explicitRetry: true,
			execution: &continuation.Execution{
				Status: continuation.StatusPaused, Phase: continuation.PhaseWork,
				Suspension: &continuation.Suspension{
					Status: continuation.StatusReady, Phase: continuation.PhaseWork,
					RetryRequired: true,
				},
			},
		},
		{
			name: "capture", resource: teamstate.StateActive,
			execution: &continuation.Execution{Status: continuation.StatusCompleted, Phase: continuation.PhaseDecision},
		},
		{
			name: "finish", resource: teamstate.StateActive,
			execution: &continuation.Execution{Status: continuation.StatusCompleted, Phase: continuation.PhaseDecision},
		},
		{
			name: "cancel", resource: teamstate.StateActive,
			execution: &continuation.Execution{Status: continuation.StatusCancelRequested, Phase: continuation.PhaseWork},
		},
		{
			name: "close", resource: teamstate.StateInterrupted, explicitRetry: true,
			execution: &continuation.Execution{Status: continuation.StatusInterrupted, Phase: continuation.PhaseWork},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.True(t, recoverableTeamResourceState(test.resource))
			if test.execution == nil {
				return
			}
			assert.True(t, recoverableContinuationState(*test.execution))
			candidate := TeamRecoveryCandidate{Attempts: []TeamAttemptRecoveryCandidate{{
				ContinuationState: test.execution.Status,
				ContinuationPhase: test.execution.Phase,
				RetryWork:         recoveryNeedsWorkRetry(*test.execution),
			}}}
			assert.Equal(t, test.explicitRetry, recoveryCandidateMayReplayWork(candidate))
		})
	}
}

func recoveryTestAttemptState(
	continuationID continuation.ID,
	attemptID team.AttemptID,
) (team.Team, teamstate.Snapshot) {
	aggregate := team.Team{
		ID: "team-one", Status: team.StatusActive, LeadMemberID: "lead",
		Members: []team.Member{
			{ID: "lead", Status: team.MemberStatusActive},
			{ID: "worker", Status: team.MemberStatusActive, CapabilityProfileRef: "profile"},
		},
		Tasks: []team.Task{{
			ID: "task-one", Status: team.TaskStatusRunning,
			AssignedMemberID: "worker", ClaimedMemberID: "worker",
			Attempts: []team.Attempt{{
				ID: attemptID, Status: team.AttemptStatusRunning,
				MemberID: "worker", ContinuationID: continuationID,
			}},
		}},
	}
	snapshot := teamstate.Snapshot{
		TeamID: aggregate.ID,
		Members: []teamstate.MemberResource{
			{MemberID: "lead", CapabilityProfileFingerprint: "profile"},
			{MemberID: "worker", CapabilityProfileFingerprint: "profile"},
		},
	}

	return aggregate, snapshot
}

type canceledContinuationWorker struct{}

func (canceledContinuationWorker) Run(
	context.Context,
	continuation.WorkRequest,
) (continuation.WorkResult, error) {
	return continuation.WorkResult{Progress: continuation.ProgressUnknown}, context.Canceled
}

var _ continuation.Worker = canceledContinuationWorker{}

func TestTeamRecoveryMarksInterruptedLiveDeliveryUnknown(t *testing.T) {
	t.Parallel()

	store, err := teamcontrol.New(filepath.Join(t.TempDir(), "control"), teamcontrol.Limits{})
	require.NoError(t, err)
	teamID := team.ID("team-one")
	now := time.Now().UTC()
	message, err := store.Submit(t.Context(), teamcontrol.Command{
		ID: "message-one", Action: teamcontrol.ActionMessage,
		Target: teamcontrol.Target{
			TeamID: teamID, MemberID: "worker", TaskID: "task-one",
			ExpectedAttemptID: "attempt-one", OwnerGeneration: 2,
		},
		Text: "continue", CreatedAt: now,
	}, 0)
	require.NoError(t, err)
	message, err = store.Begin(t.Context(), teamID, message.Entry.Command.ID, teamcontrol.Mutation{
		ID: "begin-message", ExpectedRevision: message.Revision,
	}, teamcontrol.ResolvedTarget{
		TeamID: teamID, MemberID: "worker", TaskID: "task-one",
		AttemptID: "attempt-one", ContinuationID: "continuation-one",
		SessionID: "session-one", WorkspaceID: "workspace-one", OwnerGeneration: 2,
	})
	require.NoError(t, err)

	nonLive, err := store.Submit(t.Context(), teamcontrol.Command{
		ID: "cancel-team", Action: teamcontrol.ActionCancelTeam,
		Target: teamcontrol.Target{TeamID: teamID}, CreatedAt: now,
	}, message.Revision)
	require.NoError(t, err)
	_, err = store.Begin(t.Context(), teamID, nonLive.Entry.Command.ID, teamcontrol.Mutation{
		ID: "begin-cancel", ExpectedRevision: nonLive.Revision,
	}, teamcontrol.ResolvedTarget{TeamID: teamID})
	require.NoError(t, err)

	coordinator := &teamCoordinator{id: teamID, control: store}
	require.NoError(t, reconcileInterruptedLiveControls(t.Context(), coordinator))
	message, err = store.Get(t.Context(), teamID, "message-one")
	require.NoError(t, err)
	assert.Equal(t, teamcontrol.StateDeliveryUnknown, message.Entry.State)
	assert.Equal(t, "interrupted_delivery", message.Entry.ErrorCode)
	nonLive, err = store.Get(t.Context(), teamID, "cancel-team")
	require.NoError(t, err)
	assert.Equal(t, teamcontrol.StateApplying, nonLive.Entry.State)
}

func TestTeamCloseConcurrentReleasesOwnersPermitsLeaseAndSession(t *testing.T) {
	t.Parallel()

	model := newRecoveryBlockingModel()
	runtime := openGitTestRuntime(t, model, nil)
	collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("prepare Team close test")))
	proposal, err := runtime.ProposeTeam(t.Context(), validTeamProposalRequest())
	require.NoError(t, err)
	reference, err := runtime.ConfirmTeam(t.Context(), TeamConfirmation{
		ProposalID: proposal.ID,
		Admission:  TeamAdmissionClean,
	})
	require.NoError(t, err)
	select {
	case <-model.blocked.started:
	case <-time.After(30 * time.Second):
		t.Fatal("Team Worker did not start")
	}

	coordinator := runtime.team
	require.NotNil(t, coordinator)
	sessionID := runtime.handle.Metadata().ID
	workspaceRoot := runtime.workspace.Root()
	configValue := runtime.config.Clone()
	pathsValue := runtime.paths
	options := runtime.opts
	resolved := runtime.resolved.Clone()

	results := make(chan error, 2)
	go func() { results <- runtime.Close(context.Background()) }()
	go func() { results <- runtime.Close(context.Background()) }()
	for range 2 {
		require.NoError(t, <-results)
	}
	active, queued, byKey := coordinator.admission.snapshot()
	assert.Zero(t, active)
	assert.Zero(t, queued)
	assert.Empty(t, byKey)
	assert.Zero(t, coordinator.ownerCount())
	coordinator.mu.Lock()
	assert.True(t, coordinator.closed)
	coordinator.mu.Unlock()

	resources, err := coordinator.state.Load(t.Context(), reference.TeamID)
	require.NoError(t, err)
	lease, err := coordinator.worktree.Acquire(
		t.Context(),
		reference.TeamID,
		uint64(resources.Revision)+1,
	)
	require.NoError(t, err)
	require.NoError(t, lease.Close())

	workerSpace, err := workspace.Open(workspaceRoot)
	require.NoError(t, err)
	reopened, err := Open(t.Context(), OpenOptions{
		Workspace: workerSpace, Trusted: true, Config: configValue,
		Paths: pathsValue, Session: SessionTarget{ID: sessionID},
		Model: newRuntimeModel(), Resolved: resolved, Execution: options,
	})
	require.NoError(t, err)
	require.NoError(t, reopened.Close(t.Context()))
}
