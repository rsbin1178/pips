package team

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rsbin/pips/internal/jsonx"
)

const (
	teamHeaderType = "team_aggregate"
	teamVersion    = 2
	teamFileExt    = ".jsonl"
)

type teamHeader struct {
	Type      string    `json:"type"`
	Version   int       `json:"version"`
	ID        ID        `json:"id"`
	CreatedAt time.Time `json:"created_at"`
}

// JSONLStore is a bounded single-process directory Store.
type JSONLStore struct {
	mu            sync.Mutex
	dir           string
	config        storeConfig
	migrationHook func(migrationStage) error
}

// NewJSONLStore opens a directory-backed Team store.
func NewJSONLStore(dir string, options ...StoreOption) (*JSONLStore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("%w: empty JSONL directory", ErrInvalid)
	}

	config, err := defaultStoreConfig(options...)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("team: create store directory: %w", err)
	}

	return &JSONLStore{dir: dir, config: config}, nil
}

// Create implements Store.
//
//nolint:gocyclo // Creation keeps private-file lifecycle, durability, and rollback in one transaction.
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

	headerData, err := json.Marshal(teamHeader{
		Type: teamHeaderType, Version: teamVersion,
		ID: record.Team.ID, CreatedAt: record.Team.CreatedAt,
	})
	if err != nil {
		return fmt.Errorf("team: encode store header: %w", err)
	}

	if int64(len(headerData)+len(recordData)+2) > store.config.limits.MaxFileBytes {
		return ErrStoreFull
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	path, err := store.path(record.Team.ID)
	if err != nil {
		return err
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // Team ID validation confines the path to the configured directory.
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrExists
		}

		return fmt.Errorf("team: create aggregate file: %w", err)
	}

	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(path)

		return fmt.Errorf("team: secure aggregate file: %w", err)
	}

	committed := false

	defer func() {
		if file != nil {
			_ = file.Close()
		}

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
		return fmt.Errorf("team: write aggregate file: %w", err)
	}

	if err := file.Sync(); err != nil {
		return fmt.Errorf("team: sync aggregate file: %w", err)
	}

	if err := file.Close(); err != nil {
		return fmt.Errorf("team: close aggregate file: %w", err)
	}

	file = nil

	if err := syncTeamDirectory(store.dir); err != nil {
		return err
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

	records, _, _, err := store.read(id)
	if err != nil {
		return Record{}, err
	}

	return cloneRecord(records[len(records)-1]), nil
}

// CompareAndSwap implements Store.
//
//nolint:gocyclo // CAS keeps migration, identity pinning, tail repair, and append in one transaction.
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

	records, size, version, err := store.read(id)
	if err != nil {
		return err
	}

	previous := records[len(records)-1]
	if err := validateNextRecord(previous, expected, next); err != nil {
		return err
	}

	if recordForCommand(records, next.Transition.CommandID) != nil {
		return ErrCommandConflict
	}

	if len(records) >= store.config.limits.MaxTransitions {
		return ErrStoreFull
	}

	if version == 1 {
		size, err = store.rewriteV2(id, records)
		if err != nil {
			return err
		}

		if err := store.runMigrationHook(migrationBeforeAppend); err != nil {
			return err
		}
	}

	if size+int64(len(data)+1) > store.config.limits.MaxFileBytes {
		return ErrStoreFull
	}

	path, err := store.path(id)
	if err != nil {
		return err
	}

	identity, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("team: inspect aggregate for append: %w", err)
	}

	if err := validateTeamFileInfo(path, identity); err != nil {
		return err
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // Team ID validation confines the path to the configured directory.
	if err != nil {
		return fmt.Errorf("team: open aggregate for append: %w", err)
	}
	defer func() { _ = file.Close() }()

	opened, err := file.Stat()
	if err != nil || !os.SameFile(identity, opened) {
		return &CorruptStoreError{Path: path, Reason: "aggregate changed while opening for append", Err: err}
	}

	if err := file.Truncate(size); err != nil {
		return fmt.Errorf("team: truncate uncommitted aggregate tail: %w", err)
	}

	if _, err := file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("team: append aggregate record: %w", err)
	}

	if err := file.Sync(); err != nil {
		return fmt.Errorf("team: sync aggregate record: %w", err)
	}

	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(opened, current) {
		return &CorruptStoreError{Path: path, Reason: "aggregate changed while appending", Err: err}
	}

	if err := validateTeamFileInfo(path, current); err != nil {
		return err
	}

	return nil
}

