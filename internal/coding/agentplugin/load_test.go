//nolint:wsl_v5 // Protocol fixtures stay adjacent to the assertions they establish.
package agentplugin_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/internal/coding/agentplugin"
	codingmcp "github.com/rsbin1178/pips/internal/coding/mcp"
	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadDirectoryImplementsPortableComponentsAndIsolation(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	dataBase := filepath.Join(t.TempDir(), "${PLUGIN_ROOT}", "plugin-data")
	writeFile(t, filepath.Join(root, "plugin.json"), `{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
  "name": "example.tools",
  "version": "1.2.3",
  "unknown": true,
  "extensions": 7
}`)
	writeSkill(t, root, "review", "review")
	writeFile(t, filepath.Join(root, "skills", "review", "references", "rules.md"), "portable rules")
	writeSkill(t, root, "invalid", "wrong-name")
	writeSkill(t, filepath.Join(root, "skills", "review"), "nested", "nested")

	command := filepath.Join(root, "bin", "server")
	writeFile(t, command, "#!/bin/sh\nexit 0\n")
	require.NoError(t, os.Chmod(command, 0o700)) //nolint:gosec // Executable fixture requires owner execute.
	writeFile(t, filepath.Join(root, "mcp.json"), `{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json",
  "mcpServers": {
    "local": {
      "type": "stdio",
      "command": "./bin/server",
      "args": ["${PLUGIN_ROOT}", "${PLUGIN_DATA}", "${UNKNOWN}"],
      "env": {"API_TOKEN": "${PLUGIN_ROOT}/token", "RAW": "${UNKNOWN}"},
      "cwd": "${PLUGIN_DATA}"
    },
    "remote": {
      "type": "streamable-http",
      "url": "https://tools.example.com/mcp",
      "headers": {"X-Tenant": "public"}
    },
    "legacy": {"type": "sse", "url": "https://tools.example.com/events"},
    "invalid": {"type": "streamable-http", "url": "http://example.com/mcp"}
  }
}`)

	result, err := agentplugin.LoadDirectory(
		t.Context(), root, dataBase, agentplugin.DefaultLimits(),
	)
	require.NoError(t, err)

	packages := result.Packages()
	require.Len(t, packages, 1)
	assert.Equal(t, "example.tools", packages[0].Manifest.Name)
	assert.Equal(t, "1.2.3", packages[0].Manifest.Version)
	assert.Equal(t, 1, packages[0].SkillCount)
	assert.Equal(t, 2, packages[0].ServerCount)

	skills := result.SkillEntries()
	require.Len(t, skills, 1)
	assert.Equal(t, "review", skills[0].Skill.Name)
	require.Len(t, skills[0].Skill.Resources, 2)
	assert.Equal(t, "references/rules.md", skills[0].Skill.Resources[0].Path)

	definitions := result.MCPDefinitions().List()
	require.Len(t, definitions, 2)
	stdio := definitionByTransport(t, definitions, codingmcp.TransportStdio)
	assert.Equal(t, command, stdio.Command)
	assert.Equal(t, root, stdio.Args[0])
	assert.Equal(t, stdio.PluginData, stdio.Args[1])
	assert.Contains(t, stdio.PluginData, "${PLUGIN_ROOT}", "replacement output must not be expanded recursively")
	assert.Equal(t, "${UNKNOWN}", stdio.Args[2])
	assert.Equal(t, stdio.PluginData, stdio.WorkingDir)
	assert.Equal(t, "API_TOKEN", stdio.Environment[0].Name)
	assert.Equal(t, filepath.Join(root, "token"), stdio.Environment[0].Value)
	assert.Equal(t, "${UNKNOWN}", stdio.Environment[1].Value)
	dataInfo, err := os.Stat(stdio.PluginData)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dataInfo.Mode().Perm())

	remote := definitionByTransport(t, definitions, codingmcp.TransportStreamableHTTP)
	assert.Equal(t, "https://tools.example.com/mcp", remote.URL)
	assert.Equal(t, []codingmcp.HTTPHeader{{Name: "X-Tenant", Value: "public"}}, remote.Headers)

	codes := diagnosticCodes(result.Diagnostics())
	assert.Contains(t, codes, "unknown_field_ignored")
	assert.Contains(t, codes, "extensions_ignored")
	assert.Contains(t, codes, "skill_invalid")
	assert.Contains(t, codes, "server_invalid")
	assert.Contains(t, codes, "transport_unsupported")
}

