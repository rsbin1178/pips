//nolint:wsl_v5 // Reference collection and its safety checks stay adjacent.
package session

import (
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
	"time"

	"github.com/rsbin1178/pips/agent/harness"
)

// Media GC collects the media reference set of one session directory and
// removes what it does not name. The reference set is the transcript plus
// every archive record, because an archived branch still needs its media. An
// unreadable artifact is an error: the caller must abort rather than delete a
// possibly referenced file.

// CollectMediaReferences returns every session-relative media path the
// session's transcript and archives name. A missing transcript or archive
// directory is empty, not an error; a corrupt artifact is an error.
func CollectMediaReferences(ctx context.Context, sessionDir string) (map[string]struct{}, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	refs := make(map[string]struct{})

	transcript := filepath.Join(sessionDir, TranscriptFileName)
	if err := collectTranscriptReferences(transcript, refs); err != nil {
		return nil, err
	}

	archives, err := os.ReadDir(filepath.Join(sessionDir, ArchiveDirName))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("coding session: list archives for media GC: %w", err)
	}

	for _, entry := range archives {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".archive") {
			continue
		}

		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if err := collectArchiveReferences(filepath.Join(sessionDir, ArchiveDirName, entry.Name()), refs); err != nil {
			return nil, err
		}
	}

	return refs, nil
}

func collectTranscriptReferences(path string, refs map[string]struct{}) error {
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("coding session: inspect transcript for media GC: %w", err)
	}

	store, err := harness.OpenJSONL(path)
	if err != nil {
		return fmt.Errorf("coding session: open transcript for media GC: %w", err)
	}
	defer func() { _ = store.Close() }()

	entries, err := store.Entries()
	if err != nil {
		return fmt.Errorf("coding session: read transcript for media GC: %w", err)
	}

	mergeMediaReferences(refs, MediaReferencesFromEntries(entries))

	return nil
}

func collectArchiveReferences(path string, refs map[string]struct{}) error {
	entries, err := readArchivePathEntries(path)
	if err != nil {
		return fmt.Errorf("coding session: read archive %q for media GC: %w", filepath.Base(path), err)
	}

	mergeMediaReferences(refs, MediaReferencesFromEntries(entries))

	return nil
}

// readArchivePathEntries reads the verified canonical record section of one
// archive object. It reads only the bounded manifest and path section, so a
// hostile archive cannot force an unbounded allocation.
//
//nolint:gocyclo // Bounded manifest and record validation stay in wire order.
func readArchivePathEntries(path string) ([]harness.Entry, error) {
	file, err := os.Open(path) //nolint:gosec // caller-owned session path
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	header := make([]byte, archiveHeaderBytes)
	if _, err := io.ReadFull(file, header); err != nil {
		return nil, ErrArchiveCorrupt
	}
	if string(header[:len(archiveMagic)]) != archiveMagic {
		return nil, ErrArchiveCorrupt
	}

	length := int(binary.BigEndian.Uint32(header[len(archiveMagic):]))
	if length <= 0 || length > archiveManifestBytes {
		return nil, ErrArchiveCorrupt
	}

	encoded := make([]byte, length)
	if _, err := io.ReadFull(file, encoded); err != nil {
		return nil, ErrArchiveCorrupt
	}

	var manifest archiveManifest
	if err := json.Unmarshal(encoded, &manifest); err != nil {
		return nil, ErrArchiveCorrupt
	}
	if manifest.Path.Bytes <= 0 || manifest.Path.Bytes > ArchiveMaxTranscriptBytes {
		return nil, ErrArchiveCorrupt
	}

	data := make([]byte, manifest.Path.Bytes)
	if _, err := io.ReadFull(file, data); err != nil {
		return nil, ErrArchiveCorrupt
	}

	sum := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), manifest.Path.SHA256) {
		return nil, ErrArchiveCorrupt
	}

	var entries []harness.Entry

	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(entries) >= ArchiveMaxRecords {
			return nil, ErrArchiveLimit
		}

		entry, err := harness.UnmarshalEntry([]byte(line))
		if err != nil {
			return nil, ErrArchiveCorrupt
		}

		entries = append(entries, entry)
	}

	return entries, nil
}

func mergeMediaReferences(into, from map[string]struct{}) {
	for path := range from {
		into[path] = struct{}{}
	}
}

// PruneSessionMedia removes the opened session's unreferenced media older than
// age. It returns the plan without deleting anything when dryRun is set. An
// unreadable reference aborts the prune before any deletion.
func PruneSessionMedia(
	ctx context.Context,
	sessionDir string,
	age time.Duration,
	dryRun bool,
) (GCReport, error) {
	store, err := NewMediaStore(sessionDir)
	if err != nil {
		return GCReport{}, err
	}

	refs, err := CollectMediaReferences(ctx, sessionDir)
	if err != nil {
		return GCReport{}, err
	}

	return store.GC(ctx, refs, age, dryRun)
}

// PruneMedia removes one stored session's unreferenced media older than age.
// It takes the session's writer lock, so a session another process holds is
// reported as locked instead of being pruned under a live writer.
func (r *Repository) PruneMedia(
	ctx context.Context,
	id string,
	age time.Duration,
	dryRun bool,
) (GCReport, error) {
	if err := r.validate(); err != nil {
		return GCReport{}, err
	}
	if err := validateSessionID(id); err != nil {
		return GCReport{}, err
	}
	if err := ctx.Err(); err != nil {
		return GCReport{}, err
	}

	group, found, err := r.root.FindSession(id)
	if err != nil {
		return GCReport{}, err
	}
	if !found {
		return GCReport{}, fmt.Errorf("%w: session %q is not stored", ErrInvalid, id)
	}

	sessionDir := group.SessionDirOf(id)
	lock, err := acquireSessionLock(ctx, filepath.Join(sessionDir, LockFileName))
	if err != nil {
		return GCReport{}, err
	}
	defer func() { _ = lock.Close() }()

	return PruneSessionMedia(ctx, sessionDir, age, dryRun)
}

// IdleSince returns the newest modification time among the session's top-level
// artifacts, or the zero time when nothing is readable. It is the TTL input for
// `pips session gc`.
func IdleSince(sessionDir string) (time.Time, bool) {
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return time.Time{}, false
	}

	var newest time.Time

	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}

	return newest, !newest.IsZero()
}
