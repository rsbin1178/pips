//nolint:wsl_v5 // Installation ownership, staging, and publication form one filesystem boundary.
package pluginstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/rsbin/pips/internal/jsonx"
)

const (
	artifactsDirectory = "artifacts/sha256"
	manifestsDirectory = "manifests/sha256"
	leasesDirectory    = "leases/sha256"
	recordsDirectory   = "records"
	enabledDirectory   = "enabled"
	stateDirectory     = "state"
	stagingDirectory   = "staging"
)

// New creates an application-owned content-addressed plugin store. It creates
// only metadata/artifact roots; it never scans or executes plugin files.
func New(root string, limits Limits) (*Store, error) {
	if strings.TrimSpace(root) == "" || strings.ContainsRune(root, '\x00') {
		return nil, fmt.Errorf("%w: empty or NUL store root", ErrInvalid)
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve store root: %w", ErrInvalid, err)
	}
	if err := ensureDirectory(absolute); err != nil {
		return nil, err
	}
	limits = limits.withDefaults()
	if err := limits.validate(); err != nil {
		return nil, err
	}
	store := &Store{root: absolute, limits: limits, now: time.Now}
	fileLock, err := store.acquireStoreLock(context.Background())
	if err != nil {
		return nil, err
	}
	defer func() { _ = fileLock.release() }()
	for _, relative := range []string{artifactsDirectory, manifestsDirectory, leasesDirectory, recordsDirectory, enabledDirectory, stateDirectory, stagingDirectory} {
		if err := ensureDirectory(filepath.Join(absolute, filepath.FromSlash(relative))); err != nil {
			return nil, err
		}
	}
	return store, nil
}

// Root returns the absolute store root.
func (s *Store) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

