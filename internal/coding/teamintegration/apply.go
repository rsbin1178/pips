//nolint:wsl_v5 // WAL mutation and rollback steps stay adjacent for crash-boundary auditing.
package teamintegration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/rsbin/pips/internal/coding/workspace"
)

// ApplyResult reports the terminal parent transaction without exposing private paths.
type ApplyResult struct {
	ID             string
	State          string
	ManifestDigest string
	Files          int
}

// PathState classifies one recovery path against its journal evidence.
type PathState string

// Supported recovery path classifications.
const (
	PathBase    PathState = "base"
	PathTarget  PathState = "target"
	PathUnknown PathState = "unknown"
)

type mutationBoundary string

const (
	boundaryBeforeDelete        mutationBoundary = "before_delete"
	boundaryAfterDelete         mutationBoundary = "after_delete"
	boundaryBeforeParentCreate  mutationBoundary = "before_parent_create"
	boundaryAfterParentCreate   mutationBoundary = "after_parent_create"
	boundaryBeforeTemporary     mutationBoundary = "before_temporary"
	boundaryAfterTemporary      mutationBoundary = "after_temporary"
	boundaryBeforeReplace       mutationBoundary = "before_replace"
	boundaryAfterReplace        mutationBoundary = "after_replace"
	boundaryBeforeDirectorySync mutationBoundary = "before_directory_sync"
	boundaryAfterDirectorySync  mutationBoundary = "after_directory_sync"
)

// RecoveryCandidate is a bounded read-only interrupted transaction projection.
type RecoveryCandidate struct {
	ID          string
	State       string
	Base        int
	Target      int
	Unknown     int
	Unrelated   int
	JournalHash string
}

// RecoveryAction selects an explicit convergence direction.
type RecoveryAction string

// Supported explicit recovery actions.
const (
	RecoveryComplete RecoveryAction = "complete"
	RecoveryRollback RecoveryAction = "rollback"
)

