//nolint:wsl_v5 // JSONL transaction and rollback stages intentionally stay adjacent.
package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// jsonlHeader is the first line of a session file.
type jsonlHeader struct {
	Type      string            `json:"type"`
	Version   int               `json:"version"`
	ID        string            `json:"id"`
	CreatedAt time.Time         `json:"created_at"`
	Extra     map[string]string `json:"extra,omitempty"`
}

const (
	jsonlHeaderType    = "harness_session"
	jsonlVersion       = 1
	jsonlExt           = ".jsonl"
	maxSessionFileSize = 128 << 20
	maxSessionLineSize = 16 << 20
	maxSessionEntries  = 100_000
	defaultPrefixBytes = 2 << 20
	defaultPrefixItems = 2048
)

// JSONLPrefixLimits bound the read-only prefix used by session pickers and
// indexes. Zero values select conservative defaults.
type JSONLPrefixLimits struct {
	MaxBytes   int
	MaxEntries int
}

// JSONLPrefix is a validated, bounded prefix of one session file. Truncated
// reports that more durable data exists after Entries.
type JSONLPrefix struct {
	Metadata  SessionMetadata
	Entries   []Entry
	Truncated bool
}

// JSONLStore persists a session as a JSON-Lines file: a header line followed
// by one entry per line, appended as the session grows. Entries are cached in
// memory, so reads never touch the file after opening. A file expects a
// single process and a single writing Session.
type JSONLStore struct {
	mu       sync.Mutex
	meta     SessionMetadata
	entries  []Entry
	file     *os.File
	identity os.FileInfo
}

// CreateJSONL creates a new session file at path (parent directories
// included). An empty id gets a generated one.
func CreateJSONL(path, id string, extra map[string]string) (*JSONLStore, error) {
	if id == "" {
		id = newID()
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("harness: create session dir: %w", err)
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // caller-chosen session path is the API
	if err != nil {
		return nil, fmt.Errorf("harness: create session file: %w", err)
	}
	identity, err := file.Stat()
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("harness: inspect created session file: %w", err),
			file.Close(),
		)
	}

	header := jsonlHeader{
		Type:      jsonlHeaderType,
		Version:   jsonlVersion,
		ID:        id,
		CreatedAt: time.Now().UTC(),
		Extra:     extra,
	}

	store := &JSONLStore{
		meta:     SessionMetadata{ID: header.ID, CreatedAt: header.CreatedAt, Path: path, Extra: extra},
		file:     file,
		identity: identity,
	}
	if err := store.writeLine(header); err != nil {
		_ = file.Close()
		return nil, err
	}

	return store, nil
}

// OpenJSONL opens an existing session file, validating its header and
// loading all entries.
//
//nolint:gocyclo // Bounded read, strict decode, validation, and append-open form one audit boundary.
func OpenJSONL(path string) (*JSONLStore, error) {
	file, info, err := openRegularJSONL(path, os.O_RDWR|os.O_APPEND)
	if err != nil {
		return nil, fmt.Errorf("harness: open session file: %w", err)
	}
	keepOpen := false
	defer func() {
		if !keepOpen {
			_ = file.Close()
		}
	}()
	if info.Size() > maxSessionFileSize {
		return nil, fmt.Errorf("harness: %s: session file exceeds %d bytes", path, maxSessionFileSize)
	}

	data, err := io.ReadAll(io.LimitReader(file, maxSessionFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("harness: read session file: %w", err)
	}
	if len(data) > maxSessionFileSize {
		return nil, fmt.Errorf("harness: %s: session file exceeds %d bytes", path, maxSessionFileSize)
	}

	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte{'\n'})
	if len(lines) == 0 || len(lines[0]) == 0 {
		return nil, fmt.Errorf("harness: %s: empty session file", path)
	}
	if len(lines)-1 > maxSessionEntries {
		return nil, fmt.Errorf("harness: %s: session exceeds %d entries", path, maxSessionEntries)
	}

	var header jsonlHeader
	if err := decodeStrictLine(lines[0], &header); err != nil {
		return nil, fmt.Errorf("harness: %s: invalid session header: %w", path, err)
	}
	if err := validateHeader(path, header); err != nil {
		return nil, err
	}

	entries := make([]Entry, 0, len(lines)-1)

	for i, line := range lines[1:] {
		if len(line) == 0 {
			continue
		}
		if len(line) > maxSessionLineSize {
			return nil, fmt.Errorf("harness: %s: line %d exceeds %d bytes", path, i+2, maxSessionLineSize)
		}

		var env entryJSON
		if err := decodeStrictLine(line, &env); err != nil {
			return nil, fmt.Errorf("harness: %s: line %d: %w", path, i+2, err)
		}

		entry, err := fromEnvelope(env)
		if err != nil {
			return nil, fmt.Errorf("harness: %s: line %d: %w", path, i+2, err)
		}

		entries = append(entries, entry)
	}

	store := &JSONLStore{
		meta:     SessionMetadata{ID: header.ID, CreatedAt: header.CreatedAt, Path: path, Extra: mapsClone(header.Extra)},
		entries:  cloneEntries(entries),
		file:     file,
		identity: info,
	}
	keepOpen = true

	return store, nil
}

