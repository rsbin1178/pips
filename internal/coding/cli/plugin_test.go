package cli_test

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/rsbin/pips/internal/coding/agentplugin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPluginListReportsPortablePackagesAndDiagnostics(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	root := filepath.Join(fixture.layout.PluginsDir(), "example")
	writeCLIFile(t, filepath.Join(root, "plugin.json"), `{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
  "name": "example-plugin",
  "version": "1.0.0",
  "unknown": true
}`)

	output, err := executeWithDependencies(
		t, fixture.dependencies(nil), "plugin", "list", "--json",
	)
	require.NoError(t, err)

	var decoded struct {
		Plugins []struct {
			Name string `json:"name"`
			Root string `json:"root"`
		} `json:"plugins"`
		Diagnostics []agentplugin.Diagnostic `json:"diagnostics"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &decoded))
	require.Len(t, decoded.Plugins, 1)
	assert.Equal(t, "example-plugin", decoded.Plugins[0].Name)
	assert.Equal(t, root, decoded.Plugins[0].Root)
	require.Len(t, decoded.Diagnostics, 1)
	assert.Equal(t, "unknown_field_ignored", decoded.Diagnostics[0].Code)
}

func TestPluginValidateUsesExplicitPackageDirectory(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	root := filepath.Join(t.TempDir(), "portable")
	writeCLIFile(t, filepath.Join(root, "plugin.json"), `{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
  "name": "portable-plugin"
}`)

	output, err := executeWithDependencies(
		t, fixture.dependencies(nil), "plugin", "validate", root,
	)
	require.NoError(t, err)
	assert.Contains(t, output, "portable-plugin")
	assert.Contains(t, output, root)

	_, err = executeWithDependencies(
		t, fixture.dependencies(nil), "plugin", "validate", filepath.Join(root, "missing"),
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, agentplugin.ErrInvalid)
}

func TestPluginValidateReportsRecoverableDiagnosticsBeforeFatalError(t *testing.T) {
	t.Parallel()

	fixture := newCLIFixture(t)
	root := filepath.Join(t.TempDir(), "invalid")
	writeCLIFile(t, filepath.Join(root, "plugin.json"), `{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
  "name": "INVALID",
  "unknown": true
}`)

	output, err := executeWithDependencies(
		t, fixture.dependencies(nil), "plugin", "validate", root, "--json",
	)
	require.ErrorIs(t, err, agentplugin.ErrInvalid)

	var decoded agentPluginOutputForTest
	require.NoError(t, json.Unmarshal([]byte(output), &decoded))
	require.Len(t, decoded.Diagnostics, 1)
	assert.Equal(t, "unknown_field_ignored", decoded.Diagnostics[0].Code)
}

type agentPluginOutputForTest struct {
	Diagnostics []agentplugin.Diagnostic `json:"diagnostics"`
}
