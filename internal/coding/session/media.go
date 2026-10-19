//nolint:wsl_v5 // Path validation and its safety checks stay adjacent.
package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
)

// Session media store. Originals live in images/, the bounded model-ready
// copies live in assets/. Every reference is content-addressed: the file name
// is "image-<sha256[:16]>.<ext>" and the reference carries the full digest so a
// reader can verify what it loaded.
const (
	// mediaNamePrefix is the fixed prefix of every session media file name.
	mediaNamePrefix = "image-"
	// mediaNameDigestBytes is how many SHA-256 hex characters name a file.
	mediaNameDigestBytes = 16
	// mediaNameBytes bounds one media file name.
	mediaNameBytes = 64
	// mediaRefComponents is the exact depth of a session-relative media path:
	// one directory component ("images" or "assets") plus one file name.
	mediaRefComponents = 2
	// mediaReadLimit bounds one media read; it matches the encoded ingress cap.
	mediaReadLimit = 20 << 20
)

// Media store errors.
var (
	// ErrMediaInvalid means a media reference, path, or file name is unusable.
	ErrMediaInvalid = errors.New("coding session: invalid media reference")
	// ErrMediaMissing means a referenced media file does not exist.
	ErrMediaMissing = errors.New("coding session: media file missing")
	// ErrMediaCorrupt means a media file's bytes do not match its reference.
	ErrMediaCorrupt = errors.New("coding session: media digest mismatch")
	// ErrMediaLimit means a media file exceeds the bounded read limit.
	ErrMediaLimit = errors.New("coding session: media exceeds the read limit")
)

// mediaExtensions maps a bounded set of image media types to file extensions.
var mediaExtensions = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
	"image/gif":  "gif",
	"image/webp": "webp",
}

// MediaRef is one content-addressed media reference. Path is session-relative
// ("assets/image-<sha16>.jpg"); Digest is the lowercase hex SHA-256 of the
// referenced bytes.
type MediaRef struct {
	Path     string
	Digest   string
	MIMEType string
	Width    int
	Height   int
	Bytes    int64
}

// Valid reports whether the reference is internally consistent.
func (r MediaRef) Valid() bool {
	if !validMediaPath(r.Path) || !validMediaDigest(r.Digest) {
		return false
	}

	return r.Width >= 0 && r.Height >= 0 && r.Bytes >= 0
}

// Base returns the file name without its directory or extension.
func (r MediaRef) Base() string {
	_, name := splitMediaPath(r.Path)

	return strings.TrimSuffix(name, filepath.Ext(name))
}

// GCReport summarizes one media garbage-collection pass.
type GCReport struct {
	// Deleted lists the session-relative paths that were removed.
	Deleted []string
	// Retained is the number of media files left in place.
	Retained int
	// Bytes is the total size of the deleted files.
	Bytes int64
}

// MediaStore owns the media directories of exactly one session directory. It
// is a pure path scheme plus filesystem primitives; callers hold the session
// lock while they write or collect.
type MediaStore struct {
	dir string
}

// NewMediaStore returns a media store rooted at one absolute session directory.
func NewMediaStore(sessionDir string) (*MediaStore, error) {
	if !filepath.IsAbs(sessionDir) {
		return nil, fmt.Errorf("%w: session directory must be absolute", ErrMediaInvalid)
	}

	return &MediaStore{dir: filepath.Clean(sessionDir)}, nil
}

// Dir returns the session directory the store writes into.
func (m *MediaStore) Dir() string {
	if m == nil {
		return ""
	}

	return m.dir
}

// PutOriginal writes one original media file under images/ and returns its
// reference. The file name is derived from the bytes' SHA-256, so identical
// originals converge on one file.
func (m *MediaStore) PutOriginal(ctx context.Context, mimeType string, data []byte) (MediaRef, error) {
	if err := ctx.Err(); err != nil {
		return MediaRef{}, err
	}

	extension, err := mediaExtension(mimeType)
	if err != nil {
		return MediaRef{}, err
	}

	digest := sha256.Sum256(data)

	ref := MediaRef{
		Path:     filepath.ToSlash(filepath.Join(ImagesDirName, mediaFileName(digest, extension))),
		Digest:   hex.EncodeToString(digest[:]),
		MIMEType: mimeType,
		Bytes:    int64(len(data)),
	}
	if err := m.write(ref.Path, data); err != nil {
		return MediaRef{}, err
	}

	return ref, nil
}