// Metadata implements [Store].
func (s *JSONLStore) Metadata() SessionMetadata {
	return cloneMetadata(s.meta)
}

// Append implements [Store], writing the entry through to disk.
func (s *JSONLStore) Append(e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	envelope, err := toEnvelope(e)
	if err != nil {
		return err
	}

	if err := s.writeLine(envelope); err != nil {
		return err
	}

	s.entries = append(s.entries, cloneEntry(e))

	return nil
}

// Entries implements [Store].
func (s *JSONLStore) Entries() ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return cloneEntries(s.entries), nil
}

// Close releases the underlying file. The store is unusable afterwards.
func (s *JSONLStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil

	return err
}

func (s *JSONLStore) writeLine(v any) error {
	if err := s.ensurePathIdentityLocked(); err != nil {
		return err
	}

	blob, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("harness: encode session line: %w", err)
	}

	w := bufio.NewWriter(s.file)
	if _, err := w.Write(append(blob, '\n')); err != nil {
		return fmt.Errorf("harness: write session line: %w", err)
	}

	if err := w.Flush(); err != nil {
		return fmt.Errorf("harness: write session line: %w", err)
	}
	if err := s.file.Sync(); err != nil {
		return fmt.Errorf("harness: sync session line: %w", err)
	}

	return nil
}

func (s *JSONLStore) ensurePathIdentityLocked() error {
	if s.file == nil || s.identity == nil {
		return errors.New("harness: session store is closed")
	}

	opened, err := s.file.Stat()
	if err != nil {
		return fmt.Errorf("harness: inspect open session file: %w", err)
	}
	current, err := os.Lstat(s.meta.Path)
	if err != nil {
		return fmt.Errorf("harness: inspect session path before append: %w", err)
	}
	if !opened.Mode().IsRegular() || !current.Mode().IsRegular() ||
		!os.SameFile(s.identity, opened) || !os.SameFile(s.identity, current) {
		return fmt.Errorf("harness: session path no longer identifies the opened regular file: %s", s.meta.Path)
	}

	return nil
}

func decodeStrictLine(data []byte, target any) error {
	if len(data) > maxSessionLineSize {
		return fmt.Errorf("line exceeds %d bytes", maxSessionLineSize)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}

		return err
	}

	return nil
}

func validateHeader(path string, header jsonlHeader) error {
	if header.Type != jsonlHeaderType || header.ID == "" {
		return fmt.Errorf("harness: %s: not a harness session file", path)
	}
	if header.Version != jsonlVersion {
		return fmt.Errorf("harness: %s: unsupported session version %d", path, header.Version)
	}
	if header.CreatedAt.IsZero() {
		return fmt.Errorf("harness: %s: session header has no creation time", path)
	}
	if err := validateSessionID(header.ID); err != nil {
		return fmt.Errorf("harness: %s: invalid session header: %w", path, err)
	}

	return nil
}

func mapsClone(value map[string]string) map[string]string {
	return maps.Clone(value)
}

