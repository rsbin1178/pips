//nolint:wsl_v5 // Artifact creation keeps ordered Git and identity checks beside each mutation.
package teamintegration

import (
	"context"
	"crypto/rand"
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
	"sync"
	"time"

	"github.com/rsbin/pips/internal/coding/execution/gitcontrol"
	"github.com/rsbin/pips/internal/coding/workspace"
)

const (
	privateDirectoryMode = fs.FileMode(0o700)
	privateFileMode      = fs.FileMode(0o600)
	defaultApprovalTTL   = 15 * time.Minute
	integrationRefReason = "pips coding Team integration control"
)

// Options configures the trusted Team integration boundary.
type Options struct {
	GitPath          string
	ProductRoot      string
	WorktreesRoot    string
	IntegrationsRoot string
	Limits           Limits
	ApprovalTTL      time.Duration
}

// PrepareRequest identifies one exact parent Workspace and result selection.
type PrepareRequest struct {
	Workspace string
	Selection Selection
	Verifier  Verifier
}

// VerificationRequest identifies the immutable isolated tree to validate.
type VerificationRequest struct {
	IntegrationID string
	Workspace     workspace.Workspace
	TreeOID       string
}

// Verifier runs ordinary policy-controlled validation in the isolated
// Integration Workspace. It must not mutate the Integration tree.
type Verifier interface {
	Verify(context.Context, VerificationRequest) (Verification, error)
}

// Preview is the bounded evidence presented before applying parent changes.
type Preview struct {
	ID                string
	AttemptIDs        []string
	CommitOID         string
	TreeOID           string
	Manifest          Manifest
	DiffDigest        string
	Verification      Verification
	ApprovalToken     string
	ApprovalTokenHash string
	ExpiresAt         time.Time
}

// Resource is the private application projection of one prepared Worktree.
// Frontends should use Preview instead.
type Resource struct {
	ID             string
	Workspace      workspace.Identity
	Directory      workspace.Identity
	GitDir         workspace.Identity
	CommonDir      workspace.Identity
	ObjectFormat   string
	BranchRef      string
	IntegrationRef string
	BaseOID        string
	CommitOID      string
	LockReason     string
}

// RetainedError reports an artifact that was intentionally left inspectable.
type RetainedError struct {
	IntegrationID string
	Cause         error
}

func (e *RetainedError) Error() string {
	return "coding team integration: retained artifact " + e.IntegrationID + ": " + e.Cause.Error()
}

func (e *RetainedError) Unwrap() error { return e.Cause }

// RolledBackError reports an apply attempt that wrote parent paths but safely
// restored every one to its exact base state. The Integration artifact remains
// retained for inspection.
type RolledBackError struct {
	IntegrationID string
	Cause         error
}

func (e *RolledBackError) Error() string {
	return "coding team integration: rolled back transaction " + e.IntegrationID + ": " + e.Cause.Error()
}

func (e *RolledBackError) Unwrap() error { return e.Cause }

// ConflictError carries bounded deterministic composition evidence. No parent
// or Integration Worktree mutation has occurred when this error is returned.
type ConflictError struct {
	IntegrationID string
	AttemptIDs    []string
	Composition   Composition
}

func (e *ConflictError) Error() string { return "coding team integration: captured results conflict" }

func (e *ConflictError) Unwrap() error { return ErrConflict }

type parentSnapshot struct {
	repository        gitcontrol.Repository
	workspaceIdentity string
	commonIdentity    string
	status            gitcontrol.Status
	indexDigest       string
}

type preparedRecord struct {
	preview        Preview
	workspace      workspace.Workspace
	worktree       workspace.Workspace
	common         workspace.Workspace
	gitDir         workspace.Workspace
	objectFormat   string
	lockReason     string
	selection      Selection
	composition    Composition
	binding        ApprovalBinding
	integrationRef string
	branchRef      string
}

// Manager owns immutable Integration artifacts and process-local approvals.
type Manager struct {
	git              *gitcontrol.Runner
	productRoot      workspace.Workspace
	worktreesRoot    workspace.Workspace
	integrationsRoot workspace.Workspace
	limits           Limits
	approvalTTL      time.Duration
	tokens           *TokenRegistry
	now              func() time.Time
	readBlob         func(context.Context, string, string) ([]byte, error)
	mutationFault    func(mutationBoundary, string) error

	mu      sync.Mutex
	records map[string]preparedRecord
}

