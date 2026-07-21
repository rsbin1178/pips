package mcp_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	codingmcp "github.com/rsbin/pips/internal/coding/mcp"
	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPermissionsRequireMatchingDualRecordsAndInvalidateOnChange(t *testing.T) {
	t.Parallel()

	fixture := newPermissionFixture(t, true)
	definitions := fixture.loadDefinitions(t, nil)

	resolved, err := fixture.permissions.Resolve(t.Context(), definitions)
	require.NoError(t, err)
	require.Len(t, resolved, 1)
	assert.Equal(t, codingmcp.StatusPending, resolved[0].Status)

	require.NoError(t, fixture.permissions.Decide(
		t.Context(),
		resolved[0].Definition,
		workspace.PermissionAllow,
	))
	assert.Equal(t, []string{paths.ProjectPermissionsFile()}, fixture.git.excludedPaths())

	resolved, err = fixture.permissions.Resolve(t.Context(), definitions)
	require.NoError(t, err)
	assert.Equal(t, codingmcp.StatusEnabled, resolved[0].Status)

	permissionPath := filepath.Join(
		fixture.workspace.Root(),
		filepath.FromSlash(paths.ProjectPermissionsFile()),
	)
	info, err := os.Stat(permissionPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	content, err := os.ReadFile(permissionPath) //nolint:gosec // Test reads the exact fixture path.
	require.NoError(t, err)
	assert.Contains(t, string(content), `schema = 'pips.permissions/v1alpha1'`)
	assert.NotContains(t, string(content), "secret")

	changed := fixture.loadDefinitions(t, []string{"--changed"})
	resolved, err = fixture.permissions.Resolve(t.Context(), changed)
	require.NoError(t, err)
	assert.Equal(t, codingmcp.StatusPending, resolved[0].Status)
}

func TestPermissionsPersistExplicitDeny(t *testing.T) {
	t.Parallel()

	fixture := newPermissionFixture(t, true)
	definitions := fixture.loadDefinitions(t, nil)
	definition := definitions.List()[0]

	require.NoError(t, fixture.permissions.Decide(
		t.Context(),
		definition,
		workspace.PermissionDeny,
	))

	resolved, err := fixture.permissions.Resolve(t.Context(), definitions)
	require.NoError(t, err)
	assert.Equal(t, codingmcp.StatusDisabled, resolved[0].Status)
}

func TestPermissionsCrashWindowStaysPending(t *testing.T) {
	t.Parallel()

	fixture := newPermissionFixture(t, false)
	definitions := fixture.loadDefinitions(t, nil)
	definition := definitions.List()[0]

	err := fixture.permissions.Decide(t.Context(), definition, workspace.PermissionAllow)
	require.ErrorIs(t, err, workspace.ErrWorkspaceUnknown)

	resolved, err := fixture.permissions.Resolve(t.Context(), definitions)
	require.NoError(t, err)
	assert.Equal(t, codingmcp.StatusPending, resolved[0].Status)

	_, err = os.Stat(filepath.Join(
		fixture.workspace.Root(),
		filepath.FromSlash(paths.ProjectPermissionsFile()),
	))
	require.NoError(t, err)
}

func TestPermissionsRejectTrackedAndSymlinkFiles(t *testing.T) {
	t.Parallel()

	t.Run("tracked", func(t *testing.T) {
		t.Parallel()

		fixture := newPermissionFixture(t, true)
		definitions := fixture.loadDefinitions(t, nil)
		fixture.git.setTracked(true)

		err := fixture.permissions.Decide(
			t.Context(),
			definitions.List()[0],
			workspace.PermissionAllow,
		)
		require.ErrorIs(t, err, codingmcp.ErrUnsafeFile)
	})

	t.Run("symlink", func(t *testing.T) {
		t.Parallel()

		fixture := newPermissionFixture(t, true)
		definitions := fixture.loadDefinitions(t, nil)
		projectPips := filepath.Join(fixture.workspace.Root(), filepath.FromSlash(paths.ProjectRoot()))
		target := filepath.Join(t.TempDir(), "permissions.toml")
		writeFile(t, target, `schema = "pips.permissions/v1alpha1"`)
		require.NoError(t, os.Symlink(target, filepath.Join(projectPips, "permissions.toml")))

		_, err := fixture.permissions.Resolve(t.Context(), definitions)
		require.ErrorIs(t, err, codingmcp.ErrUnsafeFile)
	})
}

type permissionFixture struct {
	layout      paths.Layout
	workspace   workspace.Workspace
	tree        *workspace.Tree
	store       *workspace.Store
	git         *fakeProjectGit
	permissions *codingmcp.Permissions
}

func newPermissionFixture(t *testing.T, trusted bool) permissionFixture {
	t.Helper()

	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.Chmod(layout.Root(), 0o700)) //nolint:gosec // Directories require owner traversal.
	opened, err := workspace.Open(t.TempDir())
	require.NoError(t, err)
	tree, err := workspace.OpenTree(opened)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })

	store := workspace.NewStore(layout.WorkspacesFile())
	if trusted {
		require.NoError(t, store.Trust(opened.Identity()))
	}

	git := &fakeProjectGit{repository: true}
	permissions, err := codingmcp.NewPermissions(codingmcp.PermissionOptions{
		Workspace: opened,
		Tree:      tree,
		Store:     store,
		Git:       git,
		Limits:    codingmcp.DefaultLimits(),
	})
	require.NoError(t, err)

	return permissionFixture{
		layout: layout, workspace: opened, tree: tree, store: store,
		git: git, permissions: permissions,
	}
}

func (f permissionFixture) loadDefinitions(t *testing.T, args []string) codingmcp.Definitions {
	t.Helper()

	argumentJSON := ""
	if len(args) > 0 {
		argumentJSON = `,"args":["--changed"]`
	}

	writeFile(
		t,
		filepath.Join(f.workspace.Root(), filepath.FromSlash(paths.ProjectMCPFile())),
		`{"schema":"pips.mcp/v1alpha1","servers":[{
          "id":"local_fs","type":"stdio","command":"/bin/echo"`+argumentJSON+`
        }]}`,
	)

	definitions, err := codingmcp.LoadDefinitions(t.Context(), codingmcp.LoadOptions{
		Paths: f.layout, Tree: f.tree, ProjectTrusted: true, Limits: codingmcp.DefaultLimits(),
	})
	require.NoError(t, err)

	return definitions
}

type fakeProjectGit struct {
	mu         sync.Mutex
	tracked    bool
	repository bool
	excluded   []string
}

func (f *fakeProjectGit) Tracked(context.Context, string) (bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.tracked, f.repository, nil
}

func (f *fakeProjectGit) ExcludeLocal(_ context.Context, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.excluded = append(f.excluded, path)

	return nil
}

func (f *fakeProjectGit) setTracked(value bool) {
	f.mu.Lock()
	f.tracked = value
	f.mu.Unlock()
}

func (f *fakeProjectGit) excludedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.excluded...)
}
