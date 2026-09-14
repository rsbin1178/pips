package planmode

import (
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
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/rsbin1178/pips/internal/coding/session"
)

const (
	planFileName  = "plan.md"
	stateFileName = "plan-mode.json"
	fileMode      = fs.FileMode(0o600)
	directoryMode = fs.FileMode(0o700)
)

var (
	// ErrNotFound means the session has no plan file yet.
	ErrNotFound = errors.New("coding plan file: not found")
	// ErrUnsafe means a filesystem object violates the private-file contract.
	ErrUnsafe = errors.New("coding plan file: unsafe filesystem object")
	// ErrUnsupported means the platform cannot provide the required guarantees.
	ErrUnsupported = errors.New("coding plan file: unsupported platform")
)

// Limits bound private plan storage.
type Limits struct {
	MaxBytes int64
}

// DefaultLimits returns production plan storage limits.
func DefaultLimits() Limits { return Limits{MaxBytes: 1 << 20} }

// Document is one immutable plan snapshot.
type Document struct {
	Content string `json:"content,omitempty"`
	Size    int64  `json:"size"`
}

type stateDocument struct {
	State State `json:"state"`
}

// Store owns one session's private plan directory: the plan file the model
// edits plus the durable plan-mode state.
type Store struct {
	sessionID string
	dir       string
	path      string
	limits    Limits
	mu        sync.Mutex
}

// NewStore opens (creating when needed) the private plan directory of one
// conversation session under the Harness sessions directory.
func NewStore(sessionsDir, sessionID string, limits Limits) (*Store, error) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return nil, ErrUnsupported
	}

	if strings.TrimSpace(sessionsDir) == "" || strings.ContainsRune(sessionsDir, '\x00') {
		return nil, fmt.Errorf("%w: invalid sessions directory", ErrInvalid)
	}

	if err := session.ValidateID(sessionID); err != nil {
		return nil, fmt.Errorf("%w: invalid session identity", ErrInvalid)
	}

	if limits == (Limits{}) {
		limits = DefaultLimits()
	}

	if limits.MaxBytes <= 0 {
		return nil, fmt.Errorf("%w: invalid limits", ErrInvalid)
	}

	directory := filepath.Join(sessionsDir, sessionID)
	if err := ensurePrivateDirectory(directory); err != nil {
		return nil, err
	}

	return &Store{
		sessionID: sessionID,
		dir:       directory,
		path:      filepath.Join(directory, planFileName),
		limits:    limits,
	}, nil
}

// SessionID returns the owning session identity.
func (s *Store) SessionID() string { return s.sessionID }

// Dir returns the private plan directory.
func (s *Store) Dir() string { return s.dir }

// Path returns the absolute plan file path disclosed to the model and the
// frontend. It never creates or reads the file.
func (s *Store) Path() string { return s.path }