// New validates private roots and returns an Integration manager.
//
//nolint:gocyclo // Construction validates every root, budget, identity, and Git boundary together.
func New(options Options) (*Manager, error) {
	if err := validateLimits(options.Limits); err != nil {
		return nil, err
	}
	if options.ApprovalTTL == 0 {
		options.ApprovalTTL = defaultApprovalTTL
	}
	if options.ApprovalTTL < time.Second || options.ApprovalTTL > 24*time.Hour {
		return nil, fmt.Errorf("%w: approval lifetime", ErrInvalid)
	}

	for _, root := range []string{options.ProductRoot, options.WorktreesRoot, options.IntegrationsRoot} {
		if !filepath.IsAbs(root) || filepath.Clean(root) != root {
			return nil, fmt.Errorf("%w: control root", ErrInvalid)
		}
		if err := ensurePrivateDirectory(root); err != nil {
			return nil, err
		}
	}

	productRoot, err := workspace.Open(options.ProductRoot)
	if err != nil {
		return nil, err
	}
	worktreesRoot, err := workspace.Open(options.WorktreesRoot)
	if err != nil {
		return nil, err
	}
	integrationsRoot, err := workspace.Open(options.IntegrationsRoot)
	if err != nil {
		return nil, err
	}
	if rootsOverlap(productRoot.Root(), worktreesRoot.Root()) ||
		rootsOverlap(worktreesRoot.Root(), integrationsRoot.Root()) {
		return nil, fmt.Errorf("%w: overlapping control roots", ErrInvalid)
	}

	runner, err := gitcontrol.New(options.GitPath, gitcontrol.Limits{
		OutputBytes: options.Limits.BlobBytes,
		InputBytes:  min(options.Limits.TreeBytes, int64(512<<20)),
		Timeout:     5 * time.Minute,
		TreeEntries: options.Limits.Entries,
	})
	if err != nil {
		return nil, err
	}

	return &Manager{
		git: runner, productRoot: productRoot, worktreesRoot: worktreesRoot,
		integrationsRoot: integrationsRoot, limits: options.Limits,
		approvalTTL: options.ApprovalTTL, tokens: NewTokenRegistry(), now: time.Now,
		readBlob: runner.CatBlob, records: make(map[string]preparedRecord),
	}, nil
}

