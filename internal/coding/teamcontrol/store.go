package teamcontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rsbin1178/pips/agent/team"
	"github.com/rsbin1178/pips/internal/jsonx"
)

const (
	directoryMode = fs.FileMode(0o700)
	journalMode   = fs.FileMode(0o600)
)

type journalRecord struct {
	Schema       string         `json:"schema"`
	Revision     Revision       `json:"revision"`
	MutationID   team.CommandID `json:"mutation_id"`
	MutationHash string         `json:"mutation_hash"`
	CommittedAt  time.Time      `json:"committed_at"`
	Entry        Entry          `json:"entry"`
}

type replayState struct {
	records    []journalRecord
	latest     map[team.CommandID]Entry
	mutations  map[team.CommandID]journalRecord
	validBytes int64
	tornTail   bool
}

// Store persists one strict append-only operator control journal per Team.
type Store struct {
	dir     string
	dirInfo fs.FileInfo
	limits  Limits
	now     func() time.Time
	mu      sync.Mutex
}

// New creates or opens a private operator control journal directory.
func New(directory string, limits Limits) (*Store, error) {
	if strings.TrimSpace(directory) == "" || strings.ContainsRune(directory, '\x00') {
		return nil, ErrInvalid
	}

	if limits == (Limits{}) {
		limits = DefaultLimits()
	}

	if err := validateLimits(limits); err != nil {
		return nil, err
	}

	abs, err := filepath.Abs(directory)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve repository: %w", ErrInvalid, err)
	}

	if err := os.MkdirAll(abs, directoryMode); err != nil {
		return nil, fmt.Errorf("coding team control: create repository: %w", err)
	}

	info, err := os.Lstat(abs)
	if err != nil {
		return nil, fmt.Errorf("coding team control: inspect repository: %w", err)
	}

	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return nil, ErrUnsafeFile
	}

	return &Store{dir: abs, dirInfo: info, limits: limits, now: time.Now}, nil
}

// Dir returns the absolute control journal directory.
func (s *Store) Dir() string {
	if s == nil {
		return ""
	}

	return s.dir
}

// Submit durably creates one pending operator command.
func (s *Store) Submit(ctx context.Context, command Command, expected Revision) (Record, error) {
	if s == nil {
		return Record{}, ErrInvalid
	}

	command.CreatedAt = command.CreatedAt.UTC()
	if err := validateCommand(command, s.limits); err != nil {
		return Record{}, err
	}

	entry := Entry{Command: command, State: StatePending, UpdatedAt: command.CreatedAt}

	return s.commit(ctx, Mutation{ID: command.ID, ExpectedRevision: expected}, entry)
}

// Begin persists the exact execution identity before an external side effect.
func (s *Store) Begin(
	ctx context.Context,
	teamID team.ID,
	commandID team.CommandID,
	mutation Mutation,
	resolved ResolvedTarget,
) (Record, error) {
	if previous, exists, err := s.loadMutation(ctx, teamID, mutation.ID); err != nil {
		return Record{}, err
	} else if exists {
		if previous.Entry.Command.ID != commandID || previous.Entry.State != StateApplying ||
			previous.Entry.Resolved == nil || *previous.Entry.Resolved != resolved ||
			!sameMutation(previous, mutation) {
			return Record{}, ErrIdempotency
		}

		return recordFromJournal(previous), nil
	}

	return s.transition(ctx, teamID, commandID, mutation, func(previous Entry) (Entry, error) {
		if previous.State != StatePending {
			return Entry{}, ErrConflict
		}

		value := resolved
		previous.State = StateApplying
		previous.Resolved = &value
		previous.UpdatedAt = s.now().UTC()

		return previous, nil
	})
}

