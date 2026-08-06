//nolint:wsl_v5,gosec,paralleltest // Filesystem fixtures intentionally model executable artifacts.
package pluginstore_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/rsbin/pips/internal/coding/pluginstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseManifestStrictAndSelectsIndependentTargets(t *testing.T) {
	t.Parallel()

	digest := strings.Repeat("a", 64)
	data := []byte(`{
  "schema":"pips.plugin/v1alpha1",
  "id":"example.review",
  "version":"1.4.0",
  "protocol":{"major":1,"min_minor":2,"max_minor":3},
  "targets":[
    {"os":"linux","arch":"amd64","path":"artifacts/linux/plugin","sha256":"` + digest + `"},
    {"os":"darwin","arch":"arm64","path":"artifacts/darwin/plugin","sha256":"` + digest + `"},
    {"os":"windows","arch":"amd64","path":"artifacts/windows/plugin.exe","sha256":"` + digest + `"}
  ],
  "capability_requests":["tools.v1"],
  "state":{"schema":"example.review/state/v2"},
  "provenance":{"publisher":"example","source":"registry/example","signature":"sig"}
}`)

	manifest, err := pluginstore.ParseManifest(data)
	require.NoError(t, err)
	assert.Equal(t, pluginstore.ManifestSchema, manifest.Schema)
	assert.Equal(t, "example.review", manifest.ID)
	assert.Equal(t, "tools.v1", manifest.CapabilityRequests[0])
	assert.Equal(t, "plugin.exe", filepath.Base(mustTarget(t, manifest, "windows", "amd64").Path))
	assert.Equal(t, "darwin", mustTarget(t, manifest, "darwin", "arm64").OS)
	_, err = manifest.SelectTarget("freebsd", "amd64")
	assert.ErrorIs(t, err, pluginstore.ErrUnsupportedTarget)
}

func TestParseManifestValidatesSemverScopesAndSingleLineMetadata(t *testing.T) {
	t.Parallel()

	base := func(version, provenance string) []byte {
		return []byte(`{"schema":"pips.plugin/v1alpha1","id":"example.review","version":"` + version + `","protocol":{"major":1,"min_minor":0},"targets":[{"os":"linux","arch":"amd64","path":"plugin","sha256":"` + strings.Repeat("0", 64) + `"}],"capability_requests":["tools.v1"]` + provenance + `}`)
	}
	for _, version := range []string{"1.0.0-01", "1.0.0-rc.01"} {
		_, err := pluginstore.ParseManifest(base(version, ""))
		require.ErrorIs(t, err, pluginstore.ErrInvalid)
	}
	_, err := pluginstore.ParseManifest(base("1.0.0-rc.1+build.01", `,"provenance":{"publisher":"example","source":"registry","signature":"sig"}`))
	require.NoError(t, err)
	_, err = pluginstore.ParseManifest(base("1.0.0", `,"provenance":{"publisher":"example\nother"}`))
	require.ErrorIs(t, err, pluginstore.ErrInvalid)
	for _, value := range []string{"example\\u007fpublisher", "example\\u0085publisher"} {
		_, err = pluginstore.ParseManifest(base("1.0.0", `,"provenance":{"publisher":"`+value+`"}`))
		require.ErrorIs(t, err, pluginstore.ErrInvalid)
	}
	_, err = pluginstore.ParseManifest([]byte(`{"schema":"pips.plugin/v1alpha1","id":"example.review","version":"1.0.0","protocol":{"major":1,"min_minor":0},"targets":[{"os":"linux","arch":"amd64","path":"plugin","sha256":"` + strings.Repeat("0", 64) + `"}],"capability_requests":["tools.v1"],"state":{"schema":"state\u0085schema"}}`))
	require.ErrorIs(t, err, pluginstore.ErrInvalid)

	root := t.TempDir()
	packageRoot := filepath.Join(root, "package")
	require.NoError(t, os.MkdirAll(packageRoot, 0o755))
	artifact := []byte("plugin")
	require.NoError(t, os.WriteFile(filepath.Join(packageRoot, "plugin.bin"), artifact, 0o700))
	digest := sha256.Sum256(artifact)
	manifestPath := filepath.Join(packageRoot, "plugin.json")
	writeManifest(t, manifestPath, runtime.GOOS, runtime.GOARCH, "plugin.bin", hex.EncodeToString(digest[:]))
	store, err := pluginstore.New(filepath.Join(root, "store"), pluginstore.Limits{})
	require.NoError(t, err)
	_, err = store.Install(context.Background(), manifestPath, pluginstore.InstallOptions{
		Source: pluginstore.InstallSource{Scope: pluginstore.SourceLocal, Reference: "local\u0085source"},
	})
	require.ErrorIs(t, err, pluginstore.ErrInvalid)
	_, err = store.Install(context.Background(), manifestPath, pluginstore.InstallOptions{
		Source:                 pluginstore.InstallSource{Scope: pluginstore.SourceLocal, Reference: "local"},
		CapabilityDecisionRefs: []string{"decision\u0085ref"},
	})
	require.ErrorIs(t, err, pluginstore.ErrInvalid)
}

