//go:build darwin || linux

//nolint:wsl_v5 // Archive fixtures keep setup, storage actions and assertions together.
package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func archiveFixture(t *testing.T) (*Repository, *Handle, *ArchiveStore) {
	t.Helper()
	repo, err := NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)
	handle, err := repo.Create(t.Context(), CreateOptions{WorkspaceID: "workspace", WorkspacePath: "/workspace"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, handle.Close()) })
	store, err := repo.Archives(handle)
	require.NoError(t, err)
	return repo, handle, store
}

func archiveEntries(text string) []harness.Entry {
	return []harness.Entry{
		{Kind: harness.KindMessage, ID: "user", Time: time.Unix(100, 0).UTC(), Message: ai.UserText(text)},
		{Kind: harness.KindMessage, ID: "answer", ParentID: "user", Time: time.Unix(101, 0).UTC(), Message: ai.AssistantText("answer\nneedle two"), Usage: &ai.Usage{InputTokens: 42, OutputTokens: 12}},
		{Kind: harness.KindBranchSummary, ID: "branch", ParentID: "answer", Time: time.Unix(102, 0).UTC(), Summary: "older branch evidence", FromID: "outside-selected-path"},
	}
}

func archiveStage(t *testing.T, store *ArchiveStore, entries []harness.Entry) *StagedArchive {
	t.Helper()
	staged, err := store.Stage(t.Context(), ArchiveSource{SessionID: store.sessionID, TipID: entries[len(entries)-1].ID}, entries)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, staged.Close()) })
	return staged
}

func archiveObjectPath(store *ArchiveStore, id string) string {
	return filepath.Join(store.dir, store.sessionID+".history", id+".archive")
}

func archiveReader(t *testing.T, store *ArchiveStore, ids ...string) *ArchiveReader {
	t.Helper()
	reader, err := store.Bind(ids)
	require.NoError(t, err)
	return reader
}

func TestArchiveRoundTripCanonicalDigestAndImmutability(t *testing.T) {
	t.Parallel()
	_, _, store := archiveFixture(t)
	entries := archiveEntries("needle one\n用户请求")
	staged := archiveStage(t, store, entries)
	require.NoError(t, ValidateArchiveID(staged.ID()))
	_, err := store.Verify(t.Context(), staged.ID())
	require.ErrorIs(t, err, ErrArchiveNotFound)
	other := archiveStage(t, store, entries)
	assert.Equal(t, staged.ID(), other.ID(), "canonical snapshots have deterministic IDs")

	entries[0].Message = ai.UserText("caller mutated the supplied snapshot")
	require.NoError(t, staged.Publish(t.Context()))
	require.NoError(t, staged.Publish(t.Context()))
	require.NoError(t, other.Publish(t.Context()), "identical immutable object is reusable")
	info, err := store.Verify(t.Context(), staged.ID())
	require.NoError(t, err)
	assert.Equal(t, staged.Info(), info)
	assert.Equal(t, 3, info.Records)

	data, err := os.ReadFile(archiveObjectPath(store, staged.ID()))
	require.NoError(t, err)
	digest := sha256.Sum256(data)
	assert.Equal(t, hex.EncodeToString(digest[:]), staged.ID(), "ID covers exact complete packed bytes")
	length := int(binary.BigEndian.Uint32(data[len(archiveMagic):archiveHeaderBytes]))
	var manifest archiveManifest
	require.NoError(t, json.Unmarshal(data[archiveHeaderBytes:archiveHeaderBytes+length], &manifest))
	var canonical bytes.Buffer
	for _, entry := range archiveEntries("needle one\n用户请求") {
		encoded, marshalErr := harness.MarshalEntry(entry)
		require.NoError(t, marshalErr)
		canonical.Write(encoded)
		canonical.WriteByte('\n')
	}
	start := archiveHeaderBytes + length
	assert.Equal(t, canonical.Bytes(), data[start:start+int(manifest.Path.Bytes)])
	assert.Contains(t, canonical.String(), "outside-selected-path", "provenance is not validated as a stand-alone graph")
	assert.NotContains(t, string(data), "caller mutated")
	fileInfo, err := os.Stat(archiveObjectPath(store, staged.ID()))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fileInfo.Mode().Perm())
	dirInfo, err := os.Stat(filepath.Dir(archiveObjectPath(store, staged.ID())))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())
	require.NoError(t, staged.Close())
	require.NoError(t, other.Close())
	names, err := os.ReadDir(filepath.Dir(archiveObjectPath(store, staged.ID())))
	require.NoError(t, err)
	require.Len(t, names, 1, "temporary names are removed but published object survives")
	require.ErrorIs(t, staged.Publish(t.Context()), ErrArchiveInvalid)
}