// CompletePending records a command that could not resolve to an exact live
// execution. No external side effect has started, so only rejected and stale
// are valid terminal states and Resolved remains empty.
func (s *Store) CompletePending(
	ctx context.Context,
	teamID team.ID,
	commandID team.CommandID,
	mutation Mutation,
	state State,
	errorCode string,
) (Record, error) {
	if state != StateRejected && state != StateStale {
		return Record{}, ErrInvalid
	}
	if previous, exists, err := s.loadMutation(ctx, teamID, mutation.ID); err != nil {
		return Record{}, err
	} else if exists {
		if previous.Entry.Command.ID != commandID || previous.Entry.State != state ||
			previous.Entry.ErrorCode != errorCode || previous.Entry.Resolved != nil ||
			!sameMutation(previous, mutation) {
			return Record{}, ErrIdempotency
		}

		return recordFromJournal(previous), nil
	}

	return s.transition(ctx, teamID, commandID, mutation, func(previous Entry) (Entry, error) {
		if previous.State != StatePending || previous.Resolved != nil {
			return Entry{}, ErrConflict
		}

		previous.State = state
		previous.ErrorCode = errorCode
		previous.UpdatedAt = s.now().UTC()

		return previous, nil
	})
}

// Complete durably records one terminal result after applying or reconciling a command.
func (s *Store) Complete(
	ctx context.Context,
	teamID team.ID,
	commandID team.CommandID,
	mutation Mutation,
	state State,
	errorCode string,
) (Record, error) {
	if !terminalState(state) {
		return Record{}, ErrInvalid
	}

	if previous, exists, err := s.loadMutation(ctx, teamID, mutation.ID); err != nil {
		return Record{}, err
	} else if exists {
		if previous.Entry.Command.ID != commandID || previous.Entry.State != state ||
			previous.Entry.ErrorCode != errorCode || !sameMutation(previous, mutation) {
			return Record{}, ErrIdempotency
		}

		return recordFromJournal(previous), nil
	}

	return s.transition(ctx, teamID, commandID, mutation, func(previous Entry) (Entry, error) {
		if previous.State != StateApplying {
			return Entry{}, ErrConflict
		}

		previous.State = state
		previous.ErrorCode = errorCode
		previous.UpdatedAt = s.now().UTC()

		return previous, nil
	})
}

