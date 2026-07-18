package harness

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	jsonlHeaderType = "harness_session"
	jsonlVersion    = 1
	jsonlExt        = ".jsonl"
)

// JSONLStore persists a session as a JSON-Lines file: a header line followed
// by one entry per line, appended as the session grows. Entries are cached in
// memory, so reads never touch the file after opening. A file expects a
// single process and a single writing Session.
type JSONLStore struct {
	mu      sync.Mutex
	meta    Metadata
	entries []Entry
	file    *os.File
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

	header := jsonlHeader{
		Type:      jsonlHeaderType,
		Version:   jsonlVersion,
		ID:        id,
		CreatedAt: time.Now().UTC(),
		Extra:     extra,
	}

	store := &JSONLStore{
		meta: Metadata{ID: header.ID, CreatedAt: header.CreatedAt, Path: path, Extra: extra},
		file: file,
	}
	if err := store.writeLine(header); err != nil {
		_ = file.Close()
		return nil, err
	}

	return store, nil
}

// OpenJSONL opens an existing session file, validating its header and
// loading all entries.
func OpenJSONL(path string) (*JSONLStore, error) {
	data, err := os.ReadFile(path) //nolint:gosec // caller-chosen session path is the API
	if err != nil {
		return nil, fmt.Errorf("harness: open session file: %w", err)
	}

	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return nil, fmt.Errorf("harness: %s: empty session file", path)
	}

	var header jsonlHeader
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
		return nil, fmt.Errorf("harness: %s: invalid session header: %w", path, err)
	}

	if header.Type != jsonlHeaderType || header.ID == "" {
		return nil, fmt.Errorf("harness: %s: not a harness session file", path)
	}

	if header.Version != jsonlVersion {
		return nil, fmt.Errorf("harness: %s: unsupported session version %d", path, header.Version)
	}

	entries := make([]Entry, 0, len(lines)-1)

	for i, line := range lines[1:] {
		if line == "" {
			continue
		}

		var env entryJSON
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			return nil, fmt.Errorf("harness: %s: line %d: %w", path, i+2, err)
		}

		entries = append(entries, fromEnvelope(env))
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // caller-chosen session path is the API
	if err != nil {
		return nil, fmt.Errorf("harness: open session file for append: %w", err)
	}

	return &JSONLStore{
		meta:    Metadata{ID: header.ID, CreatedAt: header.CreatedAt, Path: path, Extra: header.Extra},
		entries: entries,
		file:    file,
	}, nil
}

// Metadata implements [Store].
func (s *JSONLStore) Metadata() Metadata {
	return s.meta
}

// Append implements [Store], writing the entry through to disk.
func (s *JSONLStore) Append(e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.writeLine(toEnvelope(e)); err != nil {
		return err
	}

	s.entries = append(s.entries, e)

	return nil
}

// Entries implements [Store].
func (s *JSONLStore) Entries() ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.entries), nil
}

// Close releases the underlying file. The store is unusable afterwards.
func (s *JSONLStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.file.Close()
}

func (s *JSONLStore) writeLine(v any) error {
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

	return nil
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

	return CreateJSONL(filepath.Join(r.Dir, id+jsonlExt), id, extra)
}

// Open opens the stored session with the given id.
func (r Repo) Open(id string) (*JSONLStore, error) {
	return OpenJSONL(filepath.Join(r.Dir, id+jsonlExt))
}

// List returns metadata for every session in the repository directory.
func (r Repo) List() ([]Metadata, error) {
	items, err := os.ReadDir(r.Dir)
	if os.IsNotExist(err) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("harness: list sessions: %w", err)
	}

	var metas []Metadata

	for _, item := range items {
		if item.IsDir() || !strings.HasSuffix(item.Name(), jsonlExt) {
			continue
		}

		store, err := OpenJSONL(filepath.Join(r.Dir, item.Name()))
		if err != nil {
			continue // skip foreign or corrupt files
		}

		metas = append(metas, store.Metadata())
		_ = store.Close()
	}

	return metas, nil
}

// Delete removes the stored session with the given id.
func (r Repo) Delete(id string) error {
	if err := os.Remove(filepath.Join(r.Dir, id+jsonlExt)); err != nil {
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

	if atEntryID == "" {
		atEntryID = sess.LeafID()
	}

	path, err := sess.pathFrom(atEntryID)
	if err != nil {
		return nil, err
	}

	forked, err := r.Create(newID, source.Metadata().Extra)
	if err != nil {
		return nil, err
	}

	for _, entry := range path {
		if err := forked.Append(entry); err != nil {
			_ = forked.Close()
			return nil, err
		}
	}

	return forked, nil
}
