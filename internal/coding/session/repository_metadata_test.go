//go:build unix

//nolint:wsl_v5 // Listing fixtures keep setup and assertions adjacent.
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
	brokenPath := broken.Metadata().Path
	require.NoError(t, broken.Close())

	listing, err := repository.ListMetadata(t.Context())
	require.NoError(t, err)
	require.Len(t, listing.Sessions, 2, "the abandoned header is not a session")
	assert.Zero(t, listing.Unreadable)
	ids := make([]string, 0, len(listing.Sessions))
	for _, meta := range listing.Sessions {
		ids = append(ids, meta.ID)
	}
	assert.Contains(t, ids, usedID)
	assert.Empty(t, listing.Sessions[1].Name, "no body is read, so no name is projected")
	assert.Zero(t, listing.Sessions[1].NodeCount, "transcript counts come from the prefix")

	// The picker reads that prefix and still reports both sessions.
	full, err := repository.List(t.Context())
	require.NoError(t, err)
	require.Len(t, full, 2)
	for _, meta := range full {
		if meta.ID == usedID {
			assert.Positive(t, meta.NodeCount, "the picker listing projects the prefix")
		}
	}

	// A file that fails the security check is counted, not fatal.
	require.NoError(t, os.Chmod(brokenPath, 0o644))
	listing, err = repository.ListMetadata(t.Context())
	require.NoError(t, err)
	assert.Len(t, listing.Sessions, 1)
	assert.Equal(t, 1, listing.Unreadable)

	_, err = repository.List(t.Context())
	require.Error(t, err, "the picker keeps failing loudly")
}