func TestManifestRejectsUnsupportedTargetCombinations(t *testing.T) {
	t.Parallel()

	for _, target := range []struct {
		os   string
		arch string
	}{
		{os: "linux", arch: "wasm"},
		{os: "darwin", arch: "386"},
		{os: "darwin", arch: "arm"},
		{os: "windows", arch: "arm"},
		{os: "windows", arch: "riscv64"},
	} {
		data := []byte(`{"schema":"pips.plugin/v1alpha1","id":"example.review","version":"1.0.0","protocol":{"major":1,"min_minor":0},"targets":[{"os":"` + target.os + `","arch":"` + target.arch + `","path":"plugin","sha256":"` + strings.Repeat("0", 64) + `"}],"capability_requests":["tools.v1"]}`)
		_, err := pluginstore.ParseManifest(data)
		require.ErrorIs(t, err, pluginstore.ErrUnsupportedTarget, "%s/%s", target.os, target.arch)
	}
}

func TestParseManifestRejectsStrictnessAndUnsafePaths(t *testing.T) {
	t.Parallel()

	base := func(targetPath, extra string) []byte {
		return []byte(`{"schema":"pips.plugin/v1alpha1","id":"example.review","version":"1.0.0","protocol":{"major":1,"min_minor":0},"targets":[{"os":"linux","arch":"amd64","path":"` + targetPath + `","sha256":"` + strings.Repeat("0", 64) + `"}],"capability_requests":["tools.v1"]` + extra + `}`)
	}
	for _, test := range []struct {
		name string
		data []byte
		want error
	}{
		{name: "duplicate key", data: []byte(`{"schema":"pips.plugin/v1alpha1","schema":"pips.plugin/v1alpha1"}`), want: pluginstore.ErrInvalid},
		{name: "unknown field", data: base("plugin", `,"unknown":true`), want: pluginstore.ErrInvalid},
		{name: "non canonical key", data: []byte(`{"Schema":"pips.plugin/v1alpha1"}`), want: pluginstore.ErrInvalid},
		{name: "traversal", data: base("../plugin", ""), want: pluginstore.ErrInvalid},
		{name: "absolute", data: base("/plugin", ""), want: pluginstore.ErrInvalid},
		{name: "backslash", data: base(`dir\\plugin`, ""), want: pluginstore.ErrInvalid},
		{name: "duplicate target", data: []byte(`{"schema":"pips.plugin/v1alpha1","id":"example.review","version":"1.0.0","protocol":{"major":1,"min_minor":0},"targets":[{"os":"linux","arch":"amd64","path":"one","sha256":"` + strings.Repeat("0", 64) + `"},{"os":"linux","arch":"amd64","path":"two","sha256":"` + strings.Repeat("1", 64) + `"}],"capability_requests":["tools.v1"]}`), want: pluginstore.ErrDuplicate},
		{name: "bad digest", data: base("plugin", `,"capability_requests":["tools.v1"]`), want: pluginstore.ErrInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := pluginstore.ParseManifest(test.data)
			require.Error(t, err)
			require.ErrorIs(t, err, test.want)
		})
	}
}

