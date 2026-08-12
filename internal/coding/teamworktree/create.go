package teamworktree

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/rsbin1178/pips/internal/coding/execution/gitcontrol"
)

type parentSnapshot struct {
	repository gitcontrol.Repository
	index      fileSnapshot
}

type fileSnapshot struct {
	exists   bool
	identity FileIdentity
	digest   [sha256.Size]byte
	size     int64
}

// Create provisions one locked no-checkout Worktree from an exact commit.
// A non-zero Resource returned with an error is retained recovery evidence.
//
//nolint:gocyclo,funlen // Provisioning keeps every identity, CAS, rollback, and retained-state edge explicit.
func (m *Manager) Create(
	ctx context.Context,
	lease *Lease,
	request CreateRequest,
) (Resource, error) {
	if err := ctx.Err(); err != nil {
		return Resource{}, err
	}

	if err := validateOwner(request.Owner); err != nil {
		return Resource{}, err
	}

	if err := validateOID(request.BaseOID); err != nil {
		return Resource{}, err
	}

	if err := validateAbsolutePath(request.Workspace); err != nil {
		return Resource{}, err
	}

	if err := m.validateLease(lease, request.Owner); err != nil {
		return Resource{}, err
	}

	if err := m.validateRoots(); err != nil {
		return Resource{}, err
	}

	workspace, err := canonicalDirectory(request.Workspace)
	if err != nil {
		return Resource{}, err
	}

	repository, err := m.git.InspectRepository(ctx, workspace.Path)
	if err != nil {
		return Resource{}, mapGitError(err)
	}

	if repository.TopLevel != workspace.Path {
		return Resource{}, fmt.Errorf("%w: Workspace is not repository root", ErrIdentity)
	}

	commonDir, err := canonicalDirectory(repository.CommonDir)
	if err != nil {
		return Resource{}, err
	}

	if pathWithin(m.worktreesRoot.Path, workspace.Path) || pathWithin(workspace.Path, m.worktreesRoot.Path) ||
		pathWithin(m.worktreesRoot.Path, commonDir.Path) || pathWithin(commonDir.Path, m.worktreesRoot.Path) ||
		pathWithin(m.worktreesRoot.Path, m.productRoot) {
		return Resource{}, fmt.Errorf("%w: Worktree root overlaps repository or product", ErrInvalid)
	}

	if err := m.git.ValidateSafeConfig(ctx, workspace.Path); err != nil {
		return Resource{}, mapGitError(err)
	}

	if _, err := m.git.ResolveCommit(ctx, workspace.Path, request.BaseOID); err != nil {
		return Resource{}, mapGitError(err)
	}

	derived := m.derive(request.Owner, workspace)
	if pathWithin(derived.directory, workspace.Path) || pathWithin(derived.directory, commonDir.Path) ||
		pathWithin(derived.directory, m.productRoot) {
		return Resource{}, fmt.Errorf("%w: derived Worktree overlaps protected root", ErrInvalid)
	}

	if err := m.ensureTargetAbsent(derived.directory); err != nil {
		return Resource{}, err
	}

	if _, err := m.git.ResolveRef(ctx, workspace.Path, derived.branchRef); !errors.Is(err, gitcontrol.ErrNotFound) {
		if err == nil {
			return Resource{}, fmt.Errorf("%w: owned branch already exists", ErrConflict)
		}

		return Resource{}, mapGitError(err)
	}

	if _, err := m.git.ResolveRef(ctx, workspace.Path, derived.resultRef); !errors.Is(err, gitcontrol.ErrNotFound) {
		if err == nil {
			return Resource{}, fmt.Errorf("%w: owned result ref already exists", ErrConflict)
		}

		return Resource{}, mapGitError(err)
	}

	parent, err := m.snapshotParent(repository)
	if err != nil {
		return Resource{}, err
	}

	if err := m.ensureTargetParent(derived.directory); err != nil {
		return Resource{}, err
	}

	partial := Resource{
		ID: derived.id, Owner: request.Owner, Workspace: workspace,
		Directory: FileIdentity{Path: derived.directory}, CommonDir: commonDir,
		ObjectFormat: repository.ObjectFormat, BranchRef: derived.branchRef,
		ResultRef: derived.resultRef, BaseOID: request.BaseOID, LockReason: derived.lockReason,
	}
	if err := m.git.CreateRef(
		ctx, workspace.Path, repository.ObjectFormat,
		derived.branchRef, request.BaseOID, refReason,
	); err != nil {
		return Resource{}, fmt.Errorf("coding team worktree: create owned branch: %w", mapGitError(err))
	}

	if err := m.git.AddWorktree(
		ctx, workspace.Path, derived.directory, derived.branchRef, derived.lockReason,
	); err != nil {
		deleteErr := m.git.DeleteRef(ctx, workspace.Path, derived.branchRef, request.BaseOID, refReason)

		cause := fmt.Errorf("coding team worktree: add linked Worktree: %w", mapGitError(err))
		if deleteErr != nil {
			cause = errors.Join(cause, mapGitError(deleteErr))

			return partial, &RetainedError{Resource: partial, Cause: cause}
		}

		return Resource{}, cause
	}

	resource, err := m.bindCreatedResource(ctx, partial)
	if err != nil {
		return partial, &RetainedError{Resource: partial, Cause: err}
	}

	if err := m.materializeBase(ctx, resource); err != nil {
		return resource, &RetainedError{Resource: resource, Cause: err}
	}

	if _, err := m.verifyResource(ctx, lease, resource); err != nil {
		return resource, &RetainedError{Resource: resource, Cause: err}
	}

	if err := m.verifyParent(ctx, parent); err != nil {
		return resource, &RetainedError{Resource: resource, Cause: err}
	}

	return resource, nil
}