// Prepare composes captured results without modifying the parent Workspace.
//
//nolint:funlen,gocyclo // The ordered identity, Git, verification, and approval boundaries are kept together.
func (m *Manager) Prepare(ctx context.Context, request PrepareRequest) (Preview, error) {
	if err := m.validate(); err != nil {
		return Preview{}, err
	}
	parent, err := m.snapshotParent(ctx, request.Workspace)
	if err != nil {
		return Preview{}, err
	}
	if !parent.status.Clean || parent.repository.HeadOID != request.Selection.BaseOID ||
		parent.repository.BranchRef == "" {
		return Preview{}, fmt.Errorf("%w: parent is not the admitted clean branch", ErrStale)
	}
	if err := m.git.ValidateSafeConfig(ctx, parent.repository.TopLevel); err != nil {
		return Preview{}, err
	}

	selection, err := m.loadSelection(ctx, parent.repository.TopLevel, request.Selection)
	if err != nil {
		return Preview{}, err
	}
	id, err := randomID()
	if err != nil {
		return Preview{}, err
	}
	composition, err := Compose(ctx, selection, m.limits)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			return Preview{ID: id, AttemptIDs: attemptIDs(selection.Artifacts)}, &ConflictError{
				IntegrationID: id, AttemptIDs: attemptIDs(selection.Artifacts),
				Composition: composition,
			}
		}

		return Preview{}, err
	}
	derived := m.derive(selection.TeamID, id, parent.workspaceIdentity)
	if err := os.MkdirAll(filepath.Dir(derived.directory), privateDirectoryMode); err != nil {
		return Preview{}, fmt.Errorf("create Integration parent: %w", err)
	}

	indexPath, cleanupIndex, err := m.newTemporaryIndex()
	if err != nil {
		return Preview{}, err
	}
	defer cleanupIndex()

	baseTree, err := m.git.ResolveTree(ctx, parent.repository.TopLevel, selection.BaseOID)
	if err != nil {
		return Preview{}, err
	}
	if err := m.git.ReadTree(ctx, parent.repository.TopLevel, indexPath, baseTree); err != nil {
		return Preview{}, err
	}
	if err := m.updateComposedIndex(ctx, parent.repository, indexPath, selection.Base, composition.Entries); err != nil {
		return Preview{}, err
	}
	unmerged, err := m.git.UnmergedIndex(ctx, parent.repository.TopLevel, indexPath)
	if err != nil || unmerged {
		if err == nil {
			err = ErrConflict
		}

		return Preview{}, fmt.Errorf("write Integration index: %w", err)
	}
	treeOID, err := m.git.WriteTree(ctx, parent.repository.TopLevel, indexPath)
	if err != nil {
		return Preview{}, err
	}
	commitOID, err := m.git.CommitTree(ctx, parent.repository.TopLevel, gitcontrol.Commit{
		TreeOID: treeOID, ParentOID: selection.BaseOID,
		Message:   "Prepare Pips Coding Team Integration " + id + "\n",
		Timestamp: m.now().UTC(),
	})
	if err != nil {
		return Preview{}, err
	}
	if err := m.git.UpdateRefs(
		ctx, parent.repository.TopLevel, parent.repository.ObjectFormat, integrationRefReason,
		[]gitcontrol.RefUpdate{
			{Ref: derived.integrationRef, NewOID: commitOID, Create: true},
			{Ref: derived.branchRef, NewOID: commitOID, Create: true},
		},
	); err != nil {
		return Preview{}, err
	}

	retained := func(cause error) (Preview, error) {
		return Preview{
				ID: id, AttemptIDs: attemptIDs(selection.Artifacts),
				CommitOID: commitOID, TreeOID: treeOID,
			}, &RetainedError{
				IntegrationID: id, Cause: cause,
			}
	}
	if err := m.git.AddWorktree(
		ctx, parent.repository.TopLevel, derived.directory, derived.branchRef, derived.lockReason,
	); err != nil {
		return retained(err)
	}
	integrationRepository, err := m.git.InspectRepository(ctx, derived.directory)
	if err != nil {
		return retained(err)
	}
	if integrationRepository.HeadOID != commitOID || integrationRepository.BranchRef != derived.branchRef ||
		integrationRepository.CommonDir != parent.repository.CommonDir {
		return retained(fmt.Errorf("%w: Integration Worktree identity", ErrStale))
	}
	if err := m.git.ReadTree(
		ctx, derived.directory, filepath.Join(integrationRepository.GitDir, "index"), treeOID,
	); err != nil {
		return retained(err)
	}
	if err := m.materialize(ctx, derived.directory, parent.repository.TopLevel, composition.Entries); err != nil {
		return retained(err)
	}

	manifest, err := BuildManifest(
		ctx, gitBlobReader{runner: m.git, directory: parent.repository.TopLevel},
		selection.Base, composition.Entries, m.limits,
	)
	if err != nil {
		return retained(err)
	}
	diffDigest, err := digestValue(struct {
		TreeOID  string   `json:"tree_oid"`
		Manifest Manifest `json:"manifest"`
	}{TreeOID: treeOID, Manifest: manifest})
	if err != nil {
		return retained(err)
	}

	worktree, err := workspace.Open(derived.directory)
	if err != nil {
		return retained(err)
	}
	verification, err := m.verify(ctx, request.Verifier, id, worktree, treeOID)
	if err != nil {
		return retained(err)
	}
	if err := m.verifyIntegration(ctx, derived, commitOID, treeOID); err != nil {
		return retained(err)
	}
	if err := m.verifyParent(ctx, parent); err != nil {
		return retained(err)
	}

	selectionDigest, err := digestValue(selection)
	if err != nil {
		return retained(err)
	}
	expiresAt := m.now().UTC().Add(m.approvalTTL)
	binding := ApprovalBinding{
		IntegrationID:     id,
		WorkspaceIdentity: parent.workspaceIdentity, CommonIdentity: parent.commonIdentity,
		BranchRef: parent.repository.BranchRef, HeadOID: parent.repository.HeadOID,
		StatusDigest: parent.status.Digest, IndexDigest: parent.indexDigest,
		ResourceRevision: selection.ResourceRevision, SelectionDigest: selectionDigest,
		IntegrationCommit: commitOID, IntegrationTree: treeOID,
		ManifestDigest: manifest.Digest, DiffDigest: diffDigest,
		Verification: verification, ExpiresAt: expiresAt,
	}
	preview := Preview{
		ID: id, AttemptIDs: attemptIDs(selection.Artifacts), CommitOID: commitOID,
		TreeOID: treeOID, Manifest: manifest, DiffDigest: diffDigest,
		Verification: verification, ExpiresAt: expiresAt,
	}
	if verification.Status == VerificationPassed || verification.Status == VerificationNotRun {
		token, tokenHash, err := m.tokens.Issue(binding)
		if err != nil {
			return retained(err)
		}
		preview.ApprovalToken = token
		preview.ApprovalTokenHash = tokenHash
	}
	common, err := workspace.Open(parent.repository.CommonDir)
	if err != nil {
		if preview.ApprovalToken != "" {
			_ = m.tokens.Reject(preview.ApprovalToken)
		}

		return retained(err)
	}
	gitDir, err := workspace.Open(integrationRepository.GitDir)
	if err != nil {
		if preview.ApprovalToken != "" {
			_ = m.tokens.Reject(preview.ApprovalToken)
		}

		return retained(err)
	}
	parentWorkspace, err := workspace.Open(parent.repository.TopLevel)
	if err != nil {
		if preview.ApprovalToken != "" {
			_ = m.tokens.Reject(preview.ApprovalToken)
		}

		return retained(err)
	}
	if parentWorkspace.Identity().Key() != parent.workspaceIdentity {
		if preview.ApprovalToken != "" {
			_ = m.tokens.Reject(preview.ApprovalToken)
		}

		return retained(fmt.Errorf("%w: parent Workspace identity", ErrStale))
	}
	m.mu.Lock()
	m.records[id] = preparedRecord{
		preview: preview, workspace: parentWorkspace,
		worktree: worktree, common: common, gitDir: gitDir,
		objectFormat: parent.repository.ObjectFormat, lockReason: derived.lockReason,
		selection: selection, composition: composition,
		binding: binding, integrationRef: derived.integrationRef, branchRef: derived.branchRef,
	}
	m.mu.Unlock()

	return preview, nil
}

