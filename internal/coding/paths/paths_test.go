package paths_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	layout, err := paths.New(base)
	require.NoError(t, err)

	assert.Equal(t, base, layout.Root())
	assert.Equal(t, filepath.Join(base, "config.toml"), layout.ConfigFile())
	assert.Equal(t, filepath.Join(base, "trust.json"), layout.TrustFile())
	assert.Equal(t, filepath.Join(base, "sessions"), layout.SessionsDir())
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

func TestDefault(t *testing.T) {
	t.Setenv(paths.HomeEnv, "")

	layout, err := paths.Default()
	require.NoError(t, err)

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".pips"), layout.Root())
}

func TestDefaultUsesPIPSHome(t *testing.T) {
	root := t.TempDir()
	t.Setenv(paths.HomeEnv, root)

	layout, err := paths.Default()
	require.NoError(t, err)
	assert.Equal(t, root, layout.Root())
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
	assert.Equal(t, ".pips/config.toml", paths.ProjectConfigFile())
	assert.Equal(t, ".pips/permissions.toml", paths.ProjectPermissionsFile())
	assert.Equal(t, ".pips/mcp.json", paths.ProjectMCPFile())
	assert.Equal(t, ".pips/skills", paths.ProjectSkillsDir())
	assert.Equal(t, ".pips/bundles", paths.ProjectBundlesDir())
}
