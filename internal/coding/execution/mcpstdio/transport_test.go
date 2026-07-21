package mcpstdio_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rsbin/pips/internal/coding/execution/mcpstdio"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewTransportBuildsMinimalShellFreeCommandAndCleansPrivateDir(t *testing.T) {
	t.Parallel()

	ws, err := workspace.Open(t.TempDir())
	require.NoError(t, err)
	tempRoot := privateTempRoot(t)
	executable, err := os.Executable()
	require.NoError(t, err)

	parent := map[string]string{
		"API_KEY":       "secret",
		"HTTPS_PROXY":   "proxy",
		"HOME":          "/home/test",
		"LANG":          "C",
		"OTEL_HEADERS":  "telemetry-secret",
		"PATH":          "/usr/bin:/bin",
		"SSH_AUTH_SOCK": "/tmp/agent.sock",
	}
	resource, err := mcpstdio.NewTransport(mcpstdio.Config{
		Workspace: ws,
		Command:   executable,
		Args:      []string{"-test.run=TestHelper"},
		TempRoot:  tempRoot,
		Environment: func(name string) (string, bool) {
			value, ok := parent[name]

			return value, ok
		},
	})
	require.NoError(t, err)

	transport, ok := resource.Transport().(*sdk.CommandTransport)
	require.True(t, ok)
	assert.Equal(t, executable, transport.Command.Path)
	assert.Equal(t, []string{executable, "-test.run=TestHelper"}, transport.Command.Args)
	assert.Equal(t, ws.Root(), transport.Command.Dir)

	environment := strings.Join(transport.Command.Env, "\n")
	assert.Contains(t, environment, "HOME=/home/test")
	assert.NotContains(t, environment, "API_KEY")
	assert.NotContains(t, environment, "HTTPS_PROXY")
	assert.NotContains(t, environment, "OTEL_HEADERS")
	assert.NotContains(t, environment, "SSH_AUTH_SOCK")
	assert.Contains(t, environment, "TMPDIR="+filepath.Join(resource.PrivateDir(), "tmp"))

	privateDir := resource.PrivateDir()

	_, err = os.Stat(privateDir)
	require.NoError(t, err)
	require.NoError(t, resource.Close())
	require.NoError(t, resource.Close())

	_, err = os.Stat(privateDir)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestNewTransportRejectsSymlinkExecutableAndPublicTempRoot(t *testing.T) {
	t.Parallel()

	ws, err := workspace.Open(t.TempDir())
	require.NoError(t, err)
	executable, err := os.Executable()
	require.NoError(t, err)
	link := filepath.Join(t.TempDir(), "server")
	require.NoError(t, os.Symlink(executable, link))

	_, err = mcpstdio.NewTransport(mcpstdio.Config{
		Workspace: ws, Command: link, TempRoot: privateTempRoot(t), Environment: os.LookupEnv,
	})
	require.Error(t, err)

	publicRoot := t.TempDir()
	require.NoError(t, os.Chmod(publicRoot, 0o755)) //nolint:gosec // Deliberately unsafe fixture.
	_, err = mcpstdio.NewTransport(mcpstdio.Config{
		Workspace: ws, Command: executable, TempRoot: publicRoot, Environment: os.LookupEnv,
	})
	require.Error(t, err)
}

func TestResourceRefusesToRemoveReplacementDirectory(t *testing.T) {
	t.Parallel()

	ws, err := workspace.Open(t.TempDir())
	require.NoError(t, err)
	executable, err := os.Executable()
	require.NoError(t, err)
	resource, err := mcpstdio.NewTransport(mcpstdio.Config{
		Workspace:   ws,
		Command:     executable,
		TempRoot:    privateTempRoot(t),
		Environment: os.LookupEnv,
	})
	require.NoError(t, err)

	privateDir := resource.PrivateDir()
	require.NoError(t, os.Rename(privateDir, privateDir+"-original"))
	require.NoError(t, os.Mkdir(privateDir, 0o700))

	require.Error(t, resource.Close())

	_, err = os.Stat(privateDir)
	require.NoError(t, err)
}

func privateTempRoot(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	require.NoError(t, os.Chmod(root, 0o700)) //nolint:gosec // Directories require owner traversal.

	return root
}
