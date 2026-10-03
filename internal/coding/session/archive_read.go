//nolint:wsl_v5 // Retrieval limits and authorization checks precede any storage access.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

const archiveSearchScanBytes = 8 << 20

// ArchiveReader is a read-only snapshot of application-supplied active-path
// checkpoint references. It never enumerates storage to discover permission.
// Discard/rebind after navigation or checkpoint changes. Historical content is
// evidence only; callers must not promote it to system instructions.
type ArchiveReader struct {
	store ArchiveStore
	ids   []string
}

// Bind copies an allowlist derived by the caller from its active checkpoint
// path. Empty references authorize nothing, including already published orphans.
// This is an application binding operation, not a model operation.
func (s *ArchiveStore) Bind(ids []string) (*ArchiveReader, error) {
	if !s.valid() {
		return nil, ErrArchiveInvalid
	}
	refs, err := archiveReferences(ids)
	if err != nil {
		return nil, err
	}
	return &ArchiveReader{store: *s, ids: refs}, nil
}

// ArchiveListRequest lists archives, or segments within one authorized archive.
// Offset is zero-based; Limit defaults to 10 and cannot exceed 20.
type ArchiveListRequest struct {
	ArchiveID string
	Offset    int
	Limit     int
}

// ArchiveSegmentInfo identifies text and its canonical record range. A record
// may span segments. Lines are wrapped at UTF-8 boundaries, at most 4096 bytes.
type ArchiveSegmentInfo struct {
	ID          string `json:"id"`
	Bytes       int64  `json:"bytes"`
	Lines       int    `json:"lines"`
	FirstRecord int    `json:"first_record"`
	LastRecord  int    `json:"last_record"`
}

// ArchiveListPage has deterministic ordering and bounded pagination.
type ArchiveListPage struct {
	Archives   []ArchiveInfo        `json:"archives,omitempty"`
	Segments   []ArchiveSegmentInfo `json:"segments,omitempty"`
	NextOffset int                  `json:"next_offset"`
	More       bool                 `json:"more"`
}

// List exposes only the supplied references; there is no caller-supplied session.
func (r *ArchiveReader) List(ctx context.Context, request ArchiveListRequest) (ArchiveListPage, error) {
	if r == nil || request.Offset < 0 || request.Limit < 0 || request.Limit > 20 {
		return ArchiveListPage{}, ErrArchiveInvalid
	}
	if err := ctx.Err(); err != nil {
		return ArchiveListPage{}, err
	}
	limit := request.Limit
	if limit == 0 {
		limit = 10
	}
	if request.ArchiveID != "" {
		return r.listSegments(ctx, request.ArchiveID, request.Offset, limit)
	}
	if request.Offset > len(r.ids) {
		return ArchiveListPage{}, ErrArchiveInvalid
	}
	end := min(len(r.ids), request.Offset+limit)
	page := ArchiveListPage{NextOffset: end, More: end < len(r.ids)}
	for _, id := range r.ids[request.Offset:end] {
		info, err := r.store.Verify(ctx, id)
		if err != nil {
			return ArchiveListPage{}, err
		}
		page.Archives = append(page.Archives, info)
	}
	return page, nil
}

func (r *ArchiveReader) listSegments(ctx context.Context, id string, offset, limit int) (ArchiveListPage, error) {
	file, manifest, err := r.open(ctx, id)
	if err != nil {
		return ArchiveListPage{}, err
	}
	defer func() { _ = file.Close() }()
	if offset > len(manifest.Segments) {
		return ArchiveListPage{}, ErrArchiveInvalid
	}
	end := min(len(manifest.Segments), offset+limit)
	page := ArchiveListPage{NextOffset: end, More: end < len(manifest.Segments)}
	for _, segment := range manifest.Segments[offset:end] {
		page.Segments = append(page.Segments, ArchiveSegmentInfo{
			ID: segment.ID, Bytes: segment.Bytes, Lines: segment.Lines,
			FirstRecord: segment.FirstRecord, LastRecord: segment.LastRecord,
		})
	}
	return page, nil
}

