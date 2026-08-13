package mcpstdio_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/coding/execution/mcpstdio"
	"github.com/rsbin1178/pips/internal/coding/workspace"
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
		EnvironmentOverrides: []execution.EnvVar{{
			Name: "PIPS_TEST_MCP_SCOPE", Value: "session-only",
		}},
	})
	require.NoError(t, err)

	command := resource.Command()
	require.NotNil(t, command)
	assert.Equal(t, executable, command.Path)
	assert.Equal(t, []string{executable, "-test.run=TestHelper"}, command.Args)
	assert.Equal(t, ws.Root(), command.Dir)

	environment := strings.Join(command.Env, "\n")
	assert.Contains(t, environment, "HOME=/home/test")
	assert.NotContains(t, environment, "API_KEY")
	assert.NotContains(t, environment, "HTTPS_PROXY")
	assert.NotContains(t, environment, "OTEL_HEADERS")
	assert.NotContains(t, environment, "SSH_AUTH_SOCK")
	assert.Contains(t, environment, "PIPS_TEST_MCP_SCOPE=session-only")
	assert.Contains(t, environment, "TMPDIR="+filepath.Join(resource.PrivateDir(), "tmp"))

	privateDir := resource.PrivateDir()

	_, err = os.Stat(privateDir)
	require.NoError(t, err)
	require.NoError(t, resource.Close())
	require.NoError(t, resource.Close())

	_, err = os.Stat(privateDir)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestNewTransportRejectsUnsafeEnvironmentOverride(t *testing.T) {
	t.Parallel()

	ws, err := workspace.Open(t.TempDir())
	require.NoError(t, err)
	executable, err := os.Executable()
	require.NoError(t, err)
	_, err = mcpstdio.NewTransport(mcpstdio.Config{
		Workspace: ws, Command: executable, TempRoot: privateTempRoot(t), Environment: os.LookupEnv,
		EnvironmentOverrides: []execution.EnvVar{{Name: "API_KEY", Value: "secret"}},
	})
	require.ErrorIs(t, err, execution.ErrInvalidOperation)
	assert.NotContains(t, err.Error(), "secret")
}

func TestNewTransportAppliesAgentPluginEnvironmentAndWorkingDirectory(t *testing.T) {
	t.Parallel()

	ws, err := workspace.Open(t.TempDir())
	require.NoError(t, err)
	pluginRoot := canonicalTempDir(t)
	pluginData := canonicalTempDir(t)
	resource, err := mcpstdio.NewTransport(mcpstdio.Config{
		Workspace: ws, Command: "go", TempRoot: privateTempRoot(t), Environment: os.LookupEnv,
		PluginRoot: pluginRoot, PluginData: pluginData, WorkingDirectory: pluginData,
		EnvironmentOverrides: []execution.EnvVar{
			{Name: "API_KEY", Value: "configured"},
			{Name: "PATH", Value: "/plugin/path"},
			{Name: "plugin_root", Value: "lowercase-configured"},
			{Name: "plugin_data", Value: "lowercase-configured"},
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resource.Close()) })

	command := resource.Command()
	require.NotNil(t, command)
	assert.Equal(t, pluginData, command.Dir)
	environment := strings.Join(command.Env, "\n")
	assert.Contains(t, environment, "API_KEY=configured")
	assert.Contains(t, environment, "PATH=/plugin/path")
	assert.Contains(t, environment, "PLUGIN_ROOT="+pluginRoot)
	assert.Contains(t, environment, "PLUGIN_DATA="+pluginData)

	if runtime.GOOS == "windows" {
		assert.NotContains(t, environment, "plugin_root=lowercase-configured")
		assert.NotContains(t, environment, "plugin_data=lowercase-configured")
	} else {
		assert.Contains(t, environment, "plugin_root=lowercase-configured")
		assert.Contains(t, environment, "plugin_data=lowercase-configured")
	}
}

func TestNewTransportResolvesBareAgentPluginCommandWithPlatformSearch(t *testing.T) {
	t.Parallel()

	ws, err := workspace.Open(t.TempDir())
	require.NoError(t, err)
	pluginRoot := canonicalTempDir(t)
	pluginData := canonicalTempDir(t)
	resource, err := mcpstdio.NewTransport(mcpstdio.Config{
		Workspace: ws, Command: "go", TempRoot: privateTempRoot(t), Environment: os.LookupEnv,
		PluginRoot: pluginRoot, PluginData: pluginData, WorkingDirectory: pluginRoot,
		EnvironmentOverrides: []execution.EnvVar{{Name: "PATH", Value: filepath.Join(t.TempDir(), "not-used")}},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resource.Close()) })

	command := resource.Command()
	require.NotNil(t, command)
	assert.True(t, filepath.IsAbs(command.Path))
	assert.NotContains(t, command.Path, "not-used")
}

func TestNewTransportRejectsSymlinkExecutableAndPublicTempRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink privilege and mode-bit policy do not apply on Windows")
	}

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

func TestAgentPluginCommandIsRevalidatedImmediatelyBeforeStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not generally available to unprivileged Windows tests")
	}

	t.Parallel()

	ws, err := workspace.Open(t.TempDir())
	require.NoError(t, err)
	pluginRoot := canonicalTempDir(t)
	pluginData := canonicalTempDir(t)
	binDirectory := filepath.Join(pluginRoot, "bin")
	require.NoError(t, os.Mkdir(binDirectory, 0o700))
	command := filepath.Join(binDirectory, "server")
	require.NoError(t, os.WriteFile(command, []byte("#!/bin/sh\nexit 0\n"), 0o600))
	require.NoError(t, os.Chmod(command, 0o700)) //nolint:gosec // Executable fixture requires owner execute.

	resource, err := mcpstdio.NewTransport(mcpstdio.Config{
		Workspace: ws, Command: command, TempRoot: privateTempRoot(t), Environment: os.LookupEnv,
		PluginRoot: pluginRoot, PluginData: pluginData, WorkingDirectory: pluginRoot,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resource.Close()) })

	require.NoError(t, os.Rename(binDirectory, binDirectory+"-original"))
	outside := t.TempDir()
	outsideCommand := filepath.Join(outside, "server")
	require.NoError(t, os.WriteFile(outsideCommand, []byte("#!/bin/sh\nexit 0\n"), 0o600))
	require.NoError(t, os.Chmod(outsideCommand, 0o700)) //nolint:gosec // Executable fixture requires owner execute.
	require.NoError(t, os.Symlink(outside, binDirectory))

	connection, err := resource.Transport().Connect(t.Context())
	if connection != nil {
		_ = connection.Close()
	}

	require.ErrorContains(t, err, "escapes plugin root")
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

func canonicalTempDir(t *testing.T) string {
	t.Helper()

	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	return root
}