func TestArchiveBindNeverDiscoversOrphansOrSiblingReferences(t *testing.T) {
	t.Parallel()
	_, _, store := archiveFixture(t)
	first := archiveStage(t, store, archiveEntries("authorized"))
	second := archiveStage(t, store, archiveEntries("orphan"))
	require.NoError(t, first.Publish(t.Context()))
	require.NoError(t, second.Publish(t.Context()))
	unbound := archiveReader(t, store)
	page, err := unbound.List(t.Context(), ArchiveListRequest{})
	require.NoError(t, err)
	assert.Empty(t, page.Archives)
	_, err = unbound.Read(t.Context(), ArchiveReadRequest{ArchiveID: first.ID(), SegmentID: "seg-000001"})
	require.ErrorIs(t, err, ErrArchiveDenied)

	ids := []string{first.ID()}
	reader, err := store.Bind(ids)
	require.NoError(t, err)
	ids[0] = second.ID()
	page, err = reader.List(t.Context(), ArchiveListRequest{})
	require.NoError(t, err)
	require.Len(t, page.Archives, 1)
	assert.Equal(t, first.ID(), page.Archives[0].ID)
	_, err = reader.Search(t.Context(), ArchiveSearchRequest{ArchiveID: second.ID(), Query: "orphan"})
	require.ErrorIs(t, err, ErrArchiveDenied)
	_, err = reader.List(t.Context(), ArchiveListRequest{ArchiveID: second.ID()})
	require.ErrorIs(t, err, ErrArchiveDenied)
	_, _, sibling := archiveFixture(t)
	_, err = archiveReader(t, sibling, first.ID()).Read(t.Context(), ArchiveReadRequest{ArchiveID: first.ID(), SegmentID: "seg-000001"})
	require.ErrorIs(t, err, ErrArchiveNotFound, "same ID does not resolve through another owning session")
}

func TestArchiveCopyIndependentOfSourceDeletion(t *testing.T) {
	t.Parallel()
	repo, source, store := archiveFixture(t)
	staged := archiveStage(t, store, archiveEntries("copied evidence"))
	require.NoError(t, staged.Publish(t.Context()))
	require.NoError(t, staged.Close())
	destination, err := repo.Create(t.Context(), CreateOptions{WorkspaceID: "workspace", WorkspacePath: "/workspace"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, destination.Close()) })
	destinationStore, err := repo.Archives(destination)
	require.NoError(t, err)
	require.NoError(t, repo.CopyArchives(t.Context(), source, destination.Metadata().ID, []string{staged.ID()}))
	sourceInfo, err := os.Stat(archiveObjectPath(store, staged.ID()))
	require.NoError(t, err)
	copiedInfo, err := os.Stat(archiveObjectPath(destinationStore, staged.ID()))
	require.NoError(t, err)
	assert.False(t, os.SameFile(sourceInfo, copiedInfo), "fork owns an independent file, not a source hard link")
	require.NoError(t, source.Close())
	require.NoError(t, os.RemoveAll(filepath.Dir(archiveObjectPath(store, staged.ID()))))
	info, err := destinationStore.Verify(t.Context(), staged.ID())
	require.NoError(t, err)
	assert.Equal(t, source.Metadata().ID, info.Source.SessionID, "original source remains inert provenance")
	reader := archiveReader(t, destinationStore, staged.ID())
	matches, err := reader.Search(t.Context(), ArchiveSearchRequest{ArchiveID: staged.ID(), Query: "copied evidence"})
	require.NoError(t, err)
	require.Len(t, matches.Matches, 1)
}