// Read returns the current plan content.
func (s *Store) Read(ctx context.Context) (Document, error) {
	if s == nil {
		return Document{}, ErrInvalid
	}

	if err := ctx.Err(); err != nil {
		return Document{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.readLocked(ctx)
}

// Seed creates an empty plan file when none exists. An existing file (empty or
// not) is left untouched so entry can never truncate a preserved plan.
func (s *Store) Seed(ctx context.Context) (bool, error) {
	if s == nil {
		return false, ErrInvalid
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.readLocked(ctx); err == nil {
		return false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return false, err
	}

	if _, err := s.replaceLocked(ctx, ""); err != nil {
		return false, err
	}

	return true, nil
}

// Replace atomically replaces the complete plan content.
func (s *Store) Replace(ctx context.Context, content string) (Document, error) {
	if s == nil {
		return Document{}, ErrInvalid
	}

	if err := s.validateContent(content); err != nil {
		return Document{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.replaceLocked(ctx, content)
}

// LoadState reads the durable plan-mode state. Missing state is Inactive and
// transient states collapse to Inactive because they depend on in-flight
// interactions that did not survive the restart.
func (s *Store) LoadState(ctx context.Context) (State, error) {
	if s == nil {
		return "", ErrInvalid
	}

	if err := ctx.Err(); err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.readFileLocked(stateFileName, 4096)
	if errors.Is(err, ErrNotFound) {
		return StateInactive, nil
	}

	if err != nil {
		return "", err
	}

	var document stateDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return "", fmt.Errorf("%w: malformed plan mode state", ErrUnsafe)
	}

	if !document.State.Valid() {
		return "", fmt.Errorf("%w: unknown plan mode state %q", ErrUnsafe, string(document.State))
	}

	return document.State.Durable(), nil
}

// SaveState persists the durable projection of the given state.
func (s *Store) SaveState(ctx context.Context, state State) error {
	if s == nil {
		return ErrInvalid
	}

	if !state.Valid() {
		return invalidState(state)
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	data, err := json.Marshal(stateDocument{State: state.Durable()})
	if err != nil {
		return fmt.Errorf("%w: encode plan mode state", ErrInvalid)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.writeFileLocked(ctx, stateFileName, data)
}

// CopyTo copies the plan file and durable state into another session store.
// A source without a plan is a successful no-op.
func (s *Store) CopyTo(ctx context.Context, target *Store) error {
	if s == nil || target == nil {
		return ErrInvalid
	}

	if s.sessionID == target.sessionID {
		return fmt.Errorf("%w: source and target are identical", ErrInvalid)
	}

	document, err := s.Read(ctx)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}

	if err == nil {
		if _, err := target.Replace(ctx, document.Content); err != nil {
			return err
		}
	}

	state, stateErr := s.LoadState(ctx)
	if stateErr != nil || state == StateInactive {
		return stateErr
	}

	return target.SaveState(ctx, state)
}

//nolint:gocyclo // The read path keeps identity, type, size, and content checks fail-closed.
func (s *Store) readLocked(ctx context.Context) (_ Document, returnErr error) {
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return Document{}, fmt.Errorf("coding plan file: open plan directory: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, root.Close()) }()

	info, err := root.Lstat(planFileName)
	if errors.Is(err, fs.ErrNotExist) {
		return Document{}, ErrNotFound
	}

	if err != nil {
		return Document{}, fmt.Errorf("coding plan file: inspect plan: %w", err)
	}

	if err := validateFileInfo(info); err != nil {
		return Document{}, err
	}

	if info.Size() > s.limits.MaxBytes {
		return Document{}, fmt.Errorf("%w: plan exceeds size limit", ErrInvalid)
	}

	file, err := root.Open(planFileName)
	if err != nil {
		return Document{}, fmt.Errorf("coding plan file: open plan: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()

	openedInfo, err := file.Stat()
	if err != nil {
		return Document{}, fmt.Errorf("coding plan file: stat open plan: %w", err)
	}

	if !os.SameFile(info, openedInfo) {
		return Document{}, fmt.Errorf("%w: plan identity changed", ErrUnsafe)
	}

	data, err := io.ReadAll(io.LimitReader(file, s.limits.MaxBytes+1))
	if err != nil {
		return Document{}, fmt.Errorf("coding plan file: read plan: %w", err)
	}

	if int64(len(data)) > s.limits.MaxBytes {
		return Document{}, fmt.Errorf("%w: plan exceeds size limit", ErrInvalid)
	}

	if err := ctx.Err(); err != nil {
		return Document{}, err
	}

	content := string(data)
	if err := s.validateContent(content); err != nil {
		return Document{}, err
	}

	return Document{Content: content, Size: int64(len(content))}, nil
}

//nolint:gocyclo // Atomic create/replace keeps each failure and cleanup edge explicit.
func (s *Store) replaceLocked(ctx context.Context, content string) (_ Document, returnErr error) {
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return Document{}, fmt.Errorf("coding plan file: open plan directory: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, root.Close()) }()

	if existing, statErr := root.Lstat(planFileName); statErr == nil {
		if err := validateFileInfo(existing); err != nil {
			return Document{}, err
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return Document{}, fmt.Errorf("coding plan file: inspect plan: %w", statErr)
	}

	tempName, tempFile, err := createTemp(root, planFileName)
	if err != nil {
		return Document{}, err
	}

	keepTemp := true
	defer func() {
		if keepTemp {
			_ = root.Remove(tempName)
		}
	}()

	if err := writeAndSync(tempFile, []byte(content)); err != nil {
		return Document{}, err
	}

	if err := ctx.Err(); err != nil {
		return Document{}, err
	}

	if err := root.Rename(tempName, planFileName); err != nil {
		return Document{}, fmt.Errorf("coding plan file: replace plan: %w", err)
	}

	keepTemp = false

	if err := syncDirectory(s.dir); err != nil {
		return Document{}, err
	}

	return Document{Content: content, Size: int64(len(content))}, nil
}

func (s *Store) readFileLocked(name string, maximum int64) ([]byte, error) {
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return nil, fmt.Errorf("coding plan file: open plan directory: %w", err)
	}
	defer func() { _ = root.Close() }()

	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("coding plan file: inspect %s: %w", name, err)
	}

	if err := validateFileInfo(info); err != nil {
		return nil, err
	}

	if info.Size() > maximum {
		return nil, fmt.Errorf("%w: %s exceeds size limit", ErrInvalid, name)
	}

	file, err := root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("coding plan file: open %s: %w", name, err)
	}
	defer func() { _ = file.Close() }()

	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("coding plan file: stat open %s: %w", name, err)
	}

	if !os.SameFile(info, openedInfo) {
		return nil, fmt.Errorf("%w: %s identity changed", ErrUnsafe, name)
	}

	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, fmt.Errorf("coding plan file: read %s: %w", name, err)
	}

	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("%w: %s exceeds size limit", ErrInvalid, name)
	}

	return data, nil
}

//nolint:gocyclo // Atomic private-file publication keeps each cleanup edge explicit.
func (s *Store) writeFileLocked(ctx context.Context, name string, data []byte) (returnErr error) {
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return fmt.Errorf("coding plan file: open plan directory: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, root.Close()) }()

	tempName, tempFile, err := createTemp(root, name)
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

	if err := root.Rename(tempName, name); err != nil {
		return fmt.Errorf("coding plan file: replace %s: %w", name, err)
	}

	keepTemp = false

	return syncDirectory(s.dir)
}

func (s *Store) validateContent(content string) error {
	if !utf8.ValidString(content) || strings.ContainsRune(content, '\x00') {
		return fmt.Errorf("%w: plan content must be UTF-8 text", ErrInvalid)
	}

	if int64(len(content)) > s.limits.MaxBytes {
		return fmt.Errorf("%w: plan content exceeds size limit", ErrInvalid)
	}

	return nil
}

func ensurePrivateDirectory(directory string) error {
	if err := os.MkdirAll(directory, directoryMode); err != nil {
		return fmt.Errorf("coding plan file: create plan directory: %w", err)
	}

	info, err := os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("coding plan file: inspect plan directory: %w", err)
	}

	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: plan directory must be a private directory", ErrUnsafe)
	}

	if err := os.Chmod(directory, directoryMode); err != nil {
		return fmt.Errorf("coding plan file: secure plan directory: %w", err)
	}

	return nil
}

func validateFileInfo(info fs.FileInfo) error {
	if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != fileMode {
		return fmt.Errorf("%w: plan file must be a private regular file", ErrUnsafe)
	}

	return nil
}

func createTemp(root *os.Root, purpose string) (string, *os.File, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", nil, fmt.Errorf("coding plan file: random temporary name: %w", err)
	}

	name := "." + purpose + "." + hex.EncodeToString(random[:]) + ".tmp"

	file, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return "", nil, fmt.Errorf("coding plan file: create temporary: %w", err)
	}

	return name, file, nil
}

func writeAndSync(file *os.File, data []byte) error {
	if _, err := file.Write(data); err != nil {
		_ = file.Close()

		return fmt.Errorf("coding plan file: write temporary: %w", err)
	}

	if err := file.Sync(); err != nil {
		_ = file.Close()

		return fmt.Errorf("coding plan file: sync temporary: %w", err)
	}

	if err := file.Close(); err != nil {
		return fmt.Errorf("coding plan file: close temporary: %w", err)
	}

	return nil
}

func syncDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("coding plan file: open plan directory for sync: %w", err)
	}
	defer func() { _ = handle.Close() }()

	if err := handle.Sync(); err != nil {
		return fmt.Errorf("coding plan file: sync plan directory: %w", err)
	}

	return nil
}
