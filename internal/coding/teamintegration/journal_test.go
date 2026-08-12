//nolint:gosec,paralleltest,wsl_v5 // WAL fixtures use workspace modes and ordered convergence assertions.
package teamintegration

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/rsbin1178/pips/internal/jsonx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifyJournalDistinguishesBaseTargetAndUnknown(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "base.txt"), []byte("base"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "target.txt"), []byte("target"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "unknown.txt"), []byte("other"), 0o644))
	workspaceValue, err := workspace.Open(root)
	require.NoError(t, err)
	manifest, err := BuildManifest(
		t.Context(),
		blobMap{"base": []byte("base"), "target": []byte("target")},
		[]Entry{
			{Mode: "100644", OID: "base", Path: "base.txt"},
			{Mode: "100644", OID: "base", Path: "target.txt"},
			{Mode: "100644", OID: "base", Path: "unknown.txt"},
		},
		[]Entry{
			{Mode: "100644", OID: "target", Path: "base.txt"},
			{Mode: "100644", OID: "target", Path: "target.txt"},
			{Mode: "100644", OID: "target", Path: "unknown.txt"},
		},
		DefaultLimits(),
	)
	require.NoError(t, err)
	now := time.Now().UTC()
	journal := applyJournal{
		Schema: journalSchema, ID: "int-11111111111111111111111111111111",
		WorkspacePath: root, WorkspaceIdentity: workspaceValue.Identity().Key(),
		CommonIdentity: "common", BranchRef: "refs/heads/main",
		HeadOID: "head", IndexDigest: "index", IntegrationCommit: "commit",
		IntegrationTree: "tree", Manifest: manifest, State: journalApplying,
		CreatedAt: now, UpdatedAt: now,
	}
	openedRoot, err := os.OpenRoot(root)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, openedRoot.Close()) })
	base, target, unknown, err := classifyManifestPaths(
		openedRoot, journal.Manifest, DefaultLimits().BlobBytes,
	)
	require.NoError(t, err)
	assert.Equal(t, 1, base)
	assert.Equal(t, 1, target)
	assert.Equal(t, 1, unknown)
}

func TestJournalRejectsManifestDigestTampering(t *testing.T) {
	manifest, err := BuildManifest(
		t.Context(), blobMap{"base": []byte("base"), "target": []byte("target")},
		[]Entry{{Mode: "100644", OID: "base", Path: "file.txt"}},
		[]Entry{{Mode: "100644", OID: "target", Path: "file.txt"}},
		DefaultLimits(),
	)
	require.NoError(t, err)
	manifest.Entries[0].Target.SHA256 = manifest.Entries[0].Base.SHA256
	now := time.Now().UTC()
	err = validateJournal(applyJournal{
		Schema: journalSchema, ID: "int-22222222222222222222222222222222",
		WorkspacePath: t.TempDir(), WorkspaceIdentity: "workspace", CommonIdentity: "common",
		BranchRef: "refs/heads/main", HeadOID: "head", IndexDigest: "index",
		IntegrationCommit: "commit", IntegrationTree: "tree", Manifest: manifest,
		State: journalApplying, CreatedAt: now, UpdatedAt: now,
	})
	require.ErrorIs(t, err, ErrInvalid)
}

