package session_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/internal/coding/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	_, err = repo.Open(t.Context(), session.OpenOptions{ID: meta.ID, WorkspaceID: "workspace-key"})
	require.Error(t, err)
	require.ErrorIs(t, err, session.ErrLocked)

	metas, err := repo.List(t.Context())
	require.NoError(t, err)
	require.Len(t, metas, 1)
	assert.Equal(t, meta, metas[0])

	require.NoError(t, handle.Close())
	require.NoError(t, handle.Close())

	reopened, err := repo.Open(t.Context(), session.OpenOptions{ID: meta.ID, WorkspaceID: "workspace-key"})
	require.NoError(t, err)
	require.NoError(t, reopened.Close())

	lockInfo, err := os.Stat(filepath.Join(repo.Dir(), meta.ID+".lock"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), lockInfo.Mode().Perm())
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
	require.NoError(t, legacy.Close())

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
	assert.Equal(t, handle.Metadata(), metas[0])
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
