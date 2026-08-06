//nolint:wsl_v5 // Lease acquisition/release is one ownership transaction.
package pluginstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ArtifactLease pins one content-addressed artifact for a future
// IntegrationGeneration or PluginProcess. The owner must hold the lease for
// the entire lifetime of every process/generation that can read the artifact.
// Release is idempotent; after the last lease is released, deferred garbage
// collection may remove an artifact whose install record was removed.
type ArtifactLease struct {
	store      *Store
	digest     string
	markerPath string
	released   bool
	mu         sync.Mutex
}

// AcquireArtifactLease verifies and pins an installed artifact. It does not
// execute or inspect the artifact as plugin code.
//
//nolint:gocyclo // Acquisition validates the artifact before creating its durable pin.
func (s *Store) AcquireArtifactLease(ctx context.Context, digest string) (*ArtifactLease, error) {
	if s == nil {
		return nil, fmt.Errorf("%w: nil store", ErrInvalid)
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	if !validDigest(digest) {
		return nil, fmt.Errorf("%w: artifact digest", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fileLock, err := s.acquireStoreLock(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fileLock.release() }()
	path, err := s.internalPath(canonicalArtifactPath(digest))
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("%w: inspect leased artifact: %w", ErrNotFound, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: leased artifact is not a regular file", ErrUnsafeArtifact)
	}
	actual, err := digestRegularFile(path, s.limits.ArtifactBytes)
	if err != nil {
		return nil, err
	}
	if actual != digest {
		return nil, fmt.Errorf("%w: leased artifact content", ErrConflict)
	}
	leaseDirectory, err := s.internalPath(filepath.ToSlash(filepath.Join(leasesDirectory, digest)))
	if err != nil {
		return nil, err
	}
	if err := ensureDirectory(leaseDirectory); err != nil {
		return nil, err
	}
	marker, err := os.CreateTemp(leaseDirectory, ".lease-*")
	if err != nil {
		return nil, fmt.Errorf("%w: create artifact lease: %w", ErrConflict, err)
	}
	markerPath := marker.Name()
	if err := marker.Close(); err != nil {
		_ = os.Remove(markerPath)
		return nil, fmt.Errorf("%w: close artifact lease: %w", ErrConflict, err)
	}
	if err := syncDirectory(leaseDirectory); err != nil {
		_ = os.Remove(markerPath)
		return nil, err
	}
	return &ArtifactLease{store: s, digest: digest, markerPath: markerPath}, nil
}

// Release drops the artifact pin. It is safe to call more than once.
//
//nolint:gocyclo // Release keeps marker removal, durable outcome mapping, and deferred GC atomic.
func (l *ArtifactLease) Release(ctx context.Context) error {
	if l == nil || l.store == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return nil
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	s := l.store
	s.mu.Lock()
	defer s.mu.Unlock()
	fileLock, err := s.acquireStoreLock(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = fileLock.release() }()
	if l.released {
		return nil
	}
	relativeMarkerPath, err := filepath.Rel(s.root, l.markerPath)
	if err != nil {
		return fmt.Errorf("%w: artifact lease path: %w", ErrUnsafeArtifact, err)
	}
	markerPath, err := s.internalPath(filepath.ToSlash(relativeMarkerPath))
	if err != nil {
		return err
	}
	if err := removeRegularFile(markerPath); err != nil {
		if !errors.Is(err, ErrNotFound) && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	// Once marker removal returns successfully, the durable lease marker is
	// gone. A later sync or deferred collection failure must not leave this
	// in-memory lease looking held: callers must inspect the PublicationError
	// outcome and store state before retrying.
	l.released = true
	if err := syncDirectory(filepath.Dir(markerPath)); err != nil {
		return &PublicationError{
			Operation: "lease-release",
			Path:      markerPath,
			Committed: true,
			Published: true,
			Err:       err,
		}
	}
	if err := s.collectArtifactIfUnreferenced(l.digest); err != nil {
		return &PublicationError{
			Operation: "lease-release-gc",
			Path:      filepath.Join(s.root, filepath.FromSlash(canonicalArtifactPath(l.digest))),
			Committed: true,
			Published: true,
			Err:       err,
		}
	}
	return nil
}

func (s *Store) artifactLeased(digest string) (bool, error) {
	leaseDirectory, err := s.internalPath(filepath.ToSlash(filepath.Join(leasesDirectory, digest)))
	if err != nil {
		return false, err
	}
	entries, err := os.ReadDir(leaseDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: scan artifact leases: %w", ErrConflict, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		candidate, pathErr := s.internalPath(filepath.ToSlash(filepath.Join(leasesDirectory, digest, entry.Name())))
		if pathErr != nil {
			return false, pathErr
		}
		info, statErr := os.Lstat(candidate)
		if statErr != nil {
			return false, fmt.Errorf("%w: inspect artifact lease: %w", ErrConflict, statErr)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return false, fmt.Errorf("%w: invalid artifact lease marker", ErrUnsafeArtifact)
		}
		return true, nil
	}
	return false, nil
}

func (s *Store) collectArtifactIfUnreferenced(digest string) error {
	leased, err := s.artifactLeased(digest)
	if err != nil {
		return err
	}
	if leased {
		return nil
	}
	remaining, err := s.artifactReferenced(digest)
	if err != nil {
		return err
	}
	if remaining {
		return nil
	}
	path, err := s.internalPath(canonicalArtifactPath(digest))
	if err != nil {
		return err
	}
	if err := removeRegularFile(path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}
