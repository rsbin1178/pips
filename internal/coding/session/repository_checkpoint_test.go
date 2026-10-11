//go:build darwin || linux

//nolint:wsl_v5 // Repository transaction tests group durable actions and assertions.
package session

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func repositoryCheckpointSource(t *testing.T) (*Repository, *Handle) {
	t.Helper()
	repo, err := NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)
	source := repositoryCheckpointHandle(t, repo)
	return repo, source
}

func repositoryCheckpointHandle(t *testing.T, repo *Repository) *Handle {
	t.Helper()
	handle, err := repo.Create(t.Context(), CreateOptions{WorkspaceID: "workspace", WorkspacePath: "/workspace"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, handle.Close()) })
	_, err = handle.Session().AppendMessage(ai.UserText("original request and evidence"), nil)
	require.NoError(t, err)
	return handle
}

func publishRepositoryCheckpoint(t *testing.T, repo *Repository, handle *Handle, text string) (string, string) {
	t.Helper()
	tip, err := handle.Session().AppendMessage(ai.AssistantText(text), nil)
	require.NoError(t, err)
	archives, err := repo.Archives(handle)
	require.NoError(t, err)
	staged, err := archives.Stage(t.Context(), ArchiveSource{SessionID: handle.Metadata().ID, TipID: tip}, handle.Session().Path())
	require.NoError(t, err)
	require.NoError(t, staged.Publish(t.Context()))
	archiveID := staged.ID()
	require.NoError(t, staged.Close())
	checkpoint, err := handle.Session().AppendContextCheckpoint(tip, harness.ContextCheckpoint{
		Version: harness.ContextCheckpointVersion, ArchiveID: archiveID,
		Messages: ai.Messages{ai.UserText("Summary: " + text)},
	}, 1000)
	require.NoError(t, err)
	return archiveID, checkpoint
}

// repositoryArchiveDir is the content-addressed archive directory of one
// session in the fixture workspace.
func repositoryArchiveDir(repo *Repository, id string) string {
	return repo.Layout().ArchiveDir("/workspace", id)
}

