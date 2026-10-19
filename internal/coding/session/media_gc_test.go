package session_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newGCMediaSession(t *testing.T) (*session.Repository, *session.Handle, *session.MediaStore) {
	t.Helper()

	repo, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)

	handle, err := repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID: "workspace-key", WorkspacePath: testWorkspacePath,
	})
	require.NoError(t, err)

	store, err := session.NewMediaStore(handle.Dir())
	require.NoError(t, err)

	return repo, handle, store
}

// TestPruneSessionMediaKeepsReferencedMediaEvenWhenOld pins the core deletion
// invariant: a file the transcript references is never removed, regardless of
// age or grace, and the same-base original travels with the referenced copy.
func TestPruneSessionMediaKeepsReferencedMediaEvenWhenOld(t *testing.T) {
	t.Parallel()

	_, handle, store := newGCMediaSession(t)

	original, err := store.PutOriginal(t.Context(), "image/png", []byte("referenced"))
	require.NoError(t, err)
	modelCopy, err := store.PutModelCopy(t.Context(), original, "image/jpeg", []byte("copy"))
	require.NoError(t, err)

	_, err = handle.Session().AppendMessage(ai.User(ai.ImagePart{Source: ai.MediaSource{
		Path: modelCopy.Path, Digest: modelCopy.Digest, MIMEType: modelCopy.MIMEType,
	}}), nil)
	require.NoError(t, err)

	sessionDir := handle.Dir()
	require.NoError(t, handle.Close())

	old := time.Now().Add(-365 * 24 * time.Hour)
	for _, rel := range []string{original.Path, modelCopy.Path} {
		require.NoError(t, os.Chtimes(filepath.Join(sessionDir, filepath.FromSlash(rel)), old, old))
	}

	report, err := session.PruneSessionMedia(t.Context(), sessionDir, 0, false)
	require.NoError(t, err)
	assert.Empty(t, report.Deleted)

	for _, rel := range []string{original.Path, modelCopy.Path} {
		_, statErr := os.Stat(filepath.Join(sessionDir, filepath.FromSlash(rel)))
		require.NoError(t, statErr)
	}
}

// TestPruneSessionMediaKeepsMediaReferencedOnlyByArchive proves an archive
// counts as a reference: media a transcript no longer names stays reachable
// because the archived branch still names it.
func TestPruneSessionMediaKeepsMediaReferencedOnlyByArchive(t *testing.T) {
	t.Parallel()

	repo, handle, store := newGCMediaSession(t)

	original, err := store.PutOriginal(t.Context(), "image/png", []byte("archived"))
	require.NoError(t, err)
	modelCopy, err := store.PutModelCopy(t.Context(), original, "image/jpeg", []byte("archived-copy"))
	require.NoError(t, err)

	archives, err := repo.Archives(handle)
	require.NoError(t, err)

	entry := harness.Entry{
		Kind: harness.KindMessage, ID: "archived-user", Time: time.Unix(100, 0).UTC(),
		Message: ai.User(ai.ImagePart{Source: ai.MediaSource{
			Path: modelCopy.Path, Digest: modelCopy.Digest, MIMEType: modelCopy.MIMEType,
		}}),
	}
	staged, err := archives.Stage(t.Context(), session.ArchiveSource{
		SessionID: handle.Metadata().ID, TipID: entry.ID,
	}, []harness.Entry{entry})
	require.NoError(t, err)
	require.NoError(t, staged.Publish(t.Context()))
	require.NoError(t, staged.Close())

	sessionDir := handle.Dir()
	require.NoError(t, handle.Close())

	report, err := session.PruneSessionMedia(t.Context(), sessionDir, 0, false)
	require.NoError(t, err)
	assert.Empty(t, report.Deleted)

	for _, rel := range []string{original.Path, modelCopy.Path} {
		_, statErr := os.Stat(filepath.Join(sessionDir, filepath.FromSlash(rel)))
		require.NoError(t, statErr)
	}
}

// TestPruneSessionMediaAbortsOnUnreadableReference proves GC never deletes
// media when the reference set cannot be read: a corrupt transcript aborts the
// whole pass before any removal.
func TestPruneSessionMediaAbortsOnUnreadableReference(t *testing.T) {
	t.Parallel()

	_, handle, store := newGCMediaSession(t)

	// Materialize the session so its directory survives Close.
	_, err := handle.Session().AppendMessage(ai.UserText("keep"), nil)
	require.NoError(t, err)

	orphan, err := store.PutOriginal(t.Context(), "image/png", []byte("orphan"))
	require.NoError(t, err)

	sessionDir := handle.Dir()
	require.NoError(t, handle.Close())

	transcript := filepath.Join(sessionDir, session.TranscriptFileName)
	require.NoError(t, os.WriteFile(transcript, []byte("not a jsonl session\n"), 0o600))

	_, err = session.PruneSessionMedia(t.Context(), sessionDir, 0, false)
	require.Error(t, err)

	_, statErr := os.Stat(filepath.Join(sessionDir, filepath.FromSlash(orphan.Path)))
	require.NoError(t, statErr, "an unreadable reference must abort before any deletion")
}

// TestPruneMediaSkipsLockedSession proves orphan collection never runs under a
// live writer: a locked session reports ErrLocked and its media is untouched.
func TestPruneMediaSkipsLockedSession(t *testing.T) {
	t.Parallel()

	repo, handle, store := newGCMediaSession(t)
	defer func() { require.NoError(t, handle.Close()) }()

	// Materialize the session so it is discoverable while still open.
	_, err := handle.Session().AppendMessage(ai.UserText("keep"), nil)
	require.NoError(t, err)

	orphan, err := store.PutOriginal(t.Context(), "image/png", []byte("orphan"))
	require.NoError(t, err)

	// handle still owns the session writer lock.
	_, err = repo.PruneMedia(t.Context(), handle.Metadata().ID, 0, false)
	require.ErrorIs(t, err, session.ErrLocked)

	_, statErr := os.Stat(filepath.Join(handle.Dir(), filepath.FromSlash(orphan.Path)))
	require.NoError(t, statErr)
}