// Install validates and copies one selected target into the immutable artifact
// store, writes its deterministic install record, and (by default) atomically
// updates the plugin's enablement reference. The plugin executable is never
// launched or queried.
//
//nolint:funlen,gocyclo // Installation is one transactional boundary from manifest to enablement.
func (s *Store) Install(ctx context.Context, manifestPath string, options InstallOptions) (InstallRecord, error) {
	if s == nil {
		return InstallRecord{}, fmt.Errorf("%w: nil store", ErrInvalid)
	}
	if err := contextErr(ctx); err != nil {
		return InstallRecord{}, err
	}
	rawManifest, err := readStableRegularFile(manifestPath, s.limits.ManifestBytes, ErrInvalid)
	if err != nil {
		return InstallRecord{}, err
	}
	manifest, err := parseManifest(rawManifest, s.limits)
	if err != nil {
		return InstallRecord{}, err
	}
	if err := manifest.validate(s.limits); err != nil {
		return InstallRecord{}, err
	}
	targetPlatform := options.Target
	if targetPlatform.OS == "" {
		targetPlatform.OS = runtime.GOOS
	}
	if targetPlatform.Arch == "" {
		targetPlatform.Arch = runtime.GOARCH
	}
	target, err := manifest.selectTarget(targetPlatform.OS, targetPlatform.Arch, s.limits)
	if err != nil {
		return InstallRecord{}, err
	}
	if err := validateTargetPlatform(targetPlatform); err != nil {
		return InstallRecord{}, err
	}
	source := options.Source
	if source.Scope == "" {
		source.Scope = SourceLocal
	}
	if source.Reference == "" {
		absoluteManifest, absoluteErr := filepath.Abs(manifestPath)
		if absoluteErr != nil {
			return InstallRecord{}, fmt.Errorf("%w: resolve manifest reference: %w", ErrInvalid, absoluteErr)
		}
		source.Reference = filepath.Clean(absoluteManifest)
	}
	if err := validateSource(source); err != nil {
		return InstallRecord{}, err
	}
	decisionRefs, err := validateDecisionRefs(options.CapabilityDecisionRefs, s.limits.DecisionRefs)
	if err != nil {
		return InstallRecord{}, err
	}
	if err := validateInstallMetadata(options.RequestedVersion, options.AssociatedBundleDigest); err != nil {
		return InstallRecord{}, err
	}
	manifestDigest := digestBytes(rawManifest)
	artifactSource := filepath.Join(filepath.Dir(filepath.Clean(manifestPath)), filepath.FromSlash(target.Path))

	s.mu.Lock()
	defer s.mu.Unlock()
	storeLock, err := s.acquireStoreLock(ctx)
	if err != nil {
		return InstallRecord{}, err
	}
	defer func() { _ = storeLock.release() }()
	if err := contextErr(ctx); err != nil {
		return InstallRecord{}, err
	}
	stagePath, artifactDigest, sourceMode, err := s.stageArtifact(ctx, artifactSource, target.SHA256)
	if err != nil {
		return InstallRecord{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(stagePath)
		}
	}()
	artifactPath, err := s.commitArtifact(stagePath, artifactDigest, sourceMode)
	if err != nil {
		var publication *PublicationError
		if errors.As(err, &publication) {
			partial := InstallRecord{
				PluginID:       manifest.ID,
				Version:        manifest.Version,
				ManifestDigest: manifestDigest,
				ManifestPath:   canonicalManifestPath(manifestDigest),
				ArtifactDigest: artifactDigest,
				ArtifactPath:   artifactPath,
			}
			return InstallRecord{}, s.installPublicationError(err, partial, artifactPath)
		}
		return InstallRecord{}, err
	}
	committed = true
	if err := s.commitManifest(rawManifest, manifestDigest); err != nil {
		return InstallRecord{}, s.installPublicationError(err, InstallRecord{PluginID: manifest.ID, ManifestDigest: manifestDigest, ArtifactDigest: artifactDigest, ArtifactPath: artifactPath}, artifactPath)
	}

	provenance := ProvenanceResult{Status: ProvenanceAbsent}
	if manifest.Provenance != nil {
		provenance = ProvenanceResult{
			Status:    ProvenanceDeclaredUnverified,
			Publisher: manifest.Provenance.Publisher,
			Source:    manifest.Provenance.Source,
			Signature: manifest.Provenance.Signature,
		}
	}
	record := InstallRecord{
		Schema:                 recordSchema,
		PluginID:               manifest.ID,
		Version:                manifest.Version,
		RequestedVersion:       options.RequestedVersion,
		ManifestDigest:         manifestDigest,
		ManifestPath:           canonicalManifestPath(manifestDigest),
		ArtifactDigest:         artifactDigest,
		Target:                 targetPlatform,
		ArtifactPath:           artifactPath,
		StatePath:              "state/" + manifest.ID,
		Source:                 source,
		Provenance:             provenance,
		AssociatedBundleDigest: options.AssociatedBundleDigest,
		CapabilityDecisionRefs: decisionRefs,
		InstalledAt:            s.now().UTC(),
	}
	if err := validateInstallRecord(record, s.limits); err != nil {
		return InstallRecord{}, s.installPublicationError(err, record, artifactPath)
	}
	if err := s.ensureStateDirectory(manifest.ID); err != nil {
		return InstallRecord{}, s.installPublicationError(err, record, artifactPath)
	}
	recordPath, err := s.internalPath(canonicalRecordPath(record.PluginID, record.ManifestDigest, record.ArtifactDigest))
	if err != nil {
		return InstallRecord{}, s.installPublicationError(err, record, artifactPath)
	}
	stored, err := s.writeOrReadRecord(recordPath, record)
	if err != nil {
		candidate := stored
		if candidate.PluginID == "" {
			candidate = record
		}
		return stored, s.installPublicationError(err, candidate, artifactPath)
	}
	if options.Enable == nil || *options.Enable {
		if _, err := s.currentLocked(manifest.ID); err != nil && !errors.Is(err, ErrNotEnabled) {
			return stored, s.installPublicationError(err, stored, artifactPath)
		}
		if err := s.publishEnablement(stored); err != nil {
			return stored, s.installPublicationError(err, stored, artifactPath)
		}
	}
	return stored, nil
}

