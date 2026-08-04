//nolint:wsl_v5 // Security fixture setup remains adjacent to its assertions.
package execution

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrivateTempRootAllocatesCanonicalOwnerOnlyDirectory(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	root, err := NewPrivateTempRoot(base)
	require.NoError(t, err)
	path := root.Path()
	assert.Equal(t, path, filepath.Clean(path))
	info, err := os.Lstat(path)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Zero(t, info.Mode()&os.ModeSymlink)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	require.NoError(t, root.Close())
	require.NoError(t, root.Close())
	_, err = os.Lstat(path)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestPrivateTempRootOutsideRejectsExcludedAncestor(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	root, err := NewPrivateTempRootOutside(base, []string{base})
	require.ErrorIs(t, err, ErrInvalidOperation)
	assert.Nil(t, root)
}

func TestPrivateTempRootOutsideAcceptsSiblingOfExcludedRoot(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	excluded := filepath.Join(base, "product")
	root, err := NewPrivateTempRootOutside(base, []string{excluded})
	require.NoError(t, err)
	require.NotNil(t, root)
	t.Cleanup(func() { require.NoError(t, root.Close()) })

	assert.False(t, pathContains(excluded, root.Path()))
}

func TestPrivateTempRootRefusesSymlinkReplacementDuringCleanup(t *testing.T) {
	t.Parallel()

	root, err := NewPrivateTempRoot(t.TempDir())
	require.NoError(t, err)
	path := root.Path()
	moved := path + "-moved"
	require.NoError(t, os.Rename(path, moved))
	require.NoError(t, os.Symlink(moved, path))

	err = root.Close()
	require.ErrorIs(t, err, ErrInvalidOperation)
	_, err = os.Lstat(path)
	require.NoError(t, err)
	_, err = os.Stat(moved)
	require.NoError(t, err)
	assert.NoError(t, os.Remove(path))
	assert.NoError(t, os.RemoveAll(moved))
}

func TestPrivateTempRootRefusesReplacementDuringCleanup(t *testing.T) {
	t.Parallel()

	root, err := NewPrivateTempRoot(t.TempDir())
	require.NoError(t, err)
	path := root.Path()
	moved := path + "-moved"
	require.NoError(t, os.Rename(path, moved))
	require.NoError(t, os.Mkdir(path, 0o700))
	marker := filepath.Join(path, "replacement")
	require.NoError(t, os.WriteFile(marker, []byte("must remain"), 0o600))

	err = root.Close()
	require.ErrorIs(t, err, ErrInvalidOperation)
	_, err = os.Stat(marker)
	require.NoError(t, err)
	assert.NoError(t, os.RemoveAll(moved))
	assert.NoError(t, os.RemoveAll(path))
}
