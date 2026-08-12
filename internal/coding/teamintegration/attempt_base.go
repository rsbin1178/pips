//nolint:wsl_v5 // Exact selection, composition, and ref publication remain adjacent.
package teamintegration

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rsbin1178/pips/internal/coding/execution/gitcontrol"
)

const attemptBaseRefReason = "pips coding Team Attempt dependency base"

// PrepareAttemptBase verifies a captured dependency closure and either
// returns the exact single dependency result or publishes one deterministic
// multi-dependency base ref. It never changes the parent Worktree or index.
//
//nolint:gocyclo,funlen // The crash-safe Git publication sequence is one control boundary.
func (m *Manager) PrepareAttemptBase(
	ctx context.Context,
	request AttemptBaseRequest,
) (AttemptBase, error) {
	if err := m.validate(); err != nil {
		return AttemptBase{}, err
	}
	_, timestampOffset := request.Timestamp.Zone()
	if !validAttemptBaseID(request.AttemptID) || request.Timestamp.IsZero() || timestampOffset != 0 {
		return AttemptBase{}, fmt.Errorf("%w: Attempt base identity", ErrInvalid)
	}
	repository, err := m.git.InspectRepository(ctx, request.Workspace)
	if err != nil {
		return AttemptBase{}, err
	}
	if repository.TopLevel != request.Workspace || repository.HeadOID != request.Selection.BaseOID {
		return AttemptBase{}, fmt.Errorf("%w: parent repository changed", ErrStale)
	}
	if err := m.git.ValidateSafeConfig(ctx, repository.TopLevel); err != nil {
		return AttemptBase{}, err
	}

	selection, err := m.loadSelection(ctx, repository.TopLevel, request.Selection)
	if err != nil {
		return AttemptBase{}, err
	}
	dependencyDigest, err := attemptDependencyDigest(selection)
	if err != nil {
		return AttemptBase{}, err
	}
	result := AttemptBase{
		DependencyDigest: dependencyDigest,
		DependencyCount:  len(selection.Artifacts),
	}
	if request.DirectResultOID != "" {
		if !selectionContainsResult(selection, request.DirectResultOID) {
			return AttemptBase{}, fmt.Errorf("%w: direct dependency result", ErrInvalid)
		}
		result.CommitOID = request.DirectResultOID
		result.TreeOID, err = m.git.ResolveTree(ctx, repository.TopLevel, result.CommitOID)
		if err != nil {
			return AttemptBase{}, err
		}

		return result, nil
	}
	if len(selection.Artifacts) < 2 {
		return AttemptBase{}, fmt.Errorf("%w: generated base requires multiple dependencies", ErrInvalid)
	}

	composition, err := Compose(ctx, selection, m.limits)
	if err != nil {
		return AttemptBase{}, err
	}
	result.CompositionDigest = composition.Digest
	indexPath, cleanupIndex, err := m.newTemporaryIndex()
	if err != nil {
		return AttemptBase{}, err
	}
	defer cleanupIndex()

	baseTree, err := m.git.ResolveTree(ctx, repository.TopLevel, selection.BaseOID)
	if err != nil {
		return AttemptBase{}, err
	}
	if err := m.git.ReadTree(ctx, repository.TopLevel, indexPath, baseTree); err != nil {
		return AttemptBase{}, err
	}
	if err := m.updateComposedIndex(
		ctx, repository, indexPath, selection.Base, composition.Entries,
	); err != nil {
		return AttemptBase{}, err
	}
	unmerged, err := m.git.UnmergedIndex(ctx, repository.TopLevel, indexPath)
	if err != nil {
		return AttemptBase{}, err
	}
	if unmerged {
		return AttemptBase{}, ErrConflict
	}
	result.TreeOID, err = m.git.WriteTree(ctx, repository.TopLevel, indexPath)
	if err != nil {
		return AttemptBase{}, err
	}
	result.CommitOID, err = m.git.CommitTree(ctx, repository.TopLevel, gitcontrol.Commit{
		TreeOID: result.TreeOID, ParentOID: selection.BaseOID,
		Message: "Prepare Pips Coding Team Attempt Base " +
			stableToken(selection.TeamID+"\x00"+request.AttemptID) + "\n",
		Timestamp: request.Timestamp.UTC(),
	})
	if err != nil {
		return AttemptBase{}, err
	}
	result.Ref = attemptBaseRef(selection.TeamID, request.AttemptID)

	resolved, resolveErr := m.git.ResolveRef(ctx, repository.TopLevel, result.Ref)
	switch {
	case resolveErr == nil:
		if resolved != result.CommitOID {
			return AttemptBase{}, fmt.Errorf("%w: Attempt base ref changed", ErrStale)
		}

		return result, nil
	case !errors.Is(resolveErr, gitcontrol.ErrNotFound):
		return AttemptBase{}, resolveErr
	}

	createErr := m.git.CreateRef(
		ctx,
		repository.TopLevel,
		repository.ObjectFormat,
		result.Ref,
		result.CommitOID,
		attemptBaseRefReason,
	)
	if createErr == nil {
		return result, nil
	}
	resolved, resolveErr = m.git.ResolveRef(ctx, repository.TopLevel, result.Ref)
	if resolveErr == nil && resolved == result.CommitOID {
		return result, nil
	}
	if resolveErr == nil {
		return AttemptBase{}, fmt.Errorf("%w: Attempt base ref changed", ErrStale)
	}
	if errors.Is(resolveErr, gitcontrol.ErrNotFound) {
		return AttemptBase{}, createErr
	}

	return result, &AttemptBaseRetainedError{
		Base: result, Cause: errors.Join(createErr, resolveErr),
	}
}