func TestStoreInstallIsContentAddressedAndAuditable(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	packageRoot := filepath.Join(root, "package")
	require.NoError(t, os.MkdirAll(packageRoot, 0o755))
	artifact := []byte("precompiled plugin bytes\n")
	artifactPath := filepath.Join(packageRoot, "plugin.bin")
	require.NoError(t, os.WriteFile(artifactPath, artifact, 0o700))
	digest := sha256.Sum256(artifact)
	hexDigest := hex.EncodeToString(digest[:])
	manifestPath := filepath.Join(packageRoot, "plugin.json")
	writeManifest(t, manifestPath, runtime.GOOS, runtime.GOARCH, "plugin.bin", hexDigest)

	store, err := pluginstore.New(filepath.Join(root, "store"), pluginstore.Limits{})
	require.NoError(t, err)
	record, err := store.Install(context.Background(), manifestPath, pluginstore.InstallOptions{
		Source:                 pluginstore.InstallSource{Scope: pluginstore.SourceUser, Reference: "user-package"},
		RequestedVersion:       "1.2.3",
		AssociatedBundleDigest: strings.Repeat("b", 64),
		CapabilityDecisionRefs: []string{"decision-b", "decision-a"},
	})
	require.NoError(t, err)
	assert.Equal(t, "pips.plugin.install/v1alpha1", record.Schema)
	assert.Equal(t, hexDigest, record.ArtifactDigest)
	assert.Equal(t, "1.2.3", record.RequestedVersion)
	assert.Equal(t, strings.Repeat("b", 64), record.AssociatedBundleDigest)
	assert.Equal(t, "user", record.Source.Scope)
	assert.Equal(t, []string{"decision-a", "decision-b"}, record.CapabilityDecisionRefs)
	assert.Equal(t, "state/example.review", record.StatePath)

	stored, err := os.ReadFile(filepath.Join(store.Root(), filepath.FromSlash(record.ArtifactPath)))
	require.NoError(t, err)
	assert.Equal(t, artifact, stored)
	_, err = os.Stat(filepath.Join(store.Root(), "enabled", "example.review.json"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(store.Root(), "state", "example.review"))
	require.NoError(t, err)
	current, err := store.Current(context.Background(), "example.review")
	require.NoError(t, err)
	assert.Equal(t, record.ManifestDigest, current.ManifestDigest)

	// A second install of the same immutable identity is idempotent and does
	// not create a second artifact.
	repeated, err := store.Install(context.Background(), manifestPath, pluginstore.InstallOptions{
		Source:                 pluginstore.InstallSource{Scope: pluginstore.SourceUser, Reference: "user-package"},
		RequestedVersion:       "1.2.3",
		AssociatedBundleDigest: strings.Repeat("b", 64),
		CapabilityDecisionRefs: []string{"decision-a", "decision-b"},
	})
	require.NoError(t, err)
	assert.Equal(t, record.ManifestDigest, repeated.ManifestDigest)
}

func TestStoreFailedInstallPreservesCurrentEnablement(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	packageRoot := filepath.Join(root, "package")
	require.NoError(t, os.MkdirAll(packageRoot, 0o755))
	good := []byte("good plugin")
	require.NoError(t, os.WriteFile(filepath.Join(packageRoot, "good.bin"), good, 0o700))
	goodDigest := sha256.Sum256(good)
	goodManifest := filepath.Join(packageRoot, "good.json")
	writeManifest(t, goodManifest, runtime.GOOS, runtime.GOARCH, "good.bin", hex.EncodeToString(goodDigest[:]))

	store, err := pluginstore.New(filepath.Join(root, "store"), pluginstore.Limits{})
	require.NoError(t, err)
	before, err := store.Install(context.Background(), goodManifest, pluginstore.InstallOptions{})
	require.NoError(t, err)

	bad := []byte("bad plugin")
	require.NoError(t, os.WriteFile(filepath.Join(packageRoot, "bad.bin"), bad, 0o700))
	badManifest := filepath.Join(packageRoot, "bad.json")
	writeManifest(t, badManifest, runtime.GOOS, runtime.GOARCH, "bad.bin", strings.Repeat("f", 64))
	_, err = store.Install(context.Background(), badManifest, pluginstore.InstallOptions{})
	require.Error(t, err)
	require.ErrorIs(t, err, pluginstore.ErrDigestMismatch)

	after, err := store.Current(context.Background(), before.PluginID)
	require.NoError(t, err)
	assert.Equal(t, before.ManifestDigest, after.ManifestDigest)
	assert.Equal(t, before.ArtifactDigest, after.ArtifactDigest)
	entries, err := os.ReadDir(filepath.Join(store.Root(), "staging"))
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestStoreRejectsSymlinkAndSpecialArtifacts(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	packageRoot := filepath.Join(root, "package")
	require.NoError(t, os.MkdirAll(packageRoot, 0o755))
	if err := os.Symlink("missing", filepath.Join(packageRoot, "link.bin")); err == nil {
		manifest := filepath.Join(packageRoot, "link.json")
		writeManifest(t, manifest, runtime.GOOS, runtime.GOARCH, "link.bin", strings.Repeat("0", 64))
		store, storeErr := pluginstore.New(filepath.Join(root, "store"), pluginstore.Limits{})
		require.NoError(t, storeErr)
		_, installErr := store.Install(context.Background(), manifest, pluginstore.InstallOptions{})
		assert.ErrorIs(t, installErr, pluginstore.ErrUnsafeArtifact)
	}

	directory := filepath.Join(packageRoot, "directory.bin")
	require.NoError(t, os.Mkdir(directory, 0o755))
	manifest := filepath.Join(packageRoot, "directory.json")
	writeManifest(t, manifest, runtime.GOOS, runtime.GOARCH, "directory.bin", strings.Repeat("0", 64))
	store, err := pluginstore.New(filepath.Join(root, "store2"), pluginstore.Limits{})
	require.NoError(t, err)
	_, err = store.Install(context.Background(), manifest, pluginstore.InstallOptions{})
	assert.ErrorIs(t, err, pluginstore.ErrUnsafeArtifact)
}

func TestStoreRejectsNonCanonicalEnablementRecordPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	packageRoot := filepath.Join(root, "package")
	require.NoError(t, os.MkdirAll(packageRoot, 0o755))
	artifact := []byte("plugin")
	require.NoError(t, os.WriteFile(filepath.Join(packageRoot, "plugin.bin"), artifact, 0o700))
	digest := sha256.Sum256(artifact)
	manifestPath := filepath.Join(packageRoot, "plugin.json")
	writeManifest(t, manifestPath, runtime.GOOS, runtime.GOARCH, "plugin.bin", hex.EncodeToString(digest[:]))
	store, err := pluginstore.New(filepath.Join(root, "store"), pluginstore.Limits{})
	require.NoError(t, err)
	record, err := store.Install(context.Background(), manifestPath, pluginstore.InstallOptions{})
	require.NoError(t, err)
	enablementPath := filepath.Join(store.Root(), "enabled", record.PluginID+".json")
	data, err := os.ReadFile(enablementPath)
	require.NoError(t, err)
	data = []byte(strings.Replace(string(data), filepath.Base(record.ArtifactPath), "other.json", 1))
	require.NoError(t, os.WriteFile(enablementPath, data, 0o600))
	_, err = store.Current(context.Background(), record.PluginID)
	require.ErrorIs(t, err, pluginstore.ErrInvalid)
}

func TestStoreCurrentUsesStoredManifestAfterSourceRemoval(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	packageRoot := filepath.Join(root, "package")
	require.NoError(t, os.MkdirAll(packageRoot, 0o755))
	artifact := []byte("plugin")
	require.NoError(t, os.WriteFile(filepath.Join(packageRoot, "plugin.bin"), artifact, 0o700))
	digest := sha256.Sum256(artifact)
	manifestPath := filepath.Join(packageRoot, "plugin.json")
	writeManifest(t, manifestPath, runtime.GOOS, runtime.GOARCH, "plugin.bin", hex.EncodeToString(digest[:]))
	store, err := pluginstore.New(filepath.Join(root, "store"), pluginstore.Limits{})
	require.NoError(t, err)
	record, err := store.Install(context.Background(), manifestPath, pluginstore.InstallOptions{})
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(packageRoot))
	current, err := store.Current(context.Background(), record.PluginID)
	require.NoError(t, err)
	assert.Equal(t, record.ManifestDigest, current.ManifestDigest)
	assert.FileExists(t, filepath.Join(store.Root(), filepath.FromSlash(record.ManifestPath)))
}