func TestRepositoryCheckpointForkSurvivesSourceDelete(t *testing.T) {
	t.Parallel()
	repo, source := repositoryCheckpointSource(t)
	first, _ := publishRepositoryCheckpoint(t, repo, source, "first archived evidence")
	second, selected := publishRepositoryCheckpoint(t, repo, source, "second archived evidence")
	unselected, _ := publishRepositoryCheckpoint(t, repo, source, "later unselected evidence")
	before := source.Session().Entries()

	fork, err := repo.Fork(t.Context(), source, ForkOptions{AtEntryID: selected})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, fork.Close()) })
	assert.Equal(t, selected, fork.Metadata().ParentEntryID)
	assert.Equal(t, selected, fork.Session().LeafID())
	assert.Equal(t, before, source.Session().Entries())
	_, err = repo.Open(t.Context(), OpenOptions{ID: fork.Metadata().ID, WorkspaceID: "workspace"})
	require.ErrorIs(t, err, ErrLocked)
	archives, err := repo.Archives(fork)
	require.NoError(t, err)
	_, err = archives.Verify(t.Context(), unselected)
	require.ErrorIs(t, err, ErrArchiveNotFound, "only the selected path's references are copied")
	for _, id := range []string{first, second} {
		original, err := os.Stat(filepath.Join(repositoryArchiveDir(repo, source.Metadata().ID), id+".archive"))
		require.NoError(t, err)
		copied, err := os.Stat(filepath.Join(repositoryArchiveDir(repo, fork.Metadata().ID), id+".archive"))
		require.NoError(t, err)
		assert.False(t, os.SameFile(original, copied), "the fork owns independent archive files")
	}

	sourceID := source.Metadata().ID
	require.NoError(t, source.Close())
	require.NoError(t, repo.Delete(t.Context(), sourceID))
	require.NoFileExists(t, source.Metadata().Path)
	require.NoDirExists(t, repositoryArchiveDir(repo, sourceID))
	forkID := fork.Metadata().ID
	require.NoError(t, fork.Close())
	reopened, err := repo.Open(t.Context(), OpenOptions{ID: forkID, WorkspaceID: "workspace"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	archives, err = repo.Archives(reopened)
	require.NoError(t, err)
	reader, err := archives.Bind([]string{first, second})
	require.NoError(t, err)
	for _, id := range []string{first, second} {
		info, err := archives.Verify(t.Context(), id)
		require.NoError(t, err)
		assert.Equal(t, sourceID, info.Source.SessionID, "source identity remains inert provenance")
		segments, err := reader.List(t.Context(), ArchiveListRequest{ArchiveID: id})
		require.NoError(t, err)
		require.NotEmpty(t, segments.Segments)
		page, err := reader.Read(t.Context(), ArchiveReadRequest{ArchiveID: id, SegmentID: segments.Segments[0].ID})
		require.NoError(t, err)
		var text strings.Builder
		for _, line := range page.Lines {
			text.WriteString(line.Text)
			text.WriteByte('\n')
		}
		assert.Contains(t, text.String(), "original request and evidence")
	}
	require.NoError(t, reopened.Close())
	require.NoError(t, repo.Delete(t.Context(), forkID))
	require.NoDirExists(t, repositoryArchiveDir(repo, forkID))
}

func TestRepositoryCheckpointForkFailureDoesNotPublishTranscript(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"missing", "corrupt"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			repo, source := repositoryCheckpointSource(t)
			first, _ := publishRepositoryCheckpoint(t, repo, source, "first archive")
			second, _ := publishRepositoryCheckpoint(t, repo, source, "second archive")
			ids := []string{first, second}
			slices.Sort(ids)
			badPath := filepath.Join(repositoryArchiveDir(repo, source.Metadata().ID), ids[1]+".archive")
			if failure == "missing" {
				require.NoError(t, os.Remove(badPath))
			} else {
				require.NoError(t, os.WriteFile(badPath, []byte("corrupt archive"), 0o600))
			}
			before := source.Session().Entries()
			fork, err := repo.Fork(t.Context(), source, ForkOptions{})
			require.Error(t, err)
			assert.Nil(t, fork)
			assert.Equal(t, before, source.Session().Entries())
			// Only the source published a transcript; the failed fork left none.
			transcripts, err := filepath.Glob(filepath.Join(repo.Layout().GroupDir("/workspace"), "*", TranscriptFileName))
			require.NoError(t, err)
			assert.Equal(t, []string{source.Metadata().Path}, transcripts)
			stages, err := filepath.Glob(filepath.Join(repo.Layout().GroupDir("/workspace"), "*", ".fork-*.tmp"))
			require.NoError(t, err)
			assert.Empty(t, stages)

			// The first archive was already durably published before copying the
			// second failed. Fork errors never trigger broad archive rollback.
			archiveDirs, err := filepath.Glob(filepath.Join(repo.Layout().GroupDir("/workspace"), "*", ArchiveDirName))
			require.NoError(t, err)
			require.Len(t, archiveDirs, 2)
			for _, archiveDir := range archiveDirs {
				if archiveDir == repositoryArchiveDir(repo, source.Metadata().ID) {
					continue
				}
				require.FileExists(t, filepath.Join(archiveDir, ids[0]+".archive"))
				targetID := filepath.Base(filepath.Dir(archiveDir))
				_, err = repo.Open(t.Context(), OpenOptions{ID: targetID, WorkspaceID: "workspace"})
				require.ErrorIs(t, err, os.ErrNotExist, "failed fork releases the destination writer lock")
				require.NoError(t, repo.Delete(t.Context(), targetID))
				require.NoDirExists(t, archiveDir, "deleting the session removes its whole directory including orphan archives")
			}
		})
	}
}

func TestRepositoryCheckpointDeleteRetainsArchivesOnUncertainSync(t *testing.T) {
	t.Parallel()
	repo, source := repositoryCheckpointSource(t)
	id, _ := publishRepositoryCheckpoint(t, repo, source, "retained on uncertain deletion")
	require.NoError(t, source.Close())
	archivePath := filepath.Join(repositoryArchiveDir(repo, source.Metadata().ID), id+".archive")
	syncErr := errors.New("injected directory sync failure")
	err := deleteSessionDirectoryAt(t.Context(), repo.Layout().GroupDir("/workspace"), source.Metadata().ID, func(*os.Root) error {
		return syncErr
	})
	require.ErrorIs(t, err, syncErr)
	require.ErrorContains(t, err, "durability uncertain")
	// The failed sync renames the session back, leaving it fully intact.
	require.FileExists(t, archivePath)
	require.FileExists(t, source.Metadata().Path)
	require.FileExists(t, repo.Layout().MetadataFile("/workspace", source.Metadata().ID))
	// A later, durable delete removes the whole session directory.
	require.NoError(t, repo.Delete(t.Context(), source.Metadata().ID))
	require.NoFileExists(t, archivePath)
}