// Resource returns private durable identity for application-state projection.
func (m *Manager) Resource(id string) (Resource, error) {
	if err := m.validate(); err != nil {
		return Resource{}, err
	}
	m.mu.Lock()
	record, exists := m.records[id]
	m.mu.Unlock()
	if !exists {
		return Resource{}, ErrStale
	}

	return Resource{
		ID: id, Workspace: record.workspace.Identity(), Directory: record.worktree.Identity(),
		GitDir: record.gitDir.Identity(), CommonDir: record.common.Identity(),
		ObjectFormat: record.objectFormat, BranchRef: record.branchRef,
		IntegrationRef: record.integrationRef, BaseOID: record.selection.BaseOID,
		CommitOID: record.preview.CommitOID, LockReason: record.lockReason,
	}, nil
}

//nolint:gocyclo // Every captured ref and tree is independently resolved before composition.
func (m *Manager) loadSelection(
	ctx context.Context,
	directory string,
	selection Selection,
) (Selection, error) {
	if selection.TeamID == "" || selection.ResourceRevision == 0 || selection.BaseOID == "" ||
		len(selection.Artifacts) == 0 {
		return Selection{}, fmt.Errorf("%w: incomplete selection", ErrInvalid)
	}
	if _, err := m.git.ResolveCommit(ctx, directory, selection.BaseOID); err != nil {
		return Selection{}, err
	}
	baseTree, err := m.git.ResolveTree(ctx, directory, selection.BaseOID)
	if err != nil {
		return Selection{}, err
	}
	selection.Base, err = m.loadEntries(ctx, directory, baseTree)
	if err != nil {
		return Selection{}, err
	}

	for index := range selection.Artifacts {
		artifact := &selection.Artifacts[index]
		if artifact.ResultRef == "" {
			return Selection{}, fmt.Errorf("%w: missing result ref", ErrInvalid)
		}
		if _, err := m.git.ResolveCommit(ctx, directory, artifact.BaseOID); err != nil {
			return Selection{}, err
		}
		if _, err := m.git.ResolveCommit(ctx, directory, artifact.ResultOID); err != nil {
			return Selection{}, err
		}
		resolved, err := m.git.ResolveRef(ctx, directory, artifact.ResultRef)
		if err != nil || resolved != artifact.ResultOID {
			return Selection{}, fmt.Errorf("%w: result ref changed", ErrStale)
		}
		baseTree, err := m.git.ResolveTree(ctx, directory, artifact.BaseOID)
		if err != nil {
			return Selection{}, err
		}
		resultTree, err := m.git.ResolveTree(ctx, directory, artifact.ResultOID)
		if err != nil {
			return Selection{}, err
		}
		artifact.Base, err = m.loadEntries(ctx, directory, baseTree)
		if err != nil {
			return Selection{}, err
		}
		artifact.Result, err = m.loadEntries(ctx, directory, resultTree)
		if err != nil {
			return Selection{}, err
		}
	}

	return selection, nil
}

