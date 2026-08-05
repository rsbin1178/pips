//nolint:wsl_v5 // Git-backed DAG fixtures keep orchestration evidence adjacent.
package coding

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/ai"
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

func TestRuntimeIntegratesParallelTeamResultsWithoutChangingGitMetadata(t *testing.T) {
	t.Parallel()

	runtime := openCapturedTeamRuntimeWithPaths(t, "worker-a.txt", "worker-b.txt")
	runner, err := gitcontrol.New(runtime.opts.GitPath, gitcontrol.DefaultLimits())
	require.NoError(t, err)
	repositoryBefore, err := runner.InspectRepository(t.Context(), runtime.workspace.Root())
	require.NoError(t, err)
	indexBefore, err := os.ReadFile(filepath.Join(repositoryBefore.GitDir, "index"))
	require.NoError(t, err)

	preview, err := runtime.PrepareTeamIntegration(t.Context(), TeamIntegrationRequest{})
	require.NoError(t, err)
	assert.Equal(t, 2, preview.Manifest.Added)
	assert.True(t, preview.ProducesUnstagedChanges)

	result, err := runtime.ApplyTeamIntegration(t.Context(), TeamIntegrationApproval{
		ID: preview.ID, Token: preview.ApprovalToken,
	})
	require.NoError(t, err)
	assert.Equal(t, "applied", result.State)

	for _, name := range []string{"worker-a.txt", "worker-b.txt"} {
		content, readErr := os.ReadFile( //nolint:gosec // The test owns these fixed fixture names and Workspace.
			filepath.Join(runtime.workspace.Root(), name),
		)
		require.NoError(t, readErr)
		assert.Equal(t, "captured by "+name+"\n", string(content))
	}

	repositoryAfter, err := runner.InspectRepository(t.Context(), runtime.workspace.Root())
	require.NoError(t, err)
	indexAfter, err := os.ReadFile(filepath.Join(repositoryAfter.GitDir, "index"))
	require.NoError(t, err)
	assert.Equal(t, repositoryBefore.BranchRef, repositoryAfter.BranchRef)
	assert.Equal(t, repositoryBefore.HeadOID, repositoryAfter.HeadOID)
	assert.True(t, bytes.Equal(indexBefore, indexAfter), "parent index bytes changed")
	status, err := runner.SnapshotStatus(t.Context(), runtime.workspace.Root())
	require.NoError(t, err)
	assert.Equal(t, []string{"worker-a.txt", "worker-b.txt"}, status.Paths)
}