func (s *Store) loadMutation(
	ctx context.Context,
	teamID team.ID,
	mutationID team.CommandID,
) (journalRecord, bool, error) {
	if s == nil || !safeIDPattern.MatchString(string(teamID)) ||
		!safeIDPattern.MatchString(string(mutationID)) {
		return journalRecord{}, false, ErrInvalid
	}

	if err := ctx.Err(); err != nil {
		return journalRecord{}, false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.read(ctx, teamID)
	if err != nil {
		return journalRecord{}, false, err
	}

	value, exists := state.mutations[mutationID]
	if !exists {
		return journalRecord{}, false, nil
	}

	value.Entry = cloneEntry(value.Entry)

	return value, true, nil
}

func sameMutation(previous journalRecord, mutation Mutation) bool {
	hash, err := mutationHash(mutation.ExpectedRevision, mutation.ID, previous.Entry)

	return err == nil && hash == previous.MutationHash
}

func recordFromJournal(value journalRecord) Record {
	return Record{Revision: value.Revision, Entry: cloneEntry(value.Entry)}
}

func (s *Store) transition(
	ctx context.Context,
	teamID team.ID,
	commandID team.CommandID,
	mutation Mutation,
	change func(Entry) (Entry, error),
) (Record, error) {
	if s == nil || !safeIDPattern.MatchString(string(teamID)) ||
		!safeIDPattern.MatchString(string(commandID)) {
		return Record{}, ErrInvalid
	}

	previous, err := s.Get(ctx, teamID, commandID)
	if err != nil {
		return Record{}, err
	}

	next, err := change(previous.Entry)
	if err != nil {
		return Record{}, err
	}

	return s.commit(ctx, mutation, next)
}

// Get returns the latest durable state of one command.
func (s *Store) Get(ctx context.Context, teamID team.ID, commandID team.CommandID) (Record, error) {
	if s == nil || !safeIDPattern.MatchString(string(teamID)) ||
		!safeIDPattern.MatchString(string(commandID)) {
		return Record{}, ErrInvalid
	}

	if err := ctx.Err(); err != nil {
		return Record{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.read(ctx, teamID)
	if err != nil {
		return Record{}, err
	}

	entry, exists := state.latest[commandID]
	if !exists {
		return Record{}, ErrNotFound
	}

	return Record{Revision: latestRevisionFor(state.records, commandID), Entry: cloneEntry(entry)}, nil
}

// Revision returns the current journal revision. A Team without a control
// journal has revision zero; callers may use that value for the first Submit.
func (s *Store) Revision(ctx context.Context, teamID team.ID) (Revision, error) {
	if s == nil || !safeIDPattern.MatchString(string(teamID)) {
		return 0, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.read(ctx, teamID)
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	return Revision(len(state.records)), nil
}

// List returns a bounded page of journal transitions in revision order.
func (s *Store) List(ctx context.Context, teamID team.ID, options ListOptions) (Page, error) {
	if s == nil || !safeIDPattern.MatchString(string(teamID)) || options.Limit < 1 ||
		options.Limit > s.limits.MaxListResults {
		return Page{}, ErrInvalid
	}

	if err := ctx.Err(); err != nil {
		return Page{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.read(ctx, teamID)
	if err != nil {
		return Page{}, err
	}

	page := Page{Records: make([]Record, 0, min(options.Limit, len(state.records))), NextAfter: options.AfterRevision}
	for _, value := range state.records {
		if value.Revision <= options.AfterRevision {
			continue
		}

		if len(page.Records) == options.Limit {
			break
		}

		page.Records = append(page.Records, Record{Revision: value.Revision, Entry: cloneEntry(value.Entry)})
		page.NextAfter = value.Revision
	}

	return page, nil
}

// Pending returns bounded current pending/applying commands in creation order.
func (s *Store) Pending(ctx context.Context, teamID team.ID, limit int) ([]Record, error) {
	if s == nil || !safeIDPattern.MatchString(string(teamID)) || limit < 1 || limit > s.limits.MaxListResults {
		return nil, ErrInvalid
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.read(ctx, teamID)
	if err != nil {
		return nil, err
	}

	values := make([]Record, 0, min(limit, len(state.latest)))
	for id, entry := range state.latest {
		if entry.State == StatePending || entry.State == StateApplying {
			values = append(values, Record{Revision: latestRevisionFor(state.records, id), Entry: cloneEntry(entry)})
		}
	}

	sort.Slice(values, func(left, right int) bool {
		if values[left].Entry.Command.CreatedAt.Equal(values[right].Entry.Command.CreatedAt) {
			return values[left].Revision < values[right].Revision
		}

		return values[left].Entry.Command.CreatedAt.Before(values[right].Entry.Command.CreatedAt)
	})

	if len(values) > limit {
		values = values[:limit]
	}

	return values, nil
}

func (s *Store) commit(ctx context.Context, mutation Mutation, entry Entry) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}

	if !safeIDPattern.MatchString(string(mutation.ID)) || entry.Command.Target.TeamID == "" {
		return Record{}, ErrInvalid
	}

	if err := validateEntry(entry, s.limits); err != nil {
		return Record{}, err
	}

	hash, err := mutationHash(mutation.ExpectedRevision, mutation.ID, entry)
	if err != nil {
		return Record{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.commitLocked(ctx, mutation, entry, hash)
}

//nolint:gocyclo // One transaction deliberately owns identity, lock, replay, CAS, and append.
func (s *Store) commitLocked(
	ctx context.Context,
	mutation Mutation,
	entry Entry,
	hash string,
) (_ Record, returnErr error) {
	root, err := s.openRoot()
	if err != nil {
		return Record{}, err
	}
	defer func() { returnErr = errors.Join(returnErr, root.Close()) }()

	name := journalName(entry.Command.Target.TeamID)

	file, created, err := openJournalForWrite(root, name, mutation.ExpectedRevision)
	if err != nil {
		return Record{}, err
	}

	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()

	opened, err := file.Stat()
	if err != nil {
		return Record{}, fmt.Errorf("coding team control: inspect opened journal: %w", err)
	}

	if err := validateJournalInfo(opened); err != nil {
		return Record{}, err
	}

	if err := validateOpenIdentity(root, name, opened); err != nil {
		return Record{}, err
	}

	if err := acquireJournalLock(file); err != nil {
		return Record{}, err
	}

	defer func() { returnErr = errors.Join(returnErr, releaseJournalLock(file)) }()

	if created {
		if err := syncRoot(root); err != nil {
			return Record{}, err
		}
	}

	data, err := readBounded(file, s.limits.MaxFileBytes)
	if err != nil {
		return Record{}, err
	}

	state, err := replay(data, entry.Command.Target.TeamID, s.limits)
	if err != nil {
		return Record{}, err
	}

	if previous, exists := state.mutations[mutation.ID]; exists {
		if previous.MutationHash != hash {
			return Record{}, ErrIdempotency
		}

		return Record{Revision: previous.Revision, Entry: cloneEntry(previous.Entry)}, nil
	}

	if Revision(len(state.records)) != mutation.ExpectedRevision {
		return Record{}, ErrConflict
	}

	if err := validateSuccessor(state.latest[entry.Command.ID], entry); err != nil {
		return Record{}, err
	}

	if err := ctx.Err(); err != nil {
		return Record{}, err
	}

	if state.tornTail {
		if err := file.Truncate(state.validBytes); err != nil {
			return Record{}, fmt.Errorf("coding team control: truncate torn tail: %w", err)
		}
	}

	value := journalRecord{
		Schema: recordSchema, Revision: mutation.ExpectedRevision + 1,
		MutationID: mutation.ID, MutationHash: hash, CommittedAt: s.now().UTC(),
		Entry: cloneEntry(entry),
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		return Record{}, fmt.Errorf("coding team control: encode record: %w", err)
	}

	if len(encoded) > s.limits.MaxRecordBytes ||
		state.validBytes+int64(len(encoded)+1) > s.limits.MaxFileBytes || len(state.records) >= s.limits.MaxRecords {
		return Record{}, ErrLimit
	}

	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return Record{}, fmt.Errorf("coding team control: seek journal: %w", err)
	}

	if _, err := file.Write(append(encoded, '\n')); err != nil {
		return Record{}, fmt.Errorf("coding team control: append journal: %w", err)
	}

	if err := file.Sync(); err != nil {
		return Record{}, fmt.Errorf("coding team control: sync journal: %w", err)
	}

	if err := validateOpenIdentity(root, name, opened); err != nil {
		return Record{}, err
	}

	return Record{Revision: value.Revision, Entry: cloneEntry(value.Entry)}, nil
}

func (s *Store) read(ctx context.Context, teamID team.ID) (_ replayState, returnErr error) {
	root, err := s.openRoot()
	if err != nil {
		return replayState{}, err
	}
	defer func() { returnErr = errors.Join(returnErr, root.Close()) }()

	name := journalName(teamID)

	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return replayState{}, ErrNotFound
	}

	if err != nil {
		return replayState{}, fmt.Errorf("coding team control: inspect journal: %w", err)
	}

	if err := validateJournalInfo(info); err != nil {
		return replayState{}, err
	}

	file, err := root.Open(name)
	if err != nil {
		return replayState{}, fmt.Errorf("coding team control: open journal: %w", err)
	}

	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()

	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return replayState{}, ErrUnsafeFile
	}

	data, err := readBounded(file, s.limits.MaxFileBytes)
	if err != nil {
		return replayState{}, err
	}

	if err := ctx.Err(); err != nil {
		return replayState{}, err
	}

	if err := validateOpenIdentity(root, name, opened); err != nil {
		return replayState{}, err
	}

	return replay(data, teamID, s.limits)
}

//nolint:gocyclo // Replay validates every durable record and successor in one bounded pass.
func replay(data []byte, teamID team.ID, limits Limits) (replayState, error) {
	state := replayState{
		latest: make(map[team.CommandID]Entry), mutations: make(map[team.CommandID]journalRecord),
		validBytes: int64(len(data)),
	}

	complete := data
	if len(data) > 0 && data[len(data)-1] != '\n' {
		lastNewline := bytes.LastIndexByte(data, '\n')

		tail := data[lastNewline+1:]
		if len(tail) > limits.MaxRecordBytes || !isTornJSON(tail) {
			return replayState{}, ErrCorrupt
		}

		state.tornTail = true
		state.validBytes = int64(lastNewline + 1)
		complete = data[:lastNewline+1]
	}

	lines := bytes.Split(complete, []byte{'\n'})
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}

	if len(lines) > limits.MaxRecords {
		return replayState{}, ErrLimit
	}

	for index, line := range lines {
		if len(line) == 0 || len(line) > limits.MaxRecordBytes {
			return replayState{}, fmt.Errorf("%w: invalid record %d", ErrCorrupt, index+1)
		}

		var value journalRecord
		if err := jsonx.Decode(line, &value); err != nil {
			return replayState{}, fmt.Errorf("%w: decode record %d: %w", ErrCorrupt, index+1, err)
		}

		if err := validateJournalRecord(value, teamID, Revision(index+1), limits); err != nil {
			return replayState{}, fmt.Errorf("%w: record %d: %w", ErrCorrupt, index+1, err)
		}

		if _, exists := state.mutations[value.MutationID]; exists {
			return replayState{}, fmt.Errorf("%w: duplicate mutation", ErrCorrupt)
		}

		if err := validateSuccessor(state.latest[value.Entry.Command.ID], value.Entry); err != nil {
			return replayState{}, fmt.Errorf("%w: invalid successor: %w", ErrCorrupt, err)
		}

		state.records = append(state.records, value)
		state.latest[value.Entry.Command.ID] = cloneEntry(value.Entry)
		state.mutations[value.MutationID] = value
	}

	return state, nil
}

func validateJournalRecord(value journalRecord, teamID team.ID, revision Revision, limits Limits) error {
	if value.Schema != recordSchema || value.Revision != revision || value.Entry.Command.Target.TeamID != teamID ||
		!safeIDPattern.MatchString(string(value.MutationID)) || len(value.MutationHash) != 64 || value.CommittedAt.IsZero() {
		return ErrInvalid
	}

	if _, offset := value.CommittedAt.Zone(); offset != 0 {
		return ErrInvalid
	}

	if err := validateEntry(value.Entry, limits); err != nil {
		return err
	}

	hash, err := mutationHash(value.Revision-1, value.MutationID, value.Entry)
	if err != nil || hash != value.MutationHash {
		return ErrInvalid
	}

	return nil
}

//nolint:gocyclo // Durable state progression and identity immutability are checked together.
func validateSuccessor(previous, next Entry) error {
	if previous.Command.ID == "" {
		if next.State != StatePending {
			return ErrInvalid
		}

		return nil
	}

	if !sameCommand(previous.Command, next.Command) || previous.UpdatedAt.After(next.UpdatedAt) ||
		terminalState(previous.State) {
		return ErrInvalid
	}

	if previous.State == StatePending && next.State != StateApplying &&
		next.State != StateRejected && next.State != StateStale ||
		previous.State == StateApplying && !terminalState(next.State) {
		return ErrInvalid
	}

	if previous.State == StateApplying &&
		(previous.Resolved == nil || next.Resolved == nil || *previous.Resolved != *next.Resolved) {
		return ErrInvalid
	}

	return nil
}

func readBounded(file *os.File, maximum int64) ([]byte, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("coding team control: seek journal: %w", err)
	}

	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, fmt.Errorf("coding team control: read journal: %w", err)
	}

	if int64(len(data)) > maximum {
		return nil, ErrLimit
	}

	return data, nil
}

func isTornJSON(data []byte) bool {
	if len(data) == 0 {
		return true
	}

	var value any

	err := json.Unmarshal(data, &value)

	var syntax *json.SyntaxError

	return errors.As(err, &syntax) && syntax.Offset >= int64(len(data))
}

func (s *Store) openRoot() (*os.Root, error) {
	current, err := os.Lstat(s.dir)
	if err != nil || !os.SameFile(current, s.dirInfo) {
		return nil, fmt.Errorf("%w: repository path identity changed", ErrUnsafeFile)
	}

	if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || current.Mode().Perm() != directoryMode {
		return nil, ErrUnsafeFile
	}

	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return nil, fmt.Errorf("coding team control: open repository: %w", err)
	}

	opened, err := root.Lstat(".")
	if err != nil || !os.SameFile(opened, s.dirInfo) {
		_ = root.Close()
		return nil, fmt.Errorf("%w: repository changed while opening", ErrUnsafeFile)
	}

	return root, nil
}