func (m *Manager) loadEntries(ctx context.Context, directory, treeOID string) ([]Entry, error) {
	entries, err := m.git.ListTree(ctx, directory, treeOID)
	if err != nil {
		return nil, err
	}
	result := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		if entry.Type != "blob" {
			return nil, fmt.Errorf("%w: unsupported tree entry", ErrInvalid)
		}
		value := Entry{Mode: entry.Mode, OID: entry.OID, Path: entry.Path}
		if err := validateEntry(value, m.limits); err != nil {
			return nil, err
		}
		result = append(result, value)
	}

	return result, nil
}

func (m *Manager) updateComposedIndex(
	ctx context.Context,
	repository gitcontrol.Repository,
	indexPath string,
	base, target []Entry,
) error {
	baseMap, err := entryMap(base, m.limits)
	if err != nil {
		return err
	}
	targetMap, err := entryMap(target, m.limits)
	if err != nil {
		return err
	}
	updates := make([]gitcontrol.IndexEntry, 0)
	for _, path := range changedPaths(baseMap, targetMap) {
		entry, exists := targetMap[path]
		if !exists {
			updates = append(updates, gitcontrol.IndexEntry{Mode: "0", Path: path})
			continue
		}
		updates = append(updates, gitcontrol.IndexEntry{Mode: entry.Mode, OID: entry.OID, Path: path})
	}

	return m.git.UpdateIndex(
		ctx, repository.TopLevel, indexPath, repository.ObjectFormat, updates,
	)
}

type derivedIntegration struct {
	integrationRef string
	branchRef      string
	directory      string
	lockReason     string
}

func (m *Manager) derive(teamID, id, workspaceIdentity string) derivedIntegration {
	teamToken := stableToken(teamID)
	integrationToken := stableToken(id)
	workspaceToken := stableToken(workspaceIdentity)

	return derivedIntegration{
		integrationRef: "refs/pips/team/" + teamToken + "/integrations/" + integrationToken,
		branchRef:      "refs/heads/pips/team/" + teamToken + "/integration/" + integrationToken,
		directory: filepath.Join(
			m.worktreesRoot.Root(), workspaceToken, teamToken, "integrations", integrationToken,
		),
		lockReason: "pips/team-integration/v1 team=" + teamToken + " integration=" + integrationToken,
	}
}