func TestStoreRejectsTamperedRecordProvenance(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	packageRoot := filepath.Join(root, "package")
	require.NoError(t, os.MkdirAll(packageRoot, 0o755))
	artifact := []byte("plugin")
	require.NoError(t, os.WriteFile(filepath.Join(packageRoot, "plugin.bin"), artifact, 0o700))
	digest := sha256.Sum256(artifact)
	manifestPath := filepath.Join(packageRoot, "plugin.json")
	writeManifest(t, manifestPath, runtime.GOOS, runtime.GOARCH, "plugin.bin", hex.EncodeToString(digest[:]))
	manifestData, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	manifestText := strings.TrimSpace(string(manifestData))
	manifestText = strings.TrimSuffix(manifestText, "}") + `,"provenance":{"publisher":"example","source":"registry/example","signature":"sig"}}`
	require.NoError(t, os.WriteFile(manifestPath, []byte(manifestText), 0o600))

	store, err := pluginstore.New(filepath.Join(root, "store"), pluginstore.Limits{})
	require.NoError(t, err)
	record, err := store.Install(context.Background(), manifestPath, pluginstore.InstallOptions{})
	require.NoError(t, err)
	recordPath := filepath.Join(store.Root(), filepath.FromSlash("records/example.review/"+record.ManifestDigest+"-"+record.ArtifactDigest+".json"))
	recordData, err := os.ReadFile(recordPath)
	require.NoError(t, err)
	var recordObject map[string]any
	require.NoError(t, json.Unmarshal(recordData, &recordObject))
	recordObject["provenance"] = map[string]any{"status": "absent"}
	tampered, err := json.Marshal(recordObject)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(recordPath, tampered, 0o600))

	_, err = store.Current(context.Background(), record.PluginID)
	require.Error(t, err)
	assert.ErrorIs(t, err, pluginstore.ErrConflict)
}

