package mcp_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/internal/coding/execution"
	codingmcp "github.com/rsbin1178/pips/internal/coding/mcp"
	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewDefinitionsOwnsSessionEnvironmentAndMergeRejectsCollisions(t *testing.T) {
	t.Parallel()

	input := []codingmcp.Definition{{
		ID: "editor_tools", Scope: codingmcp.ScopeSession, Transport: codingmcp.TransportStdio,
		Command: "/bin/echo", Args: []string{"serve"},
		Environment:    []execution.EnvVar{{Name: "PIPS_TEST_MCP", Value: "one"}},
		ConnectTimeout: 5 * time.Second,
	}}
	definitions, err := codingmcp.NewDefinitions(input, 2)
	require.NoError(t, err)

	fingerprint := definitions.List()[0].Fingerprint()

	input[0].Args[0] = "changed"
	input[0].Environment[0].Value = "changed"
	listed := definitions.List()
	listed[0].Environment[0].Value = "listed-change"

	assert.Equal(t, "serve", definitions.List()[0].Args[0])
	assert.Equal(t, "one", definitions.List()[0].Environment[0].Value)
	assert.Equal(t, fingerprint, definitions.List()[0].Fingerprint())

	changed, err := codingmcp.NewDefinitions([]codingmcp.Definition{{
		ID: "editor_tools", Scope: codingmcp.ScopeSession, Transport: codingmcp.TransportStdio,
		Command: "/bin/echo", Environment: []execution.EnvVar{{Name: "PIPS_TEST_MCP", Value: "two"}},
		ConnectTimeout: 5 * time.Second,
	}}, 2)
	require.NoError(t, err)
	assert.NotEqual(t, fingerprint, changed.List()[0].Fingerprint())

	user, err := codingmcp.NewDefinitions([]codingmcp.Definition{{
		ID: "editor_tools", Scope: codingmcp.ScopeUser, Transport: codingmcp.TransportStdio,
		Command: "/bin/echo", ConnectTimeout: 5 * time.Second,
	}}, 2)
	require.NoError(t, err)
	_, err = user.Merge(definitions, 2)
	require.ErrorIs(t, err, codingmcp.ErrDuplicate)
}

func TestNewDefinitionsRejectsUnsafeSessionEnvironmentAndLimits(t *testing.T) {
	t.Parallel()

	definition := codingmcp.Definition{
		ID: "editor_tools", Scope: codingmcp.ScopeSession, Transport: codingmcp.TransportStdio,
		Command: "/bin/echo", Environment: []execution.EnvVar{{Name: "API_KEY", Value: "secret"}},
		ConnectTimeout: 5 * time.Second,
	}
	_, err := codingmcp.NewDefinitions([]codingmcp.Definition{definition}, 1)
	require.ErrorIs(t, err, codingmcp.ErrInvalid)
	assert.NotContains(t, err.Error(), "secret")

	definition.Environment = nil
	_, err = codingmcp.NewDefinitions([]codingmcp.Definition{definition}, 0)
	require.ErrorIs(t, err, codingmcp.ErrInvalid)

	second := definition
	second.ID = "editor_tools_two"
	_, err = codingmcp.NewDefinitions([]codingmcp.Definition{definition, second}, 1)
	require.ErrorIs(t, err, codingmcp.ErrLimitExceeded)
}

func TestLoadDefinitionsStrictScopedAndFingerprintStable(t *testing.T) {
	t.Parallel()

	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)
	writeFile(t, layout.MCPFile(), `{
  "schema":"pips.mcp/v1alpha1",
  "servers":[{
    "id":"remote_docs",
    "type":"streamable_http",
    "url":"https://mcp.example.test/api",
    "connect_timeout":"12s"
  }]
}`)

	workspaceRoot := t.TempDir()
	writeFile(t, filepath.Join(workspaceRoot, filepath.FromSlash(paths.ProjectMCPFile())), `{
  "schema":"pips.mcp/v1alpha1",
  "servers":[{
    "id":"local_fs",
    "type":"stdio",
    "command":"/usr/bin/env",
    "args":["go","run","./cmd/server"]
  }]
}`)
	opened, err := workspace.Open(workspaceRoot)
	require.NoError(t, err)
	tree, err := workspace.OpenTree(opened)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })

	loaded, _, err := codingmcp.LoadDefinitions(t.Context(), codingmcp.LoadOptions{
		Paths: layout, Tree: tree, ProjectTrusted: true, Limits: codingmcp.DefaultLimits(),
	})
	require.NoError(t, err)

	definitions := loaded.List()
	require.Len(t, definitions, 2)
	assert.Equal(t, codingmcp.ScopeUser, definitions[0].Scope)
	assert.Equal(t, codingmcp.ScopeProject, definitions[1].Scope)
	assert.Len(t, definitions[0].Fingerprint(), 64)
	projectFingerprint := definitions[1].Fingerprint()

	definitions[1].Args[0] = "changed"
	assert.NotEqual(t, projectFingerprint, definitions[1].Fingerprint())
	assert.Equal(t, projectFingerprint, loaded.List()[1].Fingerprint())
	assert.Equal(t, "go", loaded.List()[1].Args[0])
}