func TestArchiveReadListAndSearchPagination(t *testing.T) {
	t.Parallel()
	_, _, store := archiveFixture(t)
	staged := archiveStage(t, store, archiveEntries("needle one\nneedle[0]\nno match\nneedle three"))
	require.NoError(t, staged.Publish(t.Context()))
	reader := archiveReader(t, store, staged.ID())
	segments, err := reader.List(t.Context(), ArchiveListRequest{ArchiveID: staged.ID(), Limit: 1})
	require.NoError(t, err)
	require.Len(t, segments.Segments, 1)
	segment := segments.Segments[0]
	var all []ArchiveLine
	for offset := 0; ; {
		page, readErr := reader.Read(t.Context(), ArchiveReadRequest{ArchiveID: staged.ID(), SegmentID: segment.ID, Offset: offset, Limit: 2})
		require.NoError(t, readErr)
		all = append(all, page.Lines...)
		if !page.More {
			break
		}
		require.Greater(t, page.NextOffset, offset)
		offset = page.NextOffset
	}
	require.Len(t, all, segment.Lines)
	for index, line := range all {
		assert.Equal(t, index+1, line.Number)
	}
	assert.Equal(t, "needle one", all[1].Text)
	assert.Equal(t, "needle[0]", all[2].Text)

	var cursor *ArchiveSearchCursor
	var matches []ArchiveMatch
	for {
		page, searchErr := reader.Search(t.Context(), ArchiveSearchRequest{ArchiveID: staged.ID(), Query: "needle", Limit: 1, Cursor: cursor})
		require.NoError(t, searchErr)
		matches = append(matches, page.Matches...)
		if page.Next == nil {
			break
		}
		assert.NotEqual(t, cursor, page.Next)
		cursor = page.Next
	}
	require.Len(t, matches, 4)
	literal, err := reader.Search(t.Context(), ArchiveSearchRequest{ArchiveID: staged.ID(), Query: "needle[0]"})
	require.NoError(t, err)
	require.Len(t, literal.Matches, 1)
	regex, err := reader.Search(t.Context(), ArchiveSearchRequest{ArchiveID: staged.ID(), Query: `^needle (one|two)$`, Regexp: true})
	require.NoError(t, err)
	require.Len(t, regex.Matches, 2)
	missing, err := reader.Search(t.Context(), ArchiveSearchRequest{ArchiveID: staged.ID(), Query: "absent"})
	require.NoError(t, err)
	assert.Empty(t, missing.Matches)
	assert.Nil(t, missing.Next)
}

func TestArchiveSegmentsWrapUnicodeAndBoundEncodedOutput(t *testing.T) {
	t.Parallel()
	_, _, store := archiveFixture(t)
	text := strings.Repeat("界<&\x00", 60_000)
	staged := archiveStage(t, store, archiveEntries(text))
	require.NoError(t, staged.Publish(t.Context()))
	reader := archiveReader(t, store, staged.ID())
	listing, err := reader.List(t.Context(), ArchiveListRequest{ArchiveID: staged.ID(), Limit: 1})
	require.NoError(t, err)
	require.True(t, listing.More)
	assert.Equal(t, 1, listing.Segments[0].FirstRecord)
	assert.Equal(t, 1, listing.Segments[0].LastRecord)
	var recovered strings.Builder
	for offset := 0; ; {
		segments, listErr := reader.List(t.Context(), ArchiveListRequest{ArchiveID: staged.ID(), Offset: offset, Limit: 1})
		require.NoError(t, listErr)
		for _, segment := range segments.Segments {
			for lineOffset := 0; ; {
				page, readErr := reader.Read(t.Context(), ArchiveReadRequest{ArchiveID: staged.ID(), SegmentID: segment.ID, Offset: lineOffset, Limit: 1000})
				require.NoError(t, readErr)
				encoded, marshalErr := json.Marshal(page)
				require.NoError(t, marshalErr)
				assert.LessOrEqual(t, len(encoded), ArchiveMaxOutputBytes)
				for _, line := range page.Lines {
					assert.True(t, utf8.ValidString(line.Text))
					assert.LessOrEqual(t, len(line.Text), archiveLineBytes)
					if strings.ContainsAny(line.Text, "界<&\x00") {
						recovered.WriteString(line.Text)
					}
				}
				if !page.More {
					break
				}
				require.Greater(t, page.NextOffset, lineOffset)
				lineOffset = page.NextOffset
			}
		}
		if !segments.More {
			break
		}
		offset = segments.NextOffset
	}
	assert.Equal(t, text, recovered.String(), "UTF-8 wrapping never drops the unwrapped text")
	matches, err := reader.Search(t.Context(), ArchiveSearchRequest{ArchiveID: staged.ID(), Query: "界", Limit: 100})
	require.NoError(t, err)
	encoded, err := json.Marshal(matches)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(encoded), ArchiveMaxOutputBytes)
	assert.NotNil(t, matches.Next)
}

