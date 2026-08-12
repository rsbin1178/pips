package agentplugin_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/internal/coding/agentplugin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanonicalSchemaIdentifiers(t *testing.T) {
	t.Parallel()

	assert.Equal(t,
		"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
		agentplugin.ManifestSchema,
	)
	assert.Equal(t,
		"https://agent-plugins.org/schemas/1.0.0/mcp.schema.json",
		agentplugin.MCPSchema,
	)
}

func TestManifestConformanceMatrix(t *testing.T) {
	t.Parallel()

	valid := []struct {
		name               string
		manifest           string
		diagnosticCode     string
		diagnosticQuantity int
	}{
		{
			name:     "minimal",
			manifest: `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"a"}`,
		},
		{
			name: "all metadata types",
			manifest: `{
  "$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
  "name":"acme.tools-1","version":"opaque","description":"","author":{},
  "homepage":"opaque","repository":"opaque","license":"opaque","keywords":[]
}`,
		},
		{
			name: "unimplemented extension contents are opaque",
			manifest: `{
  "$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
  "name":"extension-values","extensions":{"com.example.client":7}
}`,
		},
		{
			name: "each unknown field reported",
			manifest: `{
  "$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
  "name":"unknown-fields","one":1,"two":2
}`,
			diagnosticCode: "unknown_field_ignored", diagnosticQuantity: 2,
		},
	}
	for _, test := range valid {
		t.Run("valid/"+test.name, func(t *testing.T) {
			t.Parallel()

			result, err := loadManifestFixture(t, test.manifest)
			require.NoError(t, err)
			require.Len(t, result.Packages(), 1)
			assert.Equal(t, test.diagnosticQuantity,
				countDiagnosticCode(result.Diagnostics(), test.diagnosticCode))
		})
	}

	for _, value := range []string{"null", "false", "7", `"value"`, "[]"} {
		t.Run("recover non-object extensions/"+value, func(t *testing.T) {
			t.Parallel()

			result, err := loadManifestFixture(t, `{
  "$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
  "name":"extension-recovery","extensions":`+value+`
}`)
			require.NoError(t, err)
			require.Len(t, result.Packages(), 1)
			assert.Equal(t, 1, countDiagnosticCode(result.Diagnostics(), "extensions_ignored"))
		})
	}

	invalid := []struct {
		name     string
		manifest string
	}{
		{name: "array root", manifest: `[]`},
		{name: "missing schema", manifest: `{"name":"missing-schema"}`},
		{name: "wrong schema type", manifest: `{"$schema":1,"name":"wrong-schema-type"}`},
		{name: "unsupported schema", manifest: `{"$schema":"other","name":"wrong-schema"}`},
		{name: "missing name", manifest: `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"}`},
		{name: "empty name", manifest: manifestWithName("")},
		{name: "uppercase name", manifest: manifestWithName("Upper")},
		{name: "leading punctuation", manifest: manifestWithName("-leading")},
		{name: "trailing punctuation", manifest: manifestWithName("trailing.")},
		{name: "double hyphen", manifest: manifestWithName("double--hyphen")},
		{name: "double period", manifest: manifestWithName("double..period")},
		{name: "underscore", manifest: manifestWithName("under_score")},
		{name: "more than 64 characters", manifest: manifestWithName(strings.Repeat("a", 65))},
		{name: "null version", manifest: manifestWithField("version", "null")},
		{name: "numeric description", manifest: manifestWithField("description", "1")},
		{name: "array author", manifest: manifestWithField("author", "[]")},
		{name: "unknown author field", manifest: manifestWithField("author", `{"company":"x"}`)},
		{name: "non-string author field", manifest: manifestWithField("author", `{"name":1}`)},
		{name: "null keywords", manifest: manifestWithField("keywords", "null")},
		{name: "non-string keyword", manifest: manifestWithField("keywords", `["valid",1]`)},
	}
	for _, test := range invalid {
		t.Run("invalid/"+test.name, func(t *testing.T) {
			t.Parallel()

			result, err := loadManifestFixture(t, test.manifest)
			require.ErrorIs(t, err, agentplugin.ErrInvalid)
			assert.Empty(t, result.Packages())
		})
	}
}

