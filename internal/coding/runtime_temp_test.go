package coding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//nolint:wsl_v5 // Test cleanup registration stays next to the owned root.
func TestNewRuntimeScratchRootIsOutsideProductRoot(t *testing.T) {
	t.Parallel()

	productRoot := filepath.Join(t.TempDir(), ".pips")
	root, err := newRuntimeScratchRoot(productRoot)
	require.NoError(t, err)
	path := root.Path()
	t.Cleanup(func() { _ = root.Close() })

	relative, err := filepath.Rel(productRoot, path)
	require.NoError(t, err)
	assert.True(t, relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)))
	assert.NotContains(t, path, filepath.Join(productRoot, "tmp"))
}

//nolint:wsl_v5 // Replacement setup stays adjacent to the cleanup assertion.
func TestNewRuntimeScratchRootCleanupRefusesProductReplacement(t *testing.T) {
	t.Parallel()

	root, err := newRuntimeScratchRoot(filepath.Join(t.TempDir(), ".pips"))
	require.NoError(t, err)
	path := root.Path()
	moved := path + "-moved"
	require.NoError(t, os.Rename(path, moved))
	require.NoError(t, os.Mkdir(path, 0o700))

	err = root.Close()
	require.Error(t, err)
	assert.DirExists(t, path)
	assert.NoError(t, os.RemoveAll(moved))
	assert.NoError(t, os.RemoveAll(path))
}
