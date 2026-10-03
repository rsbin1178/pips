//nolint:wsl_v5 // Archive ownership and publication checks stay adjacent to their operations.
package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/rsbin1178/pips/agent/harness"
)

// Archive limits are independent of model request and tool output budgets.
const (
	ArchiveMaxTranscriptBytes = 128 << 20
	ArchiveMaxRecordBytes     = 16 << 20
	ArchiveMaxRecords         = 100_000
	ArchiveMaxRenderedBytes   = 128 << 20
	ArchiveMaxSegments        = 1024
	ArchiveMaxReferences      = 1024
	ArchiveMaxOutputBytes     = 64 << 10

	archiveManifestBytes  = 1 << 20
	archiveSegmentBytes   = 256 << 10
	archiveLineBytes      = 4096
	archiveMaxObjectBytes = ArchiveMaxTranscriptBytes + ArchiveMaxRenderedBytes + archiveManifestBytes + 16
)

var (
	// ErrArchiveInvalid identifies invalid input or unsafe file ownership/modes.
	ErrArchiveInvalid = errors.New("session archive: invalid input")
	// ErrArchiveLimit identifies an encoded, rendered or operation bound violation.
	ErrArchiveLimit = errors.New("session archive: limit exceeded")
	// ErrArchiveCorrupt identifies failed integrity or format validation.
	ErrArchiveCorrupt = errors.New("session archive: corrupt object")
	// ErrArchiveDenied identifies a reference absent from the bound allowlist.
	ErrArchiveDenied = errors.New("session archive: reference not authorized")
	// ErrArchiveNotFound identifies an absent object in the owning session.
	ErrArchiveNotFound = errors.New("session archive: object not found")
	// ErrArchiveIO identifies a storage operation failure; its cause is preserved.
	ErrArchiveIO = errors.New("session archive: storage failure")
	// ErrArchivePublicationUncertain means the final name exists but its directory
	// sync failed. Never remove that object or append a checkpoint on this error.
	ErrArchivePublicationUncertain = errors.New("session archive: publication durability uncertain")
)

// ArchiveSource identifies the captured path. IDs are provenance, not graph
// edges to resolve, filesystem paths, or authority to access another session.
type ArchiveSource struct {
	SessionID string `json:"session_id"`
	TipID     string `json:"tip_id"`
}

// ArchiveInfo contains bounded discovery metadata, not archived content.
type ArchiveInfo struct {
	ID              string        `json:"id"`
	Source          ArchiveSource `json:"source"`
	Records         int           `json:"records"`
	TranscriptBytes int64         `json:"transcript_bytes"`
	RenderedBytes   int64         `json:"rendered_bytes"`
	Segments        int           `json:"segments"`
}

// ArchiveStore is an application-owned storage boundary, not a model-facing
// service. Construct a separately allowlisted ArchiveReader using Bind.
// Its lifetime must remain within the owning repository/Handle lifetime.
type ArchiveStore struct {
	dir       string
	sessionID string
}

// Archives binds storage ownership without enumerating or creating any objects.
func (r *Repository) Archives(handle *Handle) (*ArchiveStore, error) {
	if r == nil || handle == nil || handle.session == nil || handle.lock == nil ||
		validateSessionID(handle.meta.ID) != nil || handle.meta.Path != r.sessionPath(handle.meta.ID) {
		return nil, ErrArchiveInvalid
	}
	return &ArchiveStore{dir: r.dir, sessionID: handle.meta.ID}, nil
}

// ValidateArchiveID accepts only a lowercase SHA-256 content identity.
func ValidateArchiveID(id string) error {
	if len(id) != sha256.Size*2 {
		return ErrArchiveInvalid
	}
	for _, char := range id {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return ErrArchiveInvalid
		}
	}
	return nil
}

func (s *ArchiveStore) valid() bool {
	return s != nil && filepath.IsAbs(s.dir) && validateSessionID(s.sessionID) == nil
}

// StagedArchive owns one unpublished private temporary file. Close always
// removes only that temporary name, never a published content-addressed object.
// Publish and Close are serialized; publish is idempotent until Close.
type StagedArchive struct {
	mu        sync.Mutex
	dir       *os.File
	name      string
	info      ArchiveInfo
	closed    bool
	published bool
}

// ID is known before publication so a candidate checkpoint can be validated.
func (s *StagedArchive) ID() string { return s.info.ID }

// Info returns detached archive metadata.
func (s *StagedArchive) Info() ArchiveInfo { return s.info }

