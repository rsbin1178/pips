package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
)

const (
	extraWorkspaceID = "pips.coding.workspace_id"
	extraProvider    = "pips.coding.provider"
	extraModelID     = "pips.coding.model_id"
)

var (
	// ErrInvalid means repository, session, or metadata input is invalid.
	ErrInvalid = errors.New("coding session: invalid input")
	// ErrLocked means another writer currently owns the requested session.
	ErrLocked = errors.New("coding session: session is locked")
	// ErrWorkspaceMismatch means a stored session belongs to another workspace
	// identity.
	ErrWorkspaceMismatch = errors.New("coding session: workspace mismatch")
	// ErrUnsupportedPlatform means this platform cannot enforce the P0
	// single-writer lock contract.
	ErrUnsupportedPlatform = errors.New("coding session: unsupported platform")
)

// Repository owns one directory of coding session files and their lock files.
type Repository struct {
	dir  string
	repo harness.Repo
}

// NewRepository returns a session repository rooted at dir.
func NewRepository(dir string) (*Repository, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("%w: empty repository path", ErrInvalid)
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: absolute repository path: %w", ErrInvalid, err)
	}

	if err := secureSessionDirectory(abs); err != nil {
		return nil, err
	}

	return &Repository{dir: abs, repo: harness.Repo{Dir: abs}}, nil
}

// Dir returns the absolute session repository directory.
func (r *Repository) Dir() string {
	if r == nil {
		return ""
	}

	return r.dir
}

// CreateOptions are the non-secret attributes persisted in a session header.
type CreateOptions struct {
	WorkspaceID string
	Provider    ai.Provider
	ModelID     string
}

// OpenOptions identify a stored session and the workspace allowed to own it.
type OpenOptions struct {
	ID          string
	WorkspaceID string
}

// Metadata is the typed coding projection of Harness session metadata.
type Metadata struct {
	ID          string
	CreatedAt   time.Time
	Path        string
	WorkspaceID string
	Provider    ai.Provider
	ModelID     string
}

// Handle owns a locked, writable Harness session.
type Handle struct {
	store   *harness.JSONLStore
	session *harness.Session
	meta    Metadata
	lock    sessionLock

	closeOnce sync.Once
	closeErr  error
}

// Session returns the durable conversation tree owned by the handle.
func (h *Handle) Session() *harness.Session {
	if h == nil {
		return nil
	}

	return h.session
}

// Metadata returns the session's typed, non-secret metadata.
func (h *Handle) Metadata() Metadata {
	if h == nil {
		return Metadata{}
	}

	return h.meta
}

// Close closes the writable store before releasing its writer lock. It is
// safe to call more than once and returns the first close result each time.
func (h *Handle) Close() error {
	if h == nil {
		return nil
	}

	h.closeOnce.Do(func() {
		var storeErr error
		if h.store != nil {
			storeErr = h.store.Close()
		}

		var lockErr error
		if h.lock != nil {
			lockErr = h.lock.Close()
		}

		h.closeErr = errors.Join(storeErr, lockErr)
	})

	return h.closeErr
}

// Create creates and locks a new session.
func (r *Repository) Create(ctx context.Context, options CreateOptions) (*Handle, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}

	if err := validateCreateOptions(options); err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	id, err := newSessionID()
	if err != nil {
		return nil, err
	}

	lock, err := acquireSessionLock(ctx, r.lockPath(id))
	if err != nil {
		return nil, err
	}

	extra := map[string]string{
		extraWorkspaceID: options.WorkspaceID,
		extraProvider:    string(options.Provider),
		extraModelID:     options.ModelID,
	}

	store, err := r.repo.Create(id, extra)
	if err != nil {
		return nil, errors.Join(err, lock.Close())
	}

	return newHandle(store, lock, options.WorkspaceID)
}

// Open locks and opens a stored session after verifying its workspace owner.
// The stored provider/model describe the initial model and do not prevent a
// later runtime from recording an explicit model change.
func (r *Repository) Open(ctx context.Context, options OpenOptions) (*Handle, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}

	if err := validateSessionID(options.ID); err != nil {
		return nil, err
	}

	if strings.TrimSpace(options.WorkspaceID) == "" {
		return nil, fmt.Errorf("%w: empty workspace identity", ErrInvalid)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	lock, err := acquireSessionLock(ctx, r.lockPath(options.ID))
	if err != nil {
		return nil, err
	}

	store, err := r.repo.Open(options.ID)
	if err != nil {
		return nil, errors.Join(err, lock.Close())
	}

	return newHandle(store, lock, options.WorkspaceID)
}

