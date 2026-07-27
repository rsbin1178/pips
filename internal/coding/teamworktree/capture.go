package teamworktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rsbin/pips/internal/coding/execution/gitcontrol"
)

// Capture publishes the exact raw Worktree state as one single-parent result
// commit and result ref without modifying the parent Workspace index.
//
//nolint:gocyclo,funlen // Capture keeps scan, temporary-index, CAS publish, verify, and rollback in commit order.
func (m *Manager) Capture(
	ctx context.Context,
	lease *Lease,
	resource Resource,
	request CaptureRequest,
) (Capture, error) {
	if resource.ResultCommitOID != "" {
		return Capture{}, fmt.Errorf("%w: resource is already captured", ErrConflict)
	}

	if _, err := m.verifyResource(ctx, lease, resource); err != nil {
		return Capture{}, err
	}

	base, err := m.treeManifest(ctx, resource.Workspace.Path, resource.BaseOID)
	if err != nil {
		return Capture{}, err
	}

	final, err := m.scanWorktree(ctx, resource, base, true)
	if err != nil {
		return Capture{}, err
	}

	indexPath, cleanupIndex, err := m.newTemporaryIndex()
	if err != nil {
		return Capture{}, err
	}
	defer cleanupIndex()

	if err := m.git.ReadTree(ctx, resource.Directory.Path, indexPath, resource.BaseOID); err != nil {
		return Capture{}, mapGitError(err)
	}

	updates := make([]gitcontrol.IndexEntry, 0, len(base)+len(final))

	finalPaths := make(map[string]struct{}, len(final))
	for _, entry := range final {
		if entry.oid == "" {
			return Capture{}, fmt.Errorf("%w: missing raw blob object ID", ErrIdentity)
		}

		updates = append(updates, gitcontrol.IndexEntry{
			Mode: entry.mode, OID: entry.oid, Path: entry.path,
		})
		finalPaths[entry.path] = struct{}{}
	}

	for _, entry := range base {
		if _, exists := finalPaths[entry.path]; !exists {
			updates = append(updates, gitcontrol.IndexEntry{Mode: "0", Path: entry.path})
		}
	}

	if err := m.git.UpdateIndex(
		ctx, resource.Directory.Path, indexPath, resource.ObjectFormat, updates,
	); err != nil {
		return Capture{}, mapGitError(err)
	}

	treeOID, err := m.git.WriteTree(ctx, resource.Directory.Path, indexPath)
	if err != nil {
		return Capture{}, mapGitError(err)
	}

	message := strings.TrimSpace(request.Message)
	if message == "" {
		message = "Capture Pips Coding Team Attempt " + stableToken(string(resource.Owner.AttemptID))[:12]
	}

	if len(message) > 16<<10 || strings.ContainsRune(message, '\x00') {
		return Capture{}, fmt.Errorf("%w: capture message", ErrInvalid)
	}

	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}

	when := request.Time
	if when.IsZero() {
		when = time.Now().UTC()
	} else {
		when = when.UTC()
	}

	commitOID, err := m.git.CommitTree(ctx, resource.Workspace.Path, gitcontrol.Commit{
		TreeOID: treeOID, ParentOID: resource.BaseOID, Message: message, Timestamp: when,
	})
	if err != nil {
		return Capture{}, mapGitError(err)
	}

	if err := m.git.UpdateRefs(
		ctx, resource.Workspace.Path, resource.ObjectFormat, refReason,
		[]gitcontrol.RefUpdate{
			{Ref: resource.ResultRef, NewOID: commitOID, Create: true},
			{Ref: resource.BranchRef, NewOID: commitOID, OldOID: resource.BaseOID},
		},
	); err != nil {
		return Capture{}, mapGitError(err)
	}

	published := resource
	published.ResultCommitOID = commitOID

	linkedIndex := filepath.Join(resource.GitDir.Path, "index")
	if err := m.git.ReadTree(ctx, resource.Directory.Path, linkedIndex, commitOID); err != nil {
		return Capture{Resource: published, CommitOID: commitOID, TreeOID: treeOID},
			&RetainedError{Resource: published, Cause: mapGitError(err)}
	}

	if _, err := m.verifyResource(ctx, lease, published); err != nil {
		return Capture{Resource: published, CommitOID: commitOID, TreeOID: treeOID},
			&RetainedError{Resource: published, Cause: err}
	}

	post, err := m.scanWorktree(ctx, published, final, false)
	if err != nil || !manifestsEqual(final, post) {
		if err == nil {
			err = ErrConflict
		}

		rollbackErr := m.rollbackCapture(ctx, resource, published)
		if rollbackErr != nil {
			return Capture{Resource: published, CommitOID: commitOID, TreeOID: treeOID},
				&RetainedError{Resource: published, Cause: errors.Join(err, rollbackErr)}
		}

		return Capture{Resource: resource}, errors.Join(ErrConflict, err)
	}

	added, modified, deleted := manifestChanges(base, final)

	return Capture{
		Resource: published, CommitOID: commitOID, TreeOID: treeOID,
		ManifestDigest: manifestDigest(final), Files: len(final), Bytes: manifestBytes(final),
		Added: added, Modified: modified, Deleted: deleted,
	}, nil
}

func (m *Manager) newTemporaryIndex() (string, func(), error) {
	if err := m.validateRoots(); err != nil {
		return "", nil, err
	}

	file, err := os.CreateTemp(m.leasesRoot.Path, "capture-index-*")
	if err != nil {
		return "", nil, fmt.Errorf("%w: create temporary index: %w", ErrIdentity, err)
	}

	path := filepath.Clean(file.Name())
	closeErr := file.Close()

	removeErr := os.Remove(path)
	if closeErr != nil || removeErr != nil {
		return "", nil, fmt.Errorf(
			"%w: prepare temporary index: %w", ErrIdentity, errors.Join(closeErr, removeErr),
		)
	}

	return path, func() { _ = os.Remove(path) }, nil
}

func (m *Manager) rollbackCapture(ctx context.Context, base, published Resource) error {
	if err := m.git.UpdateRefs(
		ctx, base.Workspace.Path, base.ObjectFormat, refReason,
		[]gitcontrol.RefUpdate{
			{Ref: published.ResultRef, OldOID: published.ResultCommitOID, Delete: true},
			{Ref: published.BranchRef, NewOID: base.BaseOID, OldOID: published.ResultCommitOID},
		},
	); err != nil {
		return mapGitError(err)
	}

	indexPath := filepath.Join(base.GitDir.Path, "index")
	if err := m.git.ReadTree(ctx, base.Directory.Path, indexPath, base.BaseOID); err != nil {
		return mapGitError(err)
	}

	return nil
}

func manifestChanges(base, final []manifestEntry) (added, modified, deleted int) {
	baseByPath := make(map[string]manifestEntry, len(base))
	for _, entry := range base {
		baseByPath[entry.path] = entry
	}

	for _, entry := range final {
		previous, exists := baseByPath[entry.path]
		if !exists {
			added++
		} else {
			if previous.mode != entry.mode || previous.size != entry.size || previous.digest != entry.digest {
				modified++
			}

			delete(baseByPath, entry.path)
		}
	}

	return added, modified, len(baseByPath)
}