// CleanupAttemptBase CAS-deletes only the exact derived Attempt-owned base
// ref. A missing ref is an idempotent success; prerequisite result refs cannot
// satisfy the derivation check.
//
//nolint:gocyclo // Idempotent CAS cleanup keeps every ref race explicit.
func (m *Manager) CleanupAttemptBase(
	ctx context.Context,
	request AttemptBaseCleanupRequest,
) error {
	if err := m.validate(); err != nil {
		return err
	}
	if request.TeamID == "" || !validAttemptBaseID(request.AttemptID) ||
		request.Ref != attemptBaseRef(request.TeamID, request.AttemptID) ||
		request.CommitOID == "" {
		return fmt.Errorf("%w: Attempt base cleanup", ErrInvalid)
	}
	repository, err := m.git.InspectRepository(ctx, request.Workspace)
	if err != nil {
		return err
	}
	if repository.TopLevel != request.Workspace {
		return fmt.Errorf("%w: parent repository changed", ErrStale)
	}
	resolved, err := m.git.ResolveRef(ctx, repository.TopLevel, request.Ref)
	if errors.Is(err, gitcontrol.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if resolved != request.CommitOID {
		return fmt.Errorf("%w: Attempt base ref changed", ErrStale)
	}
	if err := m.git.DeleteRef(
		ctx, repository.TopLevel, request.Ref, request.CommitOID, attemptBaseRefReason,
	); err != nil {
		if _, resolveErr := m.git.ResolveRef(ctx, repository.TopLevel, request.Ref); errors.Is(resolveErr, gitcontrol.ErrNotFound) {
			return nil
		}

		return err
	}
	if _, err := m.git.ResolveRef(ctx, repository.TopLevel, request.Ref); !errors.Is(err, gitcontrol.ErrNotFound) {
		return errors.Join(fmt.Errorf("%w: Attempt base ref remains", ErrStale), err)
	}

	return nil
}

func attemptDependencyDigest(selection Selection) (string, error) {
	return digestValue(struct {
		TeamID    string   `json:"team_id"`
		BaseOID   string   `json:"base_oid"`
		Artifacts []string `json:"artifacts"`
	}{
		TeamID: selection.TeamID, BaseOID: selection.BaseOID,
		Artifacts: artifactIdentities(selection.Artifacts),
	})
}

func selectionContainsResult(selection Selection, oid string) bool {
	for _, artifact := range selection.Artifacts {
		if artifact.ResultOID == oid {
			return true
		}
	}

	return false
}

func attemptBaseRef(teamID, attemptID string) string {
	return "refs/pips/team/" + stableToken(teamID) +
		"/attempt-bases/" + stableToken(attemptID)
}

func validAttemptBaseID(value string) bool {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	for index, character := range value {
		if character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' ||
			character >= '0' && character <= '9' || index > 0 && strings.ContainsRune("._-", character) {
			continue
		}

		return false
	}

	return true
}