// List returns typed session metadata in newest-first order without taking
// writer locks.
func (r *Repository) List(ctx context.Context) ([]Metadata, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	stored, err := r.repo.List()
	if err != nil {
		return nil, err
	}

	metas := make([]Metadata, 0, len(stored))
	for _, value := range stored {
		meta, err := projectMetadata(value)
		if err != nil {
			return nil, err
		}

		metas = append(metas, meta)
	}

	slices.SortFunc(metas, func(a, b Metadata) int {
		if order := b.CreatedAt.Compare(a.CreatedAt); order != 0 {
			return order
		}

		return strings.Compare(a.ID, b.ID)
	})

	return metas, nil
}

func newHandle(
	store *harness.JSONLStore,
	lock sessionLock,
	workspaceID string,
) (*Handle, error) {
	meta, err := projectMetadata(store.Metadata())
	if err != nil {
		return nil, errors.Join(err, store.Close(), lock.Close())
	}

	if meta.WorkspaceID != workspaceID {
		return nil, errors.Join(
			fmt.Errorf("%w: session %q", ErrWorkspaceMismatch, meta.ID),
			store.Close(),
			lock.Close(),
		)
	}

	sess, err := harness.NewSession(store)
	if err != nil {
		return nil, errors.Join(err, store.Close(), lock.Close())
	}

	return &Handle{store: store, session: sess, meta: meta, lock: lock}, nil
}

func projectMetadata(stored harness.SessionMetadata) (Metadata, error) {
	workspaceID := stored.Extra[extraWorkspaceID]
	provider := ai.Provider(stored.Extra[extraProvider])

	modelID := stored.Extra[extraModelID]
	if stored.ID == "" || workspaceID == "" || provider == "" || modelID == "" {
		return Metadata{}, fmt.Errorf("%w: session %q has incomplete coding metadata", ErrInvalid, stored.ID)
	}

	return Metadata{
		ID:          stored.ID,
		CreatedAt:   stored.CreatedAt,
		Path:        stored.Path,
		WorkspaceID: workspaceID,
		Provider:    provider,
		ModelID:     modelID,
	}, nil
}

func validateCreateOptions(options CreateOptions) error {
	if strings.TrimSpace(options.WorkspaceID) == "" {
		return fmt.Errorf("%w: empty workspace identity", ErrInvalid)
	}

	if options.Provider == "" {
		return fmt.Errorf("%w: empty provider", ErrInvalid)
	}

	if strings.TrimSpace(options.ModelID) == "" {
		return fmt.Errorf("%w: empty model id", ErrInvalid)
	}

	return nil
}

func (r *Repository) validate() error {
	if r == nil || r.dir == "" {
		return fmt.Errorf("%w: nil repository", ErrInvalid)
	}

	return nil
}

func (r *Repository) lockPath(id string) string {
	return filepath.Join(r.dir, id+".lock")
}

func newSessionID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("coding session: generate id: %w", err)
	}

	return "s-" + hex.EncodeToString(random[:]), nil
}

func validateSessionID(id string) error {
	if len(id) == 0 || len(id) > 128 || id == "." || id == ".." || id[0] == '.' {
		return fmt.Errorf("%w: invalid session id %q", ErrInvalid, id)
	}

	for _, char := range id {
		if unicode.IsLetter(char) || unicode.IsDigit(char) || char == '-' || char == '_' || char == '.' {
			continue
		}

		return fmt.Errorf("%w: invalid session id %q", ErrInvalid, id)
	}

	return nil
}

// ValidateID verifies that id is safe to use as a durable session name.
func ValidateID(id string) error { return validateSessionID(id) }

func secureSessionDirectory(path string) error {
	if err := os.MkdirAll(path, 0o750); err != nil {
		return fmt.Errorf("coding session: create repository: %w", err)
	}

	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("coding session: inspect repository: %w", err)
	}

	if !info.IsDir() {
		return fmt.Errorf("%w: repository is not a directory", ErrInvalid)
	}

	if info.Mode().Perm()&0o027 != 0 {
		return fmt.Errorf("%w: repository mode %04o permits group write or other access", ErrInvalid, info.Mode().Perm())
	}

	return nil
}
