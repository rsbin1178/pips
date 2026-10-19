//go:build unix

//nolint:wsl_v5 // Listing fixtures keep setup and assertions adjacent.
package session_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cancelAfterCalls is a context whose Err turns cancelled after a fixed number of
// calls. ListMetadata checks its context once before the pass and once per
// Session, so a test can stop it deterministically mid-loop without a race.
type cancelAfterCalls struct {
	context.Context
	remaining int
}

func (c *cancelAfterCalls) Err() error {
	if c.remaining <= 0 {
		return context.Canceled
	}
	c.remaining--

	return nil
}

// TestRepositoryListMetadataSkipsBodiesAndCountsUnreadableSessions pins the
// statistics listing: it reads no session body, it keeps the picker's abandoned
// and kind filters, and a file it cannot trust lowers a counter instead of
// failing the whole listing the way the picker deliberately does.
func TestRepositoryListMetadataSkipsBodiesAndCountsUnreadableSessions(t *testing.T) {
	t.Parallel()

	directory := filepath.Join(t.TempDir(), "sessions")
	repository, err := session.NewRepository(directory)
	require.NoError(t, err)

	used, err := repository.Create(t.Context(), session.CreateOptions{
		WorkspaceID: "workspace-key", WorkspacePath: testWorkspacePath,
	})
	require.NoError(t, err)
	_, err = used.Session().AppendMessage(ai.UserText("hello"), nil)
	require.NoError(t, err)
	usedID := used.Metadata().ID
	usedPath := used.Metadata().Path
	require.NoError(t, used.Close())

	abandoned, err := repository.Create(t.Context(), session.CreateOptions{
		WorkspaceID: "workspace-key", WorkspacePath: testWorkspacePath,
	})
	require.NoError(t, err)
	require.NoError(t, abandoned.Close())

	broken, err := repository.Create(t.Context(), session.CreateOptions{
		WorkspaceID: "workspace-key", WorkspacePath: testWorkspacePath,
	})
	require.NoError(t, err)
	_, err = broken.Session().AppendMessage(ai.UserText("unreadable"), nil)
	require.NoError(t, err)
	brokenMeta := repository.Layout().MetadataFile(testWorkspacePath, broken.Metadata().ID)
	require.NoError(t, broken.Close())

	listing, err := repository.ListMetadata(t.Context())
	require.NoError(t, err)
	require.Len(t, listing.Sessions, 2, "the abandoned header is not a session")
	assert.Zero(t, listing.Unreadable)
	assert.False(t, listing.Truncated, "a complete pass is not truncated")
	ids := make([]string, 0, len(listing.Sessions))
	for _, meta := range listing.Sessions {
		ids = append(ids, meta.ID)
		assert.Empty(t, meta.Name, "no body is read, so no name is projected")
		assert.Zero(t, meta.BranchCount, "no body is read, so no branch count is projected")
		assert.Equal(t, 1, meta.NodeCount, "the entry count comes from the writer-maintained index")
	}
	assert.Contains(t, ids, usedID)

	// The picker reads the prefix and still reports both sessions.
	full, err := repository.List(t.Context())
	require.NoError(t, err)
	require.Len(t, full, 2)
	for _, meta := range full {
		if meta.ID == usedID {
			assert.Positive(t, meta.NodeCount, "the picker listing projects the prefix")
		}
	}

	// A session index the pass cannot trust lowers a counter, not the listing.
	require.NoError(t, os.WriteFile(brokenMeta, []byte("{"), 0o600))
	listing, err = repository.ListMetadata(t.Context())
	require.NoError(t, err)
	assert.Len(t, listing.Sessions, 1)
	assert.Equal(t, 1, listing.Unreadable)

	// A transcript the picker cannot trust still fails loudly.
	require.NoError(t, os.Chmod(usedPath, 0o644)) //nolint:gosec // Deliberately unsafe permissions exercise rejection.
	_, err = repository.List(t.Context())
	require.Error(t, err, "the picker keeps failing loudly")
}

// TestRepositoryListMetadataReturnsWhatItReadWhenCancelled pins the truncation
// contract: a cancelled pass returns the Sessions it read, marks itself
// truncated, and keeps the unreadable counter separate; a read that never started
// stays an error.
func TestRepositoryListMetadataReturnsWhatItReadWhenCancelled(t *testing.T) {
	t.Parallel()

	directory := filepath.Join(t.TempDir(), "sessions")
	repository, err := session.NewRepository(directory)
	require.NoError(t, err)

	create := func() string {
		t.Helper()
		handle, createErr := repository.Create(t.Context(), session.CreateOptions{
			WorkspaceID: "workspace-key", WorkspacePath: testWorkspacePath,
		})
		require.NoError(t, createErr)
		_, appendErr := handle.Session().AppendMessage(ai.UserText("hello"), nil)
		require.NoError(t, appendErr)
		id := handle.Metadata().ID
		require.NoError(t, handle.Close())

		return id
	}

	// The pass reads session directories in filename order, so sorting the ids
	// makes that order deterministic: the first is unreadable, the second is read,
	// and the pass stops before the third.
	ids := []string{create(), create(), create()}
	slices.Sort(ids)
	require.NoError(t, os.WriteFile(
		repository.Layout().MetadataFile(testWorkspacePath, ids[0]),
		[]byte("{"),
		0o600,
	))

	// The pre-call check plus three per-Session checks pass: the first id is
	// counted unreadable, the second is read, and the pass stops before the third.
	ctx := &cancelAfterCalls{Context: t.Context(), remaining: 3}
	listing, err := repository.ListMetadata(ctx)
	require.NoError(t, err)
	assert.True(t, listing.Truncated)
	require.Len(t, listing.Sessions, 1)
	assert.Equal(t, ids[1], listing.Sessions[0].ID)
	assert.Equal(t, 1, listing.Unreadable, "the unreadable counter keeps its own meaning")

	// A read that never started is an error, not a partial listing.
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = repository.ListMetadata(cancelled)
	require.ErrorIs(t, err, context.Canceled)
}
