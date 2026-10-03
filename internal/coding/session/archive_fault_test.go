//go:build darwin || linux

//nolint:wsl_v5 // Fault fixtures keep injected failures beside observable storage assertions.
package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArchivePublicationSyncFailureRetainsUnreferencedObject(t *testing.T) {
	t.Parallel()
	_, handle, store := archiveFixture(t)
	before := handle.Session().Entries()
	staged := archiveStage(t, store, archiveEntries("evidence"))
	failure := errors.New("directory sync failed")
	err := staged.publish(t.Context(), func() error { return failure })
	require.ErrorIs(t, err, ErrArchivePublicationUncertain)
	require.ErrorIs(t, err, failure)
	require.NoError(t, staged.Close())
	_, err = store.Verify(t.Context(), staged.ID())
	require.NoError(t, err, "Close must not delete an object after uncertain publication")
	page, err := archiveReader(t, store).List(t.Context(), ArchiveListRequest{})
	require.NoError(t, err)
	assert.Empty(t, page.Archives)
	assert.Equal(t, before, handle.Session().Entries(), "archive work never writes a checkpoint")
}

func TestArchiveCopyFailureCleansStageAndPreservesDestination(t *testing.T) {
	t.Parallel()
	repo, source, store := archiveFixture(t)
	staged := archiveStage(t, store, archiveEntries("copy evidence"))
	require.NoError(t, staged.Publish(t.Context()))
	destination := "reserved-fork"
	dir, err := openArchiveDirectory(repo.dir, destination, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, dir.Close()) })
	failure := errors.New("copy read failed")
	_, err = copyArchiveFile(t.Context(), dir, archiveFailReader{failure}, staged.Info())
	require.ErrorIs(t, err, failure)
	_, err = copyArchiveFile(t.Context(), dir, strings.NewReader("wrong bytes"), staged.Info())
	require.ErrorIs(t, err, ErrArchiveCorrupt)
	names, err := os.ReadDir(filepath.Join(repo.dir, destination+".history"))
	require.NoError(t, err)
	assert.Empty(t, names)
	destinationStore := &ArchiveStore{dir: repo.dir, sessionID: destination}
	path := archiveObjectPath(destinationStore, staged.ID())
	collision := bytes.Repeat([]byte{'x'}, 32)
	require.NoError(t, os.WriteFile(path, collision, 0o600))
	require.ErrorIs(t, repo.CopyArchives(t.Context(), source, destination, []string{staged.ID()}), ErrArchiveCorrupt)
	unchanged, err := os.ReadFile(path) //nolint:gosec // Test-owned destination under t.TempDir.
	require.NoError(t, err)
	assert.Equal(t, collision, unchanged)
	_, err = store.Verify(t.Context(), staged.ID())
	require.NoError(t, err, "failed fork publication leaves source untouched")
}

func TestArchivePublishRejectsReplacedStageAndTargetSymlinks(t *testing.T) {
	t.Parallel()
	for _, replaceStage := range []bool{false, true} {
		t.Run(map[bool]string{true: "stage", false: "target"}[replaceStage], func(t *testing.T) {
			t.Parallel()
			_, _, store := archiveFixture(t)
			staged := archiveStage(t, store, archiveEntries("private evidence"))
			path := archiveObjectPath(store, staged.ID())
			if replaceStage {
				path = filepath.Join(filepath.Dir(path), staged.name)
				require.NoError(t, os.Remove(path))
			}
			external := filepath.Join(t.TempDir(), "external")
			require.NoError(t, os.WriteFile(external, []byte("do not modify"), 0o600))
			require.NoError(t, os.Symlink(external, path))
			require.Error(t, staged.Publish(t.Context()))
			data, err := os.ReadFile(external) //nolint:gosec // Verify the test-owned symlink target was not modified.
			require.NoError(t, err)
			assert.Equal(t, "do not modify", string(data))
			if replaceStage {
				require.NoFileExists(t, archiveObjectPath(store, staged.ID()))
			}
		})
	}
}

func TestArchiveListOrdersAndDeduplicatesBoundReferences(t *testing.T) {
	t.Parallel()
	_, _, store := archiveFixture(t)
	first := archiveStage(t, store, archiveEntries("first"))
	second := archiveStage(t, store, archiveEntries("second"))
	require.NoError(t, first.Publish(t.Context()))
	require.NoError(t, second.Publish(t.Context()))
	reader := archiveReader(t, store, first.ID(), second.ID(), first.ID())
	ids := []string{first.ID(), second.ID()}
	slices.Sort(ids)
	page, err := reader.List(t.Context(), ArchiveListRequest{Limit: 1})
	require.NoError(t, err)
	require.Len(t, page.Archives, 1)
	assert.Equal(t, ids[0], page.Archives[0].ID)
	assert.True(t, page.More)
	page, err = reader.List(t.Context(), ArchiveListRequest{Limit: 1, Offset: page.NextOffset})
	require.NoError(t, err)
	require.Len(t, page.Archives, 1)
	assert.Equal(t, ids[1], page.Archives[0].ID)
	assert.False(t, page.More)
}

