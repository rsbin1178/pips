package workspace_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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

func TestStoreRoundTripAndReplacement(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("filesystem identity is implemented for P0 platforms")
	}

	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	require.NoError(t, os.Mkdir(root, 0o750))

	ws, err := workspace.Open(root)
	require.NoError(t, err)

	storePath := filepath.Join(base, "private", "workspaces.json")
	store := workspace.NewStore(storePath)

	trusted, err := store.IsTrusted(ws.Identity())
	require.NoError(t, err)
	assert.False(t, trusted)

	require.NoError(t, store.Trust(ws.Identity()))

	trusted, err = workspace.NewStore(storePath).IsTrusted(ws.Identity())
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

func TestStoreRejectsUnsafeOrUnknownData(t *testing.T) {
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
		{
			name:    "unknown field",
			content: `{"schema":"pips.workspaces/v1alpha1","workspaces":{},"extra":true}`,
			mode:    0o600,
		},
		{
			name:    "unknown schema",
			content: `{"schema":"pips.workspaces/v2","workspaces":{}}`,
			mode:    0o600,
			want:    workspace.ErrUnsupportedStoreSchema,
		},
		{
			name:    "duplicate key",
			content: `{"schema":"pips.workspaces/v1alpha1","schema":"pips.workspaces/v1alpha1","workspaces":{}}`,
			mode:    0o600,
		},
		{
			name:    "null workspaces",
			content: `{"schema":"pips.workspaces/v1alpha1","workspaces":null}`,
			mode:    0o600,
		},
		{
			name:    "trailing value",
			content: `{"schema":"pips.workspaces/v1alpha1","workspaces":{}} {}`,
			mode:    0o600,
		},
		{
			name:    "broad permissions",
			content: `{"schema":"pips.workspaces/v1alpha1","workspaces":{}}`,
			mode:    0o644,
			want:    workspace.ErrInsecurePermissions,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := filepath.Join(base, tt.name)
			require.NoError(t, os.Mkdir(dir, 0o700))
			path := filepath.Join(dir, "workspaces.json")
			require.NoError(t, os.WriteFile(path, []byte(tt.content), tt.mode))

			_, err := workspace.NewStore(path).IsTrusted(ws.Identity())
			require.Error(t, err)

			if tt.want != nil {
				assert.ErrorIs(t, err, tt.want)
			}
		})
	}
}

func TestStoreRejectsSymlink(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	require.NoError(t, os.Mkdir(root, 0o750))
	ws, err := workspace.Open(root)
	require.NoError(t, err)

	target := filepath.Join(base, "target.json")
	require.NoError(t, os.WriteFile(
		target,
		[]byte(`{"schema":"pips.workspaces/v1alpha1","workspaces":{}}`),
		0o600,
	))

	link := filepath.Join(base, "workspaces.json")
	require.NoError(t, os.Symlink(target, link))

	_, err = workspace.NewStore(link).IsTrusted(ws.Identity())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a regular file")
}

func TestStoreRejectsOversizedInput(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	require.NoError(t, os.Mkdir(root, 0o750))

	ws, err := workspace.Open(root)
	require.NoError(t, err)

	storePath := filepath.Join(base, "workspaces.json")
	require.NoError(t, os.WriteFile(storePath, make([]byte, (4<<20)+1), 0o600))

	_, err = workspace.NewStore(storePath).IsTrusted(ws.Identity())
	require.ErrorIs(t, err, workspace.ErrStoreTooLarge)
}

func TestStorePermissionRequiresTrustAndReplacesByTypedIdentity(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	require.NoError(t, os.Mkdir(root, 0o750))

	ws, err := workspace.Open(root)
	require.NoError(t, err)

	store := workspace.NewStore(filepath.Join(base, "private", "workspaces.json"))
	permission := workspace.Permission{
		Kind:        workspace.PermissionMCPServer,
		ResourceID:  "filesystem",
		Fingerprint: strings.Repeat("a", 64),
		Decision:    workspace.PermissionAllow,
		DecidedAt:   time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC),
	}

	require.ErrorIs(t, store.SetPermission(ws.Identity(), permission), workspace.ErrWorkspaceUnknown)
	require.NoError(t, store.Trust(ws.Identity()))
	require.NoError(t, store.SetPermission(ws.Identity(), permission))

	stored, exists, err := store.Permission(
		ws.Identity(),
		workspace.PermissionMCPServer,
		"filesystem",
	)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, permission, stored)

	permission.Fingerprint = strings.Repeat("b", 64)
	permission.Decision = workspace.PermissionDeny
	require.NoError(t, store.SetPermission(ws.Identity(), permission))

	stored, exists, err = store.Permission(
		ws.Identity(),
		workspace.PermissionMCPServer,
		"filesystem",
	)
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, permission, stored)
}
