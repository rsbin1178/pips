package workspace_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenCanonicalizesWorkspace(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	realPath := filepath.Join(root, "real")
	require.NoError(t, os.Mkdir(realPath, 0o750))

	link := filepath.Join(root, "link")
	require.NoError(t, os.Symlink(realPath, link))
	canonicalReal, err := filepath.EvalSymlinks(realPath)
	require.NoError(t, err)

	ws, err := workspace.Open(link)
	require.NoError(t, err)
	assert.Equal(t, canonicalReal, ws.Root())
	assert.Equal(t, canonicalReal, ws.Identity().Path())
	assert.NotZero(t, ws.Identity().Device())
	assert.NotZero(t, ws.Identity().Inode())
	assert.Len(t, ws.Identity().Key(), 64)

	again, err := workspace.Open(realPath)
	require.NoError(t, err)
	assert.Equal(t, ws.Identity().Key(), again.Identity().Key())
}

func TestOpenRejectsInvalidWorkspace(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))

	for _, path := range []string{"", file, filepath.Join(t.TempDir(), "missing")} {
		_, err := workspace.Open(path)
		require.Error(t, err)
		assert.ErrorIs(t, err, workspace.ErrInvalid)
	}
}

func TestTrustStoreRoundTripAndReplacement(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("filesystem identity is implemented for P0 platforms")
	}

	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	require.NoError(t, os.Mkdir(root, 0o750))

	ws, err := workspace.Open(root)
	require.NoError(t, err)

	storePath := filepath.Join(base, "private", "trust.json")
	store := workspace.NewTrustStore(storePath)

	trusted, err := store.IsTrusted(ws.Identity())
	require.NoError(t, err)
	assert.False(t, trusted)

	require.NoError(t, store.Trust(ws.Identity()))

	trusted, err = workspace.NewTrustStore(storePath).IsTrusted(ws.Identity())
	require.NoError(t, err)
	assert.True(t, trusted)

	dirInfo, err := os.Stat(filepath.Dir(storePath))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())

	fileInfo, err := os.Stat(storePath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fileInfo.Mode().Perm())

	require.NoError(t, os.Rename(root, root+"-old"))
	require.NoError(t, os.Mkdir(root, 0o750))
	replacement, err := workspace.Open(root)
	require.NoError(t, err)
	assert.NotEqual(t, ws.Identity().Key(), replacement.Identity().Key())

	trusted, err = store.IsTrusted(replacement.Identity())
	require.NoError(t, err)
	assert.False(t, trusted)
}

func TestTrustStoreRejectsUnsafeOrUnknownData(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	require.NoError(t, os.Mkdir(root, 0o750))
	ws, err := workspace.Open(root)
	require.NoError(t, err)

	tests := []struct {
		name    string
		content string
		mode    os.FileMode
		want    error
	}{
		{name: "unknown field", content: `{"version":1,"workspaces":{},"extra":true}`, mode: 0o600},
		{name: "unknown version", content: `{"version":2,"workspaces":{}}`, mode: 0o600, want: workspace.ErrUnsupportedTrustVersion},
		{name: "trailing value", content: `{"version":1,"workspaces":{}} {}`, mode: 0o600},
		{name: "broad permissions", content: `{"version":1,"workspaces":{}}`, mode: 0o644, want: workspace.ErrInsecurePermissions},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := filepath.Join(base, tt.name)
			require.NoError(t, os.Mkdir(dir, 0o700))
			path := filepath.Join(dir, "trust.json")
			require.NoError(t, os.WriteFile(path, []byte(tt.content), tt.mode))

			_, err := workspace.NewTrustStore(path).IsTrusted(ws.Identity())
			require.Error(t, err)

			if tt.want != nil {
				assert.ErrorIs(t, err, tt.want)
			}
		})
	}
}

func TestTrustStoreRejectsSymlink(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	require.NoError(t, os.Mkdir(root, 0o750))
	ws, err := workspace.Open(root)
	require.NoError(t, err)

	target := filepath.Join(base, "target.json")
	require.NoError(t, os.WriteFile(target, []byte(`{"version":1,"workspaces":{}}`), 0o600))

	link := filepath.Join(base, "trust.json")
	require.NoError(t, os.Symlink(target, link))

	_, err = workspace.NewTrustStore(link).IsTrusted(ws.Identity())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a regular file")
}
