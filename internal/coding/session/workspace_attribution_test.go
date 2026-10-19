package session_test

import (
	"path/filepath"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOpenAcceptsAStaleWorkspaceIdentityWhenThePathMatches is the regression test
// for a real report: a volume's device number changed, which changed every
// workspace identity without anything the user did, and every session created
// before the change became unlistable and unresumable.
//
// The canonical path is the ownership key, so a session stored under an earlier
// identity of the same path must still open. A different path must not.
func TestOpenAcceptsAStaleWorkspaceIdentityWhenThePathMatches(t *testing.T) {
	t.Parallel()

	repository, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)

	const (
		workspacePath    = "/workspace/attribution"
		storedID         = "identity-before-the-renumbering"
		currentID        = "identity-after-the-renumbering"
		otherWorkspace   = "/workspace/elsewhere"
		otherWorkspaceID = "identity-of-another-workspace"
	)

	handle, err := repository.Create(t.Context(), session.CreateOptions{
		WorkspaceID:   storedID,
		WorkspacePath: workspacePath,
	})
	require.NoError(t, err)
	_, err = handle.Session().AppendMessage(ai.UserText("remember me"), nil)
	require.NoError(t, err)

	id := handle.Metadata().ID
	require.NoError(t, handle.Close())

	// The same path under a different identity: this is what a device
	// renumbering looks like, and it must open.
	reopened, err := repository.Open(t.Context(), session.OpenOptions{
		ID:            id,
		WorkspaceID:   currentID,
		WorkspacePath: workspacePath,
	})
	require.NoError(t, err)
	assert.Equal(t, id, reopened.Metadata().ID)
	require.NoError(t, reopened.Close())

	// A path that does not match is still refused, even with the stored identity.
	_, err = repository.Open(t.Context(), session.OpenOptions{
		ID:            id,
		WorkspaceID:   otherWorkspaceID,
		WorkspacePath: otherWorkspace,
	})
	require.ErrorIs(t, err, session.ErrWorkspaceMismatch)

	// The stored identity alone keeps working, for callers that predate the path.
	byIdentity, err := repository.Open(t.Context(), session.OpenOptions{
		ID: id, WorkspaceID: storedID,
	})
	require.NoError(t, err)
	require.NoError(t, byIdentity.Close())

	// And the listing agrees with what opening accepts.
	metas, err := repository.List(t.Context())
	require.NoError(t, err)
	require.Len(t, metas, 1)
	assert.True(t, metas[0].BelongsToWorkspace(workspacePath, currentID))
	assert.False(t, metas[0].BelongsToWorkspace(otherWorkspace, otherWorkspaceID))
}