func TestArchiveInvalidRequestsAndCancellation(t *testing.T) {
	t.Parallel()
	repo, handle, store := archiveFixture(t)
	staged := archiveStage(t, store, archiveEntries("evidence"))
	reader := archiveReader(t, store, staged.ID())
	for _, id := range []string{"", "../escape", "/tmp/data", strings.Repeat("A", 64), strings.Repeat("a", 63), strings.Repeat("a", 65)} {
		t.Run(fmt.Sprintf("id_%q", id), func(t *testing.T) {
			t.Parallel()
			require.ErrorIs(t, ValidateArchiveID(id), ErrArchiveInvalid)
			_, err := store.Verify(t.Context(), id)
			require.ErrorIs(t, err, ErrArchiveInvalid)
			_, err = store.Bind([]string{id})
			require.ErrorIs(t, err, ErrArchiveInvalid)
		})
	}
	_, err := repo.Archives(nil)
	require.ErrorIs(t, err, ErrArchiveInvalid)
	otherRepo, _, _ := archiveFixture(t)
	_, err = otherRepo.Archives(handle)
	require.ErrorIs(t, err, ErrArchiveInvalid)
	require.ErrorIs(t, repo.CopyArchives(t.Context(), handle, "../escape", []string{staged.ID()}), ErrArchiveInvalid)
	_, err = store.Bind(make([]string, ArchiveMaxReferences+1))
	require.ErrorIs(t, err, ErrArchiveLimit)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, staged.Publish(ctx), context.Canceled)
	_, err = store.Verify(ctx, staged.ID())
	require.ErrorIs(t, err, context.Canceled)
	_, err = reader.List(ctx, ArchiveListRequest{})
	require.ErrorIs(t, err, context.Canceled)
	_, err = reader.Read(ctx, ArchiveReadRequest{ArchiveID: staged.ID(), SegmentID: "seg-000001"})
	require.ErrorIs(t, err, context.Canceled)
	_, err = reader.Search(ctx, ArchiveSearchRequest{ArchiveID: staged.ID(), Query: "evidence"})
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, staged.Publish(t.Context()))
	for _, request := range []ArchiveReadRequest{
		{ArchiveID: staged.ID(), SegmentID: "../path"},
		{ArchiveID: staged.ID(), SegmentID: "seg-000001", Offset: -1},
		{ArchiveID: staged.ID(), SegmentID: "seg-000001", Offset: 10000},
		{ArchiveID: staged.ID(), SegmentID: "seg-000001", Limit: 1001},
	} {
		_, err := reader.Read(t.Context(), request)
		require.ErrorIs(t, err, ErrArchiveInvalid)
	}
	for _, request := range []ArchiveSearchRequest{
		{ArchiveID: staged.ID()},
		{ArchiveID: staged.ID(), Query: "[", Regexp: true},
		{ArchiveID: staged.ID(), Query: "a", Limit: 101},
		{ArchiveID: staged.ID(), Query: "a", Cursor: &ArchiveSearchCursor{SegmentID: "../path", Line: 1}},
		{ArchiveID: staged.ID(), Query: "a", Cursor: &ArchiveSearchCursor{SegmentID: "seg-000001", Line: 0}},
	} {
		_, err := reader.Search(t.Context(), request)
		require.ErrorIs(t, err, ErrArchiveInvalid)
	}
	for _, request := range []ArchiveListRequest{{Offset: -1}, {Offset: 2}, {Limit: 21}, {ArchiveID: staged.ID(), Offset: 1000}} {
		_, err := reader.List(t.Context(), request)
		require.ErrorIs(t, err, ErrArchiveInvalid)
	}
}

func TestArchiveCollisionAndFailedPublicationDoNotOverwrite(t *testing.T) {
	t.Parallel()
	_, _, store := archiveFixture(t)
	staged := archiveStage(t, store, archiveEntries("publish candidate"))
	path := archiveObjectPath(store, staged.ID())
	existing := bytes.Repeat([]byte{'x'}, 32)
	require.NoError(t, os.WriteFile(path, existing, 0o600))
	require.ErrorIs(t, staged.Publish(t.Context()), ErrArchiveCorrupt)
	unchanged, err := os.ReadFile(path) //nolint:gosec // Test-owned path under t.TempDir.
	require.NoError(t, err)
	assert.Equal(t, existing, unchanged)
	require.NoError(t, staged.Close())
	require.FileExists(t, path)
}

