package coding

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/internal/coding/execution"
	"github.com/rsbin/pips/internal/coding/execution/gitcontrol"
	"github.com/rsbin/pips/internal/coding/teamintegration"
	"github.com/rsbin/pips/internal/coding/teamstate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildIntegrationSelectionAddsDependencyClosureInStableOrder(t *testing.T) {
	t.Parallel()

	baseOID := strings.Repeat("1", 40)
	dependencyResult := strings.Repeat("2", 40)
	childResult := strings.Repeat("3", 40)
	now := time.Now().UTC()
	aggregate := team.Team{
		ID: "team-1",
		Tasks: []team.Task{
			{
				ID: "child", Status: team.TaskStatusCompleted,
				DependencyIDs: []team.TaskID{"dependency"},
				Attempts:      []team.Attempt{{ID: "attempt-child", Status: team.AttemptStatusCompleted}},
			},
			{
				ID: "dependency", Status: team.TaskStatusCompleted,
				Attempts: []team.Attempt{{ID: "attempt-dependency", Status: team.AttemptStatusCompleted}},
			},
		},
		UpdatedAt: now,
	}
	resources := teamstate.Snapshot{
		TeamID: "team-1", Revision: 7,
		Repository: teamstate.RepositoryResource{BaseOID: baseOID},
		Attempts: []teamstate.AttemptResource{
			{
				TaskID: "child", AttemptID: "attempt-child", State: teamstate.AttemptTerminal,
				Worktree: teamstate.WorktreeResource{
					BaseOID: dependencyResult, ResultRef: "refs/pips/team/t/results/child",
					ResultCommitOID: childResult,
				},
			},
			{
				TaskID: "dependency", AttemptID: "attempt-dependency", State: teamstate.AttemptCaptured,
				Worktree: teamstate.WorktreeResource{
					BaseOID: baseOID, ResultRef: "refs/pips/team/t/results/dependency",
					ResultCommitOID: dependencyResult,
				},
			},
		},
	}

	selection, err := buildIntegrationSelection(aggregate, resources, []team.TaskID{"child"})
	require.NoError(t, err)
	require.Len(t, selection.Artifacts, 2)
	assert.Equal(t, []string{"dependency", "child"}, []string{
		selection.Artifacts[0].TaskID, selection.Artifacts[1].TaskID,
	})
	assert.Equal(t, uint64(7), selection.ResourceRevision)
	assert.Equal(t, dependencyResult, selection.Artifacts[1].BaseOID)

	_, err = buildIntegrationSelection(aggregate, resources, []team.TaskID{"child", "child"})
	require.ErrorIs(t, err, ErrTeamIntegrationSelection)

	duplicateResources := resources
	duplicateResources.Attempts = append(
		slices.Clone(resources.Attempts), resources.Attempts[0],
	)
	_, err = buildIntegrationSelection(aggregate, duplicateResources, []team.TaskID{"child"})
	require.ErrorIs(t, err, ErrTeamIntegrationSelection)

	duplicateTasks := aggregate
	duplicateTasks.Tasks = append(slices.Clone(aggregate.Tasks), aggregate.Tasks[0])
	_, err = buildIntegrationSelection(duplicateTasks, resources, []team.TaskID{"child"})
	require.ErrorIs(t, err, ErrTeamIntegrationSelection)
}

func TestStableTaskOrderUsesTaskIDTieBreak(t *testing.T) {
	t.Parallel()

	tasks := map[team.TaskID]team.Task{
		"z": {ID: "z"},
		"a": {ID: "a"},
		"m": {ID: "m", DependencyIDs: []team.TaskID{"a"}},
	}
	closure := map[team.TaskID]struct{}{"z": {}, "a": {}, "m": {}}

	ordered, err := stableTaskOrder(tasks, closure)
	require.NoError(t, err)
	assert.True(t, slices.Equal([]team.TaskID{"a", "m", "z"}, ordered), ordered)
}