func TestMCPTopLevelConformanceMatrix(t *testing.T) {
	t.Parallel()

	invalid := map[string]string{
		"array root":        `[]`,
		"missing schema":    `{"mcpServers":{}}`,
		"wrong schema":      `{"$schema":"other","mcpServers":{}}`,
		"missing servers":   `{"$schema":"https://agent-plugins.org/schemas/1.0.0/mcp.schema.json"}`,
		"null servers":      `{"$schema":"https://agent-plugins.org/schemas/1.0.0/mcp.schema.json","mcpServers":null}`,
		"array servers":     `{"$schema":"https://agent-plugins.org/schemas/1.0.0/mcp.schema.json","mcpServers":[]}`,
		"unknown top field": `{"$schema":"https://agent-plugins.org/schemas/1.0.0/mcp.schema.json","mcpServers":{},"extra":true}`,
	}
	for name, mcp := range invalid {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			result := loadMCPFixture(t, json.RawMessage(mcp))
			assert.Len(t, result.Packages(), 1)
			assert.Empty(t, result.MCPDefinitions().List())
			assert.Equal(t, 1, countDiagnosticCode(result.Diagnostics(), "component_invalid"))
		})
	}
}

func TestMCPServerConformanceMatrix(t *testing.T) {
	t.Parallel()

	valid := []struct {
		name   string
		server any
	}{
		{name: "minimal stdio", server: map[string]any{"type": "stdio", "command": "go"}},
		{name: "stdio optional fields", server: map[string]any{
			"type": "stdio", "command": "go", "args": []string{},
			"env": map[string]string{"plugin_root": "configured", "Path": "one", "PATH": "two"},
			"cwd": "${PLUGIN_ROOT}",
		}},
		{name: "HTTPS", server: map[string]any{"type": "streamable-http", "url": "HTTPS://example.com/mcp"}},
		{name: "localhost HTTP", server: map[string]any{"type": "streamable-http", "url": "http://localhost/mcp"}},
		{name: "IPv4 loopback HTTP", server: map[string]any{"type": "streamable-http", "url": "http://127.0.0.1/mcp"}},
		{name: "IPv6 loopback HTTP", server: map[string]any{"type": "streamable-http", "url": "http://[::1]/mcp"}},
		{name: "literal headers", server: map[string]any{
			"type": "streamable-http", "url": "https://example.com/mcp",
			"headers": map[string]string{"X-Tenant": "public"},
		}},
	}
	for _, test := range valid {
		t.Run("valid/"+test.name, func(t *testing.T) {
			t.Parallel()

			result := loadMCPFixture(t, test.server)
			assert.Len(t, result.MCPDefinitions().List(), 1)
			assert.Empty(t, result.Diagnostics())
		})
	}

	invalid := []struct {
		name   string
		server any
	}{
		{name: "null entry", server: nil},
		{name: "scalar entry", server: 7},
		{name: "missing type", server: map[string]any{"command": "go"}},
		{name: "unknown type", server: map[string]any{"type": "websocket", "url": "https://example.com"}},
		{name: "stdio missing command", server: map[string]any{"type": "stdio"}},
		{name: "stdio empty command", server: map[string]any{"type": "stdio", "command": ""}},
		{name: "stdio absolute command", server: map[string]any{"type": "stdio", "command": "/bin/sh"}},
		{name: "stdio null args", server: map[string]any{"type": "stdio", "command": "go", "args": nil}},
		{name: "stdio non-string arg", server: map[string]any{"type": "stdio", "command": "go", "args": []any{1}}},
		{name: "stdio null env", server: map[string]any{"type": "stdio", "command": "go", "env": nil}},
		{name: "stdio non-string env", server: map[string]any{"type": "stdio", "command": "go", "env": map[string]any{"A": 1}}},
		{name: "stdio reserved root", server: map[string]any{"type": "stdio", "command": "go", "env": map[string]string{"PLUGIN_ROOT": "x"}}},
		{name: "stdio reserved data", server: map[string]any{"type": "stdio", "command": "go", "env": map[string]string{"PLUGIN_DATA": "x"}}},
		{name: "stdio invalid cwd", server: map[string]any{"type": "stdio", "command": "go", "cwd": "data"}},
		{name: "stdio cross-variant field", server: map[string]any{"type": "stdio", "command": "go", "url": "https://example.com"}},
		{name: "HTTP missing URL", server: map[string]any{"type": "streamable-http"}},
		{name: "HTTP relative URL", server: map[string]any{"type": "streamable-http", "url": "/mcp"}},
		{name: "HTTP user information", server: map[string]any{"type": "streamable-http", "url": "https://user@example.com/mcp"}},
		{name: "HTTP empty fragment", server: map[string]any{"type": "streamable-http", "url": "https://example.com/mcp#"}},
		{name: "HTTP fragment", server: map[string]any{"type": "streamable-http", "url": "https://example.com/mcp#value"}},
		{name: "remote insecure HTTP", server: map[string]any{"type": "streamable-http", "url": "http://example.com/mcp"}},
		{name: "localhost suffix HTTP", server: map[string]any{"type": "streamable-http", "url": "http://localhost.example/mcp"}},
		{name: "null headers", server: map[string]any{"type": "streamable-http", "url": "https://example.com", "headers": nil}},
		{name: "non-string header", server: map[string]any{"type": "streamable-http", "url": "https://example.com", "headers": map[string]any{"X": 1}}},
		{name: "case duplicate header", server: map[string]any{
			"type": "streamable-http", "url": "https://example.com",
			"headers": map[string]string{"X-Test": "one", "x-test": "two"},
		}},
		{name: "invalid header field", server: map[string]any{
			"type": "streamable-http", "url": "https://example.com",
			"headers": map[string]string{"Bad Header": "value"},
		}},
		{name: "HTTP cross-variant field", server: map[string]any{
			"type": "streamable-http", "url": "https://example.com", "command": "go",
		}},
	}
	for _, test := range invalid {
		t.Run("invalid/"+test.name, func(t *testing.T) {
			t.Parallel()

			result := loadMCPFixture(t, test.server)
			assert.Empty(t, result.MCPDefinitions().List())
			assert.Equal(t, 1, countDiagnosticCode(result.Diagnostics(), "server_invalid"))
		})
	}

	t.Run("unsupported valid SSE", func(t *testing.T) {
		t.Parallel()

		result := loadMCPFixture(t, map[string]any{
			"type": "sse", "url": "https://example.com/events",
		})
		assert.Empty(t, result.MCPDefinitions().List())
		assert.Equal(t, 1, countDiagnosticCode(result.Diagnostics(), "transport_unsupported"))
	})
}