// ReadJSONLMetadata reads and validates only the bounded header line. It does
// not open an append handle or scan conversation entries.
func ReadJSONLMetadata(path string) (SessionMetadata, error) {
	file, _, err := openRegularJSONL(path, os.O_RDONLY)
	if err != nil {
		return SessionMetadata{}, fmt.Errorf("harness: open session metadata: %w", err)
	}
	defer file.Close() //nolint:errcheck // read-only descriptor

	reader := bufio.NewReaderSize(io.LimitReader(file, maxSessionLineSize+1), 64<<10)
	line, err := reader.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return SessionMetadata{}, fmt.Errorf("harness: read session metadata: %w", err)
	}
	line = bytes.TrimSuffix(line, []byte{'\n'})
	var header jsonlHeader
	if err := decodeStrictLine(line, &header); err != nil {
		return SessionMetadata{}, fmt.Errorf("harness: %s: invalid session header: %w", path, err)
	}
	if err := validateHeader(path, header); err != nil {
		return SessionMetadata{}, err
	}

	return SessionMetadata{
		ID: header.ID, CreatedAt: header.CreatedAt, Path: path, Extra: mapsClone(header.Extra),
	}, nil
}

// ReadJSONLPrefix reads a bounded, validated prefix without opening an append
// handle. It is intended for list/index projections that must not load an
// unbounded transcript. A line crossing the byte budget is left unread from
// the result and sets Truncated instead of being treated as corruption.
//
//nolint:gocyclo // Budgeted line reading and sequential graph validation stay explicit.
func ReadJSONLPrefix(path string, limits JSONLPrefixLimits) (JSONLPrefix, error) {
	if limits.MaxBytes <= 0 {
		limits.MaxBytes = defaultPrefixBytes
	}
	if limits.MaxEntries <= 0 {
		limits.MaxEntries = defaultPrefixItems
	}
	if limits.MaxBytes > maxSessionFileSize {
		limits.MaxBytes = maxSessionFileSize
	}
	if limits.MaxEntries > maxSessionEntries {
		limits.MaxEntries = maxSessionEntries
	}

	file, info, err := openRegularJSONL(path, os.O_RDONLY)
	if err != nil {
		return JSONLPrefix{}, fmt.Errorf("harness: open session prefix: %w", err)
	}
	defer file.Close() //nolint:errcheck // read-only descriptor
	if info.Size() > maxSessionFileSize {
		return JSONLPrefix{}, fmt.Errorf(
			"harness: %s: session file exceeds %d bytes", path, maxSessionFileSize,
		)
	}

	reader := bufio.NewReaderSize(file, 4<<10)
	headerLine, consumed, complete, err := readBoundedJSONLLine(reader, maxSessionLineSize, maxSessionLineSize)
	if err != nil {
		return JSONLPrefix{}, fmt.Errorf("harness: read session prefix: %w", err)
	}
	if !complete || len(headerLine) == 0 {
		return JSONLPrefix{}, fmt.Errorf("harness: %s: empty session file", path)
	}
	var header jsonlHeader
	if err := decodeStrictLine(headerLine, &header); err != nil {
		return JSONLPrefix{}, fmt.Errorf("harness: %s: invalid session header: %w", path, err)
	}
	if err := validateHeader(path, header); err != nil {
		return JSONLPrefix{}, err
	}

	result := JSONLPrefix{Metadata: SessionMetadata{
		ID: header.ID, CreatedAt: header.CreatedAt, Path: path, Extra: mapsClone(header.Extra),
	}}
	seen := make(map[string]int, min(limits.MaxEntries, 256))
	for len(result.Entries) < limits.MaxEntries && consumed < limits.MaxBytes {
		line, read, lineComplete, readErr := readBoundedJSONLLine(
			reader, maxSessionLineSize, limits.MaxBytes-consumed,
		)
		consumed += read
		if readErr != nil {
			return JSONLPrefix{}, fmt.Errorf("harness: read session prefix: %w", readErr)
		}
		if !lineComplete {
			result.Truncated = true
			break
		}
		if len(line) == 0 {
			if consumed >= int(info.Size()) {
				break
			}
			continue
		}

		var env entryJSON
		if err := decodeStrictLine(line, &env); err != nil {
			return JSONLPrefix{}, fmt.Errorf(
				"harness: %s: entry %d: %w", path, len(result.Entries)+1, err,
			)
		}
		entry, err := fromEnvelope(env)
		if err != nil {
			return JSONLPrefix{}, fmt.Errorf(
				"harness: %s: entry %d: %w", path, len(result.Entries)+1, err,
			)
		}
		if err := validateStoredEntry(entry, seen, result.Entries); err != nil {
			return JSONLPrefix{}, fmt.Errorf(
				"%w: entry %d: %w", ErrSessionCorrupt, len(result.Entries)+1, err,
			)
		}
		seen[entry.ID] = len(result.Entries)
		result.Entries = append(result.Entries, entry)
	}
	if consumed < int(info.Size()) {
		result.Truncated = true
	}
	result.Entries = cloneEntries(result.Entries)

	return result, nil
}