// PutModelCopy writes the bounded model-ready bytes under assets/ and returns
// its reference. When original names an existing original, the copy shares its
// base name so the original stays reachable from the copy's reference.
func (m *MediaStore) PutModelCopy(
	ctx context.Context,
	original MediaRef,
	mimeType string,
	data []byte,
) (MediaRef, error) {
	if err := ctx.Err(); err != nil {
		return MediaRef{}, err
	}

	extension, err := mediaExtension(mimeType)
	if err != nil {
		return MediaRef{}, err
	}

	digest := sha256.Sum256(data)

	base := original.Base()
	if base == "" {
		base = mediaNamePrefix + hex.EncodeToString(digest[:])[:mediaNameDigestBytes]
	}

	ref := MediaRef{
		Path:     filepath.ToSlash(filepath.Join(AssetsDirName, base+"."+extension)),
		Digest:   hex.EncodeToString(digest[:]),
		MIMEType: mimeType,
		Bytes:    int64(len(data)),
	}
	if err := m.write(ref.Path, data); err != nil {
		return MediaRef{}, err
	}

	return ref, nil
}

// Open reads one referenced media file, verifies its SHA-256, and returns
// detached bytes. A missing file is ErrMediaMissing; a digest mismatch is
// ErrMediaCorrupt; an unsafe path or symlink is ErrMediaInvalid.
func (m *MediaStore) Open(ctx context.Context, ref MediaRef) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if !ref.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrMediaInvalid, ref.Path)
	}

	path, err := m.resolve(ref.Path)
	if err != nil {
		return nil, err
	}

	data, err := m.read(path)
	if err != nil {
		return nil, err
	}

	sum := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), ref.Digest) {
		return nil, fmt.Errorf("%w: %q", ErrMediaCorrupt, ref.Path)
	}

	return data, nil
}

// Enumerate lists every media file in images/ and assets/ with its digest and
// stat metadata. Each returned reference is complete and passes [MediaRef.Valid].
// A missing directory is empty, not an error.
func (m *MediaStore) Enumerate(ctx context.Context) ([]MediaRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var refs []MediaRef

	for _, dir := range []string{ImagesDirName, AssetsDirName} {
		entries, err := m.list(dir)
		if err != nil {
			return nil, err
		}

		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}

			digest, err := m.digest(entry.Path)
			if err != nil {
				return nil, err
			}

			entry.Digest = digest
			refs = append(refs, entry.MediaRef)
		}
	}

	return refs, nil
}

// digest reads one media file and returns its lowercase hex SHA-256.
func (m *MediaStore) digest(relPath string) (string, error) {
	path, err := m.resolve(relPath)
	if err != nil {
		return "", err
	}

	data, err := m.read(path)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:]), nil
}

// GC removes media files that refs does not name and whose modification time
// is older than grace. A referenced assets/<base> keeps the matching
// images/<base> original. It returns what it removed and never deletes a file
// whose reference set membership or age is uncertain. A dry run reports the
// same plan without removing anything.
//
//nolint:gocyclo // Reference expansion and the age/keep checks stay adjacent.
func (m *MediaStore) GC(
	ctx context.Context,
	refs map[string]struct{},
	grace time.Duration,
	dryRun bool,
) (GCReport, error) {
	if err := ctx.Err(); err != nil {
		return GCReport{}, err
	}

	referenced := make(map[string]struct{}, len(refs))
	assetBases := make(map[string]struct{})

	for path := range refs {
		referenced[path] = struct{}{}

		dir, name := splitMediaPath(path)
		if dir == AssetsDirName {
			assetBases[strings.TrimSuffix(name, filepath.Ext(name))] = struct{}{}
		}
	}

	cutoff := time.Now().Add(-grace)

	report := GCReport{}

	for _, dir := range []string{ImagesDirName, AssetsDirName} {
		entries, err := m.list(dir)
		if err != nil {
			return report, err
		}

		for _, entry := range entries {
			if _, keep := referenced[entry.Path]; keep {
				report.Retained++

				continue
			}
			if dir == ImagesDirName {
				if _, keep := assetBases[entry.Base()]; keep {
					report.Retained++

					continue
				}
			}
			if grace > 0 && !entry.modified.Before(cutoff) {
				report.Retained++

				continue
			}

			if !dryRun {
				if err := m.remove(entry.Path); err != nil {
					return report, err
				}
			}

			report.Deleted = append(report.Deleted, entry.Path)
			report.Bytes += entry.Bytes
		}
	}

	slices.Sort(report.Deleted)

	return report, nil
}

// WriteRef publishes data at ref's session-relative path after verifying that
// the bytes match ref.Digest. It is the copy path used by fork.
func (m *MediaStore) WriteRef(ctx context.Context, ref MediaRef, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if !ref.Valid() {
		return fmt.Errorf("%w: %q", ErrMediaInvalid, ref.Path)
	}

	sum := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), ref.Digest) {
		return fmt.Errorf("%w: %q", ErrMediaCorrupt, ref.Path)
	}

	return m.write(ref.Path, data)
}