func loadManifestFixture(t *testing.T, manifest string) (agentplugin.Result, error) {
	t.Helper()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "plugin.json"), manifest)

	return agentplugin.LoadDirectory(
		t.Context(), root, filepath.Join(t.TempDir(), "data"), agentplugin.DefaultLimits(),
	)
}

func loadMCPFixture(t *testing.T, server any) agentplugin.Result {
	t.Helper()

	root := t.TempDir()
	writeManifest(t, root, "mcp-conformance")

	var configuration any
	if raw, ok := server.(json.RawMessage); ok {
		configuration = raw
	} else {
		configuration = map[string]any{
			"$schema":    agentplugin.MCPSchema,
			"mcpServers": map[string]any{"server": server},
		}
	}

	data, err := json.Marshal(configuration)
	require.NoError(t, err)
	writeFile(t, filepath.Join(root, "mcp.json"), string(data))

	result, err := agentplugin.LoadDirectory(
		t.Context(), root, filepath.Join(t.TempDir(), "data"), agentplugin.DefaultLimits(),
	)
	require.NoError(t, err)

	return result
}

func manifestWithName(name string) string {
	data, _ := json.Marshal(map[string]any{"$schema": agentplugin.ManifestSchema, "name": name})

	return string(data)
}

func manifestWithField(name, value string) string {
	return `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"metadata","` +
		name + `":` + value + `}`
}