//nolint:nestif // Create-or-open handles the adversarial replacement race without unsafe fallback.
func openJournalForWrite(
	root *os.Root,
	name string,
	expected Revision,
) (*os.File, bool, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		if expected != 0 {
			return nil, false, ErrConflict
		}

		file, createErr := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, journalMode)
		if createErr == nil {
			if chmodErr := file.Chmod(journalMode); chmodErr != nil {
				_ = file.Close()
				return nil, false, fmt.Errorf("coding team control: secure journal: %w", chmodErr)
			}

			return file, true, nil
		}

		if !errors.Is(createErr, fs.ErrExist) {
			return nil, false, fmt.Errorf("coding team control: create journal: %w", createErr)
		}

		info, err = root.Lstat(name)
	}

	if err != nil {
		return nil, false, fmt.Errorf("coding team control: inspect journal: %w", err)
	}

	if err := validateJournalInfo(info); err != nil {
		return nil, false, err
	}

	file, err := root.OpenFile(name, os.O_RDWR, journalMode)
	if err != nil {
		return nil, false, fmt.Errorf("coding team control: open journal: %w", err)
	}

	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = file.Close()
		return nil, false, fmt.Errorf("%w: journal changed while opening", ErrUnsafeFile)
	}

	return file, false, nil
}

