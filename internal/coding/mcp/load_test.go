package mcp_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	codingmcp "github.com/rsbin/pips/internal/coding/mcp"
	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
