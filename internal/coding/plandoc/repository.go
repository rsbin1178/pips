// Package plandoc owns private, session-bound planning documents.
package plandoc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	metadataPrefix = "<!-- pips-plan:v1 session="
	metadataMiddle = " workspace="
	metadataSuffix = " -->\n"
	fileMode       = fs.FileMode(0o600)
	directoryMode  = fs.FileMode(0o700)
)

var (
	// ErrInvalid means a reference, revision, or document is malformed.
	ErrInvalid = errors.New("coding plan document: invalid value")
	// ErrNotFound means the current Session has no Plan document yet.
	ErrNotFound = errors.New("coding plan document: not found")
	// ErrConflict means optimistic replacement observed another revision.
	ErrConflict = errors.New("coding plan document: revision conflict")
	// ErrUnsafe means a filesystem object does not satisfy the private-file contract.
	ErrUnsafe = errors.New("coding plan document: unsafe filesystem object")
	// ErrUnsupported means the platform cannot provide the required file guarantees.
	ErrUnsupported = errors.New("coding plan document: unsupported platform")
)

// Ref binds one Plan document to a conversation Session and Workspace.
type Ref struct {
	SessionID   string `json:"session_id"`
	WorkspaceID string `json:"workspace_id"`
}

// Document is one immutable repository snapshot.
type Document struct {
	Ref      Ref    `json:"ref"`
	Revision string `json:"revision,omitempty"`
	Content  string `json:"content,omitempty"`
	Size     int64  `json:"size"`
}

// Limits bound private Plan document storage.
type Limits struct {
	MaxBytes int64
}

// DefaultLimits returns production Plan document limits.
func DefaultLimits() Limits { return Limits{MaxBytes: 1 << 20} }

// Repository is the application-owned Plan document boundary.
type Repository interface {
	Read(context.Context, Ref) (Document, error)
	Replace(context.Context, Ref, string, string) (Document, error)
	Fork(context.Context, Ref, Ref) error
}

// Locator exposes an application-derived display path without granting path
// selection or generic filesystem access.
type Locator interface {
	Path(Ref) (string, error)
}

// FileRepository stores private Plan documents under one user Pips root.
type FileRepository struct {
	dir    string
	limits Limits
	mu     sync.Mutex
}

// New opens a private Plan document repository rooted at directory.
func New(directory string, limits Limits) (*FileRepository, error) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return nil, ErrUnsupported
	}

	if strings.TrimSpace(directory) == "" || strings.ContainsRune(directory, '\x00') {
		return nil, fmt.Errorf("%w: invalid repository path", ErrInvalid)
	}

	if limits == (Limits{}) {
		limits = DefaultLimits()
	}

	if limits.MaxBytes < 1 {
		return nil, fmt.Errorf("%w: invalid maximum size", ErrInvalid)
	}

	abs, err := filepath.Abs(directory)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve repository: %w", ErrInvalid, err)
	}

	if err := os.MkdirAll(abs, directoryMode); err != nil {
		return nil, fmt.Errorf("coding plan document: create repository: %w", err)
	}

	if err := validateDirectory(abs); err != nil {
		return nil, err
	}

	return &FileRepository{dir: abs, limits: limits}, nil
}

// Dir returns the absolute private repository path.
func (r *FileRepository) Dir() string {
	if r == nil {
		return ""
	}

	return r.dir
}

// Path returns the application-derived absolute path for ref.
func (r *FileRepository) Path(ref Ref) (string, error) {
	if r == nil {
		return "", ErrInvalid
	}

	if err := validateRef(ref); err != nil {
		return "", err
	}

	return filepath.Join(r.dir, ref.SessionID+".md"), nil
}

