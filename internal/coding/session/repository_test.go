//nolint:wsl_v5 // Repository lifecycle fixtures keep actions and assertions adjacent.
package session_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testWorkspacePath = "/workspace"

func TestRepositoryForkPreservesSourceAndProjectsBoundedPickerMetadata(t *testing.T) {
	t.Parallel()

	repo, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)
	source, err := repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID: "workspace-key", WorkspacePath: testWorkspacePath,
	})
	require.NoError(t, err)
	first, err := source.Session().AppendMessage(ai.UserText("  inspect\n the session tree  "), nil)
	require.NoError(t, err)
	_, err = source.Session().AppendMessage(ai.AssistantText("first branch"), nil)
	require.NoError(t, err)
	require.NoError(t, source.Session().MoveTo(first, ""))
	_, err = source.Session().AppendMessage(ai.AssistantText("second branch"), nil)
	require.NoError(t, err)
	require.NoError(t, source.Session().SetName("tree work"))
	before := source.Session().Entries()

	forked, err := repo.Fork(t.Context(), source, session.ForkOptions{AtEntryID: first})
	require.NoError(t, err)
	assert.Equal(t, before, source.Session().Entries())
	assert.Equal(t, source.Metadata().ID, forked.Metadata().ParentSessionID)
	assert.Equal(t, first, forked.Metadata().ParentEntryID)
	require.Len(t, forked.Session().Entries(), 1)

	_, err = repo.Open(t.Context(), session.OpenOptions{
		ID: forked.Metadata().ID, WorkspaceID: "workspace-key",
	})
	require.ErrorIs(t, err, session.ErrLocked)

	metas, err := repo.List(t.Context())
	require.NoError(t, err)
	require.Len(t, metas, 2)
	var sourceMeta, forkMeta session.Metadata
	for _, meta := range metas {
		if meta.ID == source.Metadata().ID {
			sourceMeta = meta
		} else if meta.ID == forked.Metadata().ID {
			forkMeta = meta
		}
	}
	assert.Equal(t, "tree work", sourceMeta.Name)
	assert.Equal(t, "inspect the session tree", sourceMeta.Preview)
	assert.GreaterOrEqual(t, sourceMeta.BranchCount, 1)
	assert.False(t, sourceMeta.Truncated)
	assert.Equal(t, "inspect the session tree", forkMeta.Preview)
	assert.Equal(t, 1, forkMeta.NodeCount)
	assert.Equal(t, source.Metadata().ID, forkMeta.ParentSessionID)

	forkID := forked.Metadata().ID
	require.NoError(t, forked.Close())
	reopened, err := repo.Open(t.Context(), session.OpenOptions{
		ID: forkID, WorkspaceID: "workspace-key",
	})
	require.NoError(t, err)
	assert.Equal(t, source.Metadata().ID, reopened.Metadata().ParentSessionID)
	require.NoError(t, reopened.Close())
	require.NoError(t, source.Close())
}

func TestRepositoryCreateOpenListAndLock(t *testing.T) {
	t.Parallel()

	repo, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)

	handle, err := repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID: "workspace-key", WorkspacePath: testWorkspacePath,
	})
	require.NoError(t, err)

	meta := handle.Metadata()
	assert.NotEmpty(t, meta.ID)
	assert.Equal(t, "workspace-key", meta.WorkspaceID)
	assert.Equal(t, testWorkspacePath, meta.WorkspacePath)
	assert.NotNil(t, handle.Session())
	_, err = os.Stat(meta.Path)
	require.ErrorIs(t, err, os.ErrNotExist)

	metas, err := repo.List(t.Context())
	require.NoError(t, err)
	assert.Empty(t, metas)

	_, err = repo.Open(t.Context(), session.OpenOptions{ID: meta.ID, WorkspaceID: "workspace-key"})
	require.Error(t, err)
	require.ErrorIs(t, err, session.ErrLocked)

	_, err = handle.Session().AppendCustom("test.started", nil)
	require.NoError(t, err)
	info, err := os.Stat(meta.Path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	metas, err = repo.List(t.Context())
	require.NoError(t, err)
	require.Len(t, metas, 1)
	assert.Equal(t, meta.ID, metas[0].ID)
	assert.Equal(t, meta.WorkspaceID, metas[0].WorkspaceID)
	assert.Equal(t, meta.WorkspacePath, metas[0].WorkspacePath)

	require.NoError(t, handle.Close())
	require.NoError(t, handle.Close())

	reopened, err := repo.Open(t.Context(), session.OpenOptions{ID: meta.ID, WorkspaceID: "workspace-key"})
	require.NoError(t, err)
	entries := reopened.Session().Entries()
	require.Len(t, entries, 1)
	assert.Equal(t, harness.KindCustom, entries[0].Kind)
	require.NoError(t, reopened.Close())

	lockInfo, err := os.Stat(filepath.Join(repo.Dir(), meta.ID+".lock"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), lockInfo.Mode().Perm())
}

