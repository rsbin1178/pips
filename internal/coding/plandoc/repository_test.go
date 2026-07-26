package plandoc_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/rsbin/pips/internal/coding/plandoc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileRepositoryCreateReadReplaceAndFork(t *testing.T) {
	t.Parallel()

	repository := newRepository(t)
	source := plandoc.Ref{SessionID: "s-source", WorkspaceID: "workspace-1"}
	target := plandoc.Ref{SessionID: "s-target", WorkspaceID: "workspace-1"}

	_, err := repository.Read(t.Context(), source)
	require.ErrorIs(t, err, plandoc.ErrNotFound)

	created, err := repository.Replace(t.Context(), source, "", "# Plan\n\nFirst")
	require.NoError(t, err)
	assert.NotEmpty(t, created.Revision)
	assert.Equal(t, int64(len(created.Content)), created.Size)

	read, err := repository.Read(t.Context(), source)
	require.NoError(t, err)
	assert.Equal(t, created, read)

	replaced, err := repository.Replace(
		t.Context(), source, created.Revision, "# Plan\n\nSecond",
	)
	require.NoError(t, err)
	assert.NotEqual(t, created.Revision, replaced.Revision)

	require.NoError(t, repository.Fork(t.Context(), source, target))
	forked, err := repository.Read(t.Context(), target)
	require.NoError(t, err)
	assert.Equal(t, replaced.Content, forked.Content)
	assert.Equal(t, target, forked.Ref)

	_, err = repository.Replace(t.Context(), target, forked.Revision, "independent")
	require.NoError(t, err)
	sourceAfter, err := repository.Read(t.Context(), source)
	require.NoError(t, err)
	assert.Equal(t, replaced.Content, sourceAfter.Content)
}

func TestFileRepositoryRejectsStaleAndCreateOverwrite(t *testing.T) {
	t.Parallel()

	repository := newRepository(t)
	ref := plandoc.Ref{SessionID: "s-session", WorkspaceID: "workspace-1"}
	created, err := repository.Replace(t.Context(), ref, "", "one")
	require.NoError(t, err)

	_, err = repository.Replace(t.Context(), ref, "", "overwrite")
	require.ErrorIs(t, err, plandoc.ErrConflict)
	_, err = repository.Replace(t.Context(), ref, created.Revision, "two")
	require.NoError(t, err)
	_, err = repository.Replace(t.Context(), ref, created.Revision, "stale")
	require.ErrorIs(t, err, plandoc.ErrConflict)

	current, err := repository.Read(t.Context(), ref)
	require.NoError(t, err)
	assert.Equal(t, "two", current.Content)
}

func TestFileRepositoryBindsWorkspaceIdentity(t *testing.T) {
	t.Parallel()

	repository := newRepository(t)
	ref := plandoc.Ref{SessionID: "s-session", WorkspaceID: "workspace-1"}
	_, err := repository.Replace(t.Context(), ref, "", "private")
	require.NoError(t, err)

	_, err = repository.Read(t.Context(), plandoc.Ref{
		SessionID: ref.SessionID, WorkspaceID: "workspace-2",
	})
	require.ErrorIs(t, err, plandoc.ErrUnsafe)
}

func TestFileRepositoryRejectsSymlinkAndInvalidContent(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("private Plan repository is intentionally Unix-only")
	}

	repository := newRepository(t)
	ref := plandoc.Ref{SessionID: "s-session", WorkspaceID: "workspace-1"}
	target := filepath.Join(t.TempDir(), "target")
	require.NoError(t, os.WriteFile(target, []byte("secret"), 0o600))

	path, err := repository.Path(ref)
	require.NoError(t, err)
	require.NoError(t, os.Symlink(target, path))

	_, err = repository.Read(t.Context(), ref)
	require.ErrorIs(t, err, plandoc.ErrUnsafe)
	_, err = repository.Replace(t.Context(), ref, "", "replacement")
	require.ErrorIs(t, err, plandoc.ErrUnsafe)

	other := plandoc.Ref{SessionID: "s-other", WorkspaceID: "workspace-1"}
	_, err = repository.Replace(t.Context(), other, "", "invalid\x00content")
	require.ErrorIs(t, err, plandoc.ErrInvalid)
}

func TestFileRepositoryEnforcesPrivateModesAndLimits(t *testing.T) {
	t.Parallel()

	directory := filepath.Join(t.TempDir(), "plans")
	repository, err := plandoc.New(directory, plandoc.Limits{MaxBytes: 4})
	require.NoError(t, err)

	ref := plandoc.Ref{SessionID: "s-session", WorkspaceID: "workspace-1"}

	_, err = repository.Replace(t.Context(), ref, "", "12345")
	require.ErrorIs(t, err, plandoc.ErrInvalid)
	document, err := repository.Replace(t.Context(), ref, "", "1234")
	require.NoError(t, err)
	assert.Equal(t, int64(4), document.Size)

	directoryInfo, err := os.Stat(directory)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), directoryInfo.Mode().Perm())

	path, err := repository.Path(ref)
	require.NoError(t, err)
	fileInfo, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fileInfo.Mode().Perm())
}

func TestFileRepositoryForkMissingSourceIsNoop(t *testing.T) {
	t.Parallel()

	repository := newRepository(t)
	err := repository.Fork(t.Context(),
		plandoc.Ref{SessionID: "s-source", WorkspaceID: "workspace-1"},
		plandoc.Ref{SessionID: "s-target", WorkspaceID: "workspace-1"},
	)
	require.NoError(t, err)
	_, err = repository.Read(t.Context(), plandoc.Ref{
		SessionID: "s-target", WorkspaceID: "workspace-1",
	})
	assert.ErrorIs(t, err, plandoc.ErrNotFound)
}

func newRepository(t *testing.T) *plandoc.FileRepository {
	t.Helper()

	repository, err := plandoc.New(filepath.Join(t.TempDir(), "plans"), plandoc.DefaultLimits())
	require.NoError(t, err)

	return repository
}