func TestStoreArtifactLeaseDefersGarbageCollectionAndKeepsState(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	packageRoot := filepath.Join(root, "package")
	require.NoError(t, os.MkdirAll(packageRoot, 0o755))
	artifact := []byte("plugin")
	require.NoError(t, os.WriteFile(filepath.Join(packageRoot, "plugin.bin"), artifact, 0o700))
	digest := sha256.Sum256(artifact)
	manifestPath := filepath.Join(packageRoot, "plugin.json")
	writeManifest(t, manifestPath, runtime.GOOS, runtime.GOARCH, "plugin.bin", hex.EncodeToString(digest[:]))
	store, err := pluginstore.New(filepath.Join(root, "store"), pluginstore.Limits{})
	require.NoError(t, err)
	enable := false
	record, err := store.Install(context.Background(), manifestPath, pluginstore.InstallOptions{Enable: &enable})
	require.NoError(t, err)
	lease, err := store.AcquireArtifactLease(context.Background(), record.ArtifactDigest)
	require.NoError(t, err)
	otherStore, err := pluginstore.New(filepath.Join(root, "store"), pluginstore.Limits{})
	require.NoError(t, err)
	require.NoError(t, otherStore.RemoveRecord(context.Background(), record.PluginID, record.ManifestDigest))
	assert.FileExists(t, filepath.Join(store.Root(), filepath.FromSlash(record.ArtifactPath)))
	assert.DirExists(t, filepath.Join(store.Root(), "state", record.PluginID))
	require.NoError(t, lease.Release(context.Background()))
	// Marker removal commits the logical release, so a repeated call is safe.
	require.NoError(t, lease.Release(context.Background()))
	assert.NoFileExists(t, filepath.Join(store.Root(), filepath.FromSlash(record.ArtifactPath)))
	assert.DirExists(t, filepath.Join(store.Root(), "state", record.PluginID))
}