func TestMCPVisibilityDefaultsAmbientAndChangesFingerprint(t *testing.T) {
	t.Parallel()

	base := codingmcp.Definition{
		ID: "private_docs", Scope: codingmcp.ScopeUser,
		Transport: codingmcp.TransportStreamableHTTP, URL: "https://example.test/mcp",
		ConnectTimeout: time.Second,
	}
	explicit := base
	explicit.Visibility = codingmcp.VisibilityAmbient
	private := base
	private.Visibility = codingmcp.VisibilityAgentPrivate

	assert.Equal(t, codingmcp.VisibilityAmbient, base.EffectiveVisibility())
	assert.Equal(t, base.Fingerprint(), explicit.Fingerprint())
	legacy, err := json.Marshal(struct {
		ID        string                  `json:"id"`
		Transport codingmcp.TransportType `json:"transport"`
		URL       string                  `json:"url,omitempty"`
		TimeoutNS int64                   `json:"connect_timeout_ns"`
	}{
		ID: base.ID, Transport: base.Transport, URL: base.URL,
		TimeoutNS: int64(base.ConnectTimeout),
	})
	require.NoError(t, err)

	legacySum := sha256.Sum256(legacy)
	assert.Equal(t, hex.EncodeToString(legacySum[:]), base.Fingerprint(), "ambient definitions retain legacy permission fingerprints")
	assert.NotEqual(t, base.Fingerprint(), private.Fingerprint())

	_, err = codingmcp.NewDefinitions([]codingmcp.Definition{private}, 1)
	require.NoError(t, err)

	private.Visibility = "parent_and_private"
	_, err = codingmcp.NewDefinitions([]codingmcp.Definition{private}, 1)
	require.ErrorIs(t, err, codingmcp.ErrInvalid)
}

func TestLoadDefinitionsDoesNotInspectUntrustedProject(t *testing.T) {
	t.Parallel()

	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)

	loaded, _, err := codingmcp.LoadDefinitions(t.Context(), codingmcp.LoadOptions{
		Paths: layout, ProjectTrusted: false, Limits: codingmcp.DefaultLimits(),
	})
	require.NoError(t, err)
	assert.Empty(t, loaded.List())
}

func TestLoadDefinitionsRejectsUnsafeAndUnsupportedFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    error
	}{
		{
			name: "project enabled authority",
			content: `{"schema":"pips.mcp/v1alpha1","servers":[{
              "id":"bad","type":"stdio","command":"/bin/echo","enabled":true
            }]}`,
		},
		{
			name: "auth header",
			content: `{"schema":"pips.mcp/v1alpha1","servers":[{
              "id":"bad","type":"streamable_http","url":"https://example.test","headers":{"Authorization":"secret"}
            }]}`,
		},
		{
			name: "relative command",
			content: `{"schema":"pips.mcp/v1alpha1","servers":[{
              "id":"bad","type":"stdio","command":"server"
            }]}`,
			want: codingmcp.ErrInvalid,
		},
		{
			name: "duplicate ID",
			content: `{"schema":"pips.mcp/v1alpha1","servers":[
              {"id":"same","type":"stdio","command":"/bin/echo"},
              {"id":"same","type":"stdio","command":"/bin/echo"}
            ]}`,
			want: codingmcp.ErrDuplicate,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			layout, err := paths.New(t.TempDir())
			require.NoError(t, err)
			writeFile(t, layout.MCPFile(), test.content)

			_, _, err = codingmcp.LoadDefinitions(t.Context(), codingmcp.LoadOptions{
				Paths: layout, Limits: codingmcp.DefaultLimits(),
			})
			require.Error(t, err)

			if test.want != nil {
				assert.ErrorIs(t, err, test.want)
			}
		})
	}
}