func TestValidateIntegrationApprovalSnapshotBindsResourceRevision(t *testing.T) {
	t.Parallel()

	const token = "opaque-approval"

	sum := sha256.Sum256([]byte(token))
	resource := teamstate.IntegrationResource{
		ID:                "int-0123456789abcdef0123456789abcdef",
		ResourceRevision:  7,
		State:             teamstate.IntegrationReady,
		ApprovalTokenHash: hex.EncodeToString(sum[:]),
	}
	approval := TeamIntegrationApproval{ID: resource.ID, Token: token}
	snapshot := teamstate.Snapshot{Revision: 8, Integrations: []teamstate.IntegrationResource{resource}}

	approved, err := validateIntegrationApprovalSnapshot(snapshot, approval)
	require.NoError(t, err)
	assert.Equal(t, resource, approved)

	staleRevision := snapshot
	staleRevision.Revision++
	_, err = validateIntegrationApprovalSnapshot(staleRevision, approval)
	require.ErrorIs(t, err, teamintegration.ErrStale)

	staleState := snapshot
	staleState.Integrations = slices.Clone(snapshot.Integrations)
	staleState.Integrations[0].State = teamstate.IntegrationApplying
	_, err = validateIntegrationApprovalSnapshot(staleState, approval)
	require.ErrorIs(t, err, teamintegration.ErrStale)

	_, err = validateIntegrationApprovalSnapshot(snapshot, TeamIntegrationApproval{
		ID: resource.ID, Token: "different",
	})
	require.ErrorIs(t, err, teamintegration.ErrStale)
}

func TestRuntimePreparesAndAppliesCapturedTeamResultWithoutChangingGitMetadata(t *testing.T) {
	t.Parallel()

	runtime := openCapturedTeamRuntime(t)

	runner, err := gitcontrol.New(runtime.opts.GitPath, gitcontrol.DefaultLimits())
	require.NoError(t, err)
	repositoryBefore, err := runner.InspectRepository(t.Context(), runtime.workspace.Root())
	require.NoError(t, err)
	indexBefore, err := os.ReadFile(filepath.Join(repositoryBefore.GitDir, "index"))
	require.NoError(t, err)

	verification := execution.OperationSpec{
		Kind: execution.KindGit, Tool: "team_integration_test",
		Executable: runtime.opts.GitPath, Args: []string{"--version"},
		CWD: ".", Timeout: 5 * time.Second,
		Output: execution.OutputLimits{
			CaptureBytes: 4 << 10, MaxBytes: 64 << 10,
			ChunkBytes: 1024, QueueDepth: 4,
		},
		Workspace: execution.WorkspaceReadOnly, Network: execution.NetworkNone,
	}
	preview, err := runtime.PrepareTeamIntegration(t.Context(), TeamIntegrationRequest{
		Verification: &verification,
	})
	require.NoError(t, err)
	require.NotEmpty(t, preview.ApprovalToken)
	assert.True(t, preview.ProducesUnstagedChanges)
	assert.Equal(t, 1, preview.Manifest.Added)
	assert.Equal(t, teamintegration.VerificationPassed, preview.Verification.Status)
	recoveries, err := runtime.TeamIntegrationRecoveries(t.Context())
	require.NoError(t, err)
	assert.Empty(t, recoveries)

	_, err = os.Lstat(filepath.Join(runtime.workspace.Root(), "result.txt"))
	require.ErrorIs(t, err, os.ErrNotExist)

	result, err := runtime.ApplyTeamIntegration(t.Context(), TeamIntegrationApproval{
		ID: preview.ID, Token: preview.ApprovalToken,
	})
	require.NoError(t, err)
	assert.Equal(t, "applied", result.State)

	content, err := os.ReadFile(filepath.Join(runtime.workspace.Root(), "result.txt"))
	require.NoError(t, err)
	assert.Equal(t, "captured by result.txt\n", string(content))

	repositoryAfter, err := runner.InspectRepository(t.Context(), runtime.workspace.Root())
	require.NoError(t, err)
	indexAfter, err := os.ReadFile(filepath.Join(repositoryAfter.GitDir, "index"))
	require.NoError(t, err)
	assert.Equal(t, repositoryBefore.BranchRef, repositoryAfter.BranchRef)
	assert.Equal(t, repositoryBefore.HeadOID, repositoryAfter.HeadOID)
	assert.True(t, bytes.Equal(indexBefore, indexAfter), "parent index bytes changed")
	status, err := runner.SnapshotStatus(t.Context(), runtime.workspace.Root())
	require.NoError(t, err)
	assert.Equal(t, []string{"result.txt"}, status.Paths)

	_, err = runtime.ApplyTeamIntegration(t.Context(), TeamIntegrationApproval{
		ID: preview.ID, Token: preview.ApprovalToken,
	})
	require.Error(t, err)

	coordinator := runtime.team
	resources, err := coordinator.state.Load(t.Context(), coordinator.id)
	require.NoError(t, err)
	require.Len(t, resources.Attempts, 1)
	require.Len(t, resources.Integrations, 1)
	attemptResultRef := resources.Attempts[0].Worktree.ResultRef
	attemptResultOID := resources.Attempts[0].Worktree.ResultCommitOID
	integrationBranch := resources.Integrations[0].Worktree.BranchRef
	integrationRef := resources.Integrations[0].Worktree.ResultRef
	attemptDirectory := resources.Attempts[0].Worktree.Directory.Path
	integrationDirectory := resources.Integrations[0].Worktree.Directory.Path
	stateStore := coordinator.state

	cleanup, err := runtime.CleanupTeam(t.Context(), TeamCleanupRequest{
		TeamID: coordinator.id, ExpectedResourceRevision: resources.Revision,
	})
	require.NoError(t, err)
	assert.Equal(t, teamstate.StateIntegrated, cleanup.State)
	assert.Equal(t, 2, cleanup.Cleaned)
	assert.Zero(t, cleanup.Retained)
	assert.Nil(t, runtime.team)

	terminal, err := stateStore.Load(t.Context(), coordinator.id)
	require.NoError(t, err)
	assert.Equal(t, teamstate.CleanupComplete, terminal.Cleanup)

	_, err = os.Lstat(attemptDirectory)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Lstat(integrationDirectory)
	require.ErrorIs(t, err, os.ErrNotExist)
	oid, err := runner.ResolveRef(t.Context(), runtime.workspace.Root(), attemptResultRef)
	require.NoError(t, err)
	assert.Equal(t, attemptResultOID, oid)
	_, err = runner.ResolveRef(t.Context(), runtime.workspace.Root(), integrationBranch)
	require.ErrorIs(t, err, gitcontrol.ErrNotFound)
	_, err = runner.ResolveRef(t.Context(), runtime.workspace.Root(), integrationRef)
	require.ErrorIs(t, err, gitcontrol.ErrNotFound)
}