func TestRepositoryCheckpointDeleteRemovesArchiveSymlinkWithoutFollowing(t *testing.T) {
	t.Parallel()
	repo, source := repositoryCheckpointSource(t)
	_, _ = publishRepositoryCheckpoint(t, repo, source, "source evidence")
	other := repositoryCheckpointHandle(t, repo)
	otherArchive, _ := publishRepositoryCheckpoint(t, repo, other, "other session evidence")
	require.NoError(t, source.Close())
	// A symlinked archives directory is removed as a link, never followed.
	require.NoError(t, os.RemoveAll(repositoryArchiveDir(repo, source.Metadata().ID)))
	require.NoError(t, os.Symlink(repositoryArchiveDir(repo, other.Metadata().ID), repositoryArchiveDir(repo, source.Metadata().ID)))
	require.NoError(t, repo.Delete(t.Context(), source.Metadata().ID))
	require.NoFileExists(t, source.Metadata().Path)
	require.NoDirExists(t, repositoryArchiveDir(repo, source.Metadata().ID))
	require.FileExists(t, filepath.Join(repositoryArchiveDir(repo, other.Metadata().ID), otherArchive+".archive"))
	archives, err := repo.Archives(other)
	require.NoError(t, err)
	_, err = archives.Verify(t.Context(), otherArchive)
	require.NoError(t, err)
}

func TestRepositoryCheckpointDeleteDoesNotFollowArchiveContents(t *testing.T) {
	t.Parallel()
	repo, source := repositoryCheckpointSource(t)
	_, _ = publishRepositoryCheckpoint(t, repo, source, "deletable evidence")
	other := repositoryCheckpointHandle(t, repo)
	otherArchive, _ := publishRepositoryCheckpoint(t, repo, other, "must remain")
	require.NoError(t, os.Symlink(repositoryArchiveDir(repo, other.Metadata().ID), filepath.Join(repositoryArchiveDir(repo, source.Metadata().ID), "foreign")))
	require.NoError(t, source.Close())
	require.NoError(t, repo.Delete(t.Context(), source.Metadata().ID))
	require.NoDirExists(t, repositoryArchiveDir(repo, source.Metadata().ID))
	archives, err := repo.Archives(other)
	require.NoError(t, err)
	_, err = archives.Verify(t.Context(), otherArchive)
	require.NoError(t, err)
}

func TestRepositoryCheckpointCorruptTranscriptStaysFailClosed(t *testing.T) {
	t.Parallel()
	repo, source := repositoryCheckpointSource(t)
	id, _ := publishRepositoryCheckpoint(t, repo, source, "preserve evidence")
	require.NoError(t, source.Close())
	raw, err := os.ReadFile(source.Metadata().Path)
	require.NoError(t, err)
	corrupt := append(slices.Clone(raw), []byte(`{"kind":"context_checkpoint"`)...)
	require.NoError(t, os.WriteFile(source.Metadata().Path, corrupt, 0o600)) //nolint:gosec // The path belongs to this test's temporary repository.
	_, err = repo.Open(t.Context(), OpenOptions{ID: source.Metadata().ID, WorkspaceID: "workspace"})
	require.Error(t, err)
	after, err := os.ReadFile(source.Metadata().Path)
	require.NoError(t, err)
	assert.Equal(t, corrupt, after)
	require.FileExists(t, filepath.Join(repositoryArchiveDir(repo, source.Metadata().ID), id+".archive"))
}

func TestRepositoryLegacyForkDeleteDoesNotCreateHistory(t *testing.T) {
	t.Parallel()
	repo, source := repositoryCheckpointSource(t)
	fork, err := repo.Fork(t.Context(), source, ForkOptions{})
	require.NoError(t, err)
	require.NoDirExists(t, repositoryArchiveDir(repo, fork.Metadata().ID))
	require.NoError(t, fork.Close())
	require.NoError(t, repo.Delete(t.Context(), fork.Metadata().ID))
	require.NoError(t, repo.Delete(t.Context(), fork.Metadata().ID))
	require.NoError(t, source.Close())
	require.NoError(t, repo.Delete(t.Context(), source.Metadata().ID))
	histories, err := filepath.Glob(filepath.Join(repo.Dir(), "*.history"))
	require.NoError(t, err)
	assert.Empty(t, histories)
}