func TestRepositoryDeleteConversationIsIdempotentAndLockSafe(t *testing.T) {
	t.Parallel()

	repo, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)
	handle, err := repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID: "workspace-key", WorkspacePath: testWorkspacePath,
	})
	require.NoError(t, err)
	id := handle.Metadata().ID
	path := handle.Metadata().Path
	_, err = handle.Session().AppendMessage(ai.UserText("delete this conversation"), nil)
	require.NoError(t, err)

	err = repo.Delete(t.Context(), id)
	require.ErrorIs(t, err, session.ErrLocked)
	require.FileExists(t, path)

	require.NoError(t, handle.Close())
	require.NoError(t, repo.Delete(t.Context(), id))
	require.NoFileExists(t, path)
	require.NoError(t, repo.Delete(t.Context(), id))
	require.NoError(t, repo.Delete(t.Context(), "missing-session"))
	require.NoFileExists(t, filepath.Join(repo.Dir(), "missing-session.lock"))

	metadata, err := repo.List(t.Context())
	require.NoError(t, err)
	assert.Empty(t, metadata)
}

func TestRepositoryDeleteRejectsChildTranscript(t *testing.T) {
	t.Parallel()

	repo, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)
	child, err := repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID:     "workspace-key",
		WorkspacePath:   testWorkspacePath,
		Kind:            session.KindSubagent,
		ParentSessionID: "parent-session",
		ParentRunID:     "parent-run",
		Agent:           "explore",
	})
	require.NoError(t, err)
	id := child.Metadata().ID
	path := child.Metadata().Path
	_, err = child.Session().AppendMessage(ai.UserText("retain child transcript"), nil)
	require.NoError(t, err)
	require.NoError(t, child.Close())

	err = repo.Delete(t.Context(), id)
	require.ErrorIs(t, err, session.ErrInvalid)
	require.FileExists(t, path)
}

func TestRepositorySeparatesConversationAndSubagentSessions(t *testing.T) {
	t.Parallel()

	repo, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)
	conversation, err := repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID: "workspace-key", WorkspacePath: testWorkspacePath,
	})
	require.NoError(t, err)
	_, err = conversation.Session().AppendMessage(ai.UserText("parent question"), nil)
	require.NoError(t, err)

	child, err := repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID:     "workspace-key",
		WorkspacePath:   testWorkspacePath,
		Kind:            session.KindSubagent,
		ParentSessionID: conversation.Metadata().ID,
		ParentRunID:     "run-parent",
		Agent:           "explore",
	})
	require.NoError(t, err)
	_, err = child.Session().AppendMessage(ai.UserText("inspect the package"), nil)
	require.NoError(t, err)

	conversations, err := repo.List(t.Context())
	require.NoError(t, err)
	require.Len(t, conversations, 1)
	assert.Equal(t, conversation.Metadata().ID, conversations[0].ID)
	assert.Equal(t, session.KindConversation, conversations[0].Kind)

	children, err := repo.ListSubagents(
		t.Context(),
		"workspace-key",
		conversation.Metadata().ID,
	)
	require.NoError(t, err)
	require.Len(t, children, 1)
	assert.Equal(t, child.Metadata().ID, children[0].ID)
	assert.Equal(t, session.KindSubagent, children[0].Kind)
	assert.Equal(t, "run-parent", children[0].ParentRunID)
	assert.Equal(t, "explore", children[0].Agent)
	assert.Equal(t, "inspect the package", children[0].Preview)

	require.NoError(t, child.Close())
	require.NoError(t, conversation.Close())
}

func TestRepositoryRejectsInvalidSubagentLineage(t *testing.T) {
	t.Parallel()

	repo, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)

	_, err = repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID:   "workspace-key",
		WorkspacePath: testWorkspacePath,
		Kind:          session.KindSubagent,
		Agent:         "explore",
	})
	require.ErrorIs(t, err, session.ErrInvalid)
	_, err = repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID:     "workspace-key",
		WorkspacePath:   testWorkspacePath,
		Kind:            session.KindConversation,
		ParentSessionID: "parent",
	})
	require.ErrorIs(t, err, session.ErrInvalid)
}

