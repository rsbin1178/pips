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

	loaded, err := codingmcp.LoadDefinitions(t.Context(), codingmcp.LoadOptions{
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

	loaded, err := codingmcp.LoadDefinitions(t.Context(), codingmcp.LoadOptions{
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

			_, err = codingmcp.LoadDefinitions(t.Context(), codingmcp.LoadOptions{
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

	_, err = codingmcp.LoadDefinitions(t.Context(), codingmcp.LoadOptions{
		Paths: layout, Tree: tree, ProjectTrusted: true, Limits: codingmcp.DefaultLimits(),
	})
	require.ErrorIs(t, err, codingmcp.ErrDuplicate)

	writeFile(t, layout.MCPFile(), strings.Repeat("x", 128))

	limits := codingmcp.DefaultLimits()
	limits.MaxFileBytes = 64
	_, err = codingmcp.LoadDefinitions(t.Context(), codingmcp.LoadOptions{
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

	_, err = codingmcp.LoadDefinitions(t.Context(), codingmcp.LoadOptions{
		Paths: layout, Limits: codingmcp.DefaultLimits(),
	})
	require.ErrorIs(t, err, codingmcp.ErrUnsafeFile)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}
