//nolint:wsl_v5 // Journal transaction ordering is more important than whitespace grouping.
package teamstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/internal/jsonx"
)

const (
	recordSchema  = "pips.coding.team-resource/v1alpha1"
	directoryMode = fs.FileMode(0o700)
	journalMode   = fs.FileMode(0o600)
)

type record struct {
	Schema      string         `json:"schema"`
	CommandID   team.CommandID `json:"command_id"`
	CommandHash string         `json:"command_hash"`
	CommittedAt time.Time      `json:"committed_at"`
	Snapshot    Snapshot       `json:"snapshot"`
}

type replayState struct {
	latest     Snapshot
	commands   map[team.CommandID]record
	validBytes int64
	tornTail   bool
}

// Store persists one strict full-snapshot journal per Team.
type Store struct {
	dir     string
	dirInfo fs.FileInfo
	limits  Limits
	now     func() time.Time
	mu      sync.Mutex
}

// New creates or opens a private Team resource journal directory.
func New(directory string, limits Limits) (*Store, error) {
	if strings.TrimSpace(directory) == "" || strings.ContainsRune(directory, '\x00') {
		return nil, fmt.Errorf("%w: invalid repository path", ErrInvalid)
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
		return nil, fmt.Errorf("coding team state: create repository: %w", err)
	}
	dirInfo, err := os.Lstat(abs)
	if err != nil {
		return nil, fmt.Errorf("coding team state: inspect repository: %w", err)
	}
	if err := validateDirectoryInfo(dirInfo); err != nil {
		return nil, err
	}

	return &Store{dir: abs, dirInfo: dirInfo, limits: limits, now: time.Now}, nil
}

// Dir returns the absolute resource journal directory.
func (s *Store) Dir() string {
	if s == nil {
		return ""
	}

	return s.dir
}