func TestArchiveConcurrentIdenticalPublication(t *testing.T) {
	t.Parallel()
	_, _, store := archiveFixture(t)
	first := archiveStage(t, store, archiveEntries("same object"))
	second := archiveStage(t, store, archiveEntries("same object"))
	var wait sync.WaitGroup
	results := make(chan error, 2)
	for _, staged := range []*StagedArchive{first, second} {
		wait.Go(func() { results <- staged.Publish(t.Context()) })
	}
	wait.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	_, err := store.Verify(t.Context(), first.ID())
	require.NoError(t, err)
}

func TestArchiveSymlinkAndNonprivateStorageRejected(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"history_symlink", "object_symlink", "object_directory", "history_mode", "object_mode"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			_, _, store := archiveFixture(t)
			if kind == "history_symlink" {
				target := t.TempDir()
				require.NoError(t, os.Symlink(target, filepath.Join(store.dir, store.sessionID+".history")))
				_, err := store.Stage(t.Context(), ArchiveSource{SessionID: store.sessionID, TipID: "branch"}, archiveEntries("private"))
				require.Error(t, err)
				names, err := os.ReadDir(target)
				require.NoError(t, err)
				assert.Empty(t, names)
				return
			}
			staged := archiveStage(t, store, archiveEntries("private"))
			require.NoError(t, staged.Publish(t.Context()))
			path := archiveObjectPath(store, staged.ID())
			switch kind {
			case "object_symlink":
				moved := filepath.Join(t.TempDir(), "object")
				require.NoError(t, os.Rename(path, moved))
				require.NoError(t, os.Symlink(moved, path))
			case "object_directory":
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Mkdir(path, 0o700))
			case "history_mode":
				require.NoError(t, os.Chmod(filepath.Dir(path), 0o750)) //nolint:gosec // Deliberately unsafe permissions exercise rejection.
			case "object_mode":
				require.NoError(t, os.Chmod(path, 0o640)) //nolint:gosec // Deliberately unsafe permissions exercise rejection.
			}
			_, err := store.Verify(t.Context(), staged.ID())
			require.Error(t, err)
			_, err = archiveReader(t, store, staged.ID()).Read(t.Context(), ArchiveReadRequest{ArchiveID: staged.ID(), SegmentID: "seg-000001"})
			require.Error(t, err)
		})
	}
}

func TestArchiveTamperingDetectedByReadSearchAndVerify(t *testing.T) {
	t.Parallel()
	_, _, store := archiveFixture(t)
	staged := archiveStage(t, store, archiveEntries("intact"))
	require.NoError(t, staged.Publish(t.Context()))
	path := archiveObjectPath(store, staged.ID())
	file, err := os.OpenFile(path, os.O_WRONLY, 0) //nolint:gosec // Tamper only with a test-owned object under t.TempDir.
	require.NoError(t, err)
	_, err = file.WriteAt([]byte("B"), 0)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	_, err = store.Verify(t.Context(), staged.ID())
	require.ErrorIs(t, err, ErrArchiveCorrupt)
	reader := archiveReader(t, store, staged.ID())
	_, err = reader.Read(t.Context(), ArchiveReadRequest{ArchiveID: staged.ID(), SegmentID: "seg-000001"})
	require.ErrorIs(t, err, ErrArchiveCorrupt)
	_, err = reader.Search(t.Context(), ArchiveSearchRequest{ArchiveID: staged.ID(), Query: "intact"})
	require.ErrorIs(t, err, ErrArchiveCorrupt)
}