func TestStoreRemoveRecordReportsCommittedReferenceFailure(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	packageRoot := filepath.Join(root, "package")
	require.NoError(t, os.MkdirAll(packageRoot, 0o755))
	artifact := []byte("plugin")
	require.NoError(t, os.WriteFile(filepath.Join(packageRoot, "plugin.bin"), artifact, 0o700))
	digest := sha256.Sum256(artifact)
	manifestPath := filepath.Join(packageRoot, "plugin.json")
	writeManifest(t, manifestPath, runtime.GOOS, runtime.GOARCH, "plugin.bin", hex.EncodeToString(digest[:]))
	store, err := pluginstore.New(filepath.Join(root, "store"), pluginstore.Limits{})
	require.NoError(t, err)
	enable := false
	record, err := store.Install(context.Background(), manifestPath, pluginstore.InstallOptions{Enable: &enable})
	require.NoError(t, err)

	brokenDirectory := filepath.Join(store.Root(), "records", "other")
	require.NoError(t, os.MkdirAll(brokenDirectory, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(brokenDirectory, "broken.json"), []byte(`{}`), 0o600))
	err = store.RemoveRecord(context.Background(), record.PluginID, record.ManifestDigest)
	var publication *pluginstore.PublicationError
	require.ErrorAs(t, err, &publication)
	assert.True(t, publication.Committed)
	assert.False(t, publication.Published)
	assert.Equal(t, record.PluginID, publication.PluginID)
	assert.Equal(t, record.PluginID, publication.Record.PluginID)
	assert.FileExists(t, filepath.Join(store.Root(), filepath.FromSlash(record.ArtifactPath)))
	_, currentErr := store.Current(context.Background(), record.PluginID)
	assert.ErrorIs(t, currentErr, pluginstore.ErrNotEnabled)
}

func TestStoreArtifactLeaseReportsCommittedDeferredGCFailure(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	packageRoot := filepath.Join(root, "package")
	require.NoError(t, os.MkdirAll(packageRoot, 0o755))
	artifact := []byte("plugin")
	require.NoError(t, os.WriteFile(filepath.Join(packageRoot, "plugin.bin"), artifact, 0o700))
	digest := sha256.Sum256(artifact)
	manifestPath := filepath.Join(packageRoot, "plugin.json")
	writeManifest(t, manifestPath, runtime.GOOS, runtime.GOARCH, "plugin.bin", hex.EncodeToString(digest[:]))
	store, err := pluginstore.New(filepath.Join(root, "store"), pluginstore.Limits{})
	require.NoError(t, err)
	enable := false
	record, err := store.Install(context.Background(), manifestPath, pluginstore.InstallOptions{Enable: &enable})
	require.NoError(t, err)
	lease, err := store.AcquireArtifactLease(context.Background(), record.ArtifactDigest)
	require.NoError(t, err)
	otherStore, err := pluginstore.New(filepath.Join(root, "store"), pluginstore.Limits{})
	require.NoError(t, err)
	require.NoError(t, otherStore.RemoveRecord(context.Background(), record.PluginID, record.ManifestDigest))

	brokenDirectory := filepath.Join(store.Root(), "records", "other")
	require.NoError(t, os.MkdirAll(brokenDirectory, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(brokenDirectory, "broken.json"), []byte(`{}`), 0o600))
	err = lease.Release(context.Background())
	var publication *pluginstore.PublicationError
	require.ErrorAs(t, err, &publication)
	assert.True(t, publication.Committed)
	assert.True(t, publication.Published)
	assert.FileExists(t, filepath.Join(store.Root(), filepath.FromSlash(record.ArtifactPath)))
	require.NoError(t, lease.Release(context.Background()))
}

