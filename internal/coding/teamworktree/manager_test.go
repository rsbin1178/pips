package teamworktree

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin/pips/agent/team"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManagerLeaseIsExclusiveAndRetained(t *testing.T) {
	t.Parallel()

	manager := testManager(t)
	first, err := manager.Acquire(t.Context(), team.ID("team-a"), 1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = first.Close() })

	_, err = manager.Acquire(t.Context(), team.ID("team-a"), 2)
	require.ErrorIs(t, err, ErrLeaseHeld)
	require.NoError(t, first.Close())

	second, err := manager.Acquire(t.Context(), team.ID("team-a"), 2)
	require.NoError(t, err)
	require.NoError(t, second.Close())
}

func TestManagerRejectsReplacedLeasePath(t *testing.T) {
	t.Parallel()

	manager := testManager(t)
	owner := Owner{
		TeamID: "team-a", MemberID: "member-a", AttemptID: "attempt-a", LeaseGeneration: 1,
	}
	lease, err := manager.Acquire(t.Context(), owner.TeamID, owner.LeaseGeneration)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lease.Close() })
	require.NoError(t, os.Rename(lease.path.Path, lease.path.Path+".replaced"))
	require.NoError(t, os.WriteFile(lease.path.Path, []byte("replacement\n"), 0o600))

	err = manager.validateLease(lease, owner)
	require.ErrorIs(t, err, ErrLeaseLost)
}

func TestManagerDerivesStableOwnedNames(t *testing.T) {
	t.Parallel()

	manager := testManager(t)
	owner := Owner{
		TeamID: "team-a", MemberID: "member-a", AttemptID: "attempt-a", LeaseGeneration: 1,
	}
	workspace := FileIdentity{Path: "/workspace", Device: 1, Inode: 2}
	first := manager.derive(owner, workspace)
	second := manager.derive(owner, workspace)
	assert.Equal(t, first, second)
	assert.Contains(t, first.branchRef, "refs/heads/pips/team/")
	assert.Contains(t, first.resultRef, "refs/pips/team/")
	assert.True(t, pathWithin(first.directory, manager.worktreesRoot.Path))
}

func TestManagerRejectsOverlappingRoots(t *testing.T) {
	t.Parallel()

	gitPath := "/usr/bin/git"
	if _, err := os.Stat(gitPath); err != nil {
		t.Skip("system Git is unavailable")
	}

	root := t.TempDir()
	product := filepath.Join(root, "product")
	require.NoError(t, os.Mkdir(product, 0o700))
	_, err := New(Options{
		GitPath: gitPath, ProductRoot: product,
		WorktreesRoot: filepath.Join(product, "worktrees"),
		LeasesRoot:    filepath.Join(product, "leases"), Limits: DefaultLimits(),
	})
	require.ErrorIs(t, err, ErrInvalid)
}

func TestValidateOwnerAndLimits(t *testing.T) {
	t.Parallel()

	require.ErrorIs(t, validateOwner(Owner{}), ErrInvalid)
	require.ErrorIs(t, validateLimits(Limits{}), ErrInvalid)
	require.NoError(t, validateLimits(DefaultLimits()))
	assert.NotErrorIs(t, ErrDirty, ErrRetained)
}

func testManager(t *testing.T) *Manager {
	t.Helper()

	gitPath := "/usr/bin/git"
	if _, err := os.Stat(gitPath); err != nil {
		t.Skip("system Git is unavailable")
	}

	root := t.TempDir()
	product := filepath.Join(root, "product")
	require.NoError(t, os.Mkdir(product, 0o700))
	manager, err := New(Options{
		GitPath: gitPath, ProductRoot: product,
		WorktreesRoot: filepath.Join(root, "worktrees"),
		LeasesRoot:    filepath.Join(product, "teams", "leases"),
		Limits:        DefaultLimits(),
	})
	require.NoError(t, err)

	return manager
}