func TestLoadRejectsOneManifestAndContinuesWithSibling(t *testing.T) {
	t.Parallel()

	userRoot := t.TempDir()
	layout, err := paths.New(userRoot)
	require.NoError(t, err)
	writeFile(t, filepath.Join(layout.PluginsDir(), "bad", "plugin.json"), `{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
  "name": "Bad_Name",
  "unexpected": true
}`)
	writeFile(t, filepath.Join(layout.PluginsDir(), "good", "plugin.json"), `{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
  "name": "good-plugin"
}`)

	result, err := agentplugin.Load(t.Context(), agentplugin.Options{
		Paths: layout, Limits: agentplugin.DefaultLimits(),
	})
	require.NoError(t, err)
	require.Len(t, result.Packages(), 1)
	assert.Equal(t, "good-plugin", result.Packages()[0].Manifest.Name)
	assert.Contains(t, diagnosticCodes(result.Diagnostics()), "plugin_rejected")
	assert.Contains(t, diagnosticCodes(result.Diagnostics()), "unknown_field_ignored")
}

func TestManifestAcceptsStandardMetadataAndIgnoresExtensionNamespaceValues(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "plugin.json"), `{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
  "name": "metadata.plugin",
  "version": "not-required-to-be-semver",
  "description": "Portable metadata",
  "author": {"name": "Author", "email": "not-validated", "url": "not-validated"},
  "homepage": "not-required-to-be-a-url",
  "repository": "opaque",
  "license": "custom",
  "keywords": ["one", "two"],
  "extensions": {"com.example.client": 7}
}`)

	result, err := agentplugin.LoadDirectory(
		t.Context(), root, filepath.Join(t.TempDir(), "data"), agentplugin.DefaultLimits(),
	)
	require.NoError(t, err)
	require.Len(t, result.Packages(), 1)
	manifest := result.Packages()[0].Manifest
	assert.Equal(t, "metadata.plugin", manifest.Name)
	assert.Equal(t, "not-required-to-be-semver", manifest.Version)
	require.NotNil(t, manifest.Author)
	assert.Equal(t, "not-validated", manifest.Author.Email)
	assert.Equal(t, []string{"one", "two"}, manifest.Keywords)
	assert.Empty(t, result.Diagnostics())
}

func TestInvalidMCPTopLevelDisablesOnlyMCP(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeManifest(t, root, "skill-only")
	writeSkill(t, root, "review", "review")
	writeFile(t, filepath.Join(root, "mcp.json"), `{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json",
  "mcpServers": {},
  "unknown": true
}`)

	result, err := agentplugin.LoadDirectory(
		t.Context(), root, filepath.Join(t.TempDir(), "data"), agentplugin.DefaultLimits(),
	)
	require.NoError(t, err)
	assert.Len(t, result.Packages(), 1)
	assert.Len(t, result.SkillEntries(), 1)
	assert.Empty(t, result.MCPDefinitions().List())
	assert.Contains(t, diagnosticCodes(result.Diagnostics()), "component_invalid")
}

func TestPackageBoundaryRejectsEscapingSkillAndCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not generally available to unprivileged Windows tests")
	}
	t.Parallel()

	root := t.TempDir()
	outside := t.TempDir()
	writeManifest(t, root, "boundary")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "skills", "escape"), 0o700))
	writeSkill(t, outside, "escape", "escape")
	require.NoError(t, os.Symlink(
		filepath.Join(outside, "skills", "escape", "SKILL.md"),
		filepath.Join(root, "skills", "escape", "SKILL.md"),
	))
	outsideCommand := filepath.Join(outside, "server")
	writeFile(t, outsideCommand, "#!/bin/sh\nexit 0\n")
	require.NoError(t, os.Chmod(outsideCommand, 0o700)) //nolint:gosec // Executable fixture requires owner execute.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "bin"), 0o700))
	require.NoError(t, os.Symlink(outsideCommand, filepath.Join(root, "bin", "server")))
	writeFile(t, filepath.Join(root, "mcp.json"), `{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json",
  "mcpServers": {"escape": {"type": "stdio", "command": "./bin/server"}}
}`)

	result, err := agentplugin.LoadDirectory(
		t.Context(), root, filepath.Join(t.TempDir(), "data"), agentplugin.DefaultLimits(),
	)
	require.NoError(t, err)
	assert.Empty(t, result.SkillEntries())
	assert.Empty(t, result.MCPDefinitions().List())
	assert.Contains(t, diagnosticCodes(result.Diagnostics()), "skill_invalid")
	assert.Contains(t, diagnosticCodes(result.Diagnostics()), "server_invalid")
}