func TestStoreSerializesTwoStoreInstancesForOneRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	packageRoot := filepath.Join(root, "package")
	require.NoError(t, os.MkdirAll(packageRoot, 0o755))
	artifact := []byte("plugin")
	require.NoError(t, os.WriteFile(filepath.Join(packageRoot, "plugin.bin"), artifact, 0o700))
	digest := sha256.Sum256(artifact)
	manifestPath := filepath.Join(packageRoot, "plugin.json")
	writeManifest(t, manifestPath, runtime.GOOS, runtime.GOARCH, "plugin.bin", hex.EncodeToString(digest[:]))
	first, err := pluginstore.New(filepath.Join(root, "store"), pluginstore.Limits{})
	require.NoError(t, err)
	second, err := pluginstore.New(filepath.Join(root, "store"), pluginstore.Limits{})
	require.NoError(t, err)

	var wait sync.WaitGroup
	results := make(chan error, 2)
	for _, store := range []*pluginstore.Store{first, second} {
		wait.Add(1)
		go func(store *pluginstore.Store) {
			defer wait.Done()
			_, installErr := store.Install(context.Background(), manifestPath, pluginstore.InstallOptions{})
			results <- installErr
		}(store)
	}
	wait.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	current, err := first.Current(context.Background(), "example.review")
	require.NoError(t, err)
	assert.Equal(t, hex.EncodeToString(digest[:]), current.ArtifactDigest)
}

func TestStoreRemovalKeepsStateOutsideArtifactDirectory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	packageRoot := filepath.Join(root, "package")
	require.NoError(t, os.MkdirAll(packageRoot, 0o755))
	artifact := []byte("plugin")
	require.NoError(t, os.WriteFile(filepath.Join(packageRoot, "plugin.bin"), artifact, 0o700))
	digest := sha256.Sum256(artifact)
	manifestPath := filepath.Join(packageRoot, "plugin.json")
	writeManifest(t, manifestPath, runtime.GOOS, runtime.GOARCH, "plugin.bin", hex.EncodeToString(digest[:]))
	store, err := pluginstore.New(filepath.Join(root, "store"), pluginstore.Limits{})
	require.NoError(t, err)
	enable := false
	record, err := store.Install(context.Background(), manifestPath, pluginstore.InstallOptions{Enable: &enable})
	require.NoError(t, err)
	require.NoError(t, store.RemoveRecord(context.Background(), record.PluginID, record.ManifestDigest))
	_, err = os.Stat(filepath.Join(store.Root(), "state", record.PluginID))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(store.Root(), filepath.FromSlash(record.ArtifactPath)))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func FuzzParseManifest(t *testing.F) {
	t.Add([]byte(`{"schema":"pips.plugin/v1alpha1"}`))
	t.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = pluginstore.ParseManifest(data)
	})
}

func mustTarget(t *testing.T, manifest pluginstore.PluginManifest, goos, goarch string) pluginstore.TargetArtifact {
	t.Helper()
	target, err := manifest.SelectTarget(goos, goarch)
	require.NoError(t, err)
	return target
}

func writeManifest(t *testing.T, path, goos, goarch, artifactPath, digest string) {
	t.Helper()
	data := `{"schema":"pips.plugin/v1alpha1","id":"example.review","version":"1.0.0","protocol":{"major":1,"min_minor":0},"targets":[{"os":"` + goos + `","arch":"` + goarch + `","path":"` + artifactPath + `","sha256":"` + digest + `"}],"capability_requests":["tools.v1"]}`
	require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
}
