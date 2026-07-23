//nolint:wsl_v5 // Repository lifecycle fixtures keep actions and assertions adjacent.
package session_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepositoryForkPreservesSourceAndProjectsBoundedPickerMetadata(t *testing.T) {
	t.Parallel()

	repo, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)
	source, err := repo.Create(t.Context(), session.CreateOptions{WorkspaceID: "workspace-key"})
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
		WorkspaceID: "workspace-key",
	})
	require.NoError(t, err)

	meta := handle.Metadata()
	assert.NotEmpty(t, meta.ID)
	assert.Equal(t, "workspace-key", meta.WorkspaceID)
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

func TestRepositorySeparatesConversationAndSubagentSessions(t *testing.T) {
	t.Parallel()

	repo, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)
	conversation, err := repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID: "workspace-key",
	})
	require.NoError(t, err)
	_, err = conversation.Session().AppendMessage(ai.UserText("parent question"), nil)
	require.NoError(t, err)

	child, err := repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID:     "workspace-key",
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
		WorkspaceID: "workspace-key",
		Kind:        session.KindSubagent,
		Agent:       "explore",
	})
	require.ErrorIs(t, err, session.ErrInvalid)
	_, err = repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID:     "workspace-key",
		Kind:            session.KindConversation,
		ParentSessionID: "parent",
	})
	require.ErrorIs(t, err, session.ErrInvalid)
}

func TestRepositoryCloseProvisionalDoesNotPersist(t *testing.T) {
	t.Parallel()

	repo, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)
	handle, err := repo.Create(t.Context(), session.CreateOptions{WorkspaceID: "workspace-key"})
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

func TestRepositoryRejectsWorkspaceMismatchAndReleasesLock(t *testing.T) {
	t.Parallel()

	repo, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)
	handle, err := repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID: "workspace-one",
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

	for _, id := range []string{"", ".", "..", "../escape", "/absolute", "bad/id"} {
		_, err = repo.Open(t.Context(), session.OpenOptions{ID: id, WorkspaceID: "workspace"})
		require.ErrorIs(t, err, session.ErrInvalid, id)
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = repo.Create(canceled, session.CreateOptions{
		WorkspaceID: "workspace",
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
		"pips.coding.workspace_id": "workspace-key",
		"pips.coding.provider":     "anthropic",
		"pips.coding.model_id":     "legacy-model",
	})
	require.NoError(t, err)
	legacySession, err := harness.NewSession(legacy)
	require.NoError(t, err)
	_, err = legacySession.AppendModelChange("anthropic", "legacy-model")
	require.NoError(t, err)
	require.NoError(t, legacy.Close())
	empty, err := (harness.Repo{Dir: repo.Dir()}).Create("empty", map[string]string{
		"pips.coding.workspace_id": "workspace-key",
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
			handle, err := repo.Create(t.Context(), session.CreateOptions{WorkspaceID: "workspace-key"})
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