// CopyTo copies every media file refs names, plus the images/ original sharing
// a referenced assets/ base name, into destination. A missing source file is
// an error, so a fork never publishes a transcript with a dangling reference.
func (m *MediaStore) CopyTo(
	ctx context.Context,
	destination *MediaStore,
	refs map[string]struct{},
) error {
	if m == nil || destination == nil {
		return fmt.Errorf("%w: missing media store", ErrMediaInvalid)
	}

	referenced := make(map[string]struct{}, len(refs))
	assetBases := make(map[string]struct{})

	for path := range refs {
		referenced[path] = struct{}{}

		dir, name := splitMediaPath(path)
		if dir == AssetsDirName {
			assetBases[strings.TrimSuffix(name, filepath.Ext(name))] = struct{}{}
		}
	}

	// Every referenced file must exist; a fork never publishes a dangling
	// reference.
	for path := range referenced {
		if !validMediaPath(path) {
			return fmt.Errorf("%w: %q", ErrMediaInvalid, path)
		}

		if err := m.copyFile(ctx, destination, path); err != nil {
			return err
		}
	}

	// Bring the images/ original sharing a referenced assets/ base name.
	images, err := m.list(ImagesDirName)
	if err != nil {
		return err
	}

	for _, entry := range images {
		if _, keep := assetBases[entry.Base()]; !keep {
			continue
		}

		if err := m.copyFile(ctx, destination, entry.Path); err != nil {
			return err
		}
	}

	return nil
}

func (m *MediaStore) copyFile(ctx context.Context, destination *MediaStore, relPath string) error {
	path, err := m.resolve(relPath)
	if err != nil {
		return err
	}

	data, err := m.read(path)
	if err != nil {
		return err
	}

	sum := sha256.Sum256(data)
	ref, err := MediaRefFromSource(relPath, hex.EncodeToString(sum[:]), mediaMIMEType(relPath), 0, 0)
	if err != nil {
		return err
	}

	return destination.WriteRef(ctx, ref, data)
}

// CopySessionMedia copies the media the retained fork entries reference from
// one session directory into another.
func CopySessionMedia(
	ctx context.Context,
	sourceDir string,
	destinationDir string,
	entries []harness.Entry,
) error {
	source, err := NewMediaStore(sourceDir)
	if err != nil {
		return err
	}

	destination, err := NewMediaStore(destinationDir)
	if err != nil {
		return err
	}

	return source.CopyTo(ctx, destination, MediaReferencesFromEntries(entries))
}

// MediaReferencesFromEntries collects the session-relative media paths every
// message in entries references, including nested tool-result content.
func MediaReferencesFromEntries(entries []harness.Entry) map[string]struct{} {
	refs := make(map[string]struct{})

	for _, entry := range entries {
		if entry.Message == nil {
			continue
		}

		parts, err := ai.MessageParts(entry.Message)
		if err != nil {
			continue
		}

		collectMediaReferences(parts, refs, 0)
	}

	return refs
}

func collectMediaReferences(parts []ai.Part, refs map[string]struct{}, depth int) {
	if depth > 8 {
		return
	}

	for _, part := range parts {
		switch value := part.(type) {
		case ai.ImagePart:
			if value.Source.Path != "" {
				refs[value.Source.Path] = struct{}{}
			}
		case ai.FilePart:
			if value.Source.Path != "" {
				refs[value.Source.Path] = struct{}{}
			}
		case ai.ToolResultPart:
			collectMediaReferences(value.Content, refs, depth+1)
		}
	}
}

// mediaEntry holds a listed media file with its modification time.
type mediaEntry struct {
	MediaRef
	modified time.Time
}