func TestRemoteURLLoopbackRules(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeManifest(t, root, "remote-rules")
	servers := map[string]any{
		"localhost":      map[string]string{"type": "streamable-http", "url": "http://localhost:8080/mcp"},
		"ipv4":           map[string]string{"type": "streamable-http", "url": "http://127.0.0.1/mcp"},
		"ipv6":           map[string]string{"type": "streamable-http", "url": "http://[::1]/mcp"},
		"secure":         map[string]string{"type": "streamable-http", "url": "https://example.com/mcp"},
		"uppercase":      map[string]string{"type": "streamable-http", "url": "HTTPS://example.com/upper"},
		"insecure":       map[string]string{"type": "streamable-http", "url": "http://example.com/mcp"},
		"empty-fragment": map[string]string{"type": "streamable-http", "url": "https://example.com/mcp#"},
	}
	data, err := json.Marshal(map[string]any{"$schema": agentplugin.MCPSchema, "mcpServers": servers})
	require.NoError(t, err)
	writeFile(t, filepath.Join(root, "mcp.json"), string(data))

	result, err := agentplugin.LoadDirectory(
		t.Context(), root, filepath.Join(t.TempDir(), "data"), agentplugin.DefaultLimits(),
	)
	require.NoError(t, err)
	definitions := result.MCPDefinitions().List()
	assert.Len(t, definitions, 5)
	assert.Equal(t, 2, countDiagnosticCode(result.Diagnostics(), "server_invalid"))
	for _, definition := range definitions {
		assert.False(t, strings.HasPrefix(definition.URL, "HTTPS:"))
	}
}

func TestSkillsAcceptUnicodeNamesAndCompatibility(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeManifest(t, root, "unicode-skills")
	writeFile(t, filepath.Join(root, "skills", "审查", "SKILL.md"), `---
name: 审查
description: 审查代码并给出建议。
compatibility: 支持跨平台客户端。
---
遵循审查说明。
`)

	result, err := agentplugin.LoadDirectory(
		t.Context(), root, filepath.Join(t.TempDir(), "data"), agentplugin.DefaultLimits(),
	)
	require.NoError(t, err)
	require.Len(t, result.SkillEntries(), 1)
	assert.Equal(t, "审查", result.SkillEntries()[0].Skill.Name)
	assert.Empty(t, result.Diagnostics())
}

func TestSkillsRequireStrictAgentSkillsFrontmatter(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeManifest(t, root, "strict-skills")
	writeFile(t, filepath.Join(root, "skills", "empty-body", "SKILL.md"), `---
name: empty-body
description: A valid Skill may have an empty Markdown body.
---
`)
	writeFile(t, filepath.Join(root, "skills", "unknown", "SKILL.md"), `---
name: unknown
description: This includes a non-standard field.
custom-field: true
---
Instructions.
`)
	writeFile(t, filepath.Join(root, "skills", "metadata", "SKILL.md"), `---
name: metadata
description: This has non-string metadata.
metadata:
  priority: 1
---
Instructions.
`)
	writeFile(t, filepath.Join(root, "skills", "tools", "SKILL.md"), `---
name: tools
description: This uses a non-string allowed-tools value.
allowed-tools: [Read]
---
Instructions.
`)
	writeFile(t, filepath.Join(root, "skills", "compatibility", "SKILL.md"), `---
name: compatibility
description: This has an empty compatibility value.
compatibility: ""
---
Instructions.
`)

	result, err := agentplugin.LoadDirectory(
		t.Context(), root, filepath.Join(t.TempDir(), "data"), agentplugin.DefaultLimits(),
	)
	require.NoError(t, err)
	require.Len(t, result.SkillEntries(), 1)
	assert.Equal(t, "empty-body", result.SkillEntries()[0].Skill.Name)
	assert.Len(t, result.Diagnostics(), 4)
	for _, diagnostic := range result.Diagnostics() {
		assert.Equal(t, "skill_invalid", diagnostic.Code)
	}
}

func TestPluginDataIdentityPersistsAcrossManifestUpdates(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	dataBase := filepath.Join(t.TempDir(), "data")
	writeManifest(t, root, "before-update")
	before, err := agentplugin.LoadDirectory(
		t.Context(), root, dataBase, agentplugin.DefaultLimits(),
	)
	require.NoError(t, err)

	writeManifest(t, root, "after-update")
	after, err := agentplugin.LoadDirectory(
		t.Context(), root, dataBase, agentplugin.DefaultLimits(),
	)
	require.NoError(t, err)

	require.Len(t, before.Packages(), 1)
	require.Len(t, after.Packages(), 1)
	assert.Equal(t, before.Packages()[0].Data, after.Packages()[0].Data)
}

