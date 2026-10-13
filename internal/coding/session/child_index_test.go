//nolint:wsl_v5 // Durable record fixtures keep encoding, bounds, and assertions adjacent.
package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	childIndexTestWorkspace = "/workspace"
	childIndexTestParentID  = "s-parent"
	childIndexTestChildID   = "s-child"
)

// testChildIndexEntry is a valid parent-side entry for one child.
func testChildIndexEntry(parentID, childID string) ChildIndexEntry {
	return newChildIndexEntry(childID, parentID, "run-parent", "explore", "builtin", time.Now())
}

// requireSameChildIndexEntry compares two entries field by field, because a
// stored time loses the monotonic reading a fresh one carries.
func requireSameChildIndexEntry(t *testing.T, want, got ChildIndexEntry) {
	t.Helper()

	assert.Equal(t, want.Schema, got.Schema)
	assert.Equal(t, want.Version, got.Version)
	assert.Equal(t, want.ChildSessionID, got.ChildSessionID)
	assert.Equal(t, want.ParentSessionID, got.ParentSessionID)
	assert.Equal(t, want.ParentRunID, got.ParentRunID)
	assert.Equal(t, want.AgentID, got.AgentID)
	assert.Equal(t, want.AgentKind, got.AgentKind)
	assert.Equal(t, want.State, got.State)
	assert.True(t, want.CreatedAt.Equal(got.CreatedAt), "creation time %s", got.CreatedAt)
	assert.True(t, want.EndedAt.Equal(got.EndedAt), "end time %s", got.EndedAt)
}

func TestChildIndexEntryRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "entry.json")
	entry := testChildIndexEntry(childIndexTestParentID, childIndexTestChildID)
	require.NoError(t, writeChildIndexEntry(path, entry))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "an entry is private")

	decoded, err := ReadChildIndexEntry(path, childIndexTestParentID)
	require.NoError(t, err)
	requireSameChildIndexEntry(t, entry, decoded)
	assert.True(t, decoded.EndedAt.IsZero(), "an entry with no state carries no end time")

	// A created entry claims no state: the lifecycle mirror is its only writer,
	// so both an unknown state and an end time are simply absent.
	created, err := os.ReadFile(path) //nolint:gosec // the test owns this store path
	require.NoError(t, err)
	assert.NotContains(t, string(created), "\"state\"")
	assert.NotContains(t, string(created), "ended_at")

	terminal := entry
	terminal.State = ChildIndexStateSucceeded
	terminal.EndedAt = time.Now().UTC()
	require.NoError(t, writeChildIndexEntry(path, terminal))
	decoded, err = ReadChildIndexEntry(path, childIndexTestParentID)
	require.NoError(t, err)
	requireSameChildIndexEntry(t, terminal, decoded)
}

func TestChildIndexEntryRejectsUnknownField(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "entry.json")
	entry := testChildIndexEntry(childIndexTestParentID, childIndexTestChildID)
	encoded, err := json.Marshal(entry)
	require.NoError(t, err)

	var document map[string]any
	require.NoError(t, json.Unmarshal(encoded, &document))
	document["future_field"] = true
	encoded, err = json.Marshal(document)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, encoded, 0o600))

	_, err = ReadChildIndexEntry(path, childIndexTestParentID)
	require.ErrorIs(t, err, ErrChildIndex)
}

func TestChildIndexEntryRejectsOversizedRecord(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "entry.json")
	padding := strings.Repeat("x", maxChildIndexRecord+1)
	require.NoError(t, os.WriteFile(path, []byte(padding), 0o600))

	_, err := ReadChildIndexEntry(path, childIndexTestParentID)
	require.ErrorIs(t, err, ErrLayout)
}