// Apply consumes one approval before beginning the journaled parent mutation.
//
//nolint:gocyclo // Token, preflight, WAL, apply, verification, and guarded rollback are one transaction.
func (m *Manager) Apply(ctx context.Context, id, token string) (ApplyResult, error) {
	if err := m.validate(); err != nil {
		return ApplyResult{}, err
	}
	if !validIntegrationID(id) || token == "" {
		return ApplyResult{}, fmt.Errorf("%w: apply request", ErrInvalid)
	}
	if !m.tokens.Available(token) {
		return ApplyResult{}, ErrConsumed
	}
	m.mu.Lock()
	record, exists := m.records[id]
	m.mu.Unlock()
	if !exists {
		return ApplyResult{}, ErrStale
	}

	binding, err := m.rebuildBinding(ctx, record)
	if err != nil {
		_ = m.tokens.Reject(token)

		return ApplyResult{}, err
	}
	if err := m.tokens.Consume(token, binding); err != nil {
		return ApplyResult{}, err
	}

	root, err := os.OpenRoot(record.workspace.Root())
	if err != nil {
		return ApplyResult{}, err
	}
	defer func() { _ = root.Close() }()
	if err := m.preflight(root, record.preview.Manifest, false); err != nil {
		return ApplyResult{}, err
	}

	now := m.now().UTC()
	journal := applyJournal{
		Schema: journalSchema, ID: id, WorkspacePath: record.workspace.Root(),
		WorkspaceIdentity: binding.WorkspaceIdentity, CommonIdentity: binding.CommonIdentity,
		BranchRef: binding.BranchRef, HeadOID: binding.HeadOID, IndexDigest: binding.IndexDigest,
		IntegrationCommit: binding.IntegrationCommit, IntegrationTree: binding.IntegrationTree,
		Manifest: record.preview.Manifest, State: journalApplying,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := m.writeJournal(journal); err != nil {
		return ApplyResult{}, err
	}

	for _, entry := range journal.Manifest.Entries {
		if err := ctx.Err(); err != nil {
			return m.rollbackApply(context.WithoutCancel(ctx), root, journal, err)
		}
		actual, inspectErr := inspectFileState(root, entry.Path, m.limits.BlobBytes)
		if inspectErr != nil || !sameFileState(actual, entry.Base) {
			if inspectErr == nil {
				inspectErr = fmt.Errorf("%w: path changed after preflight", ErrStale)
			}

			return m.rollbackApply(context.WithoutCancel(ctx), root, journal, inspectErr)
		}
		if entry.Base.Kind == FileRegular {
			links, linkErr := fileLinkCount(root, entry.Path)
			if linkErr != nil || links != 1 {
				return m.rollbackApply(
					context.WithoutCancel(ctx), root, journal,
					errors.Join(linkErr, fmt.Errorf("%w: hard link changed after preflight", ErrStale)),
				)
			}
		}
		if entry.Base.Kind == FileAbsent {
			if collisionErr := checkCaseCollision(root, entry.Path, m.limits.Entries); collisionErr != nil {
				return m.rollbackApply(context.WithoutCancel(ctx), root, journal, collisionErr)
			}
		}
		if err := m.writeFileState(ctx, root, record.workspace.Root(), entry.Path, entry.Target); err != nil {
			return m.rollbackApply(context.WithoutCancel(ctx), root, journal, err)
		}
		journal.Completed = append(journal.Completed, entry.Path)
		journal.UpdatedAt = m.now().UTC()
		if err := m.writeJournal(journal); err != nil {
			return m.rollbackApply(context.WithoutCancel(ctx), root, journal, err)
		}
	}

	if err := m.preflight(root, journal.Manifest, true); err != nil {
		return m.rollbackApply(context.WithoutCancel(ctx), root, journal, err)
	}
	if err := m.verifyParentStructure(ctx, record, binding, journal.Manifest); err != nil {
		return m.rollbackApply(context.WithoutCancel(ctx), root, journal, err)
	}
	journal.State = journalApplied
	journal.UpdatedAt = m.now().UTC()
	if err := m.writeJournal(journal); err != nil {
		return ApplyResult{}, &RetainedError{IntegrationID: id, Cause: err}
	}

	return ApplyResult{
		ID: id, State: string(journalApplied), ManifestDigest: journal.Manifest.Digest,
		Files: len(journal.Manifest.Entries),
	}, nil
}

// Reject consumes a preview approval without modifying the parent.
func (m *Manager) Reject(id, token string) error {
	if err := m.validate(); err != nil {
		return err
	}
	if !validIntegrationID(id) || token == "" {
		return fmt.Errorf("%w: reject request", ErrInvalid)
	}
	m.mu.Lock()
	record, exists := m.records[id]
	m.mu.Unlock()
	if !exists || record.preview.ApprovalToken != token {
		return ErrStale
	}

	return m.tokens.Reject(token)
}

func (m *Manager) rebuildBinding(ctx context.Context, record preparedRecord) (ApprovalBinding, error) {
	parent, err := m.snapshotParent(ctx, record.workspace.Root())
	if err != nil {
		return ApprovalBinding{}, err
	}
	if !parent.status.Clean {
		return ApprovalBinding{}, fmt.Errorf("%w: parent is dirty", ErrStale)
	}
	selection, err := m.loadSelection(ctx, record.workspace.Root(), record.selection)
	if err != nil {
		return ApprovalBinding{}, err
	}
	composition, err := Compose(ctx, selection, m.limits)
	if err != nil || composition.Digest != record.composition.Digest {
		return ApprovalBinding{}, fmt.Errorf("%w: selection changed", ErrStale)
	}
	manifest, err := BuildManifest(
		ctx, gitBlobReader{runner: m.git, directory: record.workspace.Root()},
		selection.Base, composition.Entries, m.limits,
	)
	if err != nil || manifest.Digest != record.preview.Manifest.Digest {
		return ApprovalBinding{}, fmt.Errorf("%w: manifest changed", ErrStale)
	}
	derived := derivedIntegration{
		integrationRef: record.integrationRef, branchRef: record.branchRef,
		directory: record.worktree.Root(),
	}
	if err := m.verifyIntegration(
		ctx, derived, record.preview.CommitOID, record.preview.TreeOID,
	); err != nil {
		return ApprovalBinding{}, err
	}
	selectionDigest, err := digestValue(selection)
	if err != nil {
		return ApprovalBinding{}, err
	}

	return ApprovalBinding{
		IntegrationID:     record.preview.ID,
		WorkspaceIdentity: parent.workspaceIdentity, CommonIdentity: parent.commonIdentity,
		BranchRef: parent.repository.BranchRef, HeadOID: parent.repository.HeadOID,
		StatusDigest: parent.status.Digest, IndexDigest: parent.indexDigest,
		ResourceRevision: selection.ResourceRevision, SelectionDigest: selectionDigest,
		IntegrationCommit: record.preview.CommitOID, IntegrationTree: record.preview.TreeOID,
		ManifestDigest: manifest.Digest, DiffDigest: record.preview.DiffDigest,
		Verification: record.preview.Verification, ExpiresAt: record.preview.ExpiresAt,
	}, nil
}

func (m *Manager) preflight(root *os.Root, manifest Manifest, target bool) error {
	casePaths := make(map[string]string, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		normalized := strings.ToLower(entry.Path)
		if previous, exists := casePaths[normalized]; exists && previous != entry.Path {
			return fmt.Errorf("%w: case collision", ErrConflict)
		}
		casePaths[normalized] = entry.Path
		if err := validateNoSymlinkParents(root, entry.Path); err != nil {
			return err
		}
		expected := entry.Base
		if target {
			expected = entry.Target
		}
		actual, err := inspectFileState(root, entry.Path, m.limits.BlobBytes)
		if err != nil {
			return err
		}
		if !sameFileState(actual, expected) {
			return fmt.Errorf("%w: path %q does not match %s", ErrStale, entry.Path, expected.Kind)
		}
		if expected.Kind == FileRegular {
			links, err := fileLinkCount(root, entry.Path)
			if err != nil || links != 1 {
				return fmt.Errorf("%w: hard-linked file %q", ErrConflict, entry.Path)
			}
		}
		if expected.Kind == FileAbsent {
			if err := checkCaseCollision(root, entry.Path, m.limits.Entries); err != nil {
				return err
			}
		}
	}

	return nil
}

//nolint:gocyclo,nestif // Typed delete/symlink/file replacement keeps each crash boundary explicit.
func (m *Manager) writeFileState(
	ctx context.Context,
	root *os.Root,
	objectDirectory, path string,
	target FileState,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if target.Kind == FileAbsent {
		if err := m.failMutation(boundaryBeforeDelete, path); err != nil {
			return err
		}
		if err := root.Remove(path); err != nil {
			return err
		}
		if err := m.failMutation(boundaryAfterDelete, path); err != nil {
			return err
		}

		return m.syncMutationDirectory(path, filepath.Join(root.Name(), filepath.Dir(path)))
	}
	if err := validateNoSymlinkParents(root, path); err != nil {
		return err
	}
	parent := filepath.Dir(path)
	if parent != "." {
		if err := m.failMutation(boundaryBeforeParentCreate, path); err != nil {
			return err
		}
		if err := root.MkdirAll(parent, 0o755); err != nil {
			return err
		}
		if err := m.failMutation(boundaryAfterParentCreate, path); err != nil {
			return err
		}
	}
	content, err := m.readBlob(ctx, objectDirectory, target.OID)
	if err != nil {
		return err
	}
	if int64(len(content)) != target.Size || digestBytes(content) != target.SHA256 {
		return fmt.Errorf("%w: target blob changed", ErrStale)
	}
	temporary, err := temporaryLeaf(path)
	if err != nil {
		return err
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = root.Remove(temporary)
		}
	}()

	if err := m.failMutation(boundaryBeforeTemporary, path); err != nil {
		return err
	}
	if target.Kind == FileSymlink {
		if bytes.IndexByte(content, 0) >= 0 {
			return fmt.Errorf("%w: symlink target", ErrInvalid)
		}
		if err := root.Symlink(string(content), temporary); err != nil {
			return err
		}
	} else {
		mode := fs.FileMode(0o644)
		if target.Mode == "100755" {
			mode = 0o755
		}
		file, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, writeErr := file.Write(content)
		chmodErr := file.Chmod(mode)
		closeErr := errors.Join(file.Sync(), file.Close())
		if writeErr != nil || chmodErr != nil || closeErr != nil {
			return errors.Join(writeErr, chmodErr, closeErr)
		}
	}
	if err := m.failMutation(boundaryAfterTemporary, path); err != nil {
		return err
	}
	if err := m.failMutation(boundaryBeforeReplace, path); err != nil {
		return err
	}
	if err := root.Rename(temporary, path); err != nil {
		return err
	}
	removeTemporary = false
	if err := m.failMutation(boundaryAfterReplace, path); err != nil {
		return err
	}

	return m.syncMutationDirectory(path, filepath.Join(root.Name(), parent))
}