func TestRuntimeStartsJoinTaskFromComposedDependencyBase(t *testing.T) {
	t.Parallel()

	model := newParallelTeamWriteModel("first.txt", "second.txt", "join.txt")
	runtime := openGitTestRuntime(t, model, nil)
	proposal, err := runtime.ProposeTeam(t.Context(), TeamProposalRequest{
		Objective: "Create two independent results, then consume both in one join task.",
		Workers: []TeamWorkerSpec{
			{Name: "First", Role: "Create the first result."},
			{Name: "Second", Role: "Create the second result."},
			{Name: "Join", Role: "Consume both prerequisite results."},
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
	reference, err := runtime.ConfirmTeam(t.Context(), TeamConfirmation{
		ProposalID: proposal.ID, Admission: TeamAdmissionClean,
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		aggregate, getErr := runtime.team.engine.Get(t.Context(), reference.TeamID)
		if getErr != nil || len(aggregate.Tasks) != 3 {
			return false
		}
		for _, taskValue := range aggregate.Tasks {
			if taskValue.Status != team.TaskStatusCompleted {
				return false
			}
		}
		resources, loadErr := runtime.team.state.Load(t.Context(), reference.TeamID)

		return loadErr == nil && len(resources.Attempts) == 3 && runtime.team.ownerCount() == 0
	}, 60*time.Second, 20*time.Millisecond)

	resources, err := runtime.team.state.Load(t.Context(), reference.TeamID)
	require.NoError(t, err)
	byTask := make(map[team.TaskID]teamstate.AttemptResource, len(resources.Attempts))
	for _, attempt := range resources.Attempts {
		byTask[attempt.TaskID] = attempt
	}
	join := byTask["join"]
	require.NotEmpty(t, join.Base.OwnedRef)
	assert.Equal(t, 2, join.Base.DependencyCount)
	assert.NotEmpty(t, join.Base.DependencyDigest)
	assert.NotEmpty(t, join.Base.CompositionDigest)
	assert.Equal(t, join.Base.OID, join.Worktree.BaseOID)
	for _, taskID := range []team.TaskID{"first", "second"} {
		assert.Equal(t, resources.Repository.BaseOID, byTask[taskID].Base.OID)
		assert.Empty(t, byTask[taskID].Base.OwnedRef)
	}

	runner, err := gitcontrol.New(runtime.opts.GitPath, gitcontrol.DefaultLimits())
	require.NoError(t, err)
	baseOID, err := runner.ResolveRef(t.Context(), runtime.workspace.Root(), join.Base.OwnedRef)
	require.NoError(t, err)
	assert.Equal(t, join.Base.OID, baseOID)
	aggregate, err := runtime.team.engine.Get(t.Context(), reference.TeamID)
	require.NoError(t, err)
	joinTask, found := taskByID(aggregate.Tasks, "join")
	require.True(t, found)
	require.NotEmpty(t, joinTask.Attempts)
	reprepared, err := (dependencyBasePreparer{manager: runtime.team.integration}).PrepareAttemptBase(
		t.Context(),
		AttemptBaseRequest{
			Team: aggregate, Task: joinTask,
			AttemptID: joinTask.Attempts[len(joinTask.Attempts)-1].ID,
			Resources: resources,
		},
	)
	require.NoError(t, err)
	assert.Equal(t, join.Base, reprepared.Resource)
	resultTree, err := runner.ResolveTree(
		t.Context(), runtime.workspace.Root(), join.Worktree.ResultCommitOID,
	)
	require.NoError(t, err)
	entries, err := runner.ListTree(t.Context(), runtime.workspace.Root(), resultTree)
	require.NoError(t, err)
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, entry.Path)
	}
	assert.Subset(t, paths, []string{"README.md", "first.txt", "second.txt", "join.txt"})
	status, err := runner.SnapshotStatus(t.Context(), runtime.workspace.Root())
	require.NoError(t, err)
	assert.True(t, status.Clean)
	for _, name := range []string{"first.txt", "second.txt", "join.txt"} {
		_, statErr := os.Lstat(filepath.Join(runtime.workspace.Root(), name))
		require.ErrorIs(t, statErr, os.ErrNotExist)
	}

	resultRefs := []string{
		byTask["first"].Worktree.ResultRef,
		byTask["second"].Worktree.ResultRef,
		join.Worktree.ResultRef,
	}
	_, err = runtime.CleanupTeam(t.Context(), TeamCleanupRequest{
		TeamID: reference.TeamID, ExpectedResourceRevision: resources.Revision,
		CloseWithoutIntegration: true,
	})
	require.NoError(t, err)
	_, err = runner.ResolveRef(t.Context(), runtime.workspace.Root(), join.Base.OwnedRef)
	require.ErrorIs(t, err, gitcontrol.ErrNotFound)
	for _, resultRef := range resultRefs {
		_, err = runner.ResolveRef(t.Context(), runtime.workspace.Root(), resultRef)
		require.NoError(t, err)
	}
}

func TestRuntimeUsesExactSingleDependencyResultAsAttemptBase(t *testing.T) {
	t.Parallel()

	runtime := openGitTestRuntime(
		t,
		newParallelTeamWriteModel("dependency.txt", "consumer.txt"),
		nil,
	)
	proposal, err := runtime.ProposeTeam(t.Context(), TeamProposalRequest{
		Objective: "Create one result and consume it without synthesizing another base.",
		Workers: []TeamWorkerSpec{
			{Name: "Dependency", Role: "Create dependency.txt."},
			{Name: "Consumer", Role: "Create consumer.txt from the dependency result."},
		},
		Tasks: []TeamTaskSpec{
			{ID: "dependency", Title: "Create dependency.txt", AssignedWorker: "Dependency"},
			{
				ID: "consumer", Title: "Create consumer.txt", AssignedWorker: "Consumer",
				Dependencies: []string{"dependency"},
			},
		},
	})
	require.NoError(t, err)
	reference, err := runtime.ConfirmTeam(t.Context(), TeamConfirmation{
		ProposalID: proposal.ID, Admission: TeamAdmissionClean,
	})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		aggregate, getErr := runtime.team.engine.Get(t.Context(), reference.TeamID)
		if getErr != nil || len(aggregate.Tasks) != 2 {
			return false
		}
		for _, taskValue := range aggregate.Tasks {
			if taskValue.Status != team.TaskStatusCompleted {
				return false
			}
		}
		resources, loadErr := runtime.team.state.Load(t.Context(), reference.TeamID)

		return loadErr == nil && len(resources.Attempts) == 2 && runtime.team.ownerCount() == 0
	}, 60*time.Second, 20*time.Millisecond)

	resources, err := runtime.team.state.Load(t.Context(), reference.TeamID)
	require.NoError(t, err)
	byTask := make(map[team.TaskID]teamstate.AttemptResource, len(resources.Attempts))
	for _, attempt := range resources.Attempts {
		byTask[attempt.TaskID] = attempt
	}
	dependency := byTask["dependency"]
	consumer := byTask["consumer"]
	assert.Equal(t, dependency.Worktree.ResultCommitOID, consumer.Base.OID)
	assert.Equal(t, consumer.Base.OID, consumer.Worktree.BaseOID)
	assert.Equal(t, 1, consumer.Base.DependencyCount)
	assert.NotEmpty(t, consumer.Base.DependencyDigest)
	assert.Empty(t, consumer.Base.OwnedRef)
	assert.Empty(t, consumer.Base.CompositionDigest)
}