func TestRepositorySeparatesTeamWorkerLineageAndWorkspace(t *testing.T) {
	t.Parallel()

	repo, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)
	lineage := session.TeamWorkerLineage{
		ParentSessionID: "s-parent",
		TeamID:          "team-one",
		MemberID:        "member-one",
		TaskID:          "task-one",
		AttemptID:       "attempt-one",
		ContinuationID:  "continuation-one",
	}
	worker, err := repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID:   "worker-workspace",
		WorkspacePath: testWorkspacePath,
		Kind:          session.KindTeamWorker,
		TeamWorker:    &lineage,
	})
	require.NoError(t, err)
	_, err = worker.Session().AppendMessage(ai.UserText("worker task"), nil)
	require.NoError(t, err)
	id := worker.Metadata().ID
	assert.Equal(t, "worker-workspace", worker.Metadata().WorkspaceID)
	assert.Equal(t, lineage, worker.Metadata().TeamWorker)
	require.NoError(t, worker.Close())

	conversations, err := repo.List(t.Context())
	require.NoError(t, err)
	assert.Empty(t, conversations)
	subagents, err := repo.ListSubagents(t.Context(), "worker-workspace", "s-parent")
	require.NoError(t, err)
	assert.Empty(t, subagents)
	workers, err := repo.ListTeamWorkers(t.Context(), "s-parent", "team-one", 10)
	require.NoError(t, err)
	require.Len(t, workers, 1)
	assert.Equal(t, id, workers[0].ID)
	assert.Equal(t, "worker-workspace", workers[0].WorkspaceID)
	assert.Equal(t, lineage, workers[0].TeamWorker)
	assert.Equal(t, "worker task", workers[0].Preview)

	_, err = repo.Open(t.Context(), session.OpenOptions{ID: id, WorkspaceID: "worker-workspace"})
	require.ErrorIs(t, err, session.ErrInvalid)

	wrong := lineage
	wrong.AttemptID = "attempt-other"
	_, err = repo.OpenTeamWorker(t.Context(), session.OpenTeamWorkerOptions{
		ID: id, WorkspaceID: "worker-workspace", Lineage: wrong,
	})
	require.ErrorIs(t, err, session.ErrLineageMismatch)
	_, err = repo.OpenTeamWorker(t.Context(), session.OpenTeamWorkerOptions{
		ID: id, WorkspaceID: "parent-workspace", Lineage: lineage,
	})
	require.ErrorIs(t, err, session.ErrWorkspaceMismatch)

	reopened, err := repo.OpenTeamWorker(t.Context(), session.OpenTeamWorkerOptions{
		ID: id, WorkspaceID: "worker-workspace", Lineage: lineage,
	})
	require.NoError(t, err)
	require.NoError(t, reopened.Close())
}

func TestRepositoryRejectsIncompleteTeamWorkerLineage(t *testing.T) {
	t.Parallel()

	repo, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)

	_, err = repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID:   "worker-workspace",
		WorkspacePath: testWorkspacePath,
		Kind:          session.KindTeamWorker,
	})
	require.ErrorIs(t, err, session.ErrInvalid)
	_, err = repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID:   "worker-workspace",
		WorkspacePath: testWorkspacePath,
		Kind:          session.KindTeamWorker,
		TeamWorker: &session.TeamWorkerLineage{
			ParentSessionID: "s-parent",
			TeamID:          "bad/team",
		},
	})
	require.ErrorIs(t, err, session.ErrInvalid)
}

func TestRepositoryCloseProvisionalDoesNotPersist(t *testing.T) {
	t.Parallel()

	repo, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)
	handle, err := repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID: "workspace-key", WorkspacePath: testWorkspacePath,
	})
	require.NoError(t, err)
	meta := handle.Metadata()

	_, err = repo.Fork(t.Context(), handle, session.ForkOptions{})
	require.ErrorIs(t, err, session.ErrInvalid)
	require.NoError(t, handle.Close())
	require.NoError(t, handle.Close())
	_, err = os.Stat(meta.Path)
	require.ErrorIs(t, err, os.ErrNotExist)

	metas, err := repo.List(t.Context())
	require.NoError(t, err)
	assert.Empty(t, metas)
	_, err = repo.Open(t.Context(), session.OpenOptions{
		ID: meta.ID, WorkspaceID: "workspace-key",
	})
	require.Error(t, err)
	require.NotErrorIs(t, err, session.ErrLocked)
	_, err = handle.Session().AppendCustom("test.started", nil)
	require.Error(t, err)
}