func (m *Manager) syncMutationDirectory(path, directory string) error {
	if err := m.failMutation(boundaryBeforeDirectorySync, path); err != nil {
		return err
	}
	if err := syncDirectory(directory); err != nil {
		return err
	}

	return m.failMutation(boundaryAfterDirectorySync, path)
}

func (m *Manager) failMutation(boundary mutationBoundary, path string) error {
	if m == nil || m.mutationFault == nil {
		return nil
	}

	return m.mutationFault(boundary, path)
}

func (m *Manager) rollbackApply(
	ctx context.Context,
	root *os.Root,
	journal applyJournal,
	cause error,
) (ApplyResult, error) {
	entries := append([]ManifestEntry(nil), journal.Manifest.Entries...)
	slices.Reverse(entries)
	for _, entry := range entries {
		actual, err := inspectFileState(root, entry.Path, m.limits.BlobBytes)
		if err != nil {
			journal.State = journalInterrupted
			journal.UpdatedAt = m.now().UTC()
			_ = m.writeJournal(journal)

			return ApplyResult{}, &RetainedError{IntegrationID: journal.ID, Cause: errors.Join(cause, err, ErrRecovery)}
		}
		if sameFileState(actual, entry.Base) {
			continue
		}
		if !sameFileState(actual, entry.Target) {
			journal.State = journalInterrupted
			journal.UpdatedAt = m.now().UTC()
			_ = m.writeJournal(journal)

			return ApplyResult{}, &RetainedError{IntegrationID: journal.ID, Cause: errors.Join(cause, ErrRecovery)}
		}
		if err := m.writeFileState(ctx, root, journal.WorkspacePath, entry.Path, entry.Base); err != nil {
			journal.State = journalInterrupted
			journal.UpdatedAt = m.now().UTC()
			_ = m.writeJournal(journal)

			return ApplyResult{}, &RetainedError{IntegrationID: journal.ID, Cause: errors.Join(cause, err)}
		}
	}
	journal.State = journalRolledBack
	journal.UpdatedAt = m.now().UTC()
	if err := m.writeJournal(journal); err != nil {
		return ApplyResult{}, &RetainedError{IntegrationID: journal.ID, Cause: errors.Join(cause, err)}
	}

	return ApplyResult{}, &RolledBackError{IntegrationID: journal.ID, Cause: cause}
}

