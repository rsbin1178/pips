package scheduler

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rsbin1178/pips/internal/coding/session"
)

// The store's private-file contract, matching the plan store's discipline.
const (
	storeDirName  = "scheduler"
	storeFileName = "tasks.json"
	storeSchema   = "pips.coding.scheduler/v1"
	storeVersion  = 1
	directoryMode = fs.FileMode(0o700)
	fileMode      = fs.FileMode(0o600)
	// DefaultMaxBytes bounds the task file. The list is small and is always read
	// whole, so a damaged file cannot force an unbounded allocation.
	DefaultMaxBytes int64 = 1 << 20
)

var (
	// ErrInvalid identifies a refused scheduler request.
	ErrInvalid = errors.New("coding scheduler: invalid request")
	// ErrUnsafe identifies a filesystem object or record that violates the
	// private-store contract.
	ErrUnsafe = errors.New("coding scheduler: unsafe scheduler store")
	// ErrUnsupported means the platform cannot provide the required guarantees.
	ErrUnsupported = errors.New("coding scheduler: unsupported platform")
	// ErrTaskNotFound means no scheduled task carries the requested id.
	ErrTaskNotFound = errors.New("coding scheduler: no scheduled task with that id")
)

// Options configure one session's scheduler store and its firing loop.
type Options struct {
	// Now returns the current time; nil uses time.Now.
	Now func() time.Time
	// After returns a channel that receives one value once the duration
	// elapses; nil uses time.After. It is the loop's timer seam, so a test can
	// advance time and observe a fire instead of waiting an interval.
	After func(time.Duration) <-chan time.Time
	// MaxBytes bounds the task file; zero uses DefaultMaxBytes.
	MaxBytes int64
}

// storeDocument is the versioned on-disk shape. The whole list is one document:
// it is small, and one atomic replace keeps every mutation durable.
type storeDocument struct {
	Schema  string `json:"schema"`
	Version int    `json:"version"`
	Tasks   []Task `json:"tasks"`
}

// CreateRequest is one task to add.
type CreateRequest struct {
	Prompt          string
	IntervalSeconds int64
	Recurring       bool
	Durable         bool
	FireImmediately bool
}

// UpdateRequest patches one task in place. A nil field is left unchanged.
type UpdateRequest struct {
	Prompt          *string
	IntervalSeconds *int64
}

// Store owns one session's durable scheduled-task list. It is the single owner:
// the Tools, the firing loop, and the surfaces all read and write it, and it
// decides what a restart keeps — a durable task is reloaded, a session-only one
// is dropped.
type Store struct {
	sessionID string
	dir       string
	path      string
	maxBytes  int64
	now       func() time.Time

	mu    sync.Mutex
	tasks []Task
}

// NewStoreAt opens (creating when needed) the private scheduler directory under
// the session's own state directory. It does not read the task file; call Load
// to reload the durable tasks.
func NewStoreAt(directory, sessionID string, options Options) (*Store, error) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return nil, ErrUnsupported
	}

	if strings.TrimSpace(directory) == "" || strings.ContainsRune(directory, '\x00') {
		return nil, fmt.Errorf("%w: invalid state directory", ErrInvalid)
	}

	if err := session.ValidateID(sessionID); err != nil {
		return nil, fmt.Errorf("%w: invalid session identity", ErrInvalid)
	}

	maxBytes := options.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}

	now := options.Now
	if now == nil {
		now = time.Now
	}

	dir := filepath.Join(directory, storeDirName)

	return &Store{
		sessionID: sessionID,
		dir:       dir,
		path:      filepath.Join(dir, storeFileName),
		maxBytes:  maxBytes,
		now:       now,
	}, nil
}

// SessionID returns the owning session identity.
func (s *Store) SessionID() string { return s.sessionID }

// Dir returns the private scheduler directory.
func (s *Store) Dir() string { return s.dir }

// Now returns the store's current time, so the firing loop and the Tools share
// one clock.
func (s *Store) Now() time.Time { return s.now() }

