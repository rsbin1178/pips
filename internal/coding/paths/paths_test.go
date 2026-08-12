//nolint:wsl_v5 // Layout construction and path assertions stay grouped.
package paths_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	layout, err := paths.New(base)
	require.NoError(t, err)

	assert.Equal(t, base, layout.Root())
	assert.Equal(t, filepath.Join(base, "tmp"), layout.TempDir())
	assert.Equal(t, filepath.Join(base, "config.toml"), layout.ConfigFile())
	assert.Equal(t, filepath.Join(base, "workspaces.json"), layout.WorkspacesFile())
	assert.Equal(t, filepath.Join(base, "sessions"), layout.SessionsDir())
	assert.Equal(t, filepath.Join(base, "plans"), layout.PlansDir())
	assert.Equal(t, filepath.Join(base, "teams"), layout.TeamsDir())
	assert.Equal(t, filepath.Join(base, "teams", "aggregates"), layout.TeamAggregatesDir())
	assert.Equal(t, filepath.Join(base, "teams", "continuations"), layout.TeamContinuationsDir())
	assert.Equal(t, filepath.Join(base, "teams", "resources"), layout.TeamResourcesDir())
	assert.Equal(t, filepath.Join(base, "teams", "control"), layout.TeamControlDir())
	assert.Equal(t, filepath.Join(base, "teams", "leases"), layout.TeamLeasesDir())
	assert.Equal(t, filepath.Join(base, "teams", "integrations"), layout.TeamIntegrationsDir())
	assert.Equal(t, base+"-worktrees", layout.WorktreesRoot())
	assert.Equal(t, filepath.Join(base, "skills"), layout.SkillsDir())
	assert.Equal(t, filepath.Join(base, "agents"), layout.AgentsDir())
	assert.Empty(t, layout.AgentSkillsDir())
	assert.Empty(t, layout.SharedAgentsDir())
	assert.Equal(t, filepath.Join(base, "bundles"), layout.BundlesDir())
	assert.Equal(t, filepath.Join(base, "plugins"), layout.PluginsDir())
	assert.Equal(t, filepath.Join(base, "plugin-data"), layout.PluginDataDir())
	assert.Equal(t, filepath.Join(base, "mcp.json"), layout.MCPFile())
	assert.Equal(t, filepath.Join(base, "hooks.json"), layout.HooksFile())
	assert.Equal(t, filepath.Join(base, "hook-trust.json"), layout.HookTrustFile())
	assert.Equal(t, filepath.Join(base, "themes"), layout.TUIThemesDir())
}

func TestNewRejectsEmptyPath(t *testing.T) {
	t.Parallel()

	_, err := paths.New("")
	require.ErrorIs(t, err, paths.ErrInvalid)
}

func TestNewRejectsNULPath(t *testing.T) {
	t.Parallel()

	_, err := paths.New("invalid\x00path")
	require.ErrorIs(t, err, paths.ErrInvalid)
}

func TestNewRejectsFilesystemRoot(t *testing.T) {
	t.Parallel()

	root := filepath.VolumeName(t.TempDir()) + string(filepath.Separator)
	_, err := paths.New(root)
	require.ErrorIs(t, err, paths.ErrInvalid)
}

func TestDefault(t *testing.T) {
	t.Setenv(paths.HomeEnv, "")

	layout, err := paths.Default()
	require.NoError(t, err)

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".pips"), layout.Root())
	assert.Equal(t, filepath.Join(home, ".agents", "skills"), layout.AgentSkillsDir())
	assert.Equal(t, filepath.Join(home, ".agents", "agents"), layout.SharedAgentsDir())
	assert.Equal(t, filepath.Join(home, ".pips", "agents"), layout.AgentsDir())
	assert.Equal(t, filepath.Join(home, ".pips", "plugins"), layout.PluginsDir())
	assert.Equal(t, filepath.Join(home, ".pips", "plugin-data"), layout.PluginDataDir())
	assert.Equal(t, filepath.Join(home, ".pips", "themes"), layout.TUIThemesDir())
}

func TestDefaultUsesPIPSHome(t *testing.T) {
	root := t.TempDir()
	t.Setenv(paths.HomeEnv, root)

	layout, err := paths.Default()
	require.NoError(t, err)
	assert.Equal(t, root, layout.Root())
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".agents", "skills"), layout.AgentSkillsDir())
	assert.Equal(t, filepath.Join(home, ".agents", "agents"), layout.SharedAgentsDir())
	assert.Equal(t, filepath.Join(root, "agents"), layout.AgentsDir())
	assert.Equal(t, filepath.Join(root, "plugins"), layout.PluginsDir())
	assert.Equal(t, filepath.Join(root, "plugin-data"), layout.PluginDataDir())
	assert.Equal(t, filepath.Join(root, "themes"), layout.TUIThemesDir())
	assert.Equal(t, filepath.Join(root, "hooks.json"), layout.HooksFile())
	assert.Equal(t, filepath.Join(root, "hook-trust.json"), layout.HookTrustFile())
}

func TestWithAgentSkillsDir(t *testing.T) {
	t.Parallel()

	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)

	shared := t.TempDir()
	layout, err = layout.WithAgentSkillsDir(shared)
	require.NoError(t, err)
	assert.Equal(t, shared, layout.AgentSkillsDir())
}

func TestWithSharedAgentsDir(t *testing.T) {
	t.Parallel()

	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)

	shared := t.TempDir()
	layout, err = layout.WithSharedAgentsDir(shared)
	require.NoError(t, err)
	assert.Equal(t, shared, layout.SharedAgentsDir())
}

func TestDefaultRejectsRelativePIPSHome(t *testing.T) {
	t.Setenv(paths.HomeEnv, "relative/pips")

	_, err := paths.Default()
	require.ErrorIs(t, err, paths.ErrInvalid)
}

func TestDefaultRejectsUncleanPIPSHome(t *testing.T) {
	root := t.TempDir() + string(filepath.Separator) + ".."
	t.Setenv(paths.HomeEnv, root)

	_, err := paths.Default()
	require.ErrorIs(t, err, paths.ErrInvalid)
}

func TestDefaultRejectsBlankPIPSHome(t *testing.T) {
	t.Setenv(paths.HomeEnv, " ")

	_, err := paths.Default()
	require.ErrorIs(t, err, paths.ErrInvalid)
}

func TestProjectPaths(t *testing.T) {
	t.Parallel()

	assert.Equal(t, ".pips", paths.ProjectRoot())
	assert.Equal(t, ".pips/permissions.toml", paths.ProjectPermissionsFile())
	assert.Equal(t, ".pips/mcp.json", paths.ProjectMCPFile())
	assert.Equal(t, ".pips/hooks.json", paths.ProjectHooksFile())
	assert.Equal(t, ".pips/skills", paths.ProjectSkillsDir())
	assert.Equal(t, ".pips/skills.toml", paths.ProjectSkillsFile())
	assert.Equal(t, ".agents/skills", paths.ProjectAgentSkillsDir())
	assert.Equal(t, ".pips/agents", paths.ProjectAgentsDir())
	assert.Equal(t, ".agents/agents", paths.ProjectSharedAgentsDir())
	assert.Equal(t, ".pips/bundles", paths.ProjectBundlesDir())
	assert.Equal(t, ".pips/plugins", paths.ProjectPluginsDir())
}