//nolint:gocyclo // Filesystem types and bounded content evidence are validated independently.
func inspectFileState(root *os.Root, path string, limit int64) (FileState, error) {
	info, err := root.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return FileState{Kind: FileAbsent}, nil
	}
	if err != nil {
		return FileState{}, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		target, err := root.Readlink(path)
		if err != nil {
			return FileState{}, err
		}
		after, err := root.Lstat(path)
		if err != nil || !os.SameFile(info, after) {
			return FileState{}, fmt.Errorf("%w: symlink changed during inspection", ErrStale)
		}
		content := []byte(target)
		if int64(len(content)) > limit {
			return FileState{}, ErrLimit
		}

		return FileState{
			Kind: FileSymlink, Mode: "120000", Size: int64(len(content)),
			SHA256: digestBytes(content), Binary: bytes.IndexByte(content, 0) >= 0,
		}, nil
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return FileState{}, fmt.Errorf("%w: unsupported path %q", ErrInvalid, path)
	}
	file, err := root.Open(path)
	if err != nil {
		return FileState{}, err
	}
	openedInfo, statErr := file.Stat()
	if statErr != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		_ = file.Close()

		return FileState{}, fmt.Errorf("%w: file changed during inspection", ErrStale)
	}
	content, readErr := io.ReadAll(io.LimitReader(file, limit+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return FileState{}, errors.Join(readErr, closeErr)
	}
	if int64(len(content)) > limit {
		return FileState{}, ErrLimit
	}
	mode := "100644"
	if info.Mode().Perm()&0o111 != 0 {
		mode = "100755"
	}

	return FileState{
		Kind: FileRegular, Mode: mode, Size: int64(len(content)), SHA256: digestBytes(content),
		Binary: bytes.IndexByte(content, 0) >= 0,
	}, nil
}

func sameFileState(actual, expected FileState) bool {
	if actual.Kind != expected.Kind {
		return false
	}
	if actual.Kind == FileAbsent {
		return true
	}

	return actual.Mode == expected.Mode && actual.Size == expected.Size &&
		actual.SHA256 == expected.SHA256 && actual.Binary == expected.Binary
}

func validateNoSymlinkParents(root *os.Root, path string) error {
	parent := filepath.Dir(path)
	if parent == "." {
		return nil
	}
	current := ""
	for component := range strings.SplitSeq(filepath.ToSlash(parent), "/") {
		if current == "" {
			current = component
		} else {
			current = filepath.Join(current, component)
		}
		info, err := root.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: unsafe parent %q", ErrConflict, current)
		}
	}

	return nil
}