// Current returns the complete record referenced by the current enablement.
func (s *Store) Current(ctx context.Context, pluginID string) (InstallRecord, error) {
	if s == nil {
		return InstallRecord{}, fmt.Errorf("%w: nil store", ErrInvalid)
	}
	if err := contextErr(ctx); err != nil {
		return InstallRecord{}, err
	}
	if !validIdentifier(pluginID) {
		return InstallRecord{}, fmt.Errorf("%w: plugin id", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	storeLock, err := s.acquireStoreLock(ctx)
	if err != nil {
		return InstallRecord{}, err
	}
	defer func() { _ = storeLock.release() }()
	return s.currentLocked(pluginID)
}

// Disable removes only the current enablement reference. Installed records,
// artifacts, and the plugin's separate state directory remain intact.
func (s *Store) Disable(ctx context.Context, pluginID string) error {
	if s == nil {
		return fmt.Errorf("%w: nil store", ErrInvalid)
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	if !validIdentifier(pluginID) {
		return fmt.Errorf("%w: plugin id", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	storeLock, err := s.acquireStoreLock(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = storeLock.release() }()
	if _, err := s.currentLocked(pluginID); err != nil {
		return err
	}
	path, err := s.internalPath(filepath.ToSlash(filepath.Join(enabledDirectory, pluginID+".json")))
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("%w: remove enablement: %w", ErrConflict, err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return &PublicationError{Operation: "disable", Path: path, PluginID: pluginID, Committed: true, Published: true, Err: err}
	}
	return nil
}

// RemoveRecord removes a disabled install record and its artifact when no
// other record references that artifact. It intentionally never removes the
// plugin state directory.
//
//nolint:gocyclo // Removal validates current ownership before reference collection.
func (s *Store) RemoveRecord(ctx context.Context, pluginID, manifestDigest string) error {
	if s == nil {
		return fmt.Errorf("%w: nil store", ErrInvalid)
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	if !validIdentifier(pluginID) || !validDigest(manifestDigest) {
		return fmt.Errorf("%w: record identity", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	storeLock, err := s.acquireStoreLock(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = storeLock.release() }()
	if current, err := s.currentLocked(pluginID); err == nil && current.ManifestDigest == manifestDigest {
		return ErrEnabled
	} else if err != nil && !errors.Is(err, ErrNotEnabled) && !errors.Is(err, ErrNotFound) {
		return err
	}
	recordPath, record, err := s.findRecord(pluginID, manifestDigest)
	if err != nil {
		return err
	}
	relativeRecordPath, relErr := filepath.Rel(s.root, recordPath)
	if relErr != nil {
		return fmt.Errorf("%w: record path: %w", ErrUnsafeArtifact, relErr)
	}
	recordPath, err = s.internalPath(filepath.ToSlash(relativeRecordPath))
	if err != nil {
		return err
	}
	if err := os.Remove(recordPath); err != nil {
		return fmt.Errorf("%w: remove install record: %w", ErrConflict, err)
	}
	if err := syncDirectory(filepath.Dir(recordPath)); err != nil {
		return s.removeRecordPublicationError(err, record, recordPath, "remove-record")
	}
	remaining, err := s.artifactReferenced(record.ArtifactDigest)
	if err != nil {
		return s.removeRecordPublicationError(err, record, recordPath, "remove-record-reference-scan")
	}
	if !remaining {
		leased, leaseErr := s.artifactLeased(record.ArtifactDigest)
		if leaseErr != nil {
			return s.removeRecordPublicationError(leaseErr, record, recordPath, "remove-record-lease-check")
		}
		if leased {
			return nil
		}
		artifactPath, pathErr := s.internalPath(canonicalArtifactPath(record.ArtifactDigest))
		if pathErr != nil {
			return s.removeRecordPublicationError(pathErr, record, recordPath, "remove-record-artifact")
		}
		if err := s.collectArtifactIfUnreferenced(record.ArtifactDigest); err != nil {
			return s.removeRecordPublicationError(err, record, artifactPath, "remove-record-artifact")
		}
	}
	return nil
}

//nolint:gocyclo // Current enablement validation binds pointer and record identities.
func (s *Store) currentLocked(pluginID string) (InstallRecord, error) {
	enablementPath, err := s.internalPath(filepath.ToSlash(filepath.Join(enabledDirectory, pluginID+".json")))
	if err != nil {
		return InstallRecord{}, err
	}
	data, err := readStableRegularFile(enablementPath, s.limits.RecordBytes, ErrNotFound)
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return InstallRecord{}, ErrNotEnabled
		}
		return InstallRecord{}, err
	}
	var enablement enablementRecord
	if err := decodeStoredJSON(data, &enablement); err != nil {
		return InstallRecord{}, err
	}
	if err := validateEnablement(enablement); err != nil {
		return InstallRecord{}, err
	}
	if enablement.PluginID != pluginID {
		return InstallRecord{}, fmt.Errorf("%w: enablement plugin identity", ErrConflict)
	}
	recordPath, err := s.internalPath(enablement.RecordPath)
	if err != nil {
		return InstallRecord{}, err
	}
	recordData, err := readStableRegularFile(recordPath, s.limits.RecordBytes, ErrNotFound)
	if err != nil {
		return InstallRecord{}, err
	}
	var record InstallRecord
	if err := decodeStoredJSON(recordData, &record); err != nil {
		return InstallRecord{}, err
	}
	if err := s.validateStoredRecord(record); err != nil {
		return InstallRecord{}, err
	}
	if record.PluginID != pluginID || record.ManifestDigest != enablement.ManifestDigest ||
		record.ArtifactDigest != enablement.ArtifactDigest || record.Version != enablement.Version {
		return InstallRecord{}, fmt.Errorf("%w: enablement does not match install record", ErrConflict)
	}
	return record, nil
}

//nolint:gocyclo // Staging owns source identity, bounded copy, digest, and cleanup.
func (s *Store) stageArtifact(ctx context.Context, sourcePath, expectedDigest string) (string, string, os.FileMode, error) {
	if err := rejectSymlinkComponents(filepath.Dir(sourcePath), sourcePath); err != nil {
		return "", "", 0, err
	}
	info, err := os.Lstat(sourcePath)
	if err != nil {
		return "", "", 0, fmt.Errorf("%w: inspect artifact: %w", ErrUnsafeArtifact, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", "", 0, fmt.Errorf("%w: artifact must be a regular non-symlink", ErrUnsafeArtifact)
	}
	if err := validateSourceArtifactMode(info.Mode()); err != nil {
		return "", "", 0, err
	}
	input, err := os.Open(sourcePath) //nolint:gosec // Source path is manifest-relative and identity-checked.
	if err != nil {
		return "", "", 0, fmt.Errorf("%w: open artifact: %w", ErrUnsafeArtifact, err)
	}
	defer func() { _ = input.Close() }()
	openedInfo, err := input.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return "", "", 0, fmt.Errorf("%w: artifact changed while opening", ErrConflict)
	}
	if err := validateSourceArtifactMode(openedInfo.Mode()); err != nil {
		return "", "", 0, err
	}

	stage, err := os.CreateTemp(filepath.Join(s.root, stagingDirectory), ".artifact-*")
	if err != nil {
		return "", "", 0, fmt.Errorf("%w: create artifact staging file: %w", ErrConflict, err)
	}
	stagePath := stage.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = stage.Close()
			_ = os.Remove(stagePath)
		}
	}()
	sha := sha256.New()
	buffer := make([]byte, 128<<10)
	var total int64
	for {
		if err := contextErr(ctx); err != nil {
			return "", "", 0, err
		}
		count, readErr := input.Read(buffer)
		if count > 0 {
			total += int64(count)
			if total > s.limits.ArtifactBytes {
				return "", "", 0, fmt.Errorf("%w: artifact exceeds %d bytes", ErrLimitExceeded, s.limits.ArtifactBytes)
			}
			if _, err := stage.Write(buffer[:count]); err != nil {
				return "", "", 0, fmt.Errorf("%w: stage artifact: %w", ErrConflict, err)
			}
			if _, err := sha.Write(buffer[:count]); err != nil {
				return "", "", 0, err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", "", 0, fmt.Errorf("%w: read artifact: %w", ErrUnsafeArtifact, readErr)
		}
	}
	if err := stage.Sync(); err != nil {
		return "", "", 0, fmt.Errorf("%w: sync staged artifact: %w", ErrConflict, err)
	}
	if err := stage.Close(); err != nil {
		return "", "", 0, fmt.Errorf("%w: close staged artifact: %w", ErrConflict, err)
	}
	cleanup = false
	if err := rejectSymlinkComponents(filepath.Dir(sourcePath), sourcePath); err != nil {
		return "", "", 0, err
	}
	after, err := os.Lstat(sourcePath)
	if err != nil || !after.Mode().IsRegular() || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, after) {
		_ = os.Remove(stagePath)
		return "", "", 0, fmt.Errorf("%w: artifact changed while reading", ErrConflict)
	}
	if err := validateSourceArtifactMode(after.Mode()); err != nil {
		_ = os.Remove(stagePath)
		return "", "", 0, err
	}
	digestBytesValue := sha.Sum(nil)
	artifactDigest := hex.EncodeToString(digestBytesValue)
	if artifactDigest != expectedDigest {
		_ = os.Remove(stagePath)
		return "", "", 0, fmt.Errorf("%w: expected %s, got %s", ErrDigestMismatch, expectedDigest, artifactDigest)
	}
	return stagePath, artifactDigest, fileModeForArtifact(info.Mode()), nil
}

//nolint:gocyclo // Content-addressed commit validates existing bytes and publication atomically.
func (s *Store) commitArtifact(stagePath, digest string, mode os.FileMode) (string, error) {
	relative := canonicalArtifactPath(digest)
	destination, err := s.internalPath(relative)
	if err != nil {
		return "", err
	}
	if info, err := os.Lstat(destination); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: existing artifact is not a regular file", ErrUnsafeArtifact)
		}
		actual, readErr := digestRegularFile(destination, s.limits.ArtifactBytes)
		if readErr != nil {
			return "", readErr
		}
		if actual != digest {
			return "", fmt.Errorf("%w: existing artifact content", ErrConflict)
		}
		_ = os.Remove(stagePath)
		return relative, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%w: inspect artifact destination: %w", ErrConflict, err)
	}
	if runtime.GOOS != "windows" && mode != 0 {
		if err := os.Chmod(stagePath, mode); err != nil {
			return "", fmt.Errorf("%w: preserve artifact mode: %w", ErrConflict, err)
		}
	}
	if err := os.Rename(stagePath, destination); err != nil {
		if errors.Is(err, os.ErrExist) {
			return s.commitArtifact(stagePath, digest, mode)
		}
		return "", fmt.Errorf("%w: commit artifact: %w", ErrConflict, err)
	}
	if err := syncDirectory(filepath.Dir(destination)); err != nil {
		return relative, &PublicationError{Operation: "artifact", Path: destination, Committed: true, Err: err}
	}
	return relative, nil
}

func (s *Store) internalPath(relative string) (string, error) {
	if !validRelativeArtifactPath(relative) {
		return "", fmt.Errorf("%w: unsafe internal path", ErrUnsafeArtifact)
	}
	absolute := filepath.Join(s.root, filepath.FromSlash(relative))
	if err := rejectSymlinkComponents(s.root, absolute); err != nil {
		return "", err
	}
	return absolute, nil
}

//nolint:gocyclo // Idempotent record publication validates an existing record before reuse.
func (s *Store) writeOrReadRecord(recordPath string, record InstallRecord) (InstallRecord, error) {
	if data, err := readStableRegularFile(recordPath, s.limits.RecordBytes, ErrNotFound); err == nil {
		var existing InstallRecord
		if err := decodeStoredJSON(data, &existing); err != nil {
			return InstallRecord{}, err
		}
		if err := s.validateStoredRecord(existing); err != nil {
			return InstallRecord{}, err
		}
		if existing.PluginID != record.PluginID || existing.ManifestDigest != record.ManifestDigest || existing.ManifestPath != record.ManifestPath || existing.ArtifactDigest != record.ArtifactDigest ||
			existing.Version != record.Version || existing.RequestedVersion != record.RequestedVersion || existing.Target != record.Target || existing.ArtifactPath != record.ArtifactPath ||
			existing.StatePath != record.StatePath || existing.Source != record.Source || existing.Provenance != record.Provenance ||
			existing.AssociatedBundleDigest != record.AssociatedBundleDigest ||
			!slices.Equal(existing.CapabilityDecisionRefs, record.CapabilityDecisionRefs) {
			return InstallRecord{}, fmt.Errorf("%w: install record identity", ErrConflict)
		}
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) && !errors.Is(err, os.ErrNotExist) {
		return InstallRecord{}, err
	}
	if err := writeJSONAtomic(recordPath, record, s.limits.RecordBytes); err != nil {
		return record, err
	}
	return record, nil
}

func (s *Store) publishEnablement(record InstallRecord) error {
	relativeRecord := canonicalRecordPath(record.PluginID, record.ManifestDigest, record.ArtifactDigest)
	enablement := enablementRecord{
		Schema:         enableSchema,
		PluginID:       record.PluginID,
		RecordPath:     relativeRecord,
		ManifestDigest: record.ManifestDigest,
		ArtifactDigest: record.ArtifactDigest,
		Version:        record.Version,
		EnabledAt:      s.now().UTC(),
	}
	path, err := s.internalPath(filepath.ToSlash(filepath.Join(enabledDirectory, record.PluginID+".json")))
	if err != nil {
		return err
	}
	if err := writeJSONAtomicNamed(path, enablement, s.limits.RecordBytes, "enablement"); err != nil {
		var publication *PublicationError
		if errors.As(err, &publication) {
			publication.Published = true
		}
		return err
	}
	return nil
}

func (s *Store) removeRecordPublicationError(err error, record InstallRecord, path, operation string) error {
	if err == nil {
		return nil
	}
	var publication *PublicationError
	if errors.As(err, &publication) {
		if publication.Operation == "" {
			publication.Operation = operation
		}
		if publication.Path == "" {
			publication.Path = path
		}
		if publication.PluginID == "" {
			publication.PluginID = record.PluginID
		}
		if publication.Record == nil {
			recordCopy := record
			publication.Record = &recordCopy
		}
		publication.Committed = true
		return publication
	}
	recordCopy := record
	return &PublicationError{
		Operation: operation,
		Path:      path,
		PluginID:  record.PluginID,
		Record:    &recordCopy,
		Committed: true,
		Err:       err,
	}
}

func (s *Store) decoratePublicationError(err error, record InstallRecord) error {
	var publication *PublicationError
	if !errors.As(err, &publication) {
		return err
	}
	if publication.PluginID == "" {
		publication.PluginID = record.PluginID
	}
	if record.PluginID != "" {
		recordCopy := record
		publication.Record = &recordCopy
	}
	return publication
}

func (s *Store) installPublicationError(err error, record InstallRecord, artifactPath string) error {
	var publication *PublicationError
	if errors.As(err, &publication) {
		return s.decoratePublicationError(err, record)
	}
	recordCopy := record
	return &PublicationError{
		Operation: "install",
		Path:      filepath.Join(s.root, filepath.FromSlash(artifactPath)),
		PluginID:  record.PluginID,
		Record:    &recordCopy,
		Committed: true,
		Err:       err,
	}
}

func (s *Store) ensureStateDirectory(pluginID string) error {
	path, err := s.internalPath(filepath.ToSlash(filepath.Join(stateDirectory, pluginID)))
	if err != nil {
		return err
	}
	return ensureDirectory(path)
}

//nolint:gocyclo // Record scanning validates each candidate before selecting its identity.
func (s *Store) findRecord(pluginID, manifestDigest string) (string, InstallRecord, error) {
	directory, pathErr := s.internalPath(filepath.ToSlash(filepath.Join(recordsDirectory, pluginID)))
	if pathErr != nil {
		return "", InstallRecord{}, pathErr
	}
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return "", InstallRecord{}, ErrNotFound
	}
	if err != nil {
		return "", InstallRecord{}, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		candidate, pathErr := s.internalPath(filepath.ToSlash(filepath.Join(recordsDirectory, pluginID, entry.Name())))
		if pathErr != nil {
			return "", InstallRecord{}, pathErr
		}
		data, readErr := readStableRegularFile(candidate, s.limits.RecordBytes, ErrNotFound)
		if readErr != nil {
			return "", InstallRecord{}, readErr
		}
		var record InstallRecord
		if err := decodeStoredJSON(data, &record); err != nil {
			return "", InstallRecord{}, err
		}
		if err := s.validateStoredRecord(record); err != nil {
			return "", InstallRecord{}, err
		}
		if !sameInternalRecordPath(s.root, candidate, record) {
			return "", InstallRecord{}, fmt.Errorf("%w: install record path", ErrConflict)
		}
		if record.PluginID == pluginID && record.ManifestDigest == manifestDigest {
			return candidate, record, nil
		}
	}
	return "", InstallRecord{}, ErrNotFound
}

//nolint:gocyclo // Reference scanning validates every stored record before collection.
func (s *Store) artifactReferenced(digest string) (bool, error) {
	recordsRoot, err := s.internalPath(recordsDirectory)
	if err != nil {
		return false, err
	}
	entries, err := os.ReadDir(recordsRoot)
	if err != nil {
		return false, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, pluginEntry := range entries {
		if !pluginEntry.IsDir() || !validIdentifier(pluginEntry.Name()) {
			continue
		}
		pluginDir, pathErr := s.internalPath(filepath.ToSlash(filepath.Join(recordsDirectory, pluginEntry.Name())))
		if pathErr != nil {
			return false, pathErr
		}
		records, err := os.ReadDir(pluginDir)
		if err != nil {
			return false, err
		}
		for _, entry := range records {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			candidate, pathErr := s.internalPath(filepath.ToSlash(filepath.Join(recordsDirectory, pluginEntry.Name(), entry.Name())))
			if pathErr != nil {
				return false, pathErr
			}
			data, err := readStableRegularFile(candidate, s.limits.RecordBytes, ErrNotFound)
			if err != nil {
				return false, err
			}
			var record InstallRecord
			if err := decodeStoredJSON(data, &record); err != nil {
				return false, err
			}
			if err := s.validateStoredRecord(record); err != nil {
				return false, err
			}
			if !sameInternalRecordPath(s.root, candidate, record) {
				return false, fmt.Errorf("%w: install record path", ErrConflict)
			}
			if record.ArtifactDigest == digest {
				return true, nil
			}
		}
	}
	return false, nil
}

func sameInternalRecordPath(root, candidate string, record InstallRecord) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && filepath.ToSlash(relative) == canonicalRecordPath(record.PluginID, record.ManifestDigest, record.ArtifactDigest)
}

func (s *Store) validateStoredRecord(record InstallRecord) error {
	if err := validateInstallRecord(record, s.limits); err != nil {
		return err
	}
	manifestPath, err := s.internalPath(record.ManifestPath)
	if err != nil {
		return err
	}
	data, err := readStableRegularFile(manifestPath, s.limits.ManifestBytes, ErrNotFound)
	if err != nil {
		return fmt.Errorf("%w: stored manifest: %w", ErrConflict, err)
	}
	if digestBytes(data) != record.ManifestDigest {
		return fmt.Errorf("%w: stored manifest digest", ErrConflict)
	}
	manifest, err := parseManifest(data, s.limits)
	if err != nil {
		return fmt.Errorf("%w: stored manifest: %w", ErrConflict, err)
	}
	if manifest.ID != record.PluginID || manifest.Version != record.Version {
		return fmt.Errorf("%w: stored manifest identity", ErrConflict)
	}
	expectedProvenance := ProvenanceResult{Status: ProvenanceAbsent}
	if manifest.Provenance != nil {
		expectedProvenance = ProvenanceResult{
			Status:    ProvenanceDeclaredUnverified,
			Publisher: manifest.Provenance.Publisher,
			Source:    manifest.Provenance.Source,
			Signature: manifest.Provenance.Signature,
		}
	}
	if record.Provenance != expectedProvenance {
		return fmt.Errorf("%w: stored manifest provenance", ErrConflict)
	}
	target, err := manifest.selectTarget(record.Target.OS, record.Target.Arch, s.limits)
	if err != nil || target.SHA256 != record.ArtifactDigest {
		return fmt.Errorf("%w: stored manifest target", ErrConflict)
	}
	return nil
}

func (s *Store) commitManifest(data []byte, digest string) error {
	path, err := s.internalPath(canonicalManifestPath(digest))
	if err != nil {
		return err
	}
	if existing, readErr := readStableRegularFile(path, s.limits.ManifestBytes, ErrNotFound); readErr == nil {
		if digestBytes(existing) != digest || string(existing) != string(data) {
			return fmt.Errorf("%w: stored manifest content", ErrConflict)
		}
		return nil
	} else if !errors.Is(readErr, ErrNotFound) && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	return writeBytesAtomic(path, data, s.limits.ManifestBytes, "manifest")
}

func decodeStoredJSON(data []byte, target any) error {
	if err := rejectNonCanonicalJSON(data); err != nil {
		return err
	}
	if err := jsonx.Decode(data, target); err != nil {
		return fmt.Errorf("%w: decode stored record: %w", ErrInvalid, err)
	}
	return nil
}

func writeJSONAtomic(filePath string, value any, limit int64) error {
	return writeJSONAtomicNamed(filePath, value, limit, "record")
}

func writeJSONAtomicNamed(filePath string, value any, limit int64, operation string) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("%w: encode %s: %w", ErrInvalid, operation, err)
	}
	data = append(data, '\n')
	return writeBytesAtomic(filePath, data, limit, operation)
}