func TestLoadDefinitionsRejectsDuplicateAcrossScopesAndOversizedInput(t *testing.T) {
	t.Parallel()

	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)

	definition := `{"schema":"pips.mcp/v1alpha1","servers":[{
      "id":"same","type":"stdio","command":"/bin/echo"
    }]}`
	writeFile(t, layout.MCPFile(), definition)

	workspaceRoot := t.TempDir()
	writeFile(t, filepath.Join(workspaceRoot, filepath.FromSlash(paths.ProjectMCPFile())), definition)
	opened, err := workspace.Open(workspaceRoot)
	require.NoError(t, err)
	tree, err := workspace.OpenTree(opened)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })

	_, _, err = codingmcp.LoadDefinitions(t.Context(), codingmcp.LoadOptions{
		Paths: layout, Tree: tree, ProjectTrusted: true, Limits: codingmcp.DefaultLimits(),
	})
	require.ErrorIs(t, err, codingmcp.ErrDuplicate)

	writeFile(t, layout.MCPFile(), strings.Repeat("x", 128))

	limits := codingmcp.DefaultLimits()
	limits.MaxFileBytes = 64
	_, _, err = codingmcp.LoadDefinitions(t.Context(), codingmcp.LoadOptions{
		Paths: layout, Limits: limits,
	})
	require.ErrorIs(t, err, codingmcp.ErrLimitExceeded)
}

func TestLoadDefinitionsRejectsUserSymlink(t *testing.T) {
	t.Parallel()

	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)
	target := filepath.Join(t.TempDir(), "mcp.json")
	writeFile(t, target, `{"schema":"pips.mcp/v1alpha1","servers":[]}`)
	require.NoError(t, os.Symlink(target, layout.MCPFile()))

	_, _, err = codingmcp.LoadDefinitions(t.Context(), codingmcp.LoadOptions{
		Paths: layout, Limits: codingmcp.DefaultLimits(),
	})
	require.ErrorIs(t, err, codingmcp.ErrUnsafeFile)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func TestLoadDefinitionsV1Alpha2MatchesAgentPluginShape(t *testing.T) {
	t.Parallel()

	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)
	writeFile(t, layout.MCPFile(), `{
  "schema": "pips.mcp/v1alpha2",
  "mcpServers": {
    "exa": {
      "type": "streamable-http",
      "url": "https://mcp.exa.ai/mcp",
      "headers": {"x-api-key": "${env:EXA_API_KEY}"}
    },
    "context7": {
      "type": "http",
      "url": "https://mcp.context7.com/mcp",
      "headers": {"CONTEXT7_API_KEY": "${env:CONTEXT7_API_KEY}"},
      "connect_timeout": "30s",
      "visibility": "agent_private"
    },
    "GitHub Tools": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": {"GITHUB_PERSONAL_ACCESS_TOKEN": "${env:GITHUB_TOKEN}"}
    }
  }
}`)

	loaded, diagnostics, err := codingmcp.LoadDefinitions(t.Context(), codingmcp.LoadOptions{
		Paths: layout, Limits: codingmcp.DefaultLimits(),
	})
	require.NoError(t, err)
	assert.Empty(t, diagnostics)

	definitions := loaded.List()
	require.Len(t, definitions, 3)

	byID := make(map[string]codingmcp.Definition, len(definitions))
	for _, definition := range definitions {
		assert.Equal(t, codingmcp.ScopeUser, definition.Scope)
		byID[definition.ID] = definition
	}

	exa := byID["exa"]
	assert.Equal(t, codingmcp.TransportStreamableHTTP, exa.Transport)
	assert.Equal(t, []codingmcp.HTTPHeader{{Name: "x-api-key", Value: "${env:EXA_API_KEY}"}}, exa.Headers)
	assert.Equal(t, 10*time.Second, exa.ConnectTimeout)
	assert.Equal(t, codingmcp.VisibilityAmbient, exa.EffectiveVisibility())

	context7 := byID["context7"]
	assert.Equal(t, codingmcp.TransportStreamableHTTP, context7.Transport)
	assert.Equal(t, 30*time.Second, context7.ConnectTimeout)
	assert.Equal(t, codingmcp.VisibilityAgentPrivate, context7.EffectiveVisibility())

	github := byID["github-tools"]
	assert.Equal(t, codingmcp.TransportStdio, github.Transport)
	assert.Equal(t, "npx", github.Command)
	assert.Equal(t, []execution.EnvVar{{Name: "GITHUB_PERSONAL_ACCESS_TOKEN", Value: "${env:GITHUB_TOKEN}"}}, github.Environment)

	changed := exa
	changed.Headers = []codingmcp.HTTPHeader{{Name: "x-api-key", Value: "${env:OTHER_KEY}"}}
	assert.NotEqual(t, exa.Fingerprint(), changed.Fingerprint(), "the reference, not the secret, is fingerprinted")
}