func TestRepositoryRetainsEmptyConversationForPublishedIdentity(t *testing.T) {
	t.Parallel()

	repo, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)
	handle, err := repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID:   "workspace-key",
		WorkspacePath: testWorkspacePath,
		RetainEmpty:   true,
	})
	require.NoError(t, err)
	meta := handle.Metadata()
	assert.True(t, meta.RetainEmpty)
	require.FileExists(t, meta.Path)

	listed, err := repo.List(t.Context())
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, meta.ID, listed[0].ID)
	assert.True(t, listed[0].RetainEmpty)
	assert.Zero(t, listed[0].NodeCount)

	require.NoError(t, handle.Close())
	reopened, err := repo.Open(t.Context(), session.OpenOptions{
		ID: meta.ID, WorkspaceID: "workspace-key",
	})
	require.NoError(t, err)
	assert.Empty(t, reopened.Session().Entries())
	assert.True(t, reopened.Metadata().RetainEmpty)
	require.NoError(t, reopened.Close())
	require.NoError(t, repo.Delete(t.Context(), meta.ID))
	require.NoFileExists(t, meta.Path)
}

func TestRepositoryRejectsWorkspaceMismatchAndReleasesLock(t *testing.T) {
	t.Parallel()

	repo, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)
	handle, err := repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID: "workspace-one", WorkspacePath: testWorkspacePath,
	})
	require.NoError(t, err)

	id := handle.Metadata().ID
	_, err = handle.Session().AppendCustom("test.started", nil)
	require.NoError(t, err)
	require.NoError(t, handle.Close())

	_, err = repo.Open(t.Context(), session.OpenOptions{ID: id, WorkspaceID: "workspace-two"})
	require.Error(t, err)
	require.ErrorIs(t, err, session.ErrWorkspaceMismatch)

	reopened, err := repo.Open(t.Context(), session.OpenOptions{ID: id, WorkspaceID: "workspace-one"})
	require.NoError(t, err)
	require.NoError(t, reopened.Close())
}

func TestRepositoryRejectsInvalidInputAndCancellation(t *testing.T) {
	t.Parallel()

	_, err := session.NewRepository("")
	require.ErrorIs(t, err, session.ErrInvalid)

	repo, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)

	_, err = repo.Create(t.Context(), session.CreateOptions{})
	require.ErrorIs(t, err, session.ErrInvalid)
	for _, workspacePath := range []string{"", "relative", "/workspace/../workspace", "/bad\x00path"} {
		_, err = repo.Create(t.Context(), session.CreateOptions{
			WorkspaceID: "workspace", WorkspacePath: workspacePath,
		})
		require.ErrorIs(t, err, session.ErrInvalid, workspacePath)
	}

	for _, id := range []string{"", ".", "..", "../escape", "/absolute", "bad/id"} {
		_, err = repo.Open(t.Context(), session.OpenOptions{ID: id, WorkspaceID: "workspace"})
		require.ErrorIs(t, err, session.ErrInvalid, id)
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = repo.Create(canceled, session.CreateOptions{
		WorkspaceID: "workspace", WorkspacePath: testWorkspacePath,
	})
	require.ErrorIs(t, err, context.Canceled)
	_, err = repo.List(canceled)
	require.ErrorIs(t, err, context.Canceled)
}

func TestRepositoryIgnoresLegacyModelMetadata(t *testing.T) {
	t.Parallel()

	repo, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)

	legacy, err := (harness.Repo{Dir: repo.Dir()}).Create("legacy", map[string]string{
		"pips.coding.workspace_id":   "workspace-key",
		"pips.coding.workspace_path": testWorkspacePath,
		"pips.coding.provider":       "anthropic",
		"pips.coding.model_id":       "legacy-model",
	})
	require.NoError(t, err)
	legacySession, err := harness.NewSession(legacy)
	require.NoError(t, err)
	_, err = legacySession.AppendModelChange("anthropic", "legacy-model")
	require.NoError(t, err)
	require.NoError(t, legacy.Close())
	empty, err := (harness.Repo{Dir: repo.Dir()}).Create("empty", map[string]string{
		"pips.coding.workspace_id":   "workspace-key",
		"pips.coding.workspace_path": testWorkspacePath,
	})
	require.NoError(t, err)
	require.NoError(t, empty.Close())

	handle, err := repo.Open(t.Context(), session.OpenOptions{
		ID:          "legacy",
		WorkspaceID: "workspace-key",
	})
	require.NoError(t, err)
	assert.Equal(t, "legacy", handle.Metadata().ID)
	assert.Equal(t, "workspace-key", handle.Metadata().WorkspaceID)
	require.NoError(t, handle.Close())

	metas, err := repo.List(t.Context())
	require.NoError(t, err)
	require.Len(t, metas, 1)
	assert.Equal(t, handle.Metadata().ID, metas[0].ID)
	assert.Equal(t, handle.Metadata().WorkspaceID, metas[0].WorkspaceID)
}