func TestChildIndexEntryRejectsUnsupportedRecord(t *testing.T) {
	t.Parallel()

	valid := testChildIndexEntry(childIndexTestParentID, childIndexTestChildID)
	terminal := valid
	terminal.State = ChildIndexStateSucceeded
	terminal.EndedAt = time.Now().UTC()

	tests := map[string]struct {
		mutate   func(*ChildIndexEntry)
		parentID string
	}{
		"wrong schema":       {mutate: func(entry *ChildIndexEntry) { entry.Schema = "pips.coding.session.subagent_entry/v2" }},
		"wrong version":      {mutate: func(entry *ChildIndexEntry) { entry.Version = 2 }},
		"empty child id":     {mutate: func(entry *ChildIndexEntry) { entry.ChildSessionID = "" }},
		"escaping child id":  {mutate: func(entry *ChildIndexEntry) { entry.ChildSessionID = "../escape" }},
		"empty parent id":    {mutate: func(entry *ChildIndexEntry) { entry.ParentSessionID = "" }},
		"another parent":     {mutate: func(entry *ChildIndexEntry) { entry.ParentSessionID = "s-other" }},
		"missing creation":   {mutate: func(entry *ChildIndexEntry) { entry.CreatedAt = time.Time{} }},
		"unknown state":      {mutate: func(entry *ChildIndexEntry) { entry.State = "finished" }},
		"terminal no end":    {mutate: func(entry *ChildIndexEntry) { entry.State = ChildIndexStateFailed }},
		"stateless with end": {mutate: func(entry *ChildIndexEntry) { entry.EndedAt = time.Now().UTC() }},
		"other index parent": {parentID: "s-other"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			entry := valid
			if test.mutate != nil {
				test.mutate(&entry)
			}
			path := filepath.Join(t.TempDir(), "entry.json")
			require.NoError(t, os.WriteFile(path, mustEncodeChildIndexEntry(t, entry), 0o600))

			parentID := childIndexTestParentID
			if test.parentID != "" {
				parentID = test.parentID
			}
			_, err := ReadChildIndexEntry(path, parentID)
			require.ErrorIs(t, err, ErrChildIndex)
		})
	}

	// The same validator guards the write path, so an unsupported entry is never
	// published in the first place.
	require.ErrorIs(t,
		writeChildIndexEntry(filepath.Join(t.TempDir(), "entry.json"), ChildIndexEntry{}),
		ErrChildIndex,
	)
	require.NoError(t, writeChildIndexEntry(filepath.Join(t.TempDir(), "entry.json"), terminal))
}

// mustEncodeChildIndexEntry marshals an entry without validating it, so a test
// can place a record on disk that the reader must reject.
func mustEncodeChildIndexEntry(t *testing.T, entry ChildIndexEntry) []byte {
	t.Helper()

	encoded, err := json.Marshal(entry)
	require.NoError(t, err)

	return encoded
}

func TestChildIndexMarkerUnknownReadsAsAbsent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	validPath := filepath.Join(dir, "valid.json")
	require.NoError(t, writeChildIndexMarker(validPath, time.Now()))
	_, ok := readChildIndexMarker(validPath)
	assert.True(t, ok, "a marker this build wrote is usable")

	tests := map[string]string{
		"missing":      "",
		"wrong schema": `{"schema":"pips.coding.session.subagent_index/v2","version":1,"updated_at":"2026-10-12T00:00:00Z"}`,
		"wrong version": `{"schema":"pips.coding.session.subagent_index/v1","version":2,` +
			`"updated_at":"2026-10-12T00:00:00Z"}`,
		"unknown field": `{"schema":"pips.coding.session.subagent_index/v1","version":1,` +
			`"updated_at":"2026-10-12T00:00:00Z","extra":true}`,
		"not json": "not a record",
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "index.json")
			if contents != "" {
				require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
			}
			_, ok := readChildIndexMarker(path)
			assert.False(t, ok, "an unusable marker reads as no marker")
		})
	}
}

// newChildIndexTestStore builds one parent record and a deferred store for a
// child of it, so a test can drive materialization with a chosen child id.
func newChildIndexTestStore(
	t *testing.T,
) (StoreRoot, StoreLayout, *deferredStore) {
	t.Helper()

	root, err := NewStoreRoot(t.TempDir())
	require.NoError(t, err)
	layout := root.Layout()
	group, err := root.EnsureGroup(childIndexTestWorkspace)
	require.NoError(t, err)
	parent := NewMetadataRecord(
		childIndexTestParentID,
		WorkspaceRef{ID: "workspace-key", Path: childIndexTestWorkspace},
		KindConversation,
		time.Now(),
	)
	require.NoError(t, WriteMetadataRecord(
		filepath.Join(group.SessionDirOf(childIndexTestParentID), MetadataFileName), parent,
	))

	record := NewMetadataRecord(
		childIndexTestChildID,
		WorkspaceRef{ID: "workspace-key", Path: childIndexTestWorkspace},
		KindSubagent,
		time.Now(),
	)
	record.Parent = &ParentRef{SessionID: childIndexTestParentID, RunID: "run-parent"}
	record.Agent = "explore"
	record.Subagent = subagentRefFrom(SubagentIdentity{
		Schema: "pips.coding.subagent.identity/v1alpha1", AgentID: "explore",
		Kind: "builtin", Name: "Explore",
	})
	childDir := group.SessionDirOf(childIndexTestChildID)
	store := newDeferredStore(
		root, childIndexTestWorkspace, record, nil,
		func() (sessionLock, error) {
			return acquireSessionLock(t.Context(), filepath.Join(childDir, LockFileName))
		},
	)

	return root, layout, store
}