func TestRuntimeBlocksConflictingDependencyBaseBeforeOpeningJoinWorker(t *testing.T) {
	t.Parallel()

	model := newConflictingDependencyModel()
	runtime := openGitTestRuntime(t, model, nil)
	proposal, err := runtime.ProposeTeam(t.Context(), TeamProposalRequest{
		Objective: "Detect the conflicting prerequisite results before starting the join Worker.",
		Workers: []TeamWorkerSpec{
			{Name: "Left", Role: "Create the left shared-file result."},
			{Name: "Right", Role: "Create the right shared-file result."},
			{Name: "Join", Role: "Consume both results only when exact composition succeeds."},
		},
		Tasks: []TeamTaskSpec{
			{ID: "left", Title: "Apply left-change", AssignedWorker: "Left"},
			{ID: "right", Title: "Apply right-change", AssignedWorker: "Right"},
			{
				ID: "join", Title: "Run join-change", AssignedWorker: "Join",
				Dependencies: []string{"left", "right"},
			},
		},
	})
	require.NoError(t, err)
	reference, err := runtime.ConfirmTeam(t.Context(), TeamConfirmation{
		ProposalID: proposal.ID, Admission: TeamAdmissionClean,
	})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		aggregate, getErr := runtime.team.engine.Get(t.Context(), reference.TeamID)
		if getErr != nil {
			return false
		}
		joinTask, found := taskByID(aggregate.Tasks, "join")
		if !found || joinTask.Status != team.TaskStatusFailed {
			return false
		}
		resources, loadErr := runtime.team.state.Load(t.Context(), reference.TeamID)
		if loadErr != nil || resources.State != teamstate.StateBlockedConflict {
			return false
		}
		for _, attempt := range resources.Attempts {
			if attempt.TaskID == "join" {
				return attempt.State == teamstate.AttemptConflicted && runtime.team.ownerCount() == 0
			}
		}

		return false
	}, 60*time.Second, 20*time.Millisecond)

	resources, err := runtime.team.state.Load(t.Context(), reference.TeamID)
	require.NoError(t, err)
	for _, attempt := range resources.Attempts {
		if attempt.TaskID != "join" {
			continue
		}
		assert.Empty(t, attempt.Base.OID)
		assert.Empty(t, attempt.Worktree.ID)
		assert.Empty(t, attempt.Session.SessionID)
	}
	assert.Zero(t, model.joinCalls())
	status, err := gitcontrol.New(runtime.opts.GitPath, gitcontrol.DefaultLimits())
	require.NoError(t, err)
	parentStatus, err := status.SnapshotStatus(t.Context(), runtime.workspace.Root())
	require.NoError(t, err)
	assert.True(t, parentStatus.Clean)
}