func writeBytesAtomic(filePath string, data []byte, limit int64, operation string) error {
	if limit < 1 || limit == int64(^uint64(0)>>1) {
		return fmt.Errorf("%w: invalid write limit", ErrInvalid)
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrLimitExceeded, operation, limit)
	}
	parent := filepath.Dir(filePath)
	if err := ensureDirectory(parent); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(parent, ".record-*")
	if err != nil {
		return fmt.Errorf("%w: create %s staging file: %w", ErrConflict, operation, err)
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if _, err := temporary.Write(data); err != nil {
		return fmt.Errorf("%w: write %s: %w", ErrConflict, operation, err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("%w: sync %s: %w", ErrConflict, operation, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("%w: close %s: %w", ErrConflict, operation, err)
	}
	if err := replaceFile(temporaryPath, filePath); err != nil {
		return fmt.Errorf("%w: publish %s: %w", ErrConflict, operation, err)
	}
	committed = true
	if err := syncDirectory(parent); err != nil {
		return &PublicationError{Operation: operation, Path: filePath, Committed: true, Err: err}
	}
	return nil
}

func ensureDirectory(directory string) error {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return fmt.Errorf("%w: resolve directory: %w", ErrUnsafeArtifact, err)
	}
	if err := rejectSymlinkComponents(filepath.Dir(absolute), absolute); err != nil {
		return err
	}
	if err := mkdirAllDurable(absolute, 0o700); err != nil {
		return err
	}
	if err := rejectSymlinkComponents(filepath.Dir(absolute), absolute); err != nil {
		return err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return fmt.Errorf("%w: inspect directory: %w", ErrUnsafeArtifact, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: directory is not a real directory", ErrUnsafeArtifact)
	}
	return nil
}

//nolint:gocyclo // Component checks keep lexical containment and filesystem identity together.
func rejectSymlinkComponents(directory, filePath string) error {
	base, err := filepath.Abs(directory)
	if err != nil {
		return fmt.Errorf("%w: resolve artifact directory: %w", ErrUnsafeArtifact, err)
	}
	target, err := filepath.Abs(filePath)
	if err != nil {
		return fmt.Errorf("%w: resolve artifact path: %w", ErrUnsafeArtifact, err)
	}
	for {
		baseInfo, statErr := os.Lstat(base)
		if statErr == nil {
			if !baseInfo.IsDir() || baseInfo.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%w: artifact directory is not a real directory", ErrUnsafeArtifact)
			}
			break
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("%w: inspect artifact directory: %w", ErrUnsafeArtifact, statErr)
		}
		parent := filepath.Dir(base)
		if parent == base {
			return fmt.Errorf("%w: artifact directory is unavailable", ErrUnsafeArtifact)
		}
		base = parent
	}
	relative, err := filepath.Rel(base, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: artifact escapes manifest directory", ErrUnsafeArtifact)
	}
	current := base
	for _, component := range append([]string{}, strings.Split(relative, string(filepath.Separator))...) {
		if component == "." || component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: inspect artifact component: %w", ErrUnsafeArtifact, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: artifact path contains a symlink", ErrUnsafeArtifact)
		}
	}
	return nil
}