func TestRuntimeCleanupRequiresExplicitCloseWithoutIntegrationAndRetainsDirtyWorktree(t *testing.T) {
	t.Parallel()

	runtime := openCapturedTeamRuntime(t)
	coordinator := runtime.team
	resources, err := coordinator.state.Load(t.Context(), coordinator.id)
	require.NoError(t, err)

	_, err = runtime.CleanupTeam(t.Context(), TeamCleanupRequest{
		TeamID: coordinator.id, ExpectedResourceRevision: resources.Revision - 1,
		CloseWithoutIntegration: true,
	})
	require.ErrorIs(t, err, ErrTeamCleanupStale)

	_, err = runtime.CleanupTeam(t.Context(), TeamCleanupRequest{
		TeamID: coordinator.id, ExpectedResourceRevision: resources.Revision,
	})
	require.ErrorIs(t, err, ErrTeamCleanupIntegrationRequired)

	dirtyPath := filepath.Join(resources.Attempts[0].Worktree.Directory.Path, "dirty.tmp")
	require.NoError(t, os.WriteFile(dirtyPath, []byte("retain\n"), 0o600))
	retained, err := runtime.CleanupTeam(t.Context(), TeamCleanupRequest{
		TeamID: coordinator.id, ExpectedResourceRevision: resources.Revision,
		CloseWithoutIntegration: true,
	})
	require.ErrorIs(t, err, ErrTeamCleanupRetained)
	assert.Zero(t, retained.Cleaned)
	assert.Equal(t, 1, retained.Retained)
	assert.NotNil(t, runtime.team)

	_, err = os.Stat(resources.Attempts[0].Worktree.Directory.Path)
	require.NoError(t, err)

	require.NoError(t, os.Remove(dirtyPath))
	retry, err := coordinator.state.Load(t.Context(), coordinator.id)
	require.NoError(t, err)
	closed, err := runtime.CleanupTeam(t.Context(), TeamCleanupRequest{
		TeamID: coordinator.id, ExpectedResourceRevision: retry.Revision,
		CloseWithoutIntegration: true,
	})
	require.NoError(t, err)
	assert.Equal(t, teamstate.StateClosedWithoutIntegration, closed.State)
	assert.Equal(t, 1, closed.Cleaned)
	assert.Zero(t, closed.Retained)
	assert.Nil(t, runtime.team)
}