func TestRuntimeComposesDiamondDependencyClosureExactlyOnce(t *testing.T) {
	t.Parallel()

	runtime := openGitTestRuntime(
		t,
		newParallelTeamWriteModel("root.txt", "left.txt", "right.txt", "diamond.txt"),
		nil,
	)
	proposal, err := runtime.ProposeTeam(t.Context(), TeamProposalRequest{
		Objective: "Compose a diamond dependency closure in stable Task order.",
		Workers: []TeamWorkerSpec{
			{Name: "RootDiamond", Role: "Create root.txt, then the final diamond result."},
			{Name: "Left", Role: "Create left.txt from root."},
			{Name: "Right", Role: "Create right.txt from root."},
		},
		Tasks: []TeamTaskSpec{
			{ID: "root", Title: "Create root.txt", AssignedWorker: "RootDiamond"},
			{
				ID: "left", Title: "Create left.txt", AssignedWorker: "Left",
				Dependencies: []string{"root"},
			},
			{
				ID: "right", Title: "Create right.txt", AssignedWorker: "Right",
				Dependencies: []string{"root"},
			},
			{
				ID: "diamond", Title: "Create diamond.txt", AssignedWorker: "RootDiamond",
				Dependencies: []string{"left", "right"},
			},
		},
	})
	require.NoError(t, err)
	reference, err := runtime.ConfirmTeam(t.Context(), TeamConfirmation{
		ProposalID: proposal.ID, Admission: TeamAdmissionClean,
	})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		aggregate, getErr := runtime.team.engine.Get(t.Context(), reference.TeamID)
		if getErr != nil || len(aggregate.Tasks) != 4 {
			return false
		}
		for _, taskValue := range aggregate.Tasks {
			if taskValue.Status != team.TaskStatusCompleted {
				return false
			}
		}

		return runtime.team.ownerCount() == 0
	}, 60*time.Second, 20*time.Millisecond)

	resources, err := runtime.team.state.Load(t.Context(), reference.TeamID)
	require.NoError(t, err)
	byTask := make(map[team.TaskID]teamstate.AttemptResource, len(resources.Attempts))
	for _, attempt := range resources.Attempts {
		byTask[attempt.TaskID] = attempt
	}
	rootResult := byTask["root"].Worktree.ResultCommitOID
	assert.Equal(t, rootResult, byTask["left"].Base.OID)
	assert.Equal(t, rootResult, byTask["right"].Base.OID)
	diamond := byTask["diamond"]
	assert.Equal(t, 3, diamond.Base.DependencyCount)
	assert.NotEmpty(t, diamond.Base.OwnedRef)
	assert.Equal(t, diamond.Base.OID, diamond.Worktree.BaseOID)

	runner, err := gitcontrol.New(runtime.opts.GitPath, gitcontrol.DefaultLimits())
	require.NoError(t, err)
	treeOID, err := runner.ResolveTree(
		t.Context(), runtime.workspace.Root(), diamond.Worktree.ResultCommitOID,
	)
	require.NoError(t, err)
	entries, err := runner.ListTree(t.Context(), runtime.workspace.Root(), treeOID)
	require.NoError(t, err)
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, entry.Path)
	}
	assert.Subset(
		t,
		paths,
		[]string{"README.md", "root.txt", "left.txt", "right.txt", "diamond.txt"},
	)
}

type conflictingDependencyModel struct {
	mu    sync.Mutex
	calls map[string]int
	join  int
}

func newConflictingDependencyModel() *conflictingDependencyModel {
	return &conflictingDependencyModel{calls: make(map[string]int, 2)}
}