func readBoundedJSONLLine(
	reader *bufio.Reader,
	lineLimit int,
	byteBudget int,
) ([]byte, int, bool, error) {
	if byteBudget <= 0 {
		return nil, 0, false, nil
	}
	line := make([]byte, 0, min(byteBudget, 4<<10))
	consumed := 0
	for {
		fragment, err := reader.ReadSlice('\n')
		consumed += len(fragment)
		if consumed > byteBudget {
			return nil, consumed, false, nil
		}
		if len(line)+len(fragment) > lineLimit {
			return nil, consumed, false, fmt.Errorf("line exceeds %d bytes", lineLimit)
		}
		line = append(line, fragment...)
		switch {
		case err == nil:
			return bytes.TrimSuffix(line, []byte{'\n'}), consumed, true, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return line, consumed, true, nil
		default:
			return nil, consumed, false, err
		}
	}
}

// Repo manages a directory of JSONL session files, one file per session
// named "<id>.jsonl".
type Repo struct {
	// Dir is the directory holding the session files.
	Dir string
}

// Create starts a new stored session. An empty id gets a generated one.
func (r Repo) Create(id string, extra map[string]string) (*JSONLStore, error) {
	if id == "" {
		id = newID()
	}

	path, err := r.sessionPath(id)
	if err != nil {
		return nil, err
	}

	return CreateJSONL(path, id, mapsClone(extra))
}

// Open opens the stored session with the given id.
func (r Repo) Open(id string) (*JSONLStore, error) {
	path, err := r.sessionPath(id)
	if err != nil {
		return nil, err
	}

	return OpenJSONL(path)
}

// List returns metadata for every session in the repository directory.
func (r Repo) List() ([]SessionMetadata, error) {
	items, err := os.ReadDir(r.Dir)
	if os.IsNotExist(err) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("harness: list sessions: %w", err)
	}

	var metas []SessionMetadata

	for _, item := range items {
		if item.IsDir() || item.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(item.Name(), jsonlExt) {
			continue
		}

		meta, err := ReadJSONLMetadata(filepath.Join(r.Dir, item.Name()))
		if err != nil {
			continue // skip foreign or corrupt files
		}

		metas = append(metas, meta)
	}

	return metas, nil
}

func openRegularJSONL(path string, flags int) (*os.File, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("session path is not a regular file: %s", path)
	}

	file, err := os.OpenFile(path, flags, 0) //nolint:gosec // caller-chosen session path is the API
	if err != nil {
		return nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		return nil, nil, errors.Join(err, file.Close())
	}
	current, err := os.Lstat(path)
	if err != nil {
		return nil, nil, errors.Join(err, file.Close())
	}
	if !opened.Mode().IsRegular() || !current.Mode().IsRegular() ||
		!os.SameFile(before, opened) || !os.SameFile(opened, current) {
		return nil, nil, errors.Join(
			fmt.Errorf("session path changed while opening regular file: %s", path),
			file.Close(),
		)
	}

	return file, opened, nil
}

// Delete removes the stored session with the given id.
func (r Repo) Delete(id string) error {
	path, err := r.sessionPath(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("harness: delete session: %w", err)
	}

	return nil
}