// Commit atomically appends one idempotent optimistic resource mutation.
func (s *Store) Commit(ctx context.Context, mutation Mutation) (Snapshot, error) {
	if s == nil {
		return Snapshot{}, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}

	validated, err := validateMutation(mutation, s.limits)
	if err != nil {
		return Snapshot{}, err
	}
	hash, err := mutationHash(validated)
	if err != nil {
		return Snapshot{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.commitLocked(ctx, validated, hash)
}

// Load returns the latest durable snapshot without mutating or repairing its journal.
func (s *Store) Load(ctx context.Context, id team.ID) (Snapshot, error) {
	if s == nil || !safeIDPattern.MatchString(string(id)) {
		return Snapshot{}, fmt.Errorf("%w: invalid Team ID", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.readJournal(ctx, id)
	if err != nil {
		return Snapshot{}, err
	}
	if state.latest.Revision == 0 {
		return Snapshot{}, ErrNotFound
	}

	return cloneSnapshot(state.latest), nil
}

// ListByParent returns bounded newest-first resource projections for one Lead
// Session. It never repairs journals or acquires writer ownership.
//
//nolint:gocyclo // Bounded projection keeps filesystem checks and filtering in one read-only pass.
func (s *Store) ListByParent(
	ctx context.Context,
	parentSessionID string,
	limit int,
) ([]Snapshot, error) {
	if s == nil || strings.TrimSpace(parentSessionID) == "" ||
		limit < 1 || limit > s.limits.MaxListResults {
		return nil, fmt.Errorf("%w: invalid parent query", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	root, err := s.openRoot()
	if err != nil {
		return nil, fmt.Errorf("coding team state: open repository: %w", err)
	}
	defer func() { _ = root.Close() }()

	directory, err := root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("coding team state: open repository directory: %w", err)
	}
	defer func() { _ = directory.Close() }()

	entries, err := directory.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("coding team state: list repository: %w", err)
	}
	if len(entries) > s.limits.MaxTeams {
		return nil, ErrLimit
	}

	values := make([]Snapshot, 0, min(limit, len(entries)))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			return nil, fmt.Errorf("%w: unexpected repository entry %q", ErrUnsafeFile, name)
		}
		id := team.ID(strings.TrimSuffix(name, ".jsonl"))
		if !safeIDPattern.MatchString(string(id)) {
			return nil, fmt.Errorf("%w: unsafe journal name %q", ErrUnsafeFile, name)
		}
		state, readErr := s.readJournal(ctx, id)
		if errors.Is(readErr, ErrNotFound) {
			continue
		}
		if readErr != nil {
			return nil, readErr
		}
		if state.latest.Parent.SessionID == parentSessionID {
			values = append(values, cloneSnapshot(state.latest))
		}
	}

	slices.SortFunc(values, func(left, right Snapshot) int {
		if order := right.UpdatedAt.Compare(left.UpdatedAt); order != 0 {
			return order
		}
		return strings.Compare(string(left.TeamID), string(right.TeamID))
	})
	if len(values) > limit {
		values = values[:limit]
	}

	return values, nil
}

//nolint:gocyclo // Commit keeps locking, replay, CAS, append, and identity checks in one transaction.
func (s *Store) commitLocked(
	ctx context.Context,
	mutation Mutation,
	hash string,
) (_ Snapshot, returnErr error) {
	root, err := s.openRoot()
	if err != nil {
		return Snapshot{}, fmt.Errorf("coding team state: open repository: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, root.Close()) }()

	name := journalName(mutation.Snapshot.TeamID)
	file, created, err := openJournalForWrite(root, name, mutation.ExpectedRevision)
	if err != nil {
		return Snapshot{}, err
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()

	if err := acquireJournalLock(file); err != nil {
		return Snapshot{}, err
	}
	defer func() { returnErr = errors.Join(returnErr, releaseJournalLock(file)) }()

	openedInfo, err := file.Stat()
	if err != nil {
		return Snapshot{}, fmt.Errorf("coding team state: stat journal: %w", err)
	}
	if err := validateJournalInfo(openedInfo); err != nil {
		return Snapshot{}, err
	}
	if err := validateOpenIdentity(root, name, openedInfo); err != nil {
		return Snapshot{}, err
	}
	if created {
		if err := syncDirectory(s.dir); err != nil {
			return Snapshot{}, err
		}
	}

	data, err := readBounded(file, s.limits.MaxFileBytes)
	if err != nil {
		return Snapshot{}, err
	}
	state, err := replay(data, mutation.Snapshot.TeamID, s.limits)
	if err != nil {
		return Snapshot{}, err
	}
	if previous, exists := state.commands[mutation.CommandID]; exists {
		if previous.CommandHash != hash {
			return Snapshot{}, ErrIdempotency
		}

		return cloneSnapshot(previous.Snapshot), nil
	}
	if state.latest.Revision != mutation.ExpectedRevision {
		return Snapshot{}, ErrConflict
	}
	if state.latest.Revision > 0 {
		if err := validateSuccessor(state.latest, mutation.Snapshot); err != nil {
			return Snapshot{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}

	if state.tornTail {
		if err := file.Truncate(state.validBytes); err != nil {
			return Snapshot{}, fmt.Errorf("coding team state: truncate torn tail: %w", err)
		}
		if err := file.Sync(); err != nil {
			return Snapshot{}, fmt.Errorf("coding team state: sync truncated journal: %w", err)
		}
	}

	value := record{
		Schema: recordSchema, CommandID: mutation.CommandID, CommandHash: hash,
		CommittedAt: s.now().UTC(), Snapshot: cloneSnapshot(mutation.Snapshot),
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return Snapshot{}, fmt.Errorf("coding team state: encode record: %w", err)
	}
	if len(encoded) > s.limits.MaxRecordBytes || state.validBytes+int64(len(encoded)+1) > s.limits.MaxFileBytes {
		return Snapshot{}, ErrLimit
	}
	encoded = append(encoded, '\n')
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return Snapshot{}, fmt.Errorf("coding team state: seek journal: %w", err)
	}
	if _, err := file.Write(encoded); err != nil {
		return Snapshot{}, fmt.Errorf("coding team state: append journal: %w", err)
	}
	if err := file.Sync(); err != nil {
		return Snapshot{}, fmt.Errorf("coding team state: sync journal: %w", err)
	}
	if err := validateOpenIdentity(root, name, openedInfo); err != nil {
		return Snapshot{}, err
	}

	return cloneSnapshot(mutation.Snapshot), nil
}

func (s *Store) readJournal(ctx context.Context, id team.ID) (_ replayState, returnErr error) {
	root, err := s.openRoot()
	if err != nil {
		return replayState{}, fmt.Errorf("coding team state: open repository: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, root.Close()) }()

	name := journalName(id)
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return replayState{}, ErrNotFound
	}
	if err != nil {
		return replayState{}, fmt.Errorf("coding team state: inspect journal: %w", err)
	}
	if err := validateJournalInfo(info); err != nil {
		return replayState{}, err
	}

	file, err := root.Open(name)
	if err != nil {
		return replayState{}, fmt.Errorf("coding team state: open journal: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()

	openedInfo, err := file.Stat()
	if err != nil {
		return replayState{}, fmt.Errorf("coding team state: stat journal: %w", err)
	}
	if !os.SameFile(info, openedInfo) {
		return replayState{}, fmt.Errorf("%w: journal changed while opening", ErrUnsafeFile)
	}
	data, err := readBounded(file, s.limits.MaxFileBytes)
	if err != nil {
		return replayState{}, err
	}
	if err := ctx.Err(); err != nil {
		return replayState{}, err
	}
	if err := validateOpenIdentity(root, name, openedInfo); err != nil {
		return replayState{}, err
	}

	state, err := replay(data, id, s.limits)
	if err != nil {
		return replayState{}, err
	}
	if state.latest.Revision == 0 {
		return replayState{}, ErrNotFound
	}

	return state, nil
}

//nolint:gocyclo // Replay validates the complete durable record chain without fallthrough.
func replay(data []byte, id team.ID, limits Limits) (replayState, error) {
	state := replayState{commands: make(map[team.CommandID]record)}
	complete := data
	if len(data) > 0 && data[len(data)-1] != '\n' {
		lastNewline := bytes.LastIndexByte(data, '\n')
		tail := data[lastNewline+1:]
		if len(tail) > limits.MaxRecordBytes {
			return replayState{}, ErrLimit
		}
		if json.Valid(tail) || !isTornJSON(tail) {
			return replayState{}, fmt.Errorf("%w: final record lacks newline", ErrCorrupt)
		}
		state.tornTail = true
		state.validBytes = int64(lastNewline + 1)
		complete = data[:lastNewline+1]
	} else {
		state.validBytes = int64(len(data))
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
		var value record
		if err := jsonx.Decode(line, &value); err != nil {
			return replayState{}, fmt.Errorf("%w: decode record %d: %w", ErrCorrupt, index+1, err)
		}
		if err := validateRecord(value, id, limits); err != nil {
			return replayState{}, fmt.Errorf("%w: record %d: %w", ErrCorrupt, index+1, err)
		}
		if _, duplicate := state.commands[value.CommandID]; duplicate {
			return replayState{}, fmt.Errorf("%w: duplicate command", ErrCorrupt)
		}
		if value.Snapshot.Revision != state.latest.Revision+1 {
			return replayState{}, fmt.Errorf("%w: non-contiguous revision", ErrCorrupt)
		}
		if state.latest.Revision > 0 {
			if err := validateSuccessor(state.latest, value.Snapshot); err != nil {
				return replayState{}, fmt.Errorf("%w: invalid successor: %w", ErrCorrupt, err)
			}
		}
		state.latest = cloneSnapshot(value.Snapshot)
		state.commands[value.CommandID] = value
	}

	return state, nil
}

func validateRecord(value record, id team.ID, limits Limits) error {
	if value.Schema != recordSchema || value.Snapshot.TeamID != id ||
		!safeIDPattern.MatchString(string(value.CommandID)) || !validDigest(value.CommandHash) {
		return ErrInvalid
	}
	if value.CommittedAt.IsZero() {
		return ErrInvalid
	}
	_, offset := value.CommittedAt.Zone()
	if offset != 0 {
		return ErrInvalid
	}
	normalized := cloneSnapshot(value.Snapshot)
	normalizeSnapshot(&normalized)
	if !reflect.DeepEqual(normalized, value.Snapshot) {
		return fmt.Errorf("%w: snapshot is not canonical", ErrInvalid)
	}
	if err := validateSnapshot(value.Snapshot, limits); err != nil {
		return err
	}
	hash, err := mutationHash(Mutation{
		CommandID: value.CommandID, ExpectedRevision: value.Snapshot.Revision - 1,
		Snapshot: value.Snapshot,
	})
	if err != nil || hash != value.CommandHash {
		return fmt.Errorf("%w: command hash mismatch", ErrInvalid)
	}

	return nil
}

//nolint:gocyclo // Identity monotonicity is deliberately explicit for every resource binding.
func validateSuccessor(previous, next Snapshot) error {
	if next.Revision != previous.Revision+1 || next.TeamID != previous.TeamID ||
		!reflect.DeepEqual(next.Parent, previous.Parent) ||
		!reflect.DeepEqual(next.Repository, previous.Repository) ||
		!next.CreatedAt.Equal(previous.CreatedAt) || next.UpdatedAt.Before(previous.UpdatedAt) {
		return fmt.Errorf("%w: immutable Team binding changed", ErrInvalid)
	}

	previousMembers := make(map[team.MemberID]MemberResource, len(previous.Members))
	for _, member := range previous.Members {
		previousMembers[member.MemberID] = member
	}
	for _, member := range next.Members {
		if prior, exists := previousMembers[member.MemberID]; exists && prior != member {
			return fmt.Errorf("%w: member binding changed", ErrInvalid)
		}
		delete(previousMembers, member.MemberID)
	}
	if len(previousMembers) != 0 {
		return fmt.Errorf("%w: member binding removed", ErrInvalid)
	}
	previousAttempts := make(map[team.AttemptID]AttemptResource, len(previous.Attempts))
	for _, attempt := range previous.Attempts {
		previousAttempts[attempt.AttemptID] = attempt
	}
	for _, attempt := range next.Attempts {
		prior, exists := previousAttempts[attempt.AttemptID]
		if !exists {
			continue
		}
		if prior.TaskID != attempt.TaskID || prior.MemberID != attempt.MemberID ||
			prior.ContinuationID != attempt.ContinuationID ||
			prior.Session != (WorkerSessionResource{}) && prior.Session != attempt.Session ||
			!worktreeSuccessor(prior.Worktree, attempt.Worktree) {
			return fmt.Errorf("%w: Attempt binding changed", ErrInvalid)
		}
		delete(previousAttempts, attempt.AttemptID)
	}
	if len(previousAttempts) != 0 {
		return fmt.Errorf("%w: Attempt binding removed", ErrInvalid)
	}

	previousIntegrations := make(map[string]IntegrationResource, len(previous.Integrations))
	for _, integration := range previous.Integrations {
		previousIntegrations[integration.ID] = integration
	}
	for _, integration := range next.Integrations {
		prior, exists := previousIntegrations[integration.ID]
		if !exists {
			continue
		}
		if !slices.Equal(prior.AttemptIDs, integration.AttemptIDs) ||
			prior.ResourceRevision != 0 && prior.ResourceRevision != integration.ResourceRevision ||
			!worktreeSuccessor(prior.Worktree, integration.Worktree) ||
			!integrationStateSuccessor(prior.State, integration.State) ||
			prior.DiffDigest != "" && prior.DiffDigest != integration.DiffDigest ||
			prior.TreeOID != "" && prior.TreeOID != integration.TreeOID ||
			prior.CommitOID != "" && prior.CommitOID != integration.CommitOID ||
			prior.ManifestDigest != "" && prior.ManifestDigest != integration.ManifestDigest ||
			prior.VerificationDigest != "" &&
				prior.VerificationDigest != integration.VerificationDigest ||
			prior.JournalDigest != "" && prior.JournalDigest != integration.JournalDigest ||
			prior.ApprovalTokenHash != "" &&
				prior.ApprovalTokenHash != integration.ApprovalTokenHash {
			return fmt.Errorf("%w: Integration binding changed", ErrInvalid)
		}
		delete(previousIntegrations, integration.ID)
	}
	if len(previousIntegrations) != 0 {
		return fmt.Errorf("%w: Integration binding removed", ErrInvalid)
	}

	return nil
}

func integrationStateSuccessor(previous, next IntegrationState) bool {
	if previous == next {
		return true
	}

	var allowed []IntegrationState
	switch previous {
	case IntegrationPlanned:
		allowed = []IntegrationState{
			IntegrationReady, IntegrationVerified, IntegrationConflict,
			IntegrationFailed, IntegrationInterrupted, IntegrationRetained,
		}
	case IntegrationReady, IntegrationVerified, IntegrationApproved:
		allowed = []IntegrationState{
			IntegrationApplying, IntegrationInterrupted, IntegrationRetained,
		}
	case IntegrationApplying:
		allowed = []IntegrationState{
			IntegrationApplied, IntegrationInterrupted,
			IntegrationRolledBack, IntegrationRetained,
		}
	case IntegrationInterrupted:
		allowed = []IntegrationState{
			IntegrationApplied, IntegrationRolledBack, IntegrationRetained,
		}
	case IntegrationApplied, IntegrationConflict, IntegrationRolledBack,
		IntegrationFailed, IntegrationRetained:
	}

	return slices.Contains(allowed, next)
}

//nolint:gocyclo // Every previously observed Worktree identity component is immutable.
func worktreeSuccessor(previous, next WorktreeResource) bool {
	if previous == (WorktreeResource{}) {
		return true
	}
	return previous.ID == next.ID &&
		(previous.Workspace == (FileIdentity{}) || previous.Workspace == next.Workspace) &&
		(previous.Directory == (FileIdentity{}) || previous.Directory == next.Directory) &&
		(previous.GitDir == (FileIdentity{}) || previous.GitDir == next.GitDir) &&
		(previous.CommonDir == (FileIdentity{}) || previous.CommonDir == next.CommonDir) &&
		(previous.ObjectFormat == "" || previous.ObjectFormat == next.ObjectFormat) &&
		(previous.BranchRef == "" || previous.BranchRef == next.BranchRef) &&
		(previous.BaseOID == "" || previous.BaseOID == next.BaseOID) &&
		(previous.ResultRef == "" || previous.ResultRef == next.ResultRef) &&
		(previous.ResultCommitOID == "" || previous.ResultCommitOID == next.ResultCommitOID) &&
		(previous.LockReason == "" || previous.LockReason == next.LockReason) &&
		next.LeaseGeneration >= previous.LeaseGeneration
}

func mutationHash(value Mutation) (string, error) {
	payload := struct {
		ExpectedRevision team.Revision `json:"expected_revision"`
		Snapshot         Snapshot      `json:"snapshot"`
	}{ExpectedRevision: value.ExpectedRevision, Snapshot: value.Snapshot}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("coding team state: encode mutation hash: %w", err)
	}
	sum := sha256.Sum256(encoded)

	return hex.EncodeToString(sum[:]), nil
}

func isTornJSON(data []byte) bool {
	if len(bytes.TrimSpace(data)) == 0 {
		return false
	}
	var value any
	err := json.Unmarshal(data, &value)
	var syntax *json.SyntaxError
	return errors.As(err, &syntax) && strings.Contains(syntax.Error(), "unexpected end")
}

//nolint:nestif // Create-or-open handles the race without following replacement paths.
func openJournalForWrite(
	root *os.Root,
	name string,
	expected team.Revision,
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
				return nil, false, fmt.Errorf("coding team state: secure journal: %w", chmodErr)
			}
			return file, true, nil
		}
		if !errors.Is(createErr, fs.ErrExist) {
			return nil, false, fmt.Errorf("coding team state: create journal: %w", createErr)
		}
		info, err = root.Lstat(name)
	}
	if err != nil {
		return nil, false, fmt.Errorf("coding team state: inspect journal: %w", err)
	}
	if err := validateJournalInfo(info); err != nil {
		return nil, false, err
	}
	file, err := root.OpenFile(name, os.O_RDWR, journalMode)
	if err != nil {
		return nil, false, fmt.Errorf("coding team state: open journal: %w", err)
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, false, fmt.Errorf("coding team state: stat opened journal: %w", err)
	}
	if !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return nil, false, fmt.Errorf("%w: journal changed while opening", ErrUnsafeFile)
	}

	return file, false, nil
}

func readBounded(file *os.File, limit int64) ([]byte, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("coding team state: seek journal: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, fmt.Errorf("coding team state: read journal: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, ErrLimit
	}

	return data, nil
}

func validateDirectoryInfo(info fs.FileInfo) error {
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != directoryMode {
		return fmt.Errorf("%w: repository must be a private directory", ErrUnsafeFile)
	}

	return nil
}

func (s *Store) openRoot() (*os.Root, error) {
	current, err := os.Lstat(s.dir)
	if err != nil || !os.SameFile(current, s.dirInfo) {
		return nil, fmt.Errorf("%w: repository path identity changed", ErrUnsafeFile)
	}
	if err := validateDirectoryInfo(current); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return nil, fmt.Errorf("coding team state: open repository: %w", err)
	}
	opened, err := root.Lstat(".")
	if err != nil || !os.SameFile(opened, s.dirInfo) {
		_ = root.Close()
		return nil, fmt.Errorf("%w: repository changed while opening", ErrUnsafeFile)
	}

	return root, nil
}

func validateJournalInfo(info fs.FileInfo) error {
	if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Mode().Perm() != journalMode {
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

func validateLimits(value Limits) error {
	if value.MaxFileBytes < 1 || value.MaxRecordBytes < 1 ||
		int64(value.MaxRecordBytes) > value.MaxFileBytes || value.MaxRecords < 1 || value.MaxTeams < 1 ||
		value.MaxMembers < 1 || value.MaxAttempts < 1 || value.MaxIntegrations < 1 ||
		value.MaxListResults < 1 {
		return fmt.Errorf("%w: invalid limits", ErrInvalid)
	}

	return nil
}

func journalName(id team.ID) string { return string(id) + ".jsonl" }

func syncDirectory(directory string) (returnErr error) {
	file, err := os.Open(directory) //nolint:gosec // Validated application-owned Team directory.
	if err != nil {
		return fmt.Errorf("coding team state: open repository for sync: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	if err := file.Sync(); err != nil {
		return fmt.Errorf("coding team state: sync repository: %w", err)
	}

	return nil
}