// LoadCommand implements Store.
func (store *JSONLStore) LoadCommand(
	ctx context.Context,
	id ID,
	commandID CommandID,
) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	records, _, _, err := store.read(id)
	if err != nil {
		return Record{}, err
	}

	record := recordForCommand(records, commandID)
	if record == nil {
		return Record{}, ErrNotFound
	}

	return cloneRecord(*record), nil
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
		return ListPage{}, fmt.Errorf("team: list aggregate files: %w", err)
	}

	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), teamFileExt) {
			continue
		}

		id := strings.TrimSuffix(entry.Name(), teamFileExt)
		if id > options.Cursor {
			ids = append(ids, id)
		}
	}

	sort.Strings(ids)
	count := min(len(ids), options.Limit)

	page := ListPage{Teams: make([]Team, 0, count)}
	for _, rawID := range ids[:count] {
		records, _, _, readErr := store.read(ID(rawID))
		if readErr != nil {
			return ListPage{}, readErr
		}

		page.Teams = append(page.Teams, cloneTeam(records[len(records)-1].Team))
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

	records, _, _, err := store.read(id)
	if err != nil {
		return nil, err
	}

	return cloneRecords(records), nil
}

// Changes implements Store.
func (store *JSONLStore) Changes(
	ctx context.Context,
	id ID,
	options ChangeOptions,
) (ChangePage, error) {
	if err := ctx.Err(); err != nil {
		return ChangePage{}, err
	}

	options, err := validateChangeOptions(options)
	if err != nil {
		return ChangePage{}, err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	records, _, _, err := store.read(id)
	if err != nil {
		return ChangePage{}, err
	}

	page := ChangePage{
		Changes:   make([]Change, 0, min(options.Limit, len(records))),
		NextAfter: options.AfterRevision,
	}
	for _, record := range records {
		if record.Team.Revision <= options.AfterRevision {
			continue
		}

		if len(page.Changes) == options.Limit {
			break
		}

		page.Changes = append(page.Changes, cloneChange(Change{
			Transition: record.Transition,
			Message:    record.Message,
		}))
		page.NextAfter = record.Team.Revision
	}

	return page, nil
}

// LoadMessage implements Store.
func (store *JSONLStore) LoadMessage(
	ctx context.Context,
	id ID,
	messageID MessageID,
) (Message, error) {
	if err := ctx.Err(); err != nil {
		return Message{}, err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	records, _, _, err := store.read(id)
	if err != nil {
		return Message{}, err
	}

	for _, record := range records {
		if record.Message != nil && record.Message.ID == messageID {
			return cloneMessage(*record.Message), nil
		}
	}

	return Message{}, ErrNotFound
}

// Mailbox implements Store.
func (store *JSONLStore) Mailbox(
	ctx context.Context,
	id ID,
	memberID MemberID,
	options MailboxOptions,
) (MessagePage, error) {
	if err := ctx.Err(); err != nil {
		return MessagePage{}, err
	}

	options, err := validateMailboxOptions(options)
	if err != nil {
		return MessagePage{}, err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	records, _, _, err := store.read(id)
	if err != nil {
		return MessagePage{}, err
	}

	page := MessagePage{
		Messages:  make([]Message, 0, min(options.Limit, len(records))),
		NextAfter: options.AfterSequence,
	}
	for _, record := range records {
		if record.Message == nil || record.Message.RecipientID != memberID ||
			record.Message.Sequence <= options.AfterSequence {
			continue
		}

		if len(page.Messages) == options.Limit {
			break
		}

		page.Messages = append(page.Messages, cloneMessage(*record.Message))
		page.NextAfter = record.Message.Sequence
	}

	return page, nil
}

func (store *JSONLStore) read(id ID) ([]Record, int64, int, error) {
	path, err := store.path(id)
	if err != nil {
		return nil, 0, 0, err
	}

	data, err := readTeamFile(path, store.config.limits.MaxFileBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, 0, ErrNotFound
		}

		return nil, 0, 0, fmt.Errorf("team: read aggregate file: %w", err)
	}

	records, committedSize, version, err := decodeTeamFile(path, id, data, store.config.limits)
	if err != nil {
		return nil, 0, 0, err
	}

	return records, int64(committedSize), version, nil
}

func decodeTeamFile(path string, id ID, data []byte, limits StoreLimits) ([]Record, int, int, error) {
	lines, committedSize, err := committedTeamLines(path, data)
	if err != nil {
		return nil, 0, 0, err
	}

	version, err := validateTeamHeader(path, id, lines[0])
	if err != nil {
		return nil, 0, 0, err
	}

	var records []Record
	if version == 1 {
		records, err = decodeLegacyRecordLines(path, id, lines[1:], limits)
	} else {
		records, err = decodeTeamRecordLines(path, id, lines[1:], limits)
	}

	if err != nil {
		return nil, 0, 0, err
	}

	return records, committedSize, version, nil
}

func committedTeamLines(path string, data []byte) ([][]byte, int, error) {
	if len(data) == 0 {
		return nil, 0, &CorruptStoreError{Path: path, Reason: "empty file"}
	}

	terminated := data[len(data)-1] == '\n'
	committedSize := len(data)

	lines := bytes.Split(data, []byte{'\n'})
	if terminated {
		lines = lines[:len(lines)-1]
	} else {
		last := lines[len(lines)-1]
		if json.Valid(last) {
			return nil, 0, &CorruptStoreError{
				Path: path, Line: len(lines), Reason: "valid record lacks newline commit marker",
			}
		}

		committedSize -= len(last)
		lines = lines[:len(lines)-1]
	}

	if len(lines) < 2 {
		return nil, 0, &CorruptStoreError{Path: path, Reason: "missing header or initial record"}
	}

	return lines, committedSize, nil
}

func validateTeamHeader(path string, id ID, line []byte) (int, error) {
	var header teamHeader
	if err := decodeStrictJSON(line, &header); err != nil {
		return 0, &CorruptStoreError{Path: path, Line: 1, Reason: "invalid header", Err: err}
	}

	if header.Type != teamHeaderType || header.ID != id {
		return 0, &CorruptStoreError{Path: path, Line: 1, Reason: "foreign header or ID mismatch"}
	}

	if header.Version != 1 && header.Version != teamVersion {
		return 0, &CorruptStoreError{Path: path, Line: 1, Reason: "unsupported version"}
	}

	return header.Version, nil
}

func decodeTeamRecordLines(path string, id ID, lines [][]byte, limits StoreLimits) ([]Record, error) {
	records := make([]Record, 0, len(lines))
	commands := make(map[CommandID]struct{}, len(lines))
	events := make(map[EventID]struct{}, len(lines))

	for index, line := range lines {
		lineNumber := index + 2

		record, err := decodeTeamRecordLine(path, id, line, lineNumber, limits.MaxRecordBytes)
		if err != nil {
			return nil, err
		}

		if _, exists := commands[record.Transition.CommandID]; exists {
			return nil, &CorruptStoreError{
				Path: path, Line: lineNumber, Reason: "duplicate command ID",
			}
		}

		if _, exists := events[record.Transition.ID]; exists {
			return nil, &CorruptStoreError{
				Path: path, Line: lineNumber, Reason: "duplicate event ID",
			}
		}

		if err := validateTeamRecordSequence(path, lineNumber, records, record); err != nil {
			return nil, err
		}

		commands[record.Transition.CommandID] = struct{}{}
		events[record.Transition.ID] = struct{}{}
		records = append(records, record)
	}

	if len(records) > limits.MaxTransitions {
		return nil, ErrStoreFull
	}

	return records, nil
}

func decodeTeamRecordLine(path string, id ID, line []byte, lineNumber, maxBytes int) (Record, error) {
	if len(line) == 0 || len(line) > maxBytes {
		return Record{}, &CorruptStoreError{
			Path: path, Line: lineNumber, Reason: "empty or oversized record",
		}
	}

	var record Record
	if err := decodeStrictJSON(line, &record); err != nil {
		return Record{}, &CorruptStoreError{
			Path: path, Line: lineNumber, Reason: "invalid record", Err: err,
		}
	}

	if err := validateRecord(record); err != nil {
		return Record{}, &CorruptStoreError{
			Path: path, Line: lineNumber, Reason: "record validation failed", Err: err,
		}
	}

	if record.Team.ID != id {
		return Record{}, &CorruptStoreError{
			Path: path, Line: lineNumber, Reason: "Team ID changed",
		}
	}

	return record, nil
}

func validateTeamRecordSequence(path string, lineNumber int, records []Record, record Record) error {
	if len(records) == 0 {
		if err := validateCreateRecord(record); err != nil {
			return &CorruptStoreError{
				Path: path, Line: lineNumber, Reason: "invalid initial record", Err: err,
			}
		}

		return nil
	}

	previous := records[len(records)-1]
	if err := validateNextRecord(previous, previous.Team.Revision, record); err != nil {
		return &CorruptStoreError{Path: path, Line: lineNumber, Reason: "revision gap", Err: err}
	}

	return nil
}

func recordForCommand(records []Record, commandID CommandID) *Record {
	for index := range records {
		if records[index].Transition.CommandID == commandID {
			return &records[index]
		}
	}

	return nil
}

func (store *JSONLStore) path(id ID) (string, error) {
	if err := validateSafeID("Team id", string(id)); err != nil {
		return "", err
	}

	return filepath.Join(store.dir, string(id)+teamFileExt), nil
}

func decodeStrictJSON(data []byte, value any) error {
	return jsonx.Decode(data, value)
}

func readTeamFile(path string, maximum int64) (_ []byte, returnErr error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}

	if err := validateTeamFileInfo(path, info); err != nil {
		return nil, err
	}

	file, err := os.Open(path) //nolint:gosec // Team ID validation confines the path to the configured directory.
	if err != nil {
		return nil, err
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()

	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, &CorruptStoreError{Path: path, Reason: "aggregate changed while opening", Err: err}
	}

	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}

	if int64(len(data)) > maximum {
		return nil, ErrStoreFull
	}

	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(opened, current) {
		return nil, &CorruptStoreError{Path: path, Reason: "aggregate changed while reading", Err: err}
	}

	if err := validateTeamFileInfo(path, current); err != nil {
		return nil, err
	}

	return data, nil
}

func syncTeamDirectory(path string) error {
	directory, err := os.Open(path) //nolint:gosec // Store constructor owns this configured directory.
	if err != nil {
		return fmt.Errorf("team: open store directory for sync: %w", err)
	}

	syncErr := directory.Sync()

	closeErr := directory.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return fmt.Errorf("team: sync store directory: %w", err)
	}

	return nil
}

func validateTeamFileInfo(path string, info os.FileInfo) error {
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return &CorruptStoreError{
			Path: path, Reason: "aggregate must be a private regular file",
		}
	}

	return nil
}

var _ Store = (*JSONLStore)(nil)