//nolint:gocyclo // Materialization keeps type, size, mode, and durability checks explicit.
func (m *Manager) materialize(
	ctx context.Context,
	targetDirectory string,
	objectDirectory string,
	entries []Entry,
) error {
	root, err := os.OpenRoot(targetDirectory)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	var total int64
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		content, err := m.git.CatBlob(ctx, objectDirectory, entry.OID)
		if err != nil {
			return err
		}
		total, err = addTreeBytes(total, int64(len(content)), m.limits.TreeBytes)
		if err != nil || int64(len(content)) > m.limits.BlobBytes {
			return ErrLimit
		}
		parent := filepath.Dir(entry.Path)
		if parent != "." {
			if err := root.MkdirAll(parent, privateDirectoryMode); err != nil {
				return err
			}
		}
		if entry.Mode == "120000" {
			if strings.IndexByte(string(content), 0) >= 0 {
				return fmt.Errorf("%w: symlink target contains nul", ErrInvalid)
			}
			if err := root.Symlink(string(content), entry.Path); err != nil {
				return err
			}

			continue
		}
		mode := fs.FileMode(0o644)
		if entry.Mode == "100755" {
			mode = 0o755
		}
		file, err := root.OpenFile(entry.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		_, writeErr := file.Write(content)
		closeErr := errors.Join(file.Sync(), file.Close())
		if writeErr != nil || closeErr != nil {
			return errors.Join(writeErr, closeErr)
		}
	}

	return syncDirectory(targetDirectory)
}

func (m *Manager) verify(
	ctx context.Context,
	verifier Verifier,
	id string,
	worktree workspace.Workspace,
	treeOID string,
) (Verification, error) {
	verification := Verification{Status: VerificationNotRun, TreeOID: treeOID}
	var err error
	if verifier != nil {
		verification, err = verifier.Verify(ctx, VerificationRequest{
			IntegrationID: id, Workspace: worktree, TreeOID: treeOID,
		})
		if err != nil {
			return Verification{}, err
		}
		if verification.TreeOID == "" {
			verification.TreeOID = treeOID
		}
	}
	if verification.TreeOID != treeOID || !validVerificationStatus(verification.Status) {
		return Verification{}, fmt.Errorf("%w: verification evidence", ErrInvalid)
	}
	verification.Digest = ""
	verification.Digest, err = digestValue(verification)
	if err != nil {
		return Verification{}, err
	}

	return verification, nil
}

func (m *Manager) verifyIntegration(
	ctx context.Context,
	derived derivedIntegration,
	commitOID, treeOID string,
) error {
	refOID, err := m.git.ResolveRef(ctx, derived.directory, derived.integrationRef)
	if err != nil || refOID != commitOID {
		return fmt.Errorf("%w: Integration ref", ErrStale)
	}
	repository, err := m.git.InspectRepository(ctx, derived.directory)
	if err != nil {
		return err
	}
	if repository.HeadOID != commitOID || repository.BranchRef != derived.branchRef {
		return fmt.Errorf("%w: Integration HEAD", ErrStale)
	}
	actualTree, err := m.git.ResolveTree(ctx, derived.directory, commitOID)
	if err != nil || actualTree != treeOID {
		return fmt.Errorf("%w: Integration tree", ErrStale)
	}
	status, err := m.git.SnapshotStatus(ctx, derived.directory)
	if err != nil || !status.Clean {
		return fmt.Errorf("%w: verifier mutated Integration Worktree", ErrStale)
	}

	return nil
}

func (m *Manager) snapshotParent(ctx context.Context, directory string) (parentSnapshot, error) {
	worktree, err := workspace.Open(directory)
	if err != nil {
		return parentSnapshot{}, err
	}
	repository, err := m.git.InspectRepository(ctx, worktree.Root())
	if err != nil {
		return parentSnapshot{}, err
	}
	if repository.TopLevel != worktree.Root() {
		return parentSnapshot{}, fmt.Errorf("%w: Workspace is not repository root", ErrInvalid)
	}
	common, err := workspace.Open(repository.CommonDir)
	if err != nil {
		return parentSnapshot{}, err
	}
	status, err := m.git.SnapshotStatus(ctx, worktree.Root())
	if err != nil {
		return parentSnapshot{}, err
	}
	indexDigest, err := snapshotFileDigest(filepath.Join(repository.GitDir, "index"), m.limits.TreeBytes)
	if err != nil {
		return parentSnapshot{}, err
	}

	return parentSnapshot{
		repository: repository, workspaceIdentity: worktree.Identity().Key(),
		commonIdentity: common.Identity().Key(), status: status, indexDigest: indexDigest,
	}, nil
}

