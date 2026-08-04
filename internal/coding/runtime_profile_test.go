package coding

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/rsbin/pips/agent/continuation"
	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/execution"
	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/rsbin/pips/internal/coding/session"
	"github.com/rsbin/pips/internal/coding/teamworktree"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTeamWorkerRuntimeUsesExactProfileAndCatalog(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(runtimeTextResponse("done"))
	runtime := openTestTeamWorkerRuntime(t, t.TempDir(), SessionTarget{}, model, nil)

	assert.Equal(t, profileTeamWorker, runtime.profile)
	assert.Equal(t, config.ModeAgent, runtime.config.Mode)
	assert.Equal(t, config.SandboxWorkspaceWrite, runtime.config.Sandbox)
	assert.Equal(t, config.ApprovalOnRequest, runtime.config.Approval)
	assert.Nil(t, runtime.subagents)
	assert.Nil(t, runtime.notifications)
	assert.Equal(t, session.KindTeamWorker, runtime.handle.Metadata().Kind)
	assert.Equal(t, runtime.worker.lineage, runtime.handle.Metadata().TeamWorker)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), ai.UserText("do the task")))
	assert.Contains(t, eventTypes(events), EventInteractionCompleted)

	requests := model.Requests()
	require.Len(t, requests, 1)
	tools := toolNamesFromRequest(requests[0])
	for _, expected := range []string{
		"read", "ls", "glob", "grep", "apply_patch", "shell", "ask_user",
		"team_get_status", "team_list_tasks", "team_send_message",
		"team_list_messages", "team_acknowledge_messages",
	} {
		assert.Contains(t, tools, expected)
	}
	for _, forbidden := range []string{
		"write_plan", "run_subagent", "spawn_agent", "team_claim_task",
		"team_release_task", "team_finish_task_attempt", "tool_search",
	} {
		assert.NotContains(t, tools, forbidden)
	}
	assert.Contains(t, requests[0].System, "# Team Worker assignment")
	assert.Contains(t, requests[0].System, `"attempt_id": "attempt-1"`)
	assert.Contains(t, requests[0].System, "Modify only the current Worktree")

	assert.ErrorIs(t, runtime.SetMode(t.Context(), ModePlan), ErrRuntimeInvalid)
	require.ErrorIs(t, runtime.ReplacementPreflight(t.Context()), ErrRuntimeInvalid)
	assert.ErrorIs(t, runtime.Reload(t.Context()), ErrRuntimeInvalid)
	assert.ErrorIs(t, runtime.SetSkillEnabled(t.Context(), "missing", false), ErrRuntimeInvalid)
	_, err := runtime.PlanDocumentPath()
	assert.ErrorIs(t, err, ErrRuntimeInvalid)
}

func TestTeamWorkerRuntimeReopensOnlyWithExactLineage(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	first := openTestTeamWorkerRuntime(
		t,
		base,
		SessionTarget{},
		newRuntimeModel(runtimeTextResponse("persisted")),
		nil,
	)
	collectRuntimeEvents(t, first.Prompt(t.Context(), ai.UserText("remember this")))
	sessionID := first.handle.Metadata().ID
	require.NoError(t, first.Close(t.Context()))

	reopened := openTestTeamWorkerRuntime(
		t,
		base,
		SessionTarget{ID: sessionID},
		newRuntimeModel(runtimeTextResponse("continued")),
		nil,
	)
	assert.Equal(t, sessionID, reopened.handle.Metadata().ID)
	require.Len(t, reopened.Snapshot().Transcript, 2)
	require.NoError(t, reopened.Close(t.Context()))

	wrong := reopened.worker.lineage
	wrong.AttemptID = "attempt-other"
	_, err := openTestTeamWorkerRuntimeValue(
		t,
		base,
		SessionTarget{ID: sessionID},
		newRuntimeModel(),
		&wrong,
	)
	assert.ErrorIs(t, err, session.ErrLineageMismatch)
}

func TestTeamWorkerRuntimeRejectsMismatchedWorkspaceBeforeSessionCreation(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	options, policy := testTeamWorkerOpenOptions(t, base, newRuntimeModel())
	otherPath := filepath.Join(base, "other-worktree")
	require.NoError(t, mkdirPrivate(otherPath))
	other, err := workspace.Open(otherPath)
	require.NoError(t, err)
	options.Workspace = other

	_, err = openRuntime(t.Context(), options, policy)
	assert.ErrorIs(t, err, ErrRuntimeInvalid)

	repository, repoErr := session.NewRepository(options.Paths.SessionsDir())
	require.NoError(t, repoErr)
	page, listErr := repository.List(t.Context())
	require.NoError(t, listErr)
	assert.Empty(t, page)
}

func openTestTeamWorkerRuntime(
	t *testing.T,
	base string,
	target SessionTarget,
	model ai.LanguageModel,
	lineage *session.TeamWorkerLineage,
) *Runtime {
	t.Helper()

	runtime, err := openTestTeamWorkerRuntimeValue(t, base, target, model, lineage)
	require.NoError(t, err)
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	return runtime
}