func checkCaseCollision(root *os.Root, path string, limit int) error {
	parent := filepath.Dir(path)
	directory, err := root.Open(parent)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	entries, readErr := directory.ReadDir(limit + 1)
	closeErr := directory.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	if len(entries) > limit {
		return ErrLimit
	}
	leaf := filepath.Base(path)
	for _, entry := range entries {
		if entry.Name() != leaf && strings.EqualFold(entry.Name(), leaf) {
			return fmt.Errorf("%w: case collision at %q", ErrConflict, path)
		}
	}

	return nil
}

func temporaryLeaf(path string) (string, error) {
	value := make([]byte, 12)
	if _, err := randRead(value); err != nil {
		return "", err
	}

	return filepath.Join(filepath.Dir(path), ".pips-integration-"+hex.EncodeToString(value)), nil
}

var randRead = cryptographicRead

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)

	return hex.EncodeToString(sum[:])
}

func (m *Manager) verifyParentStructure(
	ctx context.Context,
	record preparedRecord,
	binding ApprovalBinding,
	manifest Manifest,
) error {
	parent, err := m.snapshotParent(ctx, record.workspace.Root())
	if err != nil {
		return err
	}
	if parent.workspaceIdentity != binding.WorkspaceIdentity ||
		parent.commonIdentity != binding.CommonIdentity || parent.repository.BranchRef != binding.BranchRef ||
		parent.repository.HeadOID != binding.HeadOID || parent.indexDigest != binding.IndexDigest {
		return fmt.Errorf("%w: parent structure changed", ErrStale)
	}
	if !slices.Equal(parent.status.Paths, SortedManifestPaths(manifest)) {
		return fmt.Errorf("%w: parent status includes unrelated changes", ErrStale)
	}

	return nil
}

// Discover returns non-terminal journals without mutating their state.
func (m *Manager) Discover(ctx context.Context) ([]RecoveryCandidate, error) {
	if err := m.validate(); err != nil {
		return nil, err
	}
	journals, err := m.listJournals()
	if err != nil {
		return nil, err
	}
	result := make([]RecoveryCandidate, 0)
	for _, journal := range journals {
		if journal.State == journalApplied || journal.State == journalRolledBack {
			continue
		}
		candidate, err := m.classifyJournal(ctx, journal)
		if err != nil {
			return nil, err
		}
		result = append(result, candidate)
	}

	return result, nil
}

func (m *Manager) classifyJournal(
	ctx context.Context,
	journal applyJournal,
) (RecoveryCandidate, error) {
	if err := ctx.Err(); err != nil {
		return RecoveryCandidate{}, err
	}
	worktree, err := workspace.Open(journal.WorkspacePath)
	if err != nil || worktree.Identity().Key() != journal.WorkspaceIdentity {
		return RecoveryCandidate{}, fmt.Errorf("%w: recovery Workspace identity", ErrRecovery)
	}
	root, err := os.OpenRoot(worktree.Root())
	if err != nil {
		return RecoveryCandidate{}, err
	}
	defer func() { _ = root.Close() }()
	candidate := RecoveryCandidate{ID: journal.ID, State: string(journal.State)}
	candidate.Base, candidate.Target, candidate.Unknown, err = classifyManifestPaths(
		root, journal.Manifest, m.limits.BlobBytes,
	)
	if err != nil {
		return RecoveryCandidate{}, err
	}
	status, err := m.git.SnapshotStatus(ctx, worktree.Root())
	if err != nil {
		return RecoveryCandidate{}, err
	}
	candidate.Unrelated = countUnrelatedStatusPaths(status.Paths, journal.Manifest)
	candidate.JournalHash, err = journalDigest(journal)

	return candidate, err
}

func classifyManifestPaths(
	root *os.Root,
	manifest Manifest,
	blobLimit int64,
) (base, target, unknown int, err error) {
	for _, entry := range manifest.Entries {
		state, inspectErr := inspectFileState(root, entry.Path, blobLimit)
		if inspectErr != nil {
			return 0, 0, 0, inspectErr
		}
		switch {
		case sameFileState(state, entry.Base):
			base++
		case sameFileState(state, entry.Target):
			target++
		default:
			unknown++
		}
	}

	return base, target, unknown, nil
}