func (m *Manager) verifyParent(ctx context.Context, expected parentSnapshot) error {
	actual, err := m.snapshotParent(ctx, expected.repository.TopLevel)
	if err != nil {
		return err
	}
	if actual.repository != expected.repository ||
		actual.workspaceIdentity != expected.workspaceIdentity ||
		actual.commonIdentity != expected.commonIdentity ||
		actual.status.Clean != expected.status.Clean ||
		actual.status.Digest != expected.status.Digest ||
		!slices.Equal(actual.status.Paths, expected.status.Paths) ||
		actual.indexDigest != expected.indexDigest {
		return fmt.Errorf("%w: parent repository changed", ErrStale)
	}

	return nil
}

func snapshotFileDigest(path string, limit int64) (string, error) {
	file, err := os.Open(path) //nolint:gosec // Exact Git metadata path from repository inspection.
	if errors.Is(err, fs.ErrNotExist) {
		return stableToken("absent"), nil
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return "", fmt.Errorf("%w: parent index", ErrLimit)
	}
	device, inode, err := fileObjectIdentity(info)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, limit+1)); err != nil {
		return "", err
	}
	payload := fmt.Sprintf(
		"%d\x00%d\x00%d\x00%d\x00%x",
		device, inode, info.Mode().Perm(), info.Size(), hash.Sum(nil),
	)

	return stableToken(payload), nil
}

func (m *Manager) newTemporaryIndex() (string, func(), error) {
	file, err := os.CreateTemp(m.integrationsRoot.Root(), "integration-index-*")
	if err != nil {
		return "", nil, err
	}
	path := filepath.Clean(file.Name())
	closeErr := file.Close()
	removeErr := os.Remove(path)
	if closeErr != nil || removeErr != nil {
		return "", nil, errors.Join(closeErr, removeErr)
	}

	return path, func() { _ = os.Remove(path) }, nil
}

func (m *Manager) validate() error {
	if m == nil || m.git == nil || m.tokens == nil || m.now == nil ||
		m.readBlob == nil || m.records == nil {
		return fmt.Errorf("%w: nil manager", ErrInvalid)
	}
	for _, root := range []workspace.Workspace{m.productRoot, m.worktreesRoot, m.integrationsRoot} {
		current, err := workspace.Open(root.Root())
		if err != nil || current.Identity().Key() != root.Identity().Key() {
			return fmt.Errorf("%w: control root changed", ErrStale)
		}
	}

	return nil
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, privateDirectoryMode); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: private directory permissions", ErrInvalid)
	}

	return nil
}

func rootsOverlap(left, right string) bool {
	relative, err := filepath.Rel(left, right)
	if err == nil && relative != "." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && relative != ".." {
		return true
	}
	relative, err = filepath.Rel(right, left)

	return err == nil && (relative == "." ||
		(relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))))
}

func validVerificationStatus(status VerificationStatus) bool {
	switch status {
	case VerificationNotRun, VerificationPassed, VerificationFailed,
		VerificationTimeout, VerificationApprovalNeeded:
		return true
	default:
		return false
	}
}

func randomID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}

	return "int-" + hex.EncodeToString(value), nil
}

func stableToken(value string) string {
	sum := sha256.Sum256([]byte(value))

	return hex.EncodeToString(sum[:20])
}

func attemptIDs(artifacts []Artifact) []string {
	result := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		result = append(result, artifact.AttemptID)
	}

	return result
}

func syncDirectory(path string) error {
	directory, err := os.Open(path) //nolint:gosec // Exact owned directory.
	if err != nil {
		return err
	}

	return errors.Join(directory.Sync(), directory.Close())
}

type gitBlobReader struct {
	runner    *gitcontrol.Runner
	directory string
}

func (r gitBlobReader) Blob(ctx context.Context, oid string) ([]byte, error) {
	return r.runner.CatBlob(ctx, r.directory, oid)
}