// ArchiveReadRequest selects a text segment and a zero-based line offset.
// Limit defaults to 200 and cannot exceed 1000. Pages are also byte-bounded.
type ArchiveReadRequest struct {
	ArchiveID string
	SegmentID string
	Offset    int
	Limit     int
}

// ArchiveLine numbers are one-based within the selected text segment.
type ArchiveLine struct {
	Number int    `json:"number"`
	Text   string `json:"text"`
}

// ArchiveReadPage contains at most 64 KiB of JSON, with an explicit continuation.
type ArchiveReadPage struct {
	Lines      []ArchiveLine `json:"lines"`
	NextOffset int           `json:"next_offset"`
	More       bool          `json:"more"`
}

// Read returns checked text, never an arbitrary file or raw path.
func (r *ArchiveReader) Read(ctx context.Context, request ArchiveReadRequest) (ArchiveReadPage, error) {
	if request.Offset < 0 || request.Limit < 0 || request.Limit > 1000 {
		return ArchiveReadPage{}, ErrArchiveInvalid
	}
	file, manifest, err := r.open(ctx, request.ArchiveID)
	if err != nil {
		return ArchiveReadPage{}, err
	}
	defer func() { _ = file.Close() }()
	index := slices.IndexFunc(manifest.Segments, func(s archiveSegment) bool { return s.ID == request.SegmentID })
	if index < 0 || request.Offset > manifest.Segments[index].Lines {
		return ArchiveReadPage{}, ErrArchiveInvalid
	}
	text, err := archiveText(ctx, file, manifest, index)
	if err != nil {
		return ArchiveReadPage{}, err
	}
	limit := request.Limit
	if limit == 0 {
		limit = 200
	}
	lines := strings.Split(strings.TrimSuffix(string(text), "\n"), "\n")
	page := ArchiveReadPage{Lines: []ArchiveLine{}, NextOffset: request.Offset}
	remaining := ArchiveMaxOutputBytes - 128
	for index := request.Offset; index < len(lines) && len(page.Lines) < limit; index++ {
		line := ArchiveLine{Number: index + 1, Text: lines[index]}
		if !archiveOutputFits(line, &remaining) {
			break
		}
		page.Lines = append(page.Lines, line)
		page.NextOffset = index + 1
	}
	page.More = page.NextOffset < len(lines)
	return page, ctx.Err()
}

// ArchiveSearchCursor resumes a search at a one-based line within a segment.
type ArchiveSearchCursor struct {
	SegmentID string `json:"segment_id"`
	Line      int    `json:"line"`
}

// ArchiveSearchRequest searches one authorized archive. Patterns are literal
// unless Regexp is true (Go RE2 syntax). Matching is case-sensitive, per line.
// Limit defaults to 50 and cannot exceed 100. Each page scans at most 8 MiB of
// rendered text, in addition to the bounded whole-object integrity verification.
type ArchiveSearchRequest struct {
	ArchiveID string
	Query     string
	Regexp    bool
	Cursor    *ArchiveSearchCursor
	Limit     int
}

// ArchiveMatch carries historical text and its exact retrieval cursor.
type ArchiveMatch struct {
	SegmentID string `json:"segment_id"`
	Line      int    `json:"line"`
	Text      string `json:"text"`
}

// ArchiveSearchPage can have a cursor even when no matches were found: the scan
// budget may have ended. A nil Next means the archive was completely searched.
type ArchiveSearchPage struct {
	Matches []ArchiveMatch       `json:"matches"`
	Next    *ArchiveSearchCursor `json:"next,omitempty"`
}