// TestMaterializePublishesChildIndexEntryBeforeTheChildRecord pins the write
// order: the entry lands while the child's own record still cannot be written,
// so a later failure leaves a detectable entry instead of an invisible child.
func TestMaterializePublishesChildIndexEntryBeforeTheChildRecord(t *testing.T) {
	t.Parallel()

	_, layout, store := newChildIndexTestStore(t)
	// A directory where the child's record belongs fails the record write after
	// the entry has been published.
	require.NoError(t, os.MkdirAll(
		layout.MetadataFile(childIndexTestWorkspace, childIndexTestChildID), 0o700,
	))

	err := store.Materialize()
	require.Error(t, err, "the child record cannot be published over a directory")

	entry, readErr := ReadChildIndexEntry(
		layout.ChildIndexEntryFile(
			childIndexTestWorkspace, childIndexTestParentID, childIndexTestChildID,
		),
		childIndexTestParentID,
	)
	require.NoError(t, readErr, "the entry was published before the failed record write")
	assert.Equal(t, childIndexTestChildID, entry.ChildSessionID)
	assert.Equal(t, childIndexTestParentID, entry.ParentSessionID)
	assert.Empty(t, entry.State, "a created entry carries no state until the lifecycle mirror writes one")
	assert.False(t, hasSessionRecord(
		layout.MetadataFile(childIndexTestWorkspace, childIndexTestChildID),
	), "the child's own record never landed")
}

// TestMaterializeRemovesChildIndexMarkerWhenTheEntryWriteFails pins the
// best-effort rule: a spawn still succeeds, and the parent's index stops
// claiming completeness so the next read re-derives it.
func TestMaterializeRemovesChildIndexMarkerWhenTheEntryWriteFails(t *testing.T) {
	t.Parallel()

	_, layout, store := newChildIndexTestStore(t)
	markerPath := layout.SubagentIndexFile(childIndexTestWorkspace, childIndexTestParentID)
	require.NoError(t, writeChildIndexMarker(markerPath, time.Now()))
	// A directory where the entry belongs fails the entry write while the index
	// directory itself stays writable, so the marker can still be removed.
	require.NoError(t, os.MkdirAll(
		layout.ChildIndexEntryFile(
			childIndexTestWorkspace, childIndexTestParentID, childIndexTestChildID,
		),
		0o700,
	))

	require.NoError(t, store.Materialize(), "a failed index write never fails the spawn")
	assert.True(t, hasSessionRecord(
		layout.MetadataFile(childIndexTestWorkspace, childIndexTestChildID),
	), "the child's own record still landed")
	assert.NoFileExists(t, markerPath, "a failed entry write invalidates the index")
}

// TestMaterializeSkipsChildIndexEntryWithoutParentRecord pins the guard: the
// entry writer never creates the parent's directory, because a directory without
// a record shows up as an unreadable session in every listing.
func TestMaterializeSkipsChildIndexEntryWithoutParentRecord(t *testing.T) {
	t.Parallel()

	_, layout, store := newChildIndexTestStore(t)
	require.NoError(t, os.RemoveAll(layout.SessionDir(childIndexTestWorkspace, childIndexTestParentID)))

	require.NoError(t, store.Materialize())

	_, err := os.Lstat(layout.SessionDir(childIndexTestWorkspace, childIndexTestParentID))
	require.ErrorIs(t, err, os.ErrNotExist, "the entry writer never creates the parent directory")
	assert.NoFileExists(t, layout.SubagentIndexFile(childIndexTestWorkspace, childIndexTestParentID))
}