func TestArchiveRejectsCorrectlyHashedMalformedManifest(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"version", "unknown_field", "duplicate_field", "path_bound", "record_count", "segment_digest", "segment_id", "segment_line_count", "source_path", "trailing_payload"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			_, _, store := archiveFixture(t)
			staged := archiveStage(t, store, archiveEntries("evidence"))
			require.NoError(t, staged.Publish(t.Context()))
			data, err := os.ReadFile(archiveObjectPath(store, staged.ID()))
			require.NoError(t, err)
			length := int(binary.BigEndian.Uint32(data[len(archiveMagic):archiveHeaderBytes]))
			var manifest archiveManifest
			require.NoError(t, json.Unmarshal(data[archiveHeaderBytes:archiveHeaderBytes+length], &manifest))
			payload := data[archiveHeaderBytes+length:]
			switch kind {
			case "version":
				manifest.Version++
			case "path_bound":
				manifest.Path.Bytes = ArchiveMaxTranscriptBytes + 1
			case "record_count":
				manifest.Records++
			case "segment_digest":
				manifest.Segments[0].SHA256 = strings.Repeat("0", 64)
			case "segment_id":
				manifest.Segments[0].ID = "../escape"
			case "segment_line_count":
				manifest.Segments[0].Lines++
			case "source_path":
				manifest.Source.SessionID = "../other"
			case "trailing_payload":
				payload = append(payload, 'x')
			}
			encoded, err := json.Marshal(manifest)
			require.NoError(t, err)
			if kind == "unknown_field" {
				encoded = append([]byte(`{"unexpected":1,`), encoded[1:]...)
			}
			if kind == "duplicate_field" {
				encoded = append([]byte(`{"version":1,`), encoded[1:]...)
			}
			object := make([]byte, archiveHeaderBytes)
			copy(object, archiveMagic)
			binary.BigEndian.PutUint32(object[len(archiveMagic):], uint32(len(encoded))) //nolint:gosec // Test manifest is tiny.
			object = append(object, encoded...)
			object = append(object, payload...)
			hash := sha256.Sum256(object)
			id := hex.EncodeToString(hash[:])
			require.NoError(t, os.WriteFile(archiveObjectPath(store, id), object, 0o600)) //nolint:gosec // Hex-encoded SHA-256 filename under t.TempDir.
			_, err = store.Verify(t.Context(), id)
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrArchiveCorrupt) || errors.Is(err, ErrArchiveLimit))
		})
	}
}

func TestArchiveBoundsAndStageFailures(t *testing.T) {
	t.Parallel()
	_, _, store := archiveFixture(t)
	source := ArchiveSource{SessionID: store.sessionID, TipID: "branch"}
	for _, entries := range [][]harness.Entry{nil, make([]harness.Entry, ArchiveMaxRecords+1)} {
		_, err := store.Stage(t.Context(), source, entries)
		require.ErrorIs(t, err, ErrArchiveLimit)
	}
	invalid := archiveEntries("evidence")
	invalid[0].ID = invalid[1].ID
	_, err := store.Stage(t.Context(), source, invalid)
	require.ErrorIs(t, err, ErrArchiveInvalid)
	_, err = store.Stage(t.Context(), ArchiveSource{SessionID: "different", TipID: "branch"}, archiveEntries("evidence"))
	require.ErrorIs(t, err, ErrArchiveInvalid)
	_, err = store.Stage(t.Context(), ArchiveSource{SessionID: store.sessionID, TipID: "stale"}, archiveEntries("evidence"))
	require.ErrorIs(t, err, ErrArchiveInvalid)
	_, err = store.Stage(t.Context(), source, archiveEntries(strings.Repeat("x", ArchiveMaxRecordBytes)))
	require.ErrorIs(t, err, ErrArchiveLimit)
	names, err := os.ReadDir(filepath.Join(store.dir, store.sessionID+".history"))
	require.NoError(t, err)
	assert.Empty(t, names, "failed staging does not retain temporary objects")
	id := strings.Repeat("0", 64)
	file, err := os.OpenFile(archiveObjectPath(store, id), os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	require.NoError(t, file.Truncate(archiveMaxObjectBytes+1))
	require.NoError(t, file.Close())
	_, err = store.Verify(t.Context(), id)
	require.ErrorIs(t, err, ErrArchiveLimit)
}

func TestArchiveWriteAndRenderFailuresPropagate(t *testing.T) {
	t.Parallel()
	failure := errors.New("injected archive write failure")
	_, err := writeArchiveSections(t.Context(), archiveFailWriter{failure}, io.Discard,
		ArchiveSource{SessionID: "session", TipID: "branch"}, archiveEntries("evidence"))
	require.ErrorIs(t, err, failure)
	_, err = writeArchiveSections(t.Context(), io.Discard, archiveFailWriter{failure},
		ArchiveSource{SessionID: "session", TipID: "branch"}, archiveEntries("evidence"))
	require.ErrorIs(t, err, failure)
	renderer := archiveRenderer{writer: io.Discard, total: ArchiveMaxRenderedBytes}
	require.ErrorIs(t, renderer.text(t.Context(), 1, "x"), ErrArchiveLimit)
	renderer = archiveRenderer{writer: io.Discard, segments: make([]archiveSegment, ArchiveMaxSegments)}
	require.NoError(t, renderer.text(t.Context(), 1, "x"))
	require.ErrorIs(t, renderer.flush(), ErrArchiveLimit)
}

type archiveFailWriter struct{ err error }

func (w archiveFailWriter) Write([]byte) (int, error) { return 0, w.err }