//nolint:gocyclo // Content-addressed verification keeps identity and bounded streaming together.
func digestRegularFile(filePath string, limit int64) (string, error) {
	info, err := os.Lstat(filePath)
	if err != nil {
		return "", fmt.Errorf("%w: inspect artifact: %w", ErrUnsafeArtifact, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: artifact must be a regular non-symlink", ErrUnsafeArtifact)
	}
	file, err := os.Open(filePath) //nolint:gosec // The content-addressed path was checked before opening.
	if err != nil {
		return "", fmt.Errorf("%w: open artifact: %w", ErrUnsafeArtifact, err)
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return "", fmt.Errorf("%w: artifact changed while opening", ErrConflict)
	}
	hash := sha256.New()
	buffer := make([]byte, 128<<10)
	var total int64
	for {
		count, readErr := file.Read(buffer)
		if count > 0 {
			total += int64(count)
			if total > limit {
				return "", fmt.Errorf("%w: artifact exceeds %d bytes", ErrLimitExceeded, limit)
			}
			if _, err := hash.Write(buffer[:count]); err != nil {
				return "", err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", fmt.Errorf("%w: read artifact: %w", ErrUnsafeArtifact, readErr)
		}
	}
	if err := rejectSymlinkComponents(filepath.Dir(filePath), filePath); err != nil {
		return "", err
	}
	after, err := os.Lstat(filePath)
	if err != nil || !after.Mode().IsRegular() || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, after) {
		return "", fmt.Errorf("%w: artifact changed while reading", ErrConflict)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func removeRegularFile(filePath string) error {
	info, err := os.Lstat(filePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: inspect artifact for removal: %w", ErrConflict, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: refuse artifact removal", ErrUnsafeArtifact)
	}
	if err := os.Remove(filePath); err != nil {
		return fmt.Errorf("%w: remove artifact: %w", ErrConflict, err)
	}
	return nil
}

func syncDirectory(directory string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	file, err := os.Open(directory) //nolint:gosec // Directory was created/validated by the store.
	if err != nil {
		return fmt.Errorf("%w: open directory for sync: %w", ErrDurability, err)
	}
	defer func() { _ = file.Close() }()
	if err := file.Sync(); err != nil {
		return fmt.Errorf("%w: sync directory: %w", ErrDurability, err)
	}
	return nil
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