// Stage captures canonical entries and searchable text without changing the
// Session. The caller supplies an immutable, raw selected path through TipID.
// External label/branch/checkpoint references remain inert provenance; no
// Harness graph reconstruction or transitive archive resolution is performed.
func (s *ArchiveStore) Stage(ctx context.Context, source ArchiveSource, entries []harness.Entry) (*StagedArchive, error) {
	if !s.valid() || source.SessionID != s.sessionID || !archiveProvenanceID(source.TipID) {
		return nil, ErrArchiveInvalid
	}
	if len(entries) == 0 || len(entries) > ArchiveMaxRecords {
		return nil, ErrArchiveLimit
	}
	if entries[len(entries)-1].ID != source.TipID {
		return nil, ErrArchiveInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := openArchiveDirectory(s.dir, s.sessionID, true)
	if err != nil {
		return nil, archiveStorageError(err)
	}
	staged, err := stageArchive(ctx, dir, source, entries)
	if err != nil {
		return nil, errors.Join(err, dir.Close())
	}
	return staged, nil
}

// Publish verifies the complete staged object, exclusively publishes its final
// name and syncs the owning directory. Existing identical objects are reused;
// different/corrupt objects are never overwritten. It does not commit a Session
// checkpoint and does not grant retrieval permission.
func (s *StagedArchive) Publish(ctx context.Context) error {
	return s.publish(ctx, s.dir.Sync)
}

func (s *StagedArchive) publish(ctx context.Context, syncDirectory func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrArchiveInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	file, _, err := verifyArchiveAt(ctx, s.dir, s.name, s.info.ID)
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return archiveStorageError(err)
	}
	if !s.published {
		err = linkArchiveFile(s.dir, s.name, s.info.ID+".archive")
		if err != nil && !errors.Is(err, os.ErrExist) {
			return archiveStorageError(err)
		}
		// Check the published name itself as well as the staged source. A name
		// replaced with a symlink or a corrupt collision never counts as success.
		published, _, verifyErr := verifyArchiveAt(ctx, s.dir, s.info.ID+".archive", s.info.ID)
		if verifyErr != nil {
			return verifyErr
		}
		if err := published.Close(); err != nil {
			return archiveStorageError(err)
		}
		s.published = true
	}
	if err := syncDirectory(); err != nil {
		return errors.Join(ErrArchivePublicationUncertain, err)
	}
	return nil
}

// Close releases staging resources. It is safe after failed/uncertain Publish.
func (s *StagedArchive) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return errors.Join(removeArchiveFile(s.dir, s.name), s.dir.Close())
}

// Verify checks the entire object digest, manifest, canonical records and text
// segments. This privileged storage operation does not authorize model access.
func (s *ArchiveStore) Verify(ctx context.Context, id string) (ArchiveInfo, error) {
	file, manifest, err := s.openVerified(ctx, id)
	if err != nil {
		return ArchiveInfo{}, err
	}
	if err := file.Close(); err != nil {
		return ArchiveInfo{}, archiveStorageError(err)
	}
	return manifest.info(id), nil
}

func (s *ArchiveStore) openVerified(ctx context.Context, id string) (*os.File, archiveManifest, error) {
	if !s.valid() || ValidateArchiveID(id) != nil {
		return nil, archiveManifest{}, ErrArchiveInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, archiveManifest{}, err
	}
	dir, err := openArchiveDirectory(s.dir, s.sessionID, false)
	if err != nil {
		return nil, archiveManifest{}, archiveStorageError(err)
	}
	defer func() { _ = dir.Close() }()
	return verifyArchiveAt(ctx, dir, id+".archive", id)
}

// CopyArchives publishes independent, verified copies in a reserved destination
// session before its transcript is published. The caller must hold its writer
// lock and derive ids from the selected fork path. No destination Handle is
// required because the fork transcript does not exist yet. Provenance and IDs
// remain unchanged; neither source deletion nor old checkpoint references create
// transitive dependencies. Failure may leave inaccessible orphan objects.
func (r *Repository) CopyArchives(ctx context.Context, source *Handle, destinationID string, ids []string) error {
	store, err := r.Archives(source)
	if err != nil {
		return err
	}
	if validateSessionID(destinationID) != nil {
		return ErrArchiveInvalid
	}
	refs, err := archiveReferences(ids)
	if err != nil {
		return err
	}
	for _, id := range refs {
		if err := copyArchive(ctx, store, destinationID, id); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func copyArchive(ctx context.Context, source *ArchiveStore, destinationID, id string) error {
	file, manifest, err := source.openVerified(ctx, id)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	dir, err := openArchiveDirectory(source.dir, destinationID, true)
	if err != nil {
		return archiveStorageError(err)
	}
	staged, err := copyArchiveFile(ctx, dir, io.NewSectionReader(file, 0, archiveMaxObjectBytes+1), manifest.info(id))
	if err != nil {
		return errors.Join(err, dir.Close())
	}
	return errors.Join(staged.Publish(ctx), staged.Close())
}

func copyArchiveFile(ctx context.Context, dir *os.File, source io.Reader, info ArchiveInfo) (*StagedArchive, error) {
	file, name, err := createArchiveTemp(dir)
	if err != nil {
		return nil, archiveStorageError(err)
	}
	staged := &StagedArchive{dir: dir, name: name, info: info}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(file, hash), archiveContextReader{ctx, io.LimitReader(source, archiveMaxObjectBytes+1)})
	if copyErr == nil && (n > archiveMaxObjectBytes || hex.EncodeToString(hash.Sum(nil)) != info.ID) {
		copyErr = ErrArchiveCorrupt
	}
	if copyErr == nil {
		copyErr = file.Sync()
	}
	if err := errors.Join(copyErr, file.Close()); err != nil {
		return nil, errors.Join(archiveStorageError(err), removeArchiveFile(dir, name))
	}
	return staged, nil
}

func archiveReferences(ids []string) ([]string, error) {
	if len(ids) > ArchiveMaxReferences {
		return nil, ErrArchiveLimit
	}
	refs := slices.Clone(ids)
	for _, id := range refs {
		if ValidateArchiveID(id) != nil {
			return nil, ErrArchiveInvalid
		}
	}
	slices.Sort(refs)
	return slices.Compact(refs), nil
}

func archiveStorageError(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return errors.Join(ErrArchiveNotFound, err)
	}
	return fmt.Errorf("%w: %w", ErrArchiveIO, err)
}

//nolint:containedctx // Synchronous io.Reader adapter scoped to one operation, never retained by storage.
type archiveContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r archiveContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