func TestRuntimeRejectsTeamIntegrationWithoutParentMutation(t *testing.T) {
	t.Parallel()

	runtime := openCapturedTeamRuntime(t)
	preview, err := runtime.PrepareTeamIntegration(t.Context(), TeamIntegrationRequest{})
	require.NoError(t, err)

	approval := TeamIntegrationApproval{ID: preview.ID, Token: preview.ApprovalToken}

	require.NoError(t, runtime.RejectTeamIntegration(t.Context(), approval))
	_, err = runtime.ApplyTeamIntegration(t.Context(), approval)
	require.Error(t, err)
	_, err = os.Lstat(filepath.Join(runtime.workspace.Root(), "result.txt"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRuntimeRefusesIntegrationAfterTeamResourceRevisionDrift(t *testing.T) {
	t.Parallel()

	runtime := openCapturedTeamRuntime(t)
	preview, err := runtime.PrepareTeamIntegration(t.Context(), TeamIntegrationRequest{})
	require.NoError(t, err)
	coordinator, err := runtime.currentTeamCoordinator()
	require.NoError(t, err)
	_, err = mutateIntegrationResource(
		t.Context(), coordinator.state, coordinator.id,
		preview.ID, "test-revision-drift",
		func(_ *teamstate.Snapshot, value *teamstate.IntegrationResource) error {
			value.Cleanup = teamstate.CleanupRetain

			return nil
		},
	)
	require.NoError(t, err)

	_, err = runtime.ApplyTeamIntegration(t.Context(), TeamIntegrationApproval{
		ID: preview.ID, Token: preview.ApprovalToken,
	})
	require.ErrorIs(t, err, teamintegration.ErrStale)
	_, err = os.Lstat(filepath.Join(runtime.workspace.Root(), "result.txt"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func openCapturedTeamRuntime(t *testing.T) *Runtime {
	t.Helper()

	model := newParallelTeamWriteModel("result.txt")
	runtime := openGitTestRuntime(t, model, nil)
	proposal, err := runtime.ProposeTeam(t.Context(), TeamProposalRequest{
		Objective: "Create the captured result without modifying the parent Workspace.",
		Workers: []TeamWorkerSpec{{
			Name: "Implementer", Role: "Implement the assigned file change.",
		}},
		Tasks: []TeamTaskSpec{{
			ID: "task-1", Title: "Create result",
			Description:    "Create result.txt with the requested bounded content.",
			AssignedWorker: "Implementer",
		}},
	})
	require.NoError(t, err)
	reference, err := runtime.ConfirmTeam(t.Context(), TeamConfirmation{
		ProposalID: proposal.ID, Admission: TeamAdmissionClean,
	})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		aggregate, getErr := runtime.team.engine.Get(t.Context(), reference.TeamID)
		if getErr != nil || len(aggregate.Tasks) != 1 ||
			aggregate.Tasks[0].Status != team.TaskStatusCompleted {
			return false
		}

		resources, loadErr := runtime.team.state.Load(t.Context(), reference.TeamID)
		if loadErr != nil || len(resources.Attempts) != 1 {
			return false
		}

		terminal := resources.Attempts[0].State == teamstate.AttemptCaptured ||
			resources.Attempts[0].State == teamstate.AttemptTerminal

		return terminal && runtime.team.ownerCount() == 0
	}, 10*time.Second, 20*time.Millisecond)

	return runtime
}