// Fork copies the source session's path from the root through atEntryID
// (its whole active branch when atEntryID is empty) into a new session, and
// returns the new store positioned at the copied tip.
func (r Repo) Fork(sourceID, atEntryID, newID string) (*JSONLStore, error) {
	source, err := r.Open(sourceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = source.Close() }()

	sess, err := NewSession(source)
	if err != nil {
		return nil, err
	}

	return r.ForkSession(sess, atEntryID, newID, nil)
}

// ForkSession failure-atomically copies a validated source path into a new
// stored Session. extra overlays source header metadata; nil preserves it.
// The source Session remains unchanged and may stay open under its writer lock.
//
//nolint:gocyclo // Failure-atomic resource acquisition and cleanup remain in commit order.
func (r Repo) ForkSession(
	source *Session,
	atEntryID string,
	newID string,
	extra map[string]string,
) (*JSONLStore, error) {
	if source == nil {
		return nil, errors.New("harness: fork nil session")
	}

	if atEntryID == "" {
		atEntryID = source.LeafID()
	}

	path, err := source.pathFrom(atEntryID)
	if err != nil {
		return nil, err
	}
	path = normalizeForkPath(path)

	if newID == "" {
		newID = newIDValue()
	}
	targetPath, err := r.sessionPath(newID)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(r.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("harness: create session dir: %w", err)
	}
	temp, err := os.CreateTemp(r.Dir, ".fork-*.tmp")
	if err != nil {
		return nil, err
	}
	tempPath := temp.Name()
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)

		return nil, err
	}
	if err := os.Remove(tempPath); err != nil {
		return nil, err
	}
	metadata := source.Metadata()
	metadataExtra := metadata.Extra
	if extra != nil {
		metadataExtra = mapsClone(metadataExtra)
		maps.Copy(metadataExtra, extra)
	}
	forked, err := CreateJSONL(tempPath, newID, metadataExtra)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = forked.Close()
			_ = os.Remove(tempPath)
		}
	}()

	for _, entry := range path {
		if err := forked.Append(entry); err != nil {
			return nil, err
		}
	}
	if err := forked.Close(); err != nil {
		return nil, err
	}
	if err := os.Link(tempPath, targetPath); err != nil {
		return nil, fmt.Errorf("harness: commit fork: %w", err)
	}
	if err := os.Remove(tempPath); err != nil {
		_ = os.Remove(targetPath)

		return nil, fmt.Errorf("harness: commit fork cleanup: %w", err)
	}
	committed = true

	return OpenJSONL(targetPath)
}

// normalizeForkPath makes a selected linear branch self-contained. Labels
// targeting an omitted sibling branch have no effect in the fork and are
// dropped. Branch summaries keep their content, while an omitted source is
// represented by the existing root sentinel. Parent links are then rebuilt
// over the retained entries so later nodes never reference dropped metadata.
func normalizeForkPath(path []Entry) []Entry {
	normalized := make([]Entry, 0, len(path))
	retained := make(map[string]struct{}, len(path))
	parentID := ""
	for _, entry := range path {
		if entry.Kind == KindLabel {
			if _, ok := retained[entry.TargetID]; !ok {
				continue
			}
		}
		entry.ParentID = parentID
		if entry.Kind == KindBranchSummary && entry.FromID != rootEntryID {
			if _, ok := retained[entry.FromID]; !ok {
				entry.FromID = rootEntryID
			}
		}
		normalized = append(normalized, entry)
		retained[entry.ID] = struct{}{}
		parentID = entry.ID
	}

	return cloneEntries(normalized)
}

func (r Repo) sessionPath(id string) (string, error) {
	if err := validateSessionID(id); err != nil {
		return "", err
	}

	return filepath.Join(r.Dir, id+jsonlExt), nil
}

func validateSessionID(id string) error {
	if err := validateID("session id", id, false); err != nil {
		return err
	}
	if strings.ContainsAny(id, `/\\`) || id == "." || id == ".." {
		return fmt.Errorf("harness: invalid session id %q", id)
	}

	return nil
}

func newIDValue() string { return newID() }