func TestRepositoryRejectsInsecureDirectoryAndLock(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	insecureDir := filepath.Join(base, "insecure")
	require.NoError(t, os.Mkdir(insecureDir, 0o777)) //nolint:gosec // deliberately insecure fixture
	require.NoError(t, os.Chmod(insecureDir, 0o777)) //nolint:gosec // deliberately insecure fixture
	_, err := session.NewRepository(insecureDir)
	require.ErrorIs(t, err, session.ErrInvalid)

	repo, err := session.NewRepository(filepath.Join(base, "sessions"))
	require.NoError(t, err)

	foreign := filepath.Join(base, "foreign")
	require.NoError(t, os.WriteFile(foreign, []byte("do not follow"), 0o600))
	require.NoError(t, os.Symlink(foreign, filepath.Join(repo.Dir(), "known.lock")))
	_, err = repo.Open(t.Context(), session.OpenOptions{ID: "known", WorkspaceID: "workspace"})
	require.Error(t, err)
	require.NotErrorIs(t, err, os.ErrNotExist)

	content, err := os.ReadFile(foreign) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	assert.Equal(t, "do not follow", string(content))
}

func TestRepositoryRejectsInsecureSessionFileMode(t *testing.T) {
	t.Parallel()

	for _, mode := range []os.FileMode{0o640, 0o644} {
		t.Run(mode.String(), func(t *testing.T) {
			t.Parallel()

			repo, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
			require.NoError(t, err)
			handle, err := repo.Create(t.Context(), session.CreateOptions{
				WorkspaceID: "workspace-key", WorkspacePath: testWorkspacePath,
			})
			require.NoError(t, err)
			id := handle.Metadata().ID
			path := handle.Metadata().Path
			_, err = handle.Session().AppendCustom("test.started", nil)
			require.NoError(t, err)
			require.NoError(t, handle.Close())
			require.NoError(t, os.Chmod(path, mode))

			_, err = repo.Open(t.Context(), session.OpenOptions{
				ID: id, WorkspaceID: "workspace-key",
			})
			require.ErrorIs(t, err, session.ErrInvalid)
			_, err = repo.List(t.Context())
			require.ErrorIs(t, err, session.ErrInvalid)
		})
	}
}

func TestRepositoryIgnoresLegacySessionsWithoutCodingMetadata(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "sessions")
	repo, err := session.NewRepository(dir)
	require.NoError(t, err)

	conversation, err := repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID: "workspace-key", WorkspacePath: testWorkspacePath,
	})
	require.NoError(t, err)
	_, err = conversation.Session().AppendMessage(ai.UserText("parent question"), nil)
	require.NoError(t, err)
	child, err := repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID:     "workspace-key",
		WorkspacePath:   testWorkspacePath,
		Kind:            session.KindSubagent,
		ParentSessionID: conversation.Metadata().ID,
		ParentRunID:     "run-parent",
		Agent:           "explore",
	})
	require.NoError(t, err)
	_, err = child.Session().AppendMessage(ai.UserText("inspect the package"), nil)
	require.NoError(t, err)

	// A session written by an older build has no workspace_path in its header.
	legacyHeader := `{"type":"harness_session","version":1,"id":"s-legacy-0001",` +
		`"created_at":"2026-07-23T06:51:12.408636Z","extra":{` +
		`"pips.coding.session_kind":"subagent","pips.coding.workspace_id":"workspace-key",` +
		`"pips.coding.parent_session_id":"` + conversation.Metadata().ID + `"}}` + "\n"
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "s-legacy-0001.jsonl"),
		[]byte(legacyHeader),
		0o600,
	))

	children, err := repo.ListSubagents(t.Context(), "workspace-key", conversation.Metadata().ID)
	require.NoError(t, err)
	require.Len(t, children, 1)
	assert.Equal(t, child.Metadata().ID, children[0].ID)

	conversations, err := repo.List(t.Context())
	require.NoError(t, err)
	require.Len(t, conversations, 1)
	assert.Equal(t, conversation.Metadata().ID, conversations[0].ID)

	require.NoError(t, child.Close())
	require.NoError(t, conversation.Close())
}