func TestLoadDefinitionsV1Alpha2IsolatesInvalidEntries(t *testing.T) {
	t.Parallel()

	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)
	writeFile(t, layout.MCPFile(), `{
  "schema": "pips.mcp/v1alpha2",
  "mcpServers": {
    "a-good": {"type": "streamable-http", "url": "https://good.example.test/mcp"},
    "b-insecure": {"type": "streamable-http", "url": "http://remote.example.test/mcp"},
    "c-legacy": {"type": "sse", "url": "https://legacy.example.test/sse"},
    "d-bad-ref": {"command": "uvx", "env": {"TOKEN": "${env:1BAD}"}},
    "e-dup": {"command": "uvx"},
    "E Dup": {"command": "uvx"},
    "f-unknown": {"command": "uvx", "cwd": "./data"},
    "g-zero-timeout": {"command": "uvx", "connect_timeout": "0s"}
  }
}`)

	loaded, diagnostics, err := codingmcp.LoadDefinitions(t.Context(), codingmcp.LoadOptions{
		Paths: layout, Limits: codingmcp.DefaultLimits(),
	})
	require.NoError(t, err)

	ids := make([]string, 0)
	for _, definition := range loaded.List() {
		ids = append(ids, definition.ID)
	}

	assert.Equal(t, []string{"e-dup", "a-good"}, ids, "names are processed in byte order, as in Agent Plugins")

	codes := make(map[string]string, len(diagnostics))
	for _, diagnostic := range diagnostics {
		assert.Equal(t, "configuration", diagnostic.Stage)
		assert.Contains(t, diagnostic.Message, "user MCP server")
		codes[diagnostic.ServerID] = diagnostic.Code
	}

	assert.Equal(t, map[string]string{
		"b-insecure":     "definition_invalid",
		"c-legacy":       "transport_unsupported",
		"d-bad-ref":      "definition_invalid",
		"e-dup":          "definition_duplicate",
		"f-unknown":      "definition_invalid",
		"g-zero-timeout": "definition_invalid",
	}, codes)
}

func TestLoadDefinitionsV1Alpha2FileLevelViolationsFailClosed(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"standard file without schema": `{"mcpServers":{"exa":{"type":"http","url":"https://mcp.exa.ai/mcp"}}}`,
		"unknown top-level field":      `{"schema":"pips.mcp/v1alpha2","mcpServers":{},"servers":[]}`,
		"missing mcpServers":           `{"schema":"pips.mcp/v1alpha2"}`,
		"non-object mcpServers":        `{"schema":"pips.mcp/v1alpha2","mcpServers":[]}`,
		"duplicate nested key":         `{"schema":"pips.mcp/v1alpha2","mcpServers":{"a":{"command":"x","command":"y"}}}`,
		"invalid JSON":                 `{"schema":"pips.mcp/v1alpha2",`,
	}

	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			layout, err := paths.New(t.TempDir())
			require.NoError(t, err)
			writeFile(t, layout.MCPFile(), content)

			_, _, err = codingmcp.LoadDefinitions(t.Context(), codingmcp.LoadOptions{
				Paths: layout, Limits: codingmcp.DefaultLimits(),
			})
			require.Error(t, err)
		})
	}

	layout, err := paths.New(t.TempDir())
	require.NoError(t, err)
	writeFile(t, layout.MCPFile(), tests["standard file without schema"])
	_, _, err = codingmcp.LoadDefinitions(t.Context(), codingmcp.LoadOptions{
		Paths: layout, Limits: codingmcp.DefaultLimits(),
	})
	require.ErrorIs(t, err, codingmcp.ErrInvalid)
	assert.Contains(t, err.Error(), codingmcp.DefinitionSchema, "the error names the schema to add")
}
