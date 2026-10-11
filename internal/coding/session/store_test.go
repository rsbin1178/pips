package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStoreRootConstructionIsPure pins the split from StoreLayout: building the
// store value must not read or create the tree.
func TestStoreRootConstructionIsPure(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "not-created", "sessions")

	store, err := NewStoreRoot(missing)
	require.NoError(t, err)
	assert.Equal(t, filepath.Clean(missing), store.Root())
	assert.Equal(t, store.Root(), store.Layout().Root())

	_, statErr := os.Lstat(missing)
	require.ErrorIs(t, statErr, os.ErrNotExist, "constructing a store creates nothing")
}

func TestStoreRootGroupsOnMissingRoot(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "not-created")

	store, err := NewStoreRoot(missing)
	require.NoError(t, err)

	groups, err := store.Groups()
	require.NoError(t, err)
	assert.Empty(t, groups)

	_, statErr := os.Lstat(missing)
	require.ErrorIs(t, statErr, os.ErrNotExist, "listing a missing store creates nothing")
}

func TestStoreRootEnsureGroup(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := NewStoreRoot(root)
	require.NoError(t, err)

	const workspace = "/workspace/app"

	group, err := store.EnsureGroup(workspace)
	require.NoError(t, err)

	key, _, err := store.Layout().GroupKey(workspace)
	require.NoError(t, err)
	assert.Equal(t, key, group.Key)
	assert.Equal(t, filepath.Join(root, key), group.Dir)
	assert.Equal(t, workspace, group.Path)

	info, err := os.Stat(group.Dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	recordPath := filepath.Join(group.Dir, WorkspaceFileName)
	recordInfo, err := os.Stat(recordPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), recordInfo.Mode().Perm())

	// Idempotent, and the stored path survives a later call that no longer knows it.
	again, err := store.EnsureGroup(workspace)
	require.NoError(t, err)
	assert.Equal(t, group, again)

	anonymous, err := store.EnsureGroup("  ")
	require.NoError(t, err)
	assert.Equal(t, UnknownWorkspaceKey, anonymous.Key)
	assert.Empty(t, anonymous.Path)

	// The unknown group does not steal the known group's path.
	known, err := store.EnsureGroup(workspace)
	require.NoError(t, err)
	assert.Equal(t, workspace, known.Path)
}

func TestStoreRootGroupsReadsPathsAndSkipsForeignEntries(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := NewStoreRoot(root)
	require.NoError(t, err)

	first, err := store.EnsureGroup("/workspace/first")
	require.NoError(t, err)
	second, err := store.EnsureGroup("/workspace/second")
	require.NoError(t, err)

	// A regular file and a symlinked directory are not groups.
	require.NoError(t, os.WriteFile(filepath.Join(root, "stray.txt"), []byte("x"), 0o600))
	require.NoError(t, os.Symlink(first.Dir, filepath.Join(root, "linked")))

	groups, err := store.Groups()
	require.NoError(t, err)
	require.Len(t, groups, 2)

	byKey := map[string]Group{}
	for _, group := range groups {
		byKey[group.Key] = group
	}

	assert.Equal(t, "/workspace/first", byKey[first.Key].Path)
	assert.Equal(t, "/workspace/second", byKey[second.Key].Path)
}

func TestStoreRootFindSession(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := NewStoreRoot(root)
	require.NoError(t, err)

	const (
		workspace = "/workspace/find"
		id        = "s-find"
	)

	// A group without the session, then the group that holds it.
	_, err = store.EnsureGroup("/workspace/other")
	require.NoError(t, err)

	group, err := store.EnsureGroup(workspace)
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(group.SessionDirOf(id), 0o700))
	record := NewMetadataRecord(id, WorkspaceRef{ID: "ws-id", Path: workspace}, KindConversation, time.Now())
	require.NoError(t, WriteMetadataRecord(store.Layout().MetadataFile(workspace, id), record))

	found, ok, err := store.FindSession(id)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, group.Dir, found.Dir)
	assert.Equal(t, workspace, found.Path)

	_, ok, err = store.FindSession("s-absent")
	require.NoError(t, err)
	assert.False(t, ok)

	_, _, err = store.FindSession("../escape")
	require.ErrorIs(t, err, ErrInvalid)

	// A directory without an index record is not a stored session.
	require.NoError(t, os.MkdirAll(group.SessionDirOf("s-empty"), 0o700))

	_, ok, err = store.FindSession("s-empty")
	require.NoError(t, err)
	assert.False(t, ok)
}
