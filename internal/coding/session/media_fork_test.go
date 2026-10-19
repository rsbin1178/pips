package session_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRepositoryForkCopiesReferencedMedia pins the fork contract: the copied
// transcript's references stay readable in the destination, and the same-base
// original comes with the model copy.
func TestRepositoryForkCopiesReferencedMedia(t *testing.T) {
	t.Parallel()

	repo, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)

	source, err := repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID: "workspace-key", WorkspacePath: testWorkspacePath,
	})
	require.NoError(t, err)

	store, err := session.NewMediaStore(source.Dir())
	require.NoError(t, err)

	original, err := store.PutOriginal(t.Context(), "image/png", []byte("original-bytes"))
	require.NoError(t, err)

	modelCopy, err := store.PutModelCopy(t.Context(), original, "image/jpeg", []byte("model-copy-bytes"))
	require.NoError(t, err)

	_, err = source.Session().AppendMessage(ai.User(ai.ImagePart{Source: ai.MediaSource{
		Path: modelCopy.Path, Digest: modelCopy.Digest, MIMEType: modelCopy.MIMEType,
	}}), nil)
	require.NoError(t, err)

	forked, err := repo.Fork(t.Context(), source, session.ForkOptions{})
	require.NoError(t, err)

	destination, err := session.NewMediaStore(forked.Dir())
	require.NoError(t, err)

	copied, err := destination.Open(t.Context(), modelCopy)
	require.NoError(t, err)
	assert.Equal(t, []byte("model-copy-bytes"), copied)

	originalRef, err := session.MediaRefFromSource(original.Path, original.Digest, original.MIMEType, 0, 0)
	require.NoError(t, err)

	copiedOriginal, err := destination.Open(t.Context(), originalRef)
	require.NoError(t, err)
	assert.Equal(t, []byte("original-bytes"), copiedOriginal)

	require.NoError(t, forked.Close())
	require.NoError(t, source.Close())
}

// TestRepositoryForkFailsOnMissingReferencedMedia proves the destination is
// never published with a dangling reference.
func TestRepositoryForkFailsOnMissingReferencedMedia(t *testing.T) {
	t.Parallel()

	repo, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)

	source, err := repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID: "workspace-key", WorkspacePath: testWorkspacePath,
	})
	require.NoError(t, err)

	_, err = source.Session().AppendMessage(ai.User(ai.ImagePart{Source: ai.MediaSource{
		Path:   "assets/image-0123456789abcdef.png",
		Digest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}}), nil)
	require.NoError(t, err)

	_, err = repo.Fork(t.Context(), source, session.ForkOptions{})
	require.ErrorIs(t, err, session.ErrMediaMissing)

	require.NoError(t, source.Close())
}

// TestRepositoryDeleteRemovesSessionMedia confirms media lives inside the
// session directory and disappears with it.
func TestRepositoryDeleteRemovesSessionMedia(t *testing.T) {
	t.Parallel()

	repo, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)

	source, err := repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID: "workspace-key", WorkspacePath: testWorkspacePath,
	})
	require.NoError(t, err)

	_, err = source.Session().AppendMessage(ai.UserText("keep this"), nil)
	require.NoError(t, err)

	store, err := session.NewMediaStore(source.Dir())
	require.NoError(t, err)
	_, err = store.PutOriginal(t.Context(), "image/png", []byte("bytes"))
	require.NoError(t, err)

	sessionDir := source.Dir()
	require.NoError(t, source.Close())

	require.NoError(t, repo.Delete(t.Context(), filepath.Base(sessionDir)))

	_, statErr := os.Stat(sessionDir)
	require.ErrorIs(t, statErr, os.ErrNotExist)
}
