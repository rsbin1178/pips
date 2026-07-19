package continuation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	continuationHeaderType = "continuation_execution"
	continuationVersion    = 1
	continuationFileExt    = ".jsonl"
)

type continuationHeader struct {
	Type      string    `json:"type"`
	Version   int       `json:"version"`
	ID        ID        `json:"id"`
	CreatedAt time.Time `json:"created_at"`
}

// JSONLStore is a bounded single-process directory Store.
type JSONLStore struct {
	mu     sync.Mutex
	dir    string
	config storeConfig
}

// NewJSONLStore opens a directory-backed control store.
func NewJSONLStore(dir string, options ...StoreOption) (*JSONLStore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("%w: empty JSONL directory", ErrInvalid)
	}

	config, err := defaultStoreConfig(options...)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("continuation: create control directory: %w", err)
	}

	return &JSONLStore{dir: dir, config: config}, nil
}

// Create implements Store.
func (store *JSONLStore) Create(ctx context.Context, record Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := validateCreateRecord(record); err != nil {
		return err
	}

	recordData, err := encodedRecord(record, store.config.limits)
	if err != nil {
		return err
	}

	headerData, err := json.Marshal(continuationHeader{
		Type: continuationHeaderType, Version: continuationVersion,
		ID: record.Execution.ID, CreatedAt: record.Execution.CreatedAt,
	})
	if err != nil {
		return fmt.Errorf("continuation: encode header: %w", err)
	}

	if int64(len(headerData)+len(recordData)+2) > store.config.limits.MaxFileBytes {
		return ErrStoreFull
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	path, err := store.path(record.Execution.ID)
	if err != nil {
		return err
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // ID validation confines the path to the configured directory.
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrExists
		}

		return fmt.Errorf("continuation: create execution file: %w", err)
	}

	committed := false

	defer func() {
		_ = file.Close()

		if !committed {
			_ = os.Remove(path)
		}
	}()

	data := make([]byte, 0, len(headerData)+len(recordData)+2)
	data = append(data, headerData...)
	data = append(data, '\n')
	data = append(data, recordData...)

	data = append(data, '\n')
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("continuation: write execution file: %w", err)
	}

	if err := file.Sync(); err != nil {
		return fmt.Errorf("continuation: sync execution file: %w", err)
	}

	committed = true

	return nil
}

// Load implements Store.
func (store *JSONLStore) Load(ctx context.Context, id ID) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	records, _, err := store.read(id)
	if err != nil {
		return Record{}, err
	}

	return cloneRecord(records[len(records)-1]), nil
}

// CompareAndSwap implements Store.
func (store *JSONLStore) CompareAndSwap(
	ctx context.Context,
	id ID,
	expected Revision,
	next Record,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	data, err := encodedRecord(next, store.config.limits)
	if err != nil {
		return err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	records, size, err := store.read(id)
	if err != nil {
		return err
	}

	previous := records[len(records)-1]
	if err := validateNextRecord(previous, expected, next); err != nil {
		return err
	}

	if len(records) >= store.config.limits.MaxTransitions ||
		size+int64(len(data)+1) > store.config.limits.MaxFileBytes {
		return ErrStoreFull
	}

	path, err := store.path(id)
	if err != nil {
		return err
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // ID validation confines the path to the configured directory.
	if err != nil {
		return fmt.Errorf("continuation: open execution for append: %w", err)
	}

	defer func() { _ = file.Close() }()

	if _, err := file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("continuation: append execution record: %w", err)
	}

	if err := file.Sync(); err != nil {
		return fmt.Errorf("continuation: sync execution record: %w", err)
	}

	return nil
}

// List implements Store.
func (store *JSONLStore) List(ctx context.Context, options ListOptions) (ListPage, error) {
	if err := ctx.Err(); err != nil {
		return ListPage{}, err
	}

	if err := validateListOptions(options, store.config.limits.MaxListPage); err != nil {
		return ListPage{}, err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	entries, err := os.ReadDir(store.dir)
	if err != nil {
		return ListPage{}, fmt.Errorf("continuation: list execution files: %w", err)
	}

	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), continuationFileExt) {
			continue
		}

		id := strings.TrimSuffix(entry.Name(), continuationFileExt)
		if id > options.Cursor {
			ids = append(ids, id)
		}
	}

	sort.Strings(ids)

	count := min(len(ids), options.Limit)

	page := ListPage{Executions: make([]Execution, 0, count)}
	for _, rawID := range ids[:count] {
		records, _, readErr := store.read(ID(rawID))
		if readErr != nil {
			return ListPage{}, readErr
		}

		page.Executions = append(page.Executions, cloneExecution(records[len(records)-1].Execution))
	}

	if len(ids) > count {
		page.NextCursor = ids[count-1]
	}

	return page, nil
}