// Load reads the durable tasks and drops every session-only task, so a restart
// keeps only what was marked durable. A missing file is an empty list.
func (s *Store) Load(ctx context.Context) error {
	if s == nil {
		return ErrInvalid
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	document, err := s.readLocked()
	if err != nil {
		return err
	}

	kept := make([]Task, 0, len(document.Tasks))
	for _, task := range document.Tasks {
		if task.Durable {
			kept = append(kept, task)
		}
	}

	dropped := len(kept) != len(document.Tasks)
	s.tasks = kept

	if dropped {
		return s.writeLocked(ctx)
	}

	return nil
}

// List returns the tasks in creation order.
func (s *Store) List(ctx context.Context) ([]Task, error) {
	if s == nil {
		return nil, ErrInvalid
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.tasks), nil
}

// Create adds one task and returns it. A fire_immediately task is due at once;
// every other task waits one interval.
func (s *Store) Create(ctx context.Context, request CreateRequest) (Task, error) {
	if s == nil {
		return Task{}, ErrInvalid
	}

	if strings.TrimSpace(request.Prompt) == "" {
		return Task{}, fmt.Errorf("%w: prompt is required", ErrInvalid)
	}

	if request.IntervalSeconds < MinimumIntervalSeconds {
		return Task{}, fmt.Errorf(
			"%w: interval must be at least %d seconds", ErrInvalid, MinimumIntervalSeconds,
		)
	}

	if err := ctx.Err(); err != nil {
		return Task{}, err
	}

	id, err := newTaskID()
	if err != nil {
		return Task{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()

	task := Task{
		ID:           id,
		Prompt:       request.Prompt,
		IntervalSecs: request.IntervalSeconds,
		Recurring:    request.Recurring,
		Durable:      request.Durable,
		CreatedAt:    now,
		NextFireAt:   now.Add(time.Duration(request.IntervalSeconds) * time.Second),
	}
	if request.FireImmediately {
		task.NextFireAt = now
	}

	s.tasks = append(s.tasks, task)

	if err := s.writeLocked(ctx); err != nil {
		s.tasks = s.tasks[:len(s.tasks)-1]

		return Task{}, err
	}

	return task, nil
}

// Update patches one task in place: a given field replaces the stored one and an
// omitted field stays. An interval change re-anchors the schedule from the
// task's last fire so the phase survives, and an unknown id is refused.
func (s *Store) Update(ctx context.Context, id string, patch UpdateRequest) (Task, error) {
	if s == nil {
		return Task{}, ErrInvalid
	}

	if patch.Prompt == nil && patch.IntervalSeconds == nil {
		return Task{}, fmt.Errorf("%w: nothing to update", ErrInvalid)
	}

	if patch.Prompt != nil && strings.TrimSpace(*patch.Prompt) == "" {
		return Task{}, fmt.Errorf("%w: prompt cannot be empty", ErrInvalid)
	}

	if patch.IntervalSeconds != nil && *patch.IntervalSeconds < MinimumIntervalSeconds {
		return Task{}, fmt.Errorf(
			"%w: interval must be at least %d seconds", ErrInvalid, MinimumIntervalSeconds,
		)
	}

	if err := ctx.Err(); err != nil {
		return Task{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	index := s.indexOfLocked(id)
	if index < 0 {
		return Task{}, fmt.Errorf("%w: %s", ErrTaskNotFound, id)
	}

	previous := s.tasks[index]
	task := previous

	if patch.Prompt != nil {
		task.Prompt = *patch.Prompt
	}

	if patch.IntervalSeconds != nil {
		task.IntervalSecs = *patch.IntervalSeconds
		task.NextFireAt = s.reanchorLocked(task)
	}

	s.tasks[index] = task

	if err := s.writeLocked(ctx); err != nil {
		s.tasks[index] = previous

		return Task{}, err
	}

	return task, nil
}

// Delete removes one task. An unknown id reports false rather than an error.
func (s *Store) Delete(ctx context.Context, id string) (bool, error) {
	if s == nil {
		return false, ErrInvalid
	}

	if err := ctx.Err(); err != nil {
		return false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	index := s.indexOfLocked(id)
	if index < 0 {
		return false, nil
	}

	previous := slices.Clone(s.tasks)
	s.tasks = append(s.tasks[:index], s.tasks[index+1:]...)

	if err := s.writeLocked(ctx); err != nil {
		s.tasks = previous

		return false, err
	}

	return true, nil
}

// RecordFire records the child one fire started and advances the schedule in the
// same durable write. A task that fires is removed when it is not recurring, and
// the removal is durable before the fire is reported, so a restart cannot fire
// it twice.
func (s *Store) RecordFire(
	ctx context.Context,
	id string,
	childID string,
	firedAt time.Time,
	nextFireAt time.Time,
) (Task, error) {
	if s == nil {
		return Task{}, ErrInvalid
	}

	if err := ctx.Err(); err != nil {
		return Task{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	index := s.indexOfLocked(id)
	if index < 0 {
		return Task{}, fmt.Errorf("%w: %s", ErrTaskNotFound, id)
	}

	previous := slices.Clone(s.tasks)
	task := s.tasks[index]

	if task.Recurring {
		task.LastFiredAt = firedAt
		task.NextFireAt = nextFireAt
		task.LastChildID = childID
		s.tasks[index] = task
	} else {
		s.tasks = append(s.tasks[:index], s.tasks[index+1:]...)
	}

	if err := s.writeLocked(ctx); err != nil {
		s.tasks = previous

		return Task{}, err
	}

	return task, nil
}

// Advance moves one task's next fire without recording a child, for a fire that
// could not start one. The task stays scheduled and nothing is reported.
func (s *Store) Advance(ctx context.Context, id string, nextFireAt time.Time) error {
	if s == nil {
		return ErrInvalid
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	index := s.indexOfLocked(id)
	if index < 0 {
		return fmt.Errorf("%w: %s", ErrTaskNotFound, id)
	}

	previous := s.tasks[index]
	task := previous
	task.NextFireAt = nextFireAt
	s.tasks[index] = task

	if err := s.writeLocked(ctx); err != nil {
		s.tasks[index] = previous

		return err
	}

	return nil
}

// indexOfLocked returns the position of one task, or -1.
func (s *Store) indexOfLocked(id string) int {
	return slices.IndexFunc(s.tasks, func(task Task) bool { return task.ID == id })
}

// reanchorLocked recomputes a task's next fire after an interval change,
// anchored on its last fire (or its creation) so the schedule keeps its phase.
// A candidate already in the past restarts one interval from now, matching the
// reference's update rule.
func (s *Store) reanchorLocked(task Task) time.Time {
	anchor := task.LastFiredAt
	if anchor.IsZero() {
		anchor = task.CreatedAt
	}

	next := anchor.Add(task.Interval())
	if !next.After(s.now()) {
		next = s.now().Add(task.Interval())
	}

	return next
}

// readLocked reads and validates the task file, keeping every check fail-closed.
// The scheduler directory is created on the first write, so a session that never
// schedules anything has none and reads as empty.
func (s *Store) readLocked() (storeDocument, error) {
	root, err := os.OpenRoot(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return emptyDocument(), nil
	}

	if err != nil {
		return storeDocument{}, fmt.Errorf("coding scheduler: open scheduler directory: %w", err)
	}

	defer func() { _ = root.Close() }()

	info, err := root.Lstat(storeFileName)
	if errors.Is(err, fs.ErrNotExist) {
		return emptyDocument(), nil
	}

	if err != nil {
		return storeDocument{}, fmt.Errorf("coding scheduler: inspect task file: %w", err)
	}

	if err := validateFileInfo(info); err != nil {
		return storeDocument{}, err
	}

	if info.Size() > s.maxBytes {
		return storeDocument{}, fmt.Errorf("%w: task file exceeds size limit", ErrInvalid)
	}

	file, err := root.Open(storeFileName)
	if err != nil {
		return storeDocument{}, fmt.Errorf("coding scheduler: open task file: %w", err)
	}
	defer func() { _ = file.Close() }()

	openedInfo, err := file.Stat()
	if err != nil {
		return storeDocument{}, fmt.Errorf("coding scheduler: stat open task file: %w", err)
	}

	if !os.SameFile(info, openedInfo) {
		return storeDocument{}, fmt.Errorf("%w: task file identity changed", ErrUnsafe)
	}

	data, err := io.ReadAll(io.LimitReader(file, s.maxBytes+1))
	if err != nil {
		return storeDocument{}, fmt.Errorf("coding scheduler: read task file: %w", err)
	}

	if int64(len(data)) > s.maxBytes {
		return storeDocument{}, fmt.Errorf("%w: task file exceeds size limit", ErrInvalid)
	}

	return decodeStore(data)
}

// decodeStore decodes and validates one task document. A record this build does
// not understand is refused rather than half-read.
func decodeStore(data []byte) (storeDocument, error) {
	var document storeDocument

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&document); err != nil {
		return storeDocument{}, fmt.Errorf("%w: malformed task file", ErrUnsafe)
	}

	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return storeDocument{}, fmt.Errorf("%w: trailing data after task file", ErrUnsafe)
	}

	if document.Schema != storeSchema || document.Version != storeVersion {
		return storeDocument{}, fmt.Errorf(
			"%w: unsupported task file schema %q version %d",
			ErrUnsafe, document.Schema, document.Version,
		)
	}

	for _, task := range document.Tasks {
		if err := validateTask(task); err != nil {
			return storeDocument{}, err
		}
	}

	return document, nil
}

// validateTask refuses a record that could not have been written by this build.
func validateTask(task Task) error {
	if strings.TrimSpace(task.ID) == "" {
		return fmt.Errorf("%w: task id is required", ErrUnsafe)
	}

	if strings.TrimSpace(task.Prompt) == "" {
		return fmt.Errorf("%w: task %s has no prompt", ErrUnsafe, task.ID)
	}

	if task.IntervalSecs < MinimumIntervalSeconds {
		return fmt.Errorf("%w: task %s has an interval below the floor", ErrUnsafe, task.ID)
	}

	if task.NextFireAt.IsZero() {
		return fmt.Errorf("%w: task %s has no next fire", ErrUnsafe, task.ID)
	}

	return nil
}

// emptyDocument is the document a store with no file yet holds.
func emptyDocument() storeDocument {
	return storeDocument{Schema: storeSchema, Version: storeVersion}
}

// writeLocked replaces the task file atomically: a temporary file in the same
// directory is written, synced, renamed over the target, and the directory is
// synced so the replacement survives a crash.
func (s *Store) writeLocked(ctx context.Context) error {
	data, err := json.Marshal(storeDocument{
		Schema:  storeSchema,
		Version: storeVersion,
		Tasks:   s.tasks,
	})
	if err != nil {
		return fmt.Errorf("%w: encode task file", ErrInvalid)
	}

	if int64(len(data)) > s.maxBytes {
		return fmt.Errorf("%w: task file exceeds size limit", ErrInvalid)
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	if err := ensurePrivateDirectory(s.dir); err != nil {
		return err
	}

	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return fmt.Errorf("coding scheduler: open scheduler directory: %w", err)
	}
	defer func() { _ = root.Close() }()

	tempName, tempFile, err := createTemp(root, storeFileName)
	if err != nil {
		return err
	}

	keepTemp := true
	defer func() {
		if keepTemp {
			_ = root.Remove(tempName)
		}
	}()

	if err := writeAndSync(tempFile, data); err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	if err := root.Rename(tempName, storeFileName); err != nil {
		return fmt.Errorf("coding scheduler: replace task file: %w", err)
	}

	keepTemp = false

	return syncDirectory(s.dir)
}

// newTaskID mints one task identity. The id is a bare random value: it is
// addressed only by the scheduler Tools, so it needs no namespace.
func newTaskID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("coding scheduler: generate task id: %w", err)
	}

	return hex.EncodeToString(random[:]), nil
}

func ensurePrivateDirectory(directory string) error {
	if err := os.MkdirAll(directory, directoryMode); err != nil {
		return fmt.Errorf("coding scheduler: create scheduler directory: %w", err)
	}

	info, err := os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("coding scheduler: inspect scheduler directory: %w", err)
	}

	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: scheduler directory must be a private directory", ErrUnsafe)
	}

	if err := os.Chmod(directory, directoryMode); err != nil {
		return fmt.Errorf("coding scheduler: secure scheduler directory: %w", err)
	}

	return nil
}

func validateFileInfo(info fs.FileInfo) error {
	if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != fileMode {
		return fmt.Errorf("%w: task file must be a private regular file", ErrUnsafe)
	}

	return nil
}

func createTemp(root *os.Root, purpose string) (string, *os.File, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", nil, fmt.Errorf("coding scheduler: random temporary name: %w", err)
	}

	name := "." + purpose + "." + hex.EncodeToString(random[:]) + ".tmp"

	file, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return "", nil, fmt.Errorf("coding scheduler: create temporary: %w", err)
	}

	return name, file, nil
}

func writeAndSync(file *os.File, data []byte) error {
	if _, err := file.Write(data); err != nil {
		_ = file.Close()

		return fmt.Errorf("coding scheduler: write temporary: %w", err)
	}

	if err := file.Sync(); err != nil {
		_ = file.Close()

		return fmt.Errorf("coding scheduler: sync temporary: %w", err)
	}

	if err := file.Close(); err != nil {
		return fmt.Errorf("coding scheduler: close temporary: %w", err)
	}

	return nil
}

func syncDirectory(directory string) error {
	//nolint:gosec // The directory is the store's own private session directory, not caller input.
	handle, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("coding scheduler: open scheduler directory for sync: %w", err)
	}
	defer func() { _ = handle.Close() }()

	if err := handle.Sync(); err != nil {
		return fmt.Errorf("coding scheduler: sync scheduler directory: %w", err)
	}

	return nil
}
