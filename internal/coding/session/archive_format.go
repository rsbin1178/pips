//nolint:wsl_v5 // Bounded archive encoding and validation keep checks in wire order.
package session

import (
	"bufio"
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
	"strings"
	"unicode/utf8"

	"github.com/rsbin1178/pips/agent/harness"
)

// Format: magic, big-endian uint32 manifest length, canonical manifest JSON,
// canonical JSONL records, then indexed UTF-8 segments in order. SHA-256 of
// every byte is the object ID; section digests permit checked bounded reads.
// There is one published file, at most three staging files, and no path fields.
const (
	archiveMagic       = "PIPSAR01"
	archiveSchema      = "pips.coding.session.archive/v1"
	archiveHeaderBytes = len(archiveMagic) + 4
)

type archiveSection struct {
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type archiveSegment struct {
	ID          string `json:"id"`
	Bytes       int64  `json:"bytes"`
	SHA256      string `json:"sha256"`
	Lines       int    `json:"lines"`
	FirstRecord int    `json:"first_record"`
	LastRecord  int    `json:"last_record"`
}

type archiveManifest struct {
	Schema   string           `json:"schema"`
	Version  int              `json:"version"`
	Source   ArchiveSource    `json:"source"`
	Records  int              `json:"records"`
	Path     archiveSection   `json:"path"`
	Segments []archiveSegment `json:"segments"`
}

func (m archiveManifest) info(id string) ArchiveInfo {
	info := ArchiveInfo{ID: id, Source: m.Source, Records: m.Records, TranscriptBytes: m.Path.Bytes, Segments: len(m.Segments)}
	for _, segment := range m.Segments {
		info.RenderedBytes += segment.Bytes
	}
	return info
}

func stageArchive(ctx context.Context, dir *os.File, source ArchiveSource, entries []harness.Entry) (*StagedArchive, error) {
	pathFile, pathName, err := createArchiveTemp(dir)
	if err != nil {
		return nil, archiveStorageError(err)
	}
	defer func() { _ = pathFile.Close(); _ = removeArchiveFile(dir, pathName) }()
	textFile, textName, err := createArchiveTemp(dir)
	if err != nil {
		return nil, archiveStorageError(err)
	}
	defer func() { _ = textFile.Close(); _ = removeArchiveFile(dir, textName) }()

	manifest, err := writeArchiveSections(ctx, pathFile, textFile, source, entries)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	if len(encoded) > archiveManifestBytes {
		return nil, ErrArchiveLimit
	}
	var header [archiveHeaderBytes]byte
	copy(header[:], archiveMagic)
	binary.BigEndian.PutUint32(header[len(archiveMagic):], uint32(len(encoded))) //nolint:gosec // Manifest is bounded to 1 MiB above.
	contents := io.MultiReader(bytes.NewReader(header[:]), bytes.NewReader(encoded),
		io.NewSectionReader(pathFile, 0, manifest.Path.Bytes),
		io.NewSectionReader(textFile, 0, manifest.info("").RenderedBytes))

	file, name, err := createArchiveTemp(dir)
	if err != nil {
		return nil, archiveStorageError(err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(file, hash), archiveContextReader{ctx, contents})
	if copyErr == nil {
		copyErr = file.Sync()
	}
	if err := errors.Join(copyErr, file.Close()); err != nil {
		return nil, errors.Join(archiveStorageError(err), removeArchiveFile(dir, name))
	}
	id := hex.EncodeToString(hash.Sum(nil))
	return &StagedArchive{dir: dir, name: name, info: manifest.info(id)}, nil
}

func writeArchiveSections(ctx context.Context, pathFile, textFile io.Writer, source ArchiveSource, entries []harness.Entry) (archiveManifest, error) {
	manifest := archiveManifest{Schema: archiveSchema, Version: 1, Source: source, Records: len(entries)}
	hash := sha256.New()
	pathWriter := io.MultiWriter(pathFile, hash)
	renderer := archiveRenderer{writer: textFile}
	seen := make(map[string]struct{}, len(entries))
	for index, entry := range entries {
		if err := ctx.Err(); err != nil {
			return archiveManifest{}, err
		}
		if err := validateArchiveEntry(entry, seen); err != nil {
			return archiveManifest{}, err
		}
		encoded, err := harness.MarshalEntry(entry)
		if err != nil {
			return archiveManifest{}, errors.Join(ErrArchiveInvalid, err)
		}
		if len(encoded)+1 > ArchiveMaxRecordBytes || manifest.Path.Bytes+int64(len(encoded)+1) > ArchiveMaxTranscriptBytes {
			return archiveManifest{}, ErrArchiveLimit
		}
		// Rendering uses a decoded snapshot of exactly what was archived.
		snapshot, err := harness.UnmarshalEntry(encoded)
		if err != nil {
			return archiveManifest{}, errors.Join(ErrArchiveInvalid, err)
		}
		encoded = append(encoded, '\n')
		if _, err := pathWriter.Write(encoded); err != nil {
			return archiveManifest{}, archiveStorageError(err)
		}
		manifest.Path.Bytes += int64(len(encoded))
		if err := renderer.entry(ctx, index+1, snapshot, encoded); err != nil {
			return archiveManifest{}, err
		}
	}
	if err := renderer.flush(); err != nil {
		return archiveManifest{}, err
	}
	manifest.Path.SHA256 = hex.EncodeToString(hash.Sum(nil))
	manifest.Segments = renderer.segments
	return manifest, nil
}

func archiveProvenanceID(id string) bool {
	return id != "" && len(id) <= 256 && utf8.ValidString(id) && !strings.ContainsAny(id, "\x00\r\n")
}

func validateArchiveEntry(entry harness.Entry, seen map[string]struct{}) error {
	if !archiveProvenanceID(entry.ID) || entry.Time.IsZero() || entry.Kind == "" || len(entry.Kind) > 64 {
		return ErrArchiveInvalid
	}
	if _, duplicate := seen[entry.ID]; duplicate {
		return ErrArchiveInvalid
	}
	if entry.ParentID != "" && !archiveProvenanceID(entry.ParentID) {
		return ErrArchiveInvalid
	}
	if entry.Message != nil {
		if err := entry.Message.Validate(); err != nil {
			return errors.Join(ErrArchiveInvalid, err)
		}
	}
	seen[entry.ID] = struct{}{}
	return nil
}

func verifyArchiveAt(ctx context.Context, dir *os.File, name, id string) (*os.File, archiveManifest, error) {
	file, err := openArchiveFile(dir, name)
	if err != nil {
		return nil, archiveManifest{}, archiveStorageError(err)
	}
	manifest, err := verifyArchiveFile(ctx, file, id)
	if err != nil {
		return nil, archiveManifest{}, errors.Join(err, file.Close())
	}
	return file, manifest, nil
}

func verifyArchiveFile(ctx context.Context, file *os.File, id string) (archiveManifest, error) {
	stat, err := file.Stat()
	if err != nil {
		return archiveManifest{}, archiveStorageError(err)
	}
	if stat.Size() > archiveMaxObjectBytes || stat.Size() < int64(archiveHeaderBytes) {
		return archiveManifest{}, ErrArchiveLimit
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, archiveContextReader{ctx, io.NewSectionReader(file, 0, stat.Size())}); err != nil {
		return archiveManifest{}, archiveStorageError(err)
	}
	if hex.EncodeToString(hash.Sum(nil)) != id {
		return archiveManifest{}, ErrArchiveCorrupt
	}
	manifest, offset, err := readArchiveManifest(file, stat.Size())
	if err != nil {
		return archiveManifest{}, err
	}
	if err := verifyArchiveRecords(ctx, file, offset, manifest); err != nil {
		return archiveManifest{}, err
	}
	offset += manifest.Path.Bytes
	for _, segment := range manifest.Segments {
		if _, err := readArchiveSegment(ctx, file, offset, segment); err != nil {
			return archiveManifest{}, err
		}
		offset += segment.Bytes
	}
	return manifest, nil
}

func readArchiveManifest(file *os.File, size int64) (archiveManifest, int64, error) {
	var header [archiveHeaderBytes]byte
	if _, err := file.ReadAt(header[:], 0); err != nil || string(header[:len(archiveMagic)]) != archiveMagic {
		return archiveManifest{}, 0, ErrArchiveCorrupt
	}
	length := binary.BigEndian.Uint32(header[len(archiveMagic):])
	if length == 0 || length > archiveManifestBytes {
		return archiveManifest{}, 0, ErrArchiveLimit
	}
	encoded := make([]byte, int(length))
	if _, err := file.ReadAt(encoded, int64(archiveHeaderBytes)); err != nil {
		return archiveManifest{}, 0, ErrArchiveCorrupt
	}
	var manifest archiveManifest
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return archiveManifest{}, 0, errors.Join(ErrArchiveCorrupt, err)
	}
	canonical, err := json.Marshal(manifest)
	// Exact canonical form also rejects duplicate fields, trailing values and
	// noncanonical spellings instead of accepting ambiguous manifest input.
	if err != nil || !bytes.Equal(canonical, encoded) {
		return archiveManifest{}, 0, ErrArchiveCorrupt
	}
	offset := int64(archiveHeaderBytes) + int64(length)
	if err := validateArchiveManifest(manifest, size-offset); err != nil {
		return archiveManifest{}, 0, err
	}
	return manifest, offset, nil
}

func validateArchiveManifest(m archiveManifest, payloadBytes int64) error {
	if err := validateArchiveManifestHeader(m); err != nil {
		return err
	}
	var rendered int64
	lastRecord := 1
	for index, segment := range m.Segments {
		if err := validateArchiveSegment(segment, index, m.Records, lastRecord); err != nil {
			return err
		}
		rendered += segment.Bytes
		lastRecord = segment.LastRecord
	}
	if rendered > ArchiveMaxRenderedBytes {
		return ErrArchiveLimit
	}
	if m.Segments[0].FirstRecord != 1 || lastRecord != m.Records || m.Path.Bytes+rendered != payloadBytes {
		return ErrArchiveCorrupt
	}
	return nil
}

func validateArchiveManifestHeader(m archiveManifest) error {
	if m.Schema != archiveSchema || m.Version != 1 || validateSessionID(m.Source.SessionID) != nil || !archiveProvenanceID(m.Source.TipID) {
		return ErrArchiveCorrupt
	}
	if m.Records <= 0 || m.Records > ArchiveMaxRecords || m.Path.Bytes <= 0 || m.Path.Bytes > ArchiveMaxTranscriptBytes ||
		len(m.Segments) == 0 || len(m.Segments) > ArchiveMaxSegments {
		return ErrArchiveLimit
	}
	if ValidateArchiveID(m.Path.SHA256) != nil {
		return ErrArchiveCorrupt
	}
	return nil
}

func validateArchiveSegment(segment archiveSegment, index, records, previous int) error {
	if segment.ID != fmt.Sprintf("seg-%06d", index+1) || ValidateArchiveID(segment.SHA256) != nil ||
		segment.Bytes <= 0 || segment.Bytes > archiveSegmentBytes || segment.Lines <= 0 || int64(segment.Lines) > segment.Bytes ||
		segment.FirstRecord < previous || segment.FirstRecord > previous+1 ||
		segment.LastRecord < segment.FirstRecord || segment.LastRecord > records {
		return ErrArchiveCorrupt
	}
	return nil
}

func verifyArchiveRecords(ctx context.Context, file *os.File, offset int64, manifest archiveManifest) error {
	hash := sha256.New()
	reader := io.TeeReader(archiveContextReader{ctx, io.NewSectionReader(file, offset, manifest.Path.Bytes)}, hash)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), ArchiveMaxRecordBytes)
	seen := make(map[string]struct{}, manifest.Records)
	count := 0
	lastID := ""
	var bytesRead int64
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > manifest.Records {
			return ErrArchiveCorrupt
		}
		encoded := scanner.Bytes()
		entry, err := harness.UnmarshalEntry(encoded)
		if err != nil {
			return errors.Join(ErrArchiveCorrupt, err)
		}
		if err := validateArchiveEntry(entry, seen); err != nil {
			return errors.Join(ErrArchiveCorrupt, err)
		}
		canonical, err := harness.MarshalEntry(entry)
		if err != nil || !bytes.Equal(canonical, encoded) {
			return ErrArchiveCorrupt
		}
		lastID = entry.ID
		bytesRead += int64(len(encoded) + 1)
	}
	if err := scanner.Err(); err != nil {
		return errors.Join(ErrArchiveCorrupt, err)
	}
	if count != manifest.Records || lastID != manifest.Source.TipID || bytesRead != manifest.Path.Bytes ||
		hex.EncodeToString(hash.Sum(nil)) != manifest.Path.SHA256 {
		return ErrArchiveCorrupt
	}
	return nil
}

func readArchiveSegment(ctx context.Context, file *os.File, offset int64, segment archiveSegment) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	encoded := make([]byte, int(segment.Bytes))
	if _, err := file.ReadAt(encoded, offset); err != nil {
		return nil, errors.Join(ErrArchiveCorrupt, err)
	}
	hash := sha256.Sum256(encoded)
	if hex.EncodeToString(hash[:]) != segment.SHA256 || !utf8.Valid(encoded) ||
		encoded[len(encoded)-1] != '\n' || bytes.Count(encoded, []byte{'\n'}) != segment.Lines {
		return nil, ErrArchiveCorrupt
	}
	for line := range bytes.SplitSeq(encoded, []byte{'\n'}) {
		if len(line) > archiveLineBytes {
			return nil, ErrArchiveCorrupt
		}
	}
	return encoded, nil
}