func TestReadJournalRejectsDuplicateKeys(t *testing.T) {
	root := t.TempDir()
	integrationsRoot := filepath.Join(root, "integrations")
	require.NoError(t, os.Mkdir(integrationsRoot, 0o700))
	rootWorkspace, err := workspace.Open(integrationsRoot)
	require.NoError(t, err)

	manifest, err := BuildManifest(
		t.Context(), blobMap{"base": []byte("base"), "target": []byte("target")},
		[]Entry{{Mode: "100644", OID: "base", Path: "file.txt"}},
		[]Entry{{Mode: "100644", OID: "target", Path: "file.txt"}},
		DefaultLimits(),
	)
	require.NoError(t, err)
	id := "int-33333333333333333333333333333333"
	now := time.Now().UTC()
	encoded, err := json.Marshal(applyJournal{
		Schema: journalSchema, ID: id, WorkspacePath: root,
		WorkspaceIdentity: "workspace", CommonIdentity: "common",
		BranchRef: "refs/heads/main", HeadOID: "head", IndexDigest: "index",
		IntegrationCommit: "commit", IntegrationTree: "tree", Manifest: manifest,
		State: journalApplying, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	encoded = []byte(strings.Replace(
		string(encoded), `"schema":`, `"schema":"duplicate","schema":`, 1,
	))
	directory := filepath.Join(integrationsRoot, id)
	require.NoError(t, os.Mkdir(directory, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(directory, journalFileName), encoded, 0o600))

	manager := &Manager{integrationsRoot: rootWorkspace, limits: DefaultLimits()}
	_, err = manager.readJournal(id)
	require.ErrorIs(t, err, jsonx.ErrDuplicateKey)
}

func TestCountUnrelatedStatusPaths(t *testing.T) {
	manifest := Manifest{Entries: []ManifestEntry{
		{Path: "a.txt"},
		{Path: "nested/b.txt"},
	}}

	assert.Zero(t, countUnrelatedStatusPaths(
		[]string{"a.txt", "nested/b.txt"}, manifest,
	))
	assert.Equal(t, 2, countUnrelatedStatusPaths(
		[]string{"a.txt", "outside.txt", "nested/other.txt"}, manifest,
	))
}

func TestConvergeManifestCompletesAndRollsBackMixedState(t *testing.T) {
	t.Parallel()

	blobs := blobMap{
		"base-a": []byte("base-a"), "base-b": []byte("base-b"),
		"target-a": []byte("target-a"), "target-b": []byte("target-b"),
	}
	manifest, err := BuildManifest(
		t.Context(), blobs,
		[]Entry{
			{Mode: "100644", OID: "base-a", Path: "a.txt"},
			{Mode: "100644", OID: "base-b", Path: "b.txt"},
		},
		[]Entry{
			{Mode: "100644", OID: "target-a", Path: "a.txt"},
			{Mode: "100644", OID: "target-b", Path: "b.txt"},
		},
		DefaultLimits(),
	)
	require.NoError(t, err)

	for _, test := range []struct {
		name   string
		action RecoveryAction
		base   int
		target int
	}{
		{name: "complete", action: RecoveryComplete, target: 2},
		{name: "rollback", action: RecoveryRollback, base: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			rootPath := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(rootPath, "a.txt"), blobs["base-a"], 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(rootPath, "b.txt"), blobs["target-b"], 0o644))
			root, err := os.OpenRoot(rootPath)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, root.Close()) })
			manager := &Manager{
				limits: DefaultLimits(),
				readBlob: func(_ context.Context, _, oid string) ([]byte, error) {
					return blobs.Blob(t.Context(), oid)
				},
			}
			require.NoError(t, manager.convergeManifest(
				t.Context(), root, rootPath, manifest, test.action,
			))
			base, target, unknown, err := classifyManifestPaths(
				root, manifest, DefaultLimits().BlobBytes,
			)
			require.NoError(t, err)
			assert.Equal(t, test.base, base)
			assert.Equal(t, test.target, target)
			assert.Zero(t, unknown)
		})
	}
}

func TestConvergeManifestRefusesUnknownState(t *testing.T) {
	t.Parallel()

	blobs := blobMap{"base": []byte("base"), "target": []byte("target")}
	manifest, err := BuildManifest(
		t.Context(), blobs,
		[]Entry{{Mode: "100644", OID: "base", Path: "file.txt"}},
		[]Entry{{Mode: "100644", OID: "target", Path: "file.txt"}},
		DefaultLimits(),
	)
	require.NoError(t, err)
	rootPath := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(rootPath, "file.txt"), []byte("unknown"), 0o644))
	root, err := os.OpenRoot(rootPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	manager := &Manager{
		limits: DefaultLimits(),
		readBlob: func(_ context.Context, _, oid string) ([]byte, error) {
			return blobs.Blob(t.Context(), oid)
		},
	}
	require.ErrorIs(t, manager.convergeManifest(
		t.Context(), root, rootPath, manifest, RecoveryComplete,
	), ErrRecovery)
	assert.Equal(t, "unknown", string(mustReadRootFile(t, root, "file.txt")))
}

func mustReadRootFile(t *testing.T, root *os.Root, path string) []byte {
	t.Helper()
	file, err := root.Open(path)
	require.NoError(t, err)
	value, readErr := io.ReadAll(file)
	closeErr := file.Close()
	require.NoError(t, readErr)
	require.NoError(t, closeErr)

	return value
}