// Read loads one bounded Plan document without following a final symlink.
func (r *FileRepository) Read(ctx context.Context, ref Ref) (Document, error) {
	if r == nil {
		return Document{}, ErrInvalid
	}

	if err := ctx.Err(); err != nil {
		return Document{}, err
	}

	if err := validateRef(ref); err != nil {
		return Document{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return r.readLocked(ctx, ref)
}

// Replace creates or atomically replaces one complete Markdown document.
// Empty expectedRevision is create-only.
func (r *FileRepository) Replace(
	ctx context.Context,
	ref Ref,
	expectedRevision string,
	content string,
) (Document, error) {
	if r == nil {
		return Document{}, ErrInvalid
	}

	if err := ctx.Err(); err != nil {
		return Document{}, err
	}

	if err := validateRef(ref); err != nil {
		return Document{}, err
	}

	if err := r.validateContent(content); err != nil {
		return Document{}, err
	}

	if expectedRevision != "" && !validRevision(expectedRevision) {
		return Document{}, fmt.Errorf("%w: malformed expected revision", ErrInvalid)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return r.replaceLocked(ctx, ref, expectedRevision, content)
}

// Fork copies an existing source Plan to a new session-bound target. A source
// without a Plan is a successful no-op.
func (r *FileRepository) Fork(ctx context.Context, source, target Ref) error {
	if r == nil {
		return ErrInvalid
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	if err := validateRef(source); err != nil {
		return err
	}

	if err := validateRef(target); err != nil {
		return err
	}

	if source == target {
		return fmt.Errorf("%w: source and target are identical", ErrInvalid)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	document, err := r.readLocked(ctx, source)
	if errors.Is(err, ErrNotFound) {
		return nil
	}

	if err != nil {
		return err
	}

	_, err = r.replaceLocked(ctx, target, "", document.Content)

	return err
}

//nolint:gocyclo // The read path keeps identity, type, size, and content checks fail-closed.
func (r *FileRepository) readLocked(
	ctx context.Context,
	ref Ref,
) (_ Document, returnErr error) {
	root, err := os.OpenRoot(r.dir)
	if err != nil {
		return Document{}, fmt.Errorf("coding plan document: open repository: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, root.Close()) }()

	name := fileName(ref)

	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return Document{Ref: ref}, ErrNotFound
	}

	if err != nil {
		return Document{}, fmt.Errorf("coding plan document: inspect document: %w", err)
	}

	if err := validateFileInfo(info); err != nil {
		return Document{}, err
	}

	if info.Size() > r.storedLimit(ref) {
		return Document{}, fmt.Errorf("%w: document exceeds size limit", ErrInvalid)
	}

	file, err := root.Open(name)
	if err != nil {
		return Document{}, fmt.Errorf("coding plan document: open document: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()

	openedInfo, err := file.Stat()
	if err != nil {
		return Document{}, fmt.Errorf("coding plan document: stat open document: %w", err)
	}

	if !os.SameFile(info, openedInfo) {
		return Document{}, fmt.Errorf("%w: document identity changed", ErrUnsafe)
	}

	data, err := io.ReadAll(io.LimitReader(file, r.storedLimit(ref)+1))
	if err != nil {
		return Document{}, fmt.Errorf("coding plan document: read document: %w", err)
	}

	if int64(len(data)) > r.storedLimit(ref) {
		return Document{}, fmt.Errorf("%w: document exceeds size limit", ErrInvalid)
	}

	if err := ctx.Err(); err != nil {
		return Document{}, err
	}

	content, err := decodeStored(ref, data)
	if err != nil {
		return Document{}, err
	}

	if err := r.validateContent(content); err != nil {
		return Document{}, err
	}

	return document(ref, content), nil
}

//nolint:gocyclo,nestif // Atomic create/replace keeps each failure and cleanup edge explicit.
func (r *FileRepository) replaceLocked(
	ctx context.Context,
	ref Ref,
	expectedRevision string,
	content string,
) (_ Document, returnErr error) {
	current, readErr := r.readLocked(ctx, ref)
	switch {
	case errors.Is(readErr, ErrNotFound) && expectedRevision != "":
		return Document{}, ErrConflict
	case readErr != nil && !errors.Is(readErr, ErrNotFound):
		return Document{}, readErr
	case readErr == nil && expectedRevision == "":
		return Document{}, ErrConflict
	case readErr == nil && current.Revision != expectedRevision:
		return Document{}, ErrConflict
	}

	root, err := os.OpenRoot(r.dir)
	if err != nil {
		return Document{}, fmt.Errorf("coding plan document: open repository: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, root.Close()) }()

	tempName, tempFile, err := createTemp(root, ref.SessionID)
	if err != nil {
		return Document{}, err
	}

	keepTemp := true
	defer func() {
		if keepTemp {
			_ = root.Remove(tempName)
		}
	}()

	stored := encodeStored(ref, content)
	if err := writeAndSync(tempFile, stored); err != nil {
		return Document{}, err
	}

	if err := ctx.Err(); err != nil {
		return Document{}, err
	}

	name := fileName(ref)
	if errors.Is(readErr, ErrNotFound) {
		if err := root.Link(tempName, name); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return Document{}, ErrConflict
			}

			return Document{}, fmt.Errorf("coding plan document: publish new document: %w", err)
		}

		if err := root.Remove(tempName); err != nil {
			return Document{}, fmt.Errorf("coding plan document: remove linked temporary: %w", err)
		}

		keepTemp = false
	} else {
		latest, err := root.Lstat(name)
		if err != nil {
			return Document{}, ErrConflict
		}

		if err := validateFileInfo(latest); err != nil || latest.Size() > r.storedLimit(ref) {
			return Document{}, ErrConflict
		}

		latestDocument, err := r.readLocked(ctx, ref)
		if err != nil || latestDocument.Revision != expectedRevision {
			return Document{}, ErrConflict
		}

		if err := root.Rename(tempName, name); err != nil {
			return Document{}, fmt.Errorf("coding plan document: replace document: %w", err)
		}

		keepTemp = false
	}

	if err := syncDirectory(r.dir); err != nil {
		return Document{}, err
	}

	return document(ref, content), nil
}

func validateDirectory(directory string) error {
	info, err := os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("coding plan document: inspect repository: %w", err)
	}

	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != directoryMode {
		return fmt.Errorf("%w: repository must be a private directory", ErrUnsafe)
	}

	return nil
}

func validateRef(ref Ref) error {
	if err := session.ValidateID(ref.SessionID); err != nil {
		return fmt.Errorf("%w: invalid session identity", ErrInvalid)
	}

	if strings.TrimSpace(ref.WorkspaceID) == "" || len(ref.WorkspaceID) > 512 ||
		strings.ContainsRune(ref.WorkspaceID, '\x00') || !utf8.ValidString(ref.WorkspaceID) {
		return fmt.Errorf("%w: invalid workspace identity", ErrInvalid)
	}

	return nil
}

func (r *FileRepository) validateContent(content string) error {
	if !utf8.ValidString(content) || strings.ContainsRune(content, '\x00') {
		return fmt.Errorf("%w: content must be UTF-8 text", ErrInvalid)
	}

	if int64(len(content)) > r.limits.MaxBytes {
		return fmt.Errorf("%w: content exceeds size limit", ErrInvalid)
	}

	return nil
}

func validateFileInfo(info fs.FileInfo) error {
	if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != fileMode {
		return fmt.Errorf("%w: document must be a private regular file", ErrUnsafe)
	}

	return nil
}

func fileName(ref Ref) string { return ref.SessionID + ".md" }

func document(ref Ref, content string) Document {
	sum := sha256.Sum256([]byte(content))

	return Document{
		Ref: ref, Revision: hex.EncodeToString(sum[:]), Content: content, Size: int64(len(content)),
	}
}

func validRevision(revision string) bool {
	if len(revision) != sha256.Size*2 {
		return false
	}

	_, err := hex.DecodeString(revision)

	return err == nil
}

func encodeStored(ref Ref, content string) []byte {
	workspace := base64.RawURLEncoding.EncodeToString([]byte(ref.WorkspaceID))
	header := metadataPrefix + ref.SessionID + metadataMiddle + workspace + metadataSuffix

	return []byte(header + content)
}

func decodeStored(ref Ref, data []byte) (string, error) {
	headerEnd := strings.IndexByte(string(data), '\n')
	if headerEnd < 0 {
		return "", fmt.Errorf("%w: missing document identity", ErrUnsafe)
	}

	header := string(data[:headerEnd+1])

	expected := string(encodeStored(ref, ""))
	if header != expected {
		return "", fmt.Errorf("%w: document identity does not match session", ErrUnsafe)
	}

	return string(data[headerEnd+1:]), nil
}

func (r *FileRepository) storedLimit(ref Ref) int64 {
	return r.limits.MaxBytes + int64(len(encodeStored(ref, "")))
}

func createTemp(root *os.Root, sessionID string) (string, *os.File, error) {
	for range 16 {
		var random [12]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", nil, fmt.Errorf("coding plan document: generate temporary name: %w", err)
		}

		name := "." + sessionID + "." + hex.EncodeToString(random[:]) + ".tmp"

		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
		if err == nil {
			return name, file, nil
		}

		if !errors.Is(err, fs.ErrExist) {
			return "", nil, fmt.Errorf("coding plan document: create temporary: %w", err)
		}
	}

	return "", nil, errors.New("coding plan document: allocate temporary file")
}

func writeAndSync(file *os.File, data []byte) error {
	if _, err := file.Write(data); err != nil {
		_ = file.Close()

		return fmt.Errorf("coding plan document: write temporary: %w", err)
	}

	if err := file.Sync(); err != nil {
		_ = file.Close()

		return fmt.Errorf("coding plan document: sync temporary: %w", err)
	}

	if err := file.Close(); err != nil {
		return fmt.Errorf("coding plan document: close temporary: %w", err)
	}

	return nil
}

func syncDirectory(directory string) (returnErr error) {
	file, err := os.Open(directory) //nolint:gosec // Validated application-owned Plan directory.
	if err != nil {
		return fmt.Errorf("coding plan document: open repository for sync: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()

	if err := file.Sync(); err != nil {
		return fmt.Errorf("coding plan document: sync repository: %w", err)
	}

	return nil
}