func (m *Manager) bindCreatedResource(
	ctx context.Context,
	resource Resource,
) (Resource, error) {
	directory, err := canonicalDirectory(resource.Directory.Path)
	if err != nil {
		return resource, err
	}

	resource.Directory = directory

	repository, err := m.git.InspectRepository(ctx, directory.Path)
	if err != nil {
		return resource, mapGitError(err)
	}

	gitDir, err := canonicalDirectory(repository.GitDir)
	if err != nil {
		return resource, err
	}

	resource.GitDir = gitDir

	actualCommon, err := canonicalDirectory(repository.CommonDir)
	if err != nil {
		return resource, err
	}

	if actualCommon != resource.CommonDir || repository.TopLevel != directory.Path ||
		repository.ObjectFormat != resource.ObjectFormat || repository.HeadOID != resource.BaseOID ||
		repository.BranchRef != resource.BranchRef {
		return resource, ErrIdentity
	}

	return resource, nil
}

//nolint:gocyclo // Raw materialization audits each supported Git entry and failure edge in order.
func (m *Manager) materializeBase(ctx context.Context, resource Resource) error {
	entries, err := m.git.ListTree(ctx, resource.Workspace.Path, resource.BaseOID)
	if err != nil {
		return mapGitError(err)
	}

	if len(entries) > m.limits.Files {
		return ErrLimit
	}

	indexPath := filepath.Join(resource.GitDir.Path, "index")
	if err := m.git.ReadTree(ctx, resource.Directory.Path, indexPath, resource.BaseOID); err != nil {
		return mapGitError(err)
	}

	root, err := os.OpenRoot(resource.Directory.Path)
	if err != nil {
		return fmt.Errorf("%w: open Worktree root: %w", ErrIdentity, err)
	}
	defer func() { _ = root.Close() }()

	var total int64

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}

		if len(entry.Path) > m.limits.PathBytes || entry.Type != "blob" ||
			entry.Mode != "100644" && entry.Mode != "100755" && entry.Mode != "120000" {
			return fmt.Errorf("%w: unsupported base tree entry", ErrUnsafeRepository)
		}

		content, err := m.git.CatBlob(ctx, resource.Workspace.Path, entry.OID)
		if err != nil {
			return mapGitError(err)
		}

		if int64(len(content)) > m.limits.FileBytes || total > m.limits.Bytes-int64(len(content)) {
			return ErrLimit
		}

		total += int64(len(content))

		parent := filepath.Dir(entry.Path)
		if parent != "." {
			if err := root.MkdirAll(parent, privateDirectoryMode); err != nil {
				return fmt.Errorf("%w: create Worktree directory: %w", ErrIdentity, err)
			}
		}

		if entry.Mode == "120000" {
			if bytesContainNUL(content) {
				return fmt.Errorf("%w: symlink target contains NUL", ErrUnsafeRepository)
			}

			if err := root.Symlink(string(content), entry.Path); err != nil {
				return fmt.Errorf("%w: create Worktree symlink: %w", ErrIdentity, err)
			}

			continue
		}

		mode := fs.FileMode(0o644)
		if entry.Mode == "100755" {
			mode = 0o755
		}

		file, err := root.OpenFile(entry.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return fmt.Errorf("%w: create Worktree file: %w", ErrIdentity, err)
		}

		_, writeErr := file.Write(content)

		closeErr := errors.Join(file.Sync(), file.Close())
		if writeErr != nil || closeErr != nil {
			return fmt.Errorf("%w: materialize Worktree file: %w", ErrIdentity, errors.Join(writeErr, closeErr))
		}
	}

	return nil
}