func (m *MediaStore) list(dir string) ([]mediaEntry, error) {
	entries, err := os.ReadDir(filepath.Join(m.dir, dir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("coding session: list media: %w", err)
	}

	var out []mediaEntry

	for _, entry := range entries {
		name := entry.Name()
		if !validMediaName(name) {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("coding session: inspect media: %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}

		out = append(out, mediaEntry{
			MediaRef{
				Path:     dir + "/" + name,
				MIMEType: mediaMIMEType(name),
				Bytes:    info.Size(),
			},
			info.ModTime(),
		})
	}

	return out, nil
}

func (m *MediaStore) write(relPath string, data []byte) error {
	if !validMediaPath(relPath) {
		return fmt.Errorf("%w: %q", ErrMediaInvalid, relPath)
	}

	path := filepath.Join(m.dir, filepath.FromSlash(relPath))

	// A media directory is private to the session; refuse a symlinked parent so
	// a write can never be redirected outside it.
	if err := ensureSessionDirectory(filepath.Dir(path)); err != nil {
		return err
	}

	return writeFileAtomic(path, data, 0o600)
}

func (m *MediaStore) resolve(relPath string) (string, error) {
	path := filepath.Join(m.dir, filepath.FromSlash(relPath))

	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %q", ErrMediaMissing, relPath)
		}

		return "", fmt.Errorf("coding session: inspect media: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %q is not a regular file", ErrMediaInvalid, relPath)
	}

	if err := secureMediaFile(path); err != nil {
		return "", err
	}

	return path, nil
}

func (m *MediaStore) read(path string) ([]byte, error) {
	file, err := os.Open(path) //nolint:gosec // caller-owned session path
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %q", ErrMediaMissing, path)
		}

		return nil, fmt.Errorf("coding session: open media: %w", err)
	}

	data, readErr := readBoundedFile(file, mediaReadLimit)
	closeErr := file.Close()

	if readErr != nil {
		if errors.Is(readErr, ErrLayout) {
			return nil, fmt.Errorf("%w: %q", ErrMediaLimit, path)
		}

		return nil, errors.Join(readErr, closeErr)
	}

	return data, closeErr
}

func (m *MediaStore) remove(relPath string) error {
	if !validMediaPath(relPath) {
		return fmt.Errorf("%w: %q", ErrMediaInvalid, relPath)
	}

	path := filepath.Join(m.dir, filepath.FromSlash(relPath))
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("coding session: remove media: %w", err)
	}

	return nil
}

// MediaRelativePath joins a media directory and file name into a
// session-relative path and validates the result.
func MediaRelativePath(dir, name string) (string, error) {
	if dir != ImagesDirName && dir != AssetsDirName {
		return "", fmt.Errorf("%w: unknown media directory %q", ErrMediaInvalid, dir)
	}
	if !validMediaName(name) {
		return "", fmt.Errorf("%w: invalid media name %q", ErrMediaInvalid, name)
	}

	return dir + "/" + name, nil
}

// MediaRefFromSource validates a session-relative media path and digest.
func MediaRefFromSource(path, digest, mimeType string, width, height int) (MediaRef, error) {
	ref := MediaRef{
		Path:     path,
		Digest:   strings.ToLower(digest),
		MIMEType: mimeType,
		Width:    width,
		Height:   height,
	}
	if !ref.Valid() {
		return MediaRef{}, fmt.Errorf("%w: %q", ErrMediaInvalid, path)
	}

	return ref, nil
}

// ValidMediaReference reports whether path is a session-relative media path
// (one media directory plus one content-addressed file name) and digest is a
// lowercase hex SHA-256.
func ValidMediaReference(path, digest string) bool {
	return validMediaPath(path) && validMediaDigest(digest)
}

func mediaExtension(mimeType string) (string, error) {
	extension, ok := mediaExtensions[strings.ToLower(strings.TrimSpace(mimeType))]
	if !ok {
		return "", fmt.Errorf("%w: unsupported media type %q", ErrMediaInvalid, mimeType)
	}

	return extension, nil
}

func mediaMIMEType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return ""
	}
}

func mediaFileName(digest [sha256.Size]byte, extension string) string {
	return mediaNamePrefix + hex.EncodeToString(digest[:])[:mediaNameDigestBytes] + "." + extension
}

func validMediaName(name string) bool {
	if len(name) == 0 || len(name) > mediaNameBytes {
		return false
	}
	if !strings.HasPrefix(name, mediaNamePrefix) {
		return false
	}

	extension := filepath.Ext(name)
	if extension == "" {
		return false
	}

	stem := strings.TrimSuffix(name, extension)
	if len(stem) != len(mediaNamePrefix)+mediaNameDigestBytes {
		return false
	}

	for _, char := range stem[len(mediaNamePrefix):] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}

	return mediaMIMEType(name) != ""
}

func validMediaPath(path string) bool {
	dir, name := splitMediaPath(path)
	if dir != ImagesDirName && dir != AssetsDirName {
		return false
	}

	return validMediaName(name)
}

func splitMediaPath(path string) (dir, name string) {
	parts := strings.Split(path, "/")
	if len(parts) != mediaRefComponents {
		return "", ""
	}

	return parts[0], parts[1]
}

func validMediaDigest(digest string) bool {
	if len(digest) != sha256.Size*2 {
		return false
	}

	for _, char := range digest {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}

	return true
}