// History implements Store.
func (store *JSONLStore) History(ctx context.Context, id ID) ([]Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	records, _, err := store.read(id)
	if err != nil {
		return nil, err
	}

	return cloneRecords(records), nil
}

func (store *JSONLStore) read(id ID) ([]Record, int64, error) {
	path, err := store.path(id)
	if err != nil {
		return nil, 0, err
	}

	data, err := os.ReadFile(path) //nolint:gosec // ID validation confines the path to the configured directory.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, ErrNotFound
		}

		return nil, 0, fmt.Errorf("continuation: read execution file: %w", err)
	}

	if int64(len(data)) > store.config.limits.MaxFileBytes {
		return nil, 0, ErrStoreFull
	}

	records, err := decodeExecutionFile(path, id, data, store.config.limits)
	if err != nil {
		return nil, 0, err
	}

	return records, int64(len(data)), nil
}

func decodeExecutionFile(path string, id ID, data []byte, limits StoreLimits) ([]Record, error) {
	lines, err := committedLines(path, data)
	if err != nil {
		return nil, err
	}

	if err := validateExecutionHeader(path, id, lines[0]); err != nil {
		return nil, err
	}

	return decodeRecordLines(path, id, lines[1:], limits)
}

func committedLines(path string, data []byte) ([][]byte, error) {
	if len(data) == 0 {
		return nil, &CorruptStoreError{Path: path, Reason: "empty file"}
	}

	terminated := data[len(data)-1] == '\n'

	lines := bytes.Split(data, []byte{'\n'})
	if terminated {
		lines = lines[:len(lines)-1]
	} else {
		last := lines[len(lines)-1]
		if json.Valid(last) {
			return nil, &CorruptStoreError{Path: path, Line: len(lines), Reason: "valid record lacks newline commit marker"}
		}

		lines = lines[:len(lines)-1]
	}

	if len(lines) < 2 {
		return nil, &CorruptStoreError{Path: path, Reason: "missing header or initial record"}
	}

	return lines, nil
}

func validateExecutionHeader(path string, id ID, line []byte) error {
	var header continuationHeader
	if err := json.Unmarshal(line, &header); err != nil {
		return &CorruptStoreError{Path: path, Line: 1, Reason: "invalid header", Err: err}
	}

	if header.Type != continuationHeaderType || header.ID != id {
		return &CorruptStoreError{Path: path, Line: 1, Reason: "foreign header or ID mismatch"}
	}

	if header.Version != continuationVersion {
		return &CorruptStoreError{Path: path, Line: 1, Reason: "unsupported version"}
	}

	return nil
}

func decodeRecordLines(path string, id ID, lines [][]byte, limits StoreLimits) ([]Record, error) {
	records := make([]Record, 0, len(lines))
	for index, line := range lines {
		lineNumber := index + 2

		record, err := decodeRecordLine(path, id, line, lineNumber, limits.MaxRecordBytes)
		if err != nil {
			return nil, err
		}

		if err := validateRecordSequence(path, lineNumber, records, record); err != nil {
			return nil, err
		}

		records = append(records, record)
	}

	if len(records) > limits.MaxTransitions {
		return nil, ErrStoreFull
	}

	return records, nil
}

func decodeRecordLine(path string, id ID, line []byte, lineNumber, maxBytes int) (Record, error) {
	if len(line) == 0 || len(line) > maxBytes {
		return Record{}, &CorruptStoreError{Path: path, Line: lineNumber, Reason: "empty or oversized record"}
	}

	var record Record
	if err := json.Unmarshal(line, &record); err != nil {
		return Record{}, &CorruptStoreError{Path: path, Line: lineNumber, Reason: "invalid record", Err: err}
	}

	if err := validateRecord(record); err != nil {
		return Record{}, &CorruptStoreError{Path: path, Line: lineNumber, Reason: "record validation failed", Err: err}
	}

	if record.Execution.ID != id {
		return Record{}, &CorruptStoreError{Path: path, Line: lineNumber, Reason: "execution ID changed"}
	}

	return record, nil
}

func validateRecordSequence(path string, lineNumber int, records []Record, record Record) error {
	if len(records) == 0 {
		if err := validateCreateRecord(record); err != nil {
			return &CorruptStoreError{Path: path, Line: lineNumber, Reason: "invalid initial record", Err: err}
		}

		return nil
	}

	previous := records[len(records)-1]
	if err := validateNextRecord(previous, previous.Execution.Revision, record); err != nil {
		return &CorruptStoreError{Path: path, Line: lineNumber, Reason: "revision gap", Err: err}
	}

	return nil
}

func (store *JSONLStore) path(id ID) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}

	return filepath.Join(store.dir, string(id)+continuationFileExt), nil
}

var _ Store = (*JSONLStore)(nil)