func (m *conflictingDependencyModel) Generate(
	_ context.Context,
	request ai.Request,
) (*ai.Response, error) {
	key := ""
	for _, message := range request.Messages {
		if message.Role != ai.RoleUser {
			continue
		}
		text := runtimeMessageText(message)
		for _, candidate := range []string{"left-change", "right-change", "join-change"} {
			if strings.Contains(text, candidate) {
				key = candidate
				break
			}
		}
	}
	if key == "" {
		return nil, errors.New("conflicting dependency model could not identify the task")
	}
	m.mu.Lock()
	if key == "join-change" {
		m.join++
		m.mu.Unlock()

		return nil, errors.New("conflicting join Worker must not start")
	}
	m.calls[key]++
	call := m.calls[key]
	m.mu.Unlock()
	if call == 1 {
		content := "left\n"
		if key == "right-change" {
			content = "right\n"
		}
		arguments, err := json.Marshal(struct {
			Patch string `json:"patch"`
		}{Patch: "*** Begin Patch\n*** Add File: shared.txt\n+" +
			strings.TrimSuffix(content, "\n") + "\n*** End Patch"})
		if err != nil {
			return nil, err
		}

		return runtimeToolResponse("write-"+key, "apply_patch", string(arguments)), nil
	}
	if call == 2 {
		return runtimeTextResponse("Conflicting prerequisite result is ready for capture."), nil
	}

	return nil, errors.New("conflicting dependency model script exhausted")
}

func (m *conflictingDependencyModel) Stream(ctx context.Context, request ai.Request) ai.Stream {
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

func (*conflictingDependencyModel) Provider() ai.Provider { return ai.ProviderOpenAI }
func (*conflictingDependencyModel) ModelID() string       { return "conflicting-team-test" }
func (*conflictingDependencyModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

func (m *conflictingDependencyModel) joinCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.join
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

	return openCapturedTeamRuntimeWithPaths(t, "result.txt")
}

func openCapturedTeamRuntimeWithPaths(t *testing.T, paths ...string) *Runtime {
	t.Helper()

	require.NotEmpty(t, paths)

	workers := make([]TeamWorkerSpec, len(paths))

	tasks := make([]TeamTaskSpec, len(paths))

	for index, filePath := range paths {
		workerName := fmt.Sprintf("Worker %d", index+1)
		workers[index] = TeamWorkerSpec{
			Name: workerName, Role: "Implement the assigned file change.",
		}
		tasks[index] = TeamTaskSpec{
			ID: fmt.Sprintf("task-%d", index+1), Title: "Create " + filePath,
			Description:    "Create " + filePath + " with the requested bounded content.",
			AssignedWorker: workerName,
		}
	}

	model := newParallelTeamWriteModel(paths...)
	runtime := openGitTestRuntime(t, model, nil)
	proposal, err := runtime.ProposeTeam(t.Context(), TeamProposalRequest{
		Objective: "Create captured results without modifying the parent Workspace.",
		Workers:   workers,
		Tasks:     tasks,
	})
	require.NoError(t, err)
	reference, err := runtime.ConfirmTeam(t.Context(), TeamConfirmation{
		ProposalID: proposal.ID, Admission: TeamAdmissionClean,
	})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		aggregate, getErr := runtime.team.engine.Get(t.Context(), reference.TeamID)
		if getErr != nil || len(aggregate.Tasks) != len(tasks) {
			return false
		}

		for _, task := range aggregate.Tasks {
			if task.Status != team.TaskStatusCompleted {
				return false
			}
		}

		resources, loadErr := runtime.team.state.Load(t.Context(), reference.TeamID)
		if loadErr != nil || len(resources.Attempts) != len(tasks) {
			return false
		}

		for _, attempt := range resources.Attempts {
			terminal := attempt.State == teamstate.AttemptCaptured ||
				attempt.State == teamstate.AttemptTerminal
			if !terminal {
				return false
			}
		}

		return runtime.team.ownerCount() == 0
	}, 60*time.Second, 20*time.Millisecond)

	return runtime
}