func openTestTeamWorkerRuntimeValue(
	t *testing.T,
	base string,
	target SessionTarget,
	model ai.LanguageModel,
	lineage *session.TeamWorkerLineage,
) (*Runtime, error) {
	t.Helper()

	options, policy := testTeamWorkerOpenOptions(t, base, model)
	options.Session = target
	if lineage != nil {
		policy.worker.lineage = *lineage
		policy.worker.worktree.Owner.TeamID = lineage.TeamID
		policy.worker.worktree.Owner.MemberID = lineage.MemberID
		policy.worker.worktree.Owner.AttemptID = lineage.AttemptID
	}

	return openRuntime(t.Context(), options, policy)
}

func testTeamWorkerOpenOptions(
	t *testing.T,
	base string,
	model ai.LanguageModel,
) (OpenOptions, runtimeOpenPolicy) {
	t.Helper()

	worktreePath := filepath.Join(base, "worker-worktree")
	parentPath := filepath.Join(base, "parent-workspace")
	gitDir := filepath.Join(base, "worker-git-dir")
	commonDir := filepath.Join(base, "common-git-dir")
	for _, path := range []string{worktreePath, parentPath, gitDir, commonDir} {
		require.NoError(t, mkdirPrivate(path))
	}

	worktree, err := workspace.Open(worktreePath)
	require.NoError(t, err)
	parent, err := workspace.Open(parentPath)
	require.NoError(t, err)
	gitWorkspace, err := workspace.Open(gitDir)
	require.NoError(t, err)
	commonWorkspace, err := workspace.Open(commonDir)
	require.NoError(t, err)

	layout, err := paths.New(filepath.Join(base, "home"))
	require.NoError(t, err)
	cfg := config.Defaults()
	cfg.Model.Provider = model.Provider()
	cfg.Model.Model = model.ModelID()
	cfg.Sandbox = config.SandboxFullAccess
	cfg.Approval = config.ApprovalNever

	store, err := team.NewMemoryStore()
	require.NoError(t, err)
	engine, err := team.New(store)
	require.NoError(t, err)
	created, err := engine.Create(t.Context(), team.CreateRequest{
		Command: team.CommandMetadata{
			ID: "create-team", ExpectedRevision: 0,
			Actor: team.Actor{Kind: team.ActorKindCoordinator, ID: "test-coordinator"},
		},
		ID: "team-1", Objective: "Implement the approved Team objective.",
		Lead: team.MemberSpec{ID: "lead", Name: "Lead", Role: "coordinate"},
	})
	require.NoError(t, err)
	_, err = engine.RegisterMember(t.Context(), created.ID, team.RegisterMemberRequest{
		Command: team.CommandMetadata{
			ID: "register-worker", ExpectedRevision: created.Revision,
			Actor: team.Actor{Kind: team.ActorKindCoordinator, ID: "test-coordinator"},
		},
		Member: team.MemberSpec{ID: "worker-1", Name: "Worker", Role: "implement"},
	})
	require.NoError(t, err)
	lineage := session.TeamWorkerLineage{
		ParentSessionID: "s-parent",
		TeamID:          "team-1",
		MemberID:        "worker-1",
		TaskID:          "task-1",
		AttemptID:       "attempt-1",
		ContinuationID:  continuation.ID("continuation-1"),
	}
	identity := func(value workspace.Identity) teamworktree.FileIdentity {
		return teamworktree.FileIdentity{
			Path: value.Path(), Device: value.Device(), Inode: value.Inode(),
		}
	}
	resource := teamworktree.Resource{
		ID: "worktree-1",
		Owner: teamworktree.Owner{
			TeamID: lineage.TeamID, MemberID: lineage.MemberID,
			AttemptID: lineage.AttemptID, LeaseGeneration: 7,
		},
		Workspace: identity(parent.Identity()),
		Directory: identity(worktree.Identity()),
		GitDir:    identity(gitWorkspace.Identity()),
		CommonDir: identity(commonWorkspace.Identity()),
		BaseOID:   "0123456789012345678901234567890123456789",
		BranchRef: "refs/heads/pips/team-1/attempt-1",
		ResultRef: "refs/pips/results/team-1/attempt-1",
	}

	return OpenOptions{
			Workspace: worktree,
			Trusted:   true,
			Config:    cfg,
			Paths:     layout,
			Model:     model,
			Execution: ExecutionOptions{
				SandboxProbe: func(context.Context, *execution.Executor) error { return nil },
			},
		}, runtimeOpenPolicy{
			profile: profileTeamWorker,
			worker: &workerRuntimeBinding{
				lineage: lineage, worktree: resource, team: engine,
				objective:             "Implement the approved Team objective.",
				task:                  "Modify the assigned files and verify the result.",
				dependencyEvidence:    "No dependencies.",
				capabilityFingerprint: "worker-v1",
				ownerGeneration:       7,
			},
		}
}