// Recover explicitly completes or rolls back one interrupted journal.
//
//nolint:gocyclo // Recovery intentionally reclassifies every path before each mutation.
func (m *Manager) Recover(
	ctx context.Context,
	id string,
	action RecoveryAction,
) (ApplyResult, error) {
	if action != RecoveryComplete && action != RecoveryRollback {
		return ApplyResult{}, fmt.Errorf("%w: recovery action", ErrInvalid)
	}
	journal, err := m.readJournal(id)
	if err != nil {
		return ApplyResult{}, err
	}
	if journal.State == journalApplied || journal.State == journalRolledBack {
		return ApplyResult{}, fmt.Errorf("%w: terminal journal", ErrInvalid)
	}
	candidate, err := m.classifyJournal(ctx, journal)
	if err != nil || candidate.Unknown != 0 || candidate.Unrelated != 0 {
		return ApplyResult{}, errors.Join(ErrRecovery, err)
	}
	parent, err := m.snapshotParent(ctx, journal.WorkspacePath)
	if err != nil {
		return ApplyResult{}, err
	}
	if parent.workspaceIdentity != journal.WorkspaceIdentity ||
		parent.commonIdentity != journal.CommonIdentity || parent.repository.BranchRef != journal.BranchRef ||
		parent.repository.HeadOID != journal.HeadOID || parent.indexDigest != journal.IndexDigest {
		return ApplyResult{}, ErrRecovery
	}
	root, err := os.OpenRoot(journal.WorkspacePath)
	if err != nil {
		return ApplyResult{}, err
	}
	defer func() { _ = root.Close() }()
	if err := m.convergeManifest(
		ctx, root, journal.WorkspacePath, journal.Manifest, action,
	); err != nil {
		journal.State = journalInterrupted
		journal.UpdatedAt = m.now().UTC()
		_ = m.writeJournal(journal)

		return ApplyResult{}, &RetainedError{IntegrationID: id, Cause: err}
	}
	target := action == RecoveryComplete
	if err := m.preflight(root, journal.Manifest, target); err != nil {
		return ApplyResult{}, &RetainedError{IntegrationID: id, Cause: err}
	}
	parent, err = m.snapshotParent(ctx, journal.WorkspacePath)
	if err != nil {
		return ApplyResult{}, &RetainedError{IntegrationID: id, Cause: err}
	}
	expectedStatus := []string(nil)
	if action == RecoveryComplete {
		expectedStatus = SortedManifestPaths(journal.Manifest)
	}
	if parent.workspaceIdentity != journal.WorkspaceIdentity ||
		parent.commonIdentity != journal.CommonIdentity || parent.repository.BranchRef != journal.BranchRef ||
		parent.repository.HeadOID != journal.HeadOID || parent.indexDigest != journal.IndexDigest ||
		!slices.Equal(parent.status.Paths, expectedStatus) {
		return ApplyResult{}, &RetainedError{IntegrationID: id, Cause: ErrRecovery}
	}
	journal.State = journalApplied
	if action == RecoveryRollback {
		journal.State = journalRolledBack
	}
	journal.UpdatedAt = m.now().UTC()
	if err := m.writeJournal(journal); err != nil {
		return ApplyResult{}, &RetainedError{IntegrationID: id, Cause: err}
	}

	return ApplyResult{
		ID: id, State: string(journal.State), ManifestDigest: journal.Manifest.Digest,
		Files: len(journal.Manifest.Entries),
	}, nil
}

func (m *Manager) convergeManifest(
	ctx context.Context,
	root *os.Root,
	objectDirectory string,
	manifest Manifest,
	action RecoveryAction,
) error {
	entries := append([]ManifestEntry(nil), manifest.Entries...)
	if action == RecoveryRollback {
		slices.Reverse(entries)
	}
	for _, entry := range entries {
		actual, err := inspectFileState(root, entry.Path, m.limits.BlobBytes)
		if err != nil {
			return err
		}
		from, to := entry.Base, entry.Target
		if action == RecoveryRollback {
			from, to = entry.Target, entry.Base
		}
		if sameFileState(actual, to) {
			continue
		}
		if !sameFileState(actual, from) {
			return ErrRecovery
		}
		if err := m.writeFileState(ctx, root, objectDirectory, entry.Path, to); err != nil {
			return err
		}
	}

	return nil
}

func countUnrelatedStatusPaths(paths []string, manifest Manifest) int {
	allowed := make(map[string]struct{}, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		allowed[entry.Path] = struct{}{}
	}
	unrelated := 0
	for _, path := range paths {
		if _, exists := allowed[path]; !exists {
			unrelated++
		}
	}

	return unrelated
}