func validateJournalInfo(info fs.FileInfo) error {
	if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != journalMode {
		return fmt.Errorf("%w: journal must be a private regular file", ErrUnsafeFile)
	}

	return nil
}

func validateOpenIdentity(root *os.Root, name string, opened fs.FileInfo) error {
	current, err := root.Lstat(name)
	if err != nil || !os.SameFile(current, opened) {
		return fmt.Errorf("%w: journal path identity changed", ErrUnsafeFile)
	}

	return validateJournalInfo(current)
}

func syncRoot(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("coding team control: open repository for sync: %w", err)
	}

	err = errors.Join(directory.Sync(), directory.Close())
	if err != nil {
		return fmt.Errorf("coding team control: sync repository: %w", err)
	}

	return nil
}

func journalName(id team.ID) string { return string(id) + ".jsonl" }

func latestRevisionFor(records []journalRecord, id team.CommandID) Revision {
	for _, v := range slices.Backward(records) {
		if v.Entry.Command.ID == id {
			return v.Revision
		}
	}

	return 0
}

func cloneEntry(value Entry) Entry {
	out := value
	out.Command.Payload = slices.Clone(value.Command.Payload)
	if value.Resolved != nil {
		resolved := *value.Resolved
		out.Resolved = &resolved
	}

	return out
}

func sameCommand(left, right Command) bool {
	return left.ID == right.ID && left.Action == right.Action && left.Target == right.Target &&
		left.Text == right.Text && left.CreatedAt.Equal(right.CreatedAt) &&
		bytes.Equal(left.Payload, right.Payload)
}