func (m *Manager) ensureTargetAbsent(target string) error {
	relative, err := filepath.Rel(m.worktreesRoot.Path, target)
	if err != nil || relative == "." || strings.HasPrefix(relative, "..") {
		return fmt.Errorf("%w: target outside Worktree root", ErrInvalid)
	}

	root, err := os.OpenRoot(m.worktreesRoot.Path)
	if err != nil {
		return fmt.Errorf("%w: open Worktree root: %w", ErrIdentity, err)
	}
	defer func() { _ = root.Close() }()

	_, err = root.Lstat(relative)
	if err == nil {
		return fmt.Errorf("%w: target already exists", ErrConflict)
	}

	if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: inspect target: %w", ErrIdentity, err)
	}

	return nil
}

func (m *Manager) ensureTargetParent(target string) error {
	relative, err := filepath.Rel(m.worktreesRoot.Path, filepath.Dir(target))
	if err != nil || relative == "." || strings.HasPrefix(relative, "..") {
		return fmt.Errorf("%w: target parent outside Worktree root", ErrInvalid)
	}

	root, err := os.OpenRoot(m.worktreesRoot.Path)
	if err != nil {
		return fmt.Errorf("%w: open Worktree root: %w", ErrIdentity, err)
	}
	defer func() { _ = root.Close() }()

	if err := root.MkdirAll(relative, privateDirectoryMode); err != nil {
		return fmt.Errorf("%w: create target parent: %w", ErrIdentity, err)
	}

	return nil
}

func (m *Manager) snapshotParent(
	repository gitcontrol.Repository,
) (parentSnapshot, error) {
	index, err := snapshotFile(filepath.Join(repository.GitDir, "index"), m.limits.GitBytes)
	if err != nil {
		return parentSnapshot{}, err
	}

	return parentSnapshot{repository: repository, index: index}, nil
}

func (m *Manager) verifyParent(ctx context.Context, expected parentSnapshot) error {
	actualRepository, err := m.git.InspectRepository(ctx, expected.repository.TopLevel)
	if err != nil {
		return mapGitError(err)
	}

	if actualRepository != expected.repository {
		return ErrIdentity
	}

	actualIndex, err := snapshotFile(filepath.Join(expected.repository.GitDir, "index"), m.limits.GitBytes)
	if err != nil {
		return err
	}

	if actualIndex != expected.index {
		return fmt.Errorf("%w: parent index changed", ErrConflict)
	}

	return nil
}

func snapshotFile(path string, limit int64) (fileSnapshot, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fileSnapshot{}, nil
	}

	if err != nil {
		return fileSnapshot{}, fmt.Errorf("%w: stat index: %w", ErrIdentity, err)
	}

	if !info.Mode().IsRegular() || info.Size() > limit {
		return fileSnapshot{}, fmt.Errorf("%w: unsafe parent index", ErrLimit)
	}

	identity, err := fileIdentity(path)
	if err != nil {
		return fileSnapshot{}, err
	}

	file, err := os.Open(path) //nolint:gosec // The path is exact Git metadata validated by repository inspection.
	if err != nil {
		return fileSnapshot{}, fmt.Errorf("%w: open parent index: %w", ErrIdentity, err)
	}

	hash := sha256.New()
	_, copyErr := io.Copy(hash, io.LimitReader(file, limit+1))

	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return fileSnapshot{}, fmt.Errorf("%w: read parent index: %w", ErrIdentity, errors.Join(copyErr, closeErr))
	}

	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))

	return fileSnapshot{exists: true, identity: identity, digest: digest, size: info.Size()}, nil
}

func bytesContainNUL(value []byte) bool {
	return slices.Contains(value, byte(0))
}
