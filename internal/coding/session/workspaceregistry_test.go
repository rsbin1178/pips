package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	registryWorkspaceID   = "8ca7bed7d6581b2bc73c047419e42404faf0a1f85728a02038a3738e063b04e7"
	registryWorkspacePath = "/workspace/hub"
)

// writeRegistry writes a pre-v2 workspace registry beside a store root.
func writeRegistry(t *testing.T, storeRoot, schema, body string) string {
	t.Helper()

	path := DefaultLegacyWorkspaceRegistryPath(storeRoot)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))

	if body == "" {
		body = `{"schema":"` + schema + `","workspaces":{"` + registryWorkspaceID +
			`":{"path":"` + registryWorkspacePath + `","device":1,"inode":2}}}`
	}

	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	return path
}

// seedLegacyTranscript plants one flat pre-v2 session with the given header extra.
func seedLegacyTranscript(t *testing.T, root, id string, extra map[string]string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(root, 0o700))

	store, err := harness.CreateJSONL(filepath.Join(root, id+legacyTranscriptSuffix), id, extra)
	require.NoError(t, err)

	when := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	require.NoError(t, store.Append(harness.Entry{
		Kind: harness.KindMessage, ID: "m1", Time: when, Message: ai.UserText("legacy prompt"),
	}))
	require.NoError(t, store.Close())
}

func TestLoadLegacyWorkspaceRegistry(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "sessions")
	require.NoError(t, os.MkdirAll(root, 0o700))

	t.Run("resolves a recorded identity", func(t *testing.T) {
		t.Parallel()

		path := writeRegistry(t, root, legacyWorkspaceRegistrySchema, "")
		registry := LoadLegacyWorkspaceRegistry(path)
		require.Equal(t, 1, registry.Len())

		got, ok := registry.Path(registryWorkspaceID)
		require.True(t, ok)
		assert.Equal(t, registryWorkspacePath, got)

		_, ok = registry.Path("not-a-key")
		assert.False(t, ok)
	})

	t.Run("tolerates anything it cannot use", func(t *testing.T) {
		t.Parallel()

		for name, body := range map[string]string{
			"missing":       "",
			"malformed":     "{not json",
			"wrong schema":  `{"schema":"pips.workspaces/v9","workspaces":{"k":{"path":"/p"}}}`,
			"empty path":    `{"schema":"` + legacyWorkspaceRegistrySchema + `","workspaces":{"k":{"path":"  "}}}`,
			"no workspaces": `{"schema":"` + legacyWorkspaceRegistrySchema + `"}`,
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				dir := filepath.Join(t.TempDir(), "sessions")
				require.NoError(t, os.MkdirAll(dir, 0o700))

				path := filepath.Join(t.TempDir(), "absent.json")
				if body != "" {
					path = writeRegistry(t, dir, legacyWorkspaceRegistrySchema, body)
				}

				assert.Equal(t, 0, LoadLegacyWorkspaceRegistry(path).Len())
			})
		}
	})
}

func TestResolveLegacyWorkspace(t *testing.T) {
	t.Parallel()

	registry := LegacyWorkspaceRegistry{paths: map[string]string{registryWorkspaceID: registryWorkspacePath}}

	t.Run("a recorded path wins", func(t *testing.T) {
		t.Parallel()

		id, path := resolveLegacyWorkspace(map[string]string{
			extraWorkspaceID:   "other-id",
			extraWorkspacePath: "/workspace/direct",
		}, registry)
		assert.Equal(t, "other-id", id)
		assert.Equal(t, "/workspace/direct", path)
	})

	// This is the case that produced a session filed under "unknown" while its
	// real workspace was still recoverable.
	t.Run("an identity without a path is resolved through the registry", func(t *testing.T) {
		t.Parallel()

		id, path := resolveLegacyWorkspace(map[string]string{
			extraWorkspaceID: registryWorkspaceID,
		}, registry)
		assert.Equal(t, registryWorkspaceID, id)
		assert.Equal(t, registryWorkspacePath, path)
	})

	t.Run("an unmapped identity keeps its identity", func(t *testing.T) {
		t.Parallel()

		id, path := resolveLegacyWorkspace(map[string]string{extraWorkspaceID: "unmapped"}, registry)
		assert.Equal(t, "unmapped", id, "a known identity is never discarded")
		assert.Empty(t, path)
	})

	t.Run("a header with no identity is unknown", func(t *testing.T) {
		t.Parallel()

		id, path := resolveLegacyWorkspace(map[string]string{}, registry)
		assert.Equal(t, legacyUnknownWorkspaceID, id)
		assert.Empty(t, path)
	})
}

// TestMigrateGroupsByIdentityWhenThePathIsMissing is the regression test for the
// bug where a header that recorded only the workspace identity was filed under
// the unknown group even though the registry could still resolve its path.
func TestMigrateGroupsByIdentityWhenThePathIsMissing(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	root := filepath.Join(base, "sessions")

	seedLegacyTranscript(t, root, "s-identityonly", map[string]string{
		extraWorkspaceID: registryWorkspaceID,
		extraKind:        string(KindConversation),
	})
	writeRegistry(t, root, legacyWorkspaceRegistrySchema, "")

	report, err := MigrateLegacyStore(t.Context(), MigrateOptions{Root: root, Apply: true})
	require.NoError(t, err)
	require.Len(t, report.Migrated, 1)

	layout, err := NewStoreLayout(root)
	require.NoError(t, err)

	expected := layout.SessionDir(registryWorkspacePath, "s-identityonly")
	assert.Equal(t, expected, report.Migrated[0].SessionDir)
	assert.Equal(t, registryWorkspacePath, report.Migrated[0].WorkspacePath)

	record, err := ReadMetadataRecord(filepath.Join(expected, MetadataFileName))
	require.NoError(t, err)
	assert.Equal(t, registryWorkspaceID, record.Workspace.ID)
	assert.Equal(t, registryWorkspacePath, record.Workspace.Path)

	_, err = os.Stat(layout.SessionDir("", "s-identityonly"))
	require.ErrorIs(t, err, os.ErrNotExist, "nothing lands in the unknown group")
}