func TestPluginDataIdentityPersistsWhenInstalledSymlinkTargetChanges(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not generally available to unprivileged Windows tests")
	}
	t.Parallel()

	installRoot := t.TempDir()
	firstRoot := filepath.Join(installRoot, "release-1")
	secondRoot := filepath.Join(installRoot, "release-2")
	stableRoot := filepath.Join(installRoot, "installed")
	dataBase := filepath.Join(t.TempDir(), "data")
	for _, root := range []string{firstRoot, secondRoot} {
		writeManifest(t, root, "stable-plugin")
		writeFile(t, filepath.Join(root, "mcp.json"), `{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json",
  "mcpServers": {"server": {"type": "stdio", "command": "go"}}
}`)
	}
	require.NoError(t, os.Symlink(firstRoot, stableRoot))

	before, err := agentplugin.LoadDirectory(
		t.Context(), stableRoot, dataBase, agentplugin.DefaultLimits(),
	)
	require.NoError(t, err)
	require.Len(t, before.Packages(), 1)
	require.Len(t, before.MCPDefinitions().List(), 1)
	writeFile(t, filepath.Join(before.Packages()[0].Data, "state.txt"), "preserved")

	require.NoError(t, os.Remove(stableRoot))
	require.NoError(t, os.Symlink(secondRoot, stableRoot))
	after, err := agentplugin.LoadDirectory(
		t.Context(), stableRoot, dataBase, agentplugin.DefaultLimits(),
	)
	require.NoError(t, err)
	require.Len(t, after.Packages(), 1)
	require.Len(t, after.MCPDefinitions().List(), 1)

	assert.NotEqual(t, before.Packages()[0].Root, after.Packages()[0].Root)
	assert.Equal(t, before.Packages()[0].Data, after.Packages()[0].Data)
	assert.Equal(t, before.MCPDefinitions().List()[0].ID, after.MCPDefinitions().List()[0].ID)
	state, err := os.ReadFile(filepath.Join(after.Packages()[0].Data, "state.txt"))
	require.NoError(t, err)
	assert.Equal(t, "preserved", string(state))
}

func TestPluginDataRestoresOwnerWritePermission(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX owner permission bits do not apply on Windows")
	}
	t.Parallel()

	root := t.TempDir()
	dataBase := filepath.Join(t.TempDir(), "data")
	writeManifest(t, root, "writable-data")
	writeFile(t, filepath.Join(root, "mcp.json"), `{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json",
  "mcpServers": {"server": {"type": "stdio", "command": "go"}}
}`)
	first, err := agentplugin.LoadDirectory(
		t.Context(), root, dataBase, agentplugin.DefaultLimits(),
	)
	require.NoError(t, err)
	dataDirectory := first.Packages()[0].Data
	require.NoError(t, os.Chmod(dataDirectory, 0o500)) //nolint:gosec // Deliberately remove owner-write.

	_, err = agentplugin.LoadDirectory(
		t.Context(), root, dataBase, agentplugin.DefaultLimits(),
	)
	require.NoError(t, err)
	info, err := os.Stat(dataDirectory)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestEnvironmentReservedNamesUsePlatformSemantics(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeManifest(t, root, "environment-names")
	writeFile(t, filepath.Join(root, "mcp.json"), `{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json",
  "mcpServers": {
    "local": {
      "type": "stdio",
      "command": "go",
      "env": {"plugin_root": "configured"}
    }
  }
}`)

	result, err := agentplugin.LoadDirectory(
		t.Context(), root, filepath.Join(t.TempDir(), "data"), agentplugin.DefaultLimits(),
	)
	require.NoError(t, err)
	assert.Len(t, result.MCPDefinitions().List(), 1)
	assert.Empty(t, result.Diagnostics())
}

func definitionByTransport(
	t *testing.T,
	definitions []codingmcp.Definition,
	transport codingmcp.TransportType,
) codingmcp.Definition {
	t.Helper()
	for _, definition := range definitions {
		if definition.Transport == transport {
			return definition
		}
	}
	t.Fatalf("transport %q not found", transport)

	return codingmcp.Definition{}
}

func diagnosticCodes(diagnostics []agentplugin.Diagnostic) []string {
	values := make([]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		values = append(values, diagnostic.Code)
	}

	return values
}

func countDiagnosticCode(diagnostics []agentplugin.Diagnostic, code string) int {
	count := 0
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			count++
		}
	}

	return count
}

func writeManifest(t *testing.T, root, name string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "plugin.json"), `{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
  "name": "`+name+`"
}`)
}

func writeSkill(t *testing.T, root, directory, name string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "skills", directory, "SKILL.md"), `---
name: `+name+`
description: Review changes safely.
---
Follow the review instructions.
`)
}

func writeFile(t *testing.T, name, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o700))
	require.NoError(t, os.WriteFile(name, []byte(content), 0o600))
}