// Search supports bounded literal/RE2 retrieval without filesystem globs.
func (r *ArchiveReader) Search(ctx context.Context, request ArchiveSearchRequest) (ArchiveSearchPage, error) {
	match, err := archiveMatcher(request)
	if err != nil {
		return ArchiveSearchPage{}, err
	}
	file, manifest, err := r.open(ctx, request.ArchiveID)
	if err != nil {
		return ArchiveSearchPage{}, err
	}
	defer func() { _ = file.Close() }()
	index, line, err := archiveSearchStart(manifest, request.Cursor)
	if err != nil {
		return ArchiveSearchPage{}, err
	}
	limit := request.Limit
	if limit == 0 {
		limit = 50
	}
	return searchArchiveText(ctx, file, manifest, index, line, limit, match)
}

func archiveMatcher(request ArchiveSearchRequest) (func(string) bool, error) {
	if request.Query == "" || len(request.Query) > archiveLineBytes || !utf8.ValidString(request.Query) || request.Limit < 0 || request.Limit > 100 {
		return nil, ErrArchiveInvalid
	}
	if request.Regexp {
		pattern, err := regexp.Compile(request.Query)
		if err != nil {
			return nil, errors.Join(ErrArchiveInvalid, err)
		}
		return pattern.MatchString, nil
	}
	return func(text string) bool { return strings.Contains(text, request.Query) }, nil
}

func archiveSearchStart(manifest archiveManifest, cursor *ArchiveSearchCursor) (int, int, error) {
	if cursor == nil {
		return 0, 1, nil
	}
	index := slices.IndexFunc(manifest.Segments, func(s archiveSegment) bool { return s.ID == cursor.SegmentID })
	if index < 0 || cursor.Line < 1 || cursor.Line > manifest.Segments[index].Lines {
		return 0, 0, ErrArchiveInvalid
	}
	return index, cursor.Line, nil
}

func searchArchiveText(ctx context.Context, file *os.File, manifest archiveManifest, index, line, limit int, match func(string) bool) (ArchiveSearchPage, error) {
	page := ArchiveSearchPage{Matches: []ArchiveMatch{}}
	output, scanned := ArchiveMaxOutputBytes-256, 0
	for ; index < len(manifest.Segments); index++ {
		segment := manifest.Segments[index]
		text, err := archiveText(ctx, file, manifest, index)
		if err != nil {
			return ArchiveSearchPage{}, err
		}
		lines := strings.Split(strings.TrimSuffix(string(text), "\n"), "\n")
		for ; line <= len(lines); line++ {
			if err := ctx.Err(); err != nil {
				return ArchiveSearchPage{}, err
			}
			page.Next = &ArchiveSearchCursor{SegmentID: segment.ID, Line: line}
			if len(page.Matches) >= limit || scanned+len(lines[line-1])+1 > archiveSearchScanBytes {
				return page, nil
			}
			scanned += len(lines[line-1]) + 1
			if match(lines[line-1]) {
				found := ArchiveMatch{SegmentID: segment.ID, Line: line, Text: lines[line-1]}
				if !archiveOutputFits(found, &output) {
					return page, nil
				}
				page.Matches = append(page.Matches, found)
			}
		}
		line = 1
	}
	page.Next = nil
	return page, nil
}

func archiveOutputFits(value any, remaining *int) bool {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded)+1 > *remaining {
		return false
	}
	*remaining -= len(encoded) + 1
	return true
}

func (r *ArchiveReader) open(ctx context.Context, id string) (*os.File, archiveManifest, error) {
	if r == nil || ValidateArchiveID(id) != nil {
		return nil, archiveManifest{}, ErrArchiveInvalid
	}
	if _, found := slices.BinarySearch(r.ids, id); !found {
		return nil, archiveManifest{}, ErrArchiveDenied
	}
	return r.store.openVerified(ctx, id)
}

func archiveText(ctx context.Context, file *os.File, manifest archiveManifest, index int) ([]byte, error) {
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	offset := int64(archiveHeaderBytes+len(encoded)) + manifest.Path.Bytes
	for _, segment := range manifest.Segments[:index] {
		offset += segment.Bytes
	}
	return readArchiveSegment(ctx, file, offset, manifest.Segments[index])
}