func TestArchiveSearchScanBudgetReturnsResumableMiss(t *testing.T) {
	t.Parallel()
	_, _, store := archiveFixture(t)
	staged := archiveStage(t, store, archiveEntries(strings.Repeat("x", archiveSearchScanBytes+archiveSegmentBytes)))
	require.NoError(t, staged.Publish(t.Context()))
	reader := archiveReader(t, store, staged.ID())
	page, err := reader.Search(t.Context(), ArchiveSearchRequest{ArchiveID: staged.ID(), Query: "needle two"})
	require.NoError(t, err)
	assert.Empty(t, page.Matches)
	require.NotNil(t, page.Next)
	page, err = reader.Search(t.Context(), ArchiveSearchRequest{ArchiveID: staged.ID(), Query: "needle two", Cursor: page.Next})
	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	assert.Equal(t, "needle two", page.Matches[0].Text)
	assert.Nil(t, page.Next)
}

func TestArchiveCanceledDuringCopyLeavesNoTemporaryObject(t *testing.T) {
	t.Parallel()
	_, _, store := archiveFixture(t)
	dir, err := openArchiveDirectory(store.dir, store.sessionID, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, dir.Close()) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader := &archiveCancelReader{cancel: cancel}
	_, err = copyArchiveFile(ctx, dir, reader, ArchiveInfo{ID: strings.Repeat("0", 64)})
	require.ErrorIs(t, err, context.Canceled)
	names, err := os.ReadDir(filepath.Join(store.dir, store.sessionID+".history"))
	require.NoError(t, err)
	assert.Empty(t, names)
}

func TestArchiveRejectsOversizedManifestBeforeAllocation(t *testing.T) {
	t.Parallel()
	_, _, store := archiveFixture(t)
	dir, err := openArchiveDirectory(store.dir, store.sessionID, true)
	require.NoError(t, err)
	require.NoError(t, dir.Close())
	object := make([]byte, archiveHeaderBytes)
	copy(object, archiveMagic)
	binary.BigEndian.PutUint32(object[len(archiveMagic):], archiveManifestBytes+1)
	hash := sha256.Sum256(object)
	id := hex.EncodeToString(hash[:])
	require.NoError(t, os.WriteFile(archiveObjectPath(store, id), object, 0o600))
	_, err = store.Verify(t.Context(), id)
	require.ErrorIs(t, err, ErrArchiveLimit)
}

func TestArchiveRendersToolEvidenceAndPreservesMedia(t *testing.T) {
	t.Parallel()
	_, _, store := archiveFixture(t)
	entries := []harness.Entry{
		{Kind: harness.KindMessage, ID: "user", Time: time.Unix(100, 0).UTC(), Message: ai.UserMessage{Parts: []ai.UserPart{
			ai.Text("inspect the attachment"),
			ai.ImagePart{Source: ai.MediaSource{MIMEType: "image/png", Data: []byte("image payload")}},
			ai.FilePart{Name: "document.pdf", Source: ai.MediaSource{MIMEType: "application/pdf", Data: []byte("file payload")}},
		}}},
		{Kind: harness.KindMessage, ID: "call", ParentID: "user", Time: time.Unix(101, 0).UTC(), Message: ai.AssistantMessage{Parts: []ai.AssistantPart{
			ai.ReasoningPart{Text: "historical rationale", Signature: "opaque-signature"},
			ai.ToolCallPart{ID: "call-id", Name: "inspect", Args: ai.JSON(`{"query":"evidence"}`)},
		}}},
		{Kind: harness.KindMessage, ID: "result", ParentID: "call", Time: time.Unix(102, 0).UTC(), Message: ai.ToolMessage{Parts: []ai.ToolResultPart{
			{ToolCallID: "call-id", Name: "inspect", Content: []ai.Part{
				ai.Text("first result\nsecond result"),
				ai.StructuredContentPart{Data: ai.JSON(`{"answer":"structured evidence"}`)},
				ai.ResourceLinkPart{URI: "file:///historical/resource", Name: "reference", Description: "linked evidence"},
				ai.EmbeddedResourcePart{URI: "resource://snapshot", Text: "embedded evidence"},
			}},
		}}},
	}
	staged := archiveStage(t, store, entries)
	require.NoError(t, staged.Publish(t.Context()))
	reader := archiveReader(t, store, staged.ID())
	for _, query := range []string{"first result", "second result", "structured evidence", "linked evidence", "embedded evidence", "Tool call inspect", "historical rationale", "message / assistant", "message / user", "message / tool"} {
		matches, err := reader.Search(t.Context(), ArchiveSearchRequest{ArchiveID: staged.ID(), Query: query})
		require.NoError(t, err)
		assert.NotEmpty(t, matches.Matches, query)
	}
	file, manifest, err := store.openVerified(t.Context(), staged.ID())
	require.NoError(t, err)
	defer func() { require.NoError(t, file.Close()) }()
	stat, err := file.Stat()
	require.NoError(t, err)
	_, offset, err := readArchiveManifest(file, stat.Size())
	require.NoError(t, err)
	canonical, err := io.ReadAll(io.NewSectionReader(file, offset, manifest.Path.Bytes))
	require.NoError(t, err)
	assert.Contains(t, string(canonical), "opaque-signature")
	for index, line := range bytes.Split(bytes.TrimSuffix(canonical, []byte{'\n'}), []byte{'\n'}) {
		decoded, err := harness.UnmarshalEntry(line)
		require.NoError(t, err)
		assert.Equal(t, entries[index], decoded)
	}
}

type archiveFailReader struct{ err error }

func (r archiveFailReader) Read([]byte) (int, error) { return 0, r.err }

type archiveCancelReader struct{ cancel context.CancelFunc }

func (r *archiveCancelReader) Read(p []byte) (int, error) {
	r.cancel()
	return copy(p, "partial object"), nil
}
