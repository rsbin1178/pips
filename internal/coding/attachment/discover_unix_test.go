//go:build unix

package attachment

import (
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverSkipsSpecialFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFixture(t, root, "regular.txt", "included")
	require.NoError(t, syscall.Mkfifo(filepath.Join(root, "named-pipe"), 0o600))
	tree := openAttachmentTree(t, root)

	snapshot, err := Discover(t.Context(), tree)
	require.NoError(t, err)
	require.Len(t, snapshot.Files, 1)
	assert.Equal(t, "regular.txt", snapshot.Files[0].Path)
}
