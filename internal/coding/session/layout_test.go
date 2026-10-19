package session

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreLayoutRejectsUnusableRoots(t *testing.T) {
	t.Parallel()

	for name, root := range map[string]string{
		"empty":      "",
		"whitespace": "   ",
		"NUL":        "/tmp/a\x00b",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := NewStoreLayout(root)
			require.ErrorIs(t, err, ErrLayout)
		})
	}
}

// TestStoreLayoutIsPure pins the split between the path scheme and the store on
// disk: building a layout must never read or create anything.
func TestStoreLayoutIsPure(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "not-created", "sessions")

	layout, err := NewStoreLayout(missing)
	require.NoError(t, err)

	// Every accessor is a pure join, so calling them changes nothing either.
	_ = layout.SessionDir("/workspace", "s-1")
	_ = layout.MetadataFile("/workspace", "s-1")
	_ = layout.ArchiveDir("/workspace", "s-1")
	_ = layout.SessionRelDir("/workspace", "s-1")

	_, statErr := os.Lstat(missing)
	require.ErrorIs(t, statErr, os.ErrNotExist, "a layout never creates its root")
}

func TestStoreLayoutGroupKey(t *testing.T) {
	t.Parallel()

	layout, err := NewStoreLayout(t.TempDir())
	require.NoError(t, err)

	t.Run("escapes the absolute path", func(t *testing.T) {
		t.Parallel()

		key, fallback, err := layout.GroupKey("/Users/me/app")
		require.NoError(t, err)
		assert.False(t, fallback)
		assert.Equal(t, url.PathEscape("/Users/me/app"), key)
		assert.NotContains(t, key, "/", "a group key is one filesystem name")
	})

	t.Run("cleans before escaping", func(t *testing.T) {
		t.Parallel()

		key, _, err := layout.GroupKey("/Users/me/app/")
		require.NoError(t, err)
		assert.Equal(t, url.PathEscape("/Users/me/app"), key)
	})

	t.Run("empty path groups under the unknown key", func(t *testing.T) {
		t.Parallel()

		key, fallback, err := layout.GroupKey("  ")
		require.NoError(t, err)
		assert.Equal(t, UnknownWorkspaceKey, key)
		assert.True(t, fallback)
	})

	t.Run("rejects NUL", func(t *testing.T) {
		t.Parallel()

		_, _, err := layout.GroupKey("/tmp/a\x00b")
		require.ErrorIs(t, err, ErrLayout)
	})

	t.Run("falls back to a bounded digest for a long path", func(t *testing.T) {
		t.Parallel()

		long := "/" + strings.Repeat("a", 300)

		key, fallback, err := layout.GroupKey(long)
		require.NoError(t, err)
		assert.True(t, fallback)
		require.LessOrEqual(t, len(key), maxWorkspaceKeyBytes)

		sum := sha256.Sum256([]byte(long))
		assert.Equal(t, "ws-"+hex.EncodeToString(sum[:])[:workspaceFallbackBytes], key)

		// The fallback is deterministic, so a later run resolves the same group.
		again, _, err := layout.GroupKey(long)
		require.NoError(t, err)
		assert.Equal(t, key, again)
	})
}

func TestStoreLayoutAccessors(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	layout, err := NewStoreLayout(root)
	require.NoError(t, err)

	const (
		workspace = "/workspace"
		id        = "s-abc"
	)

	groupDir := filepath.Join(root, url.PathEscape(workspace))
	sessionDir := filepath.Join(groupDir, id)

	assert.Equal(t, filepath.Clean(root), layout.Root())
	assert.Equal(t, groupDir, layout.GroupDir(workspace))
	assert.Equal(t, sessionDir, layout.SessionDir(workspace, id))

	for name, path := range map[string]string{
		MetadataFileName:   layout.MetadataFile(workspace, id),
		TranscriptFileName: layout.TranscriptFile(workspace, id),
		JournalFileName:    layout.JournalFile(workspace, id),
		SearchFileName:     layout.SearchFile(workspace, id),
		LockFileName:       layout.LockFile(workspace, id),
	} {
		assert.Equal(t, filepath.Join(sessionDir, name), path)
	}

	for name, dir := range map[string]string{
		ModelDirName:      layout.ModelDir(workspace, id),
		StateDirName:      layout.StateDir(workspace, id),
		ArchiveDirName:    layout.ArchiveDir(workspace, id),
		CompactionDirName: layout.CompactionDir(workspace, id),
		SubagentsDirName:  layout.SubagentsDir(workspace, id),
		TerminalDirName:   layout.TerminalDir(workspace, id),
	} {
		assert.Equal(t, filepath.Join(sessionDir, name), dir)
	}

	// The parent-side child index lives inside the parent's own session
	// directory: one marker beside one directory per indexed child.
	indexDir := filepath.Join(sessionDir, SubagentsDirName)
	assert.Equal(t, filepath.Join(indexDir, SubagentIndexFileName), layout.SubagentIndexFile(workspace, id))
	assert.Equal(t, filepath.Join(indexDir, "s-child"), layout.ChildIndexEntryDir(workspace, id, "s-child"))
	assert.Equal(t,
		filepath.Join(indexDir, "s-child", ChildIndexEntryFileName),
		layout.ChildIndexEntryFile(workspace, id, "s-child"),
	)

	// The relative forms are single components, which is what descriptor-relative
	// callers walk one at a time.
	assert.Equal(t, []string{url.PathEscape(workspace), id}, layout.SessionRelDir(workspace, id))
	assert.Equal(t,
		[]string{url.PathEscape(workspace), id, ArchiveDirName},
		layout.ArchiveRelDir(workspace, id),
	)

	// An unknown workspace path still resolves to a real group directory.
	assert.Equal(t,
		filepath.Join(root, UnknownWorkspaceKey),
		layout.GroupDir("  "),
	)
}
