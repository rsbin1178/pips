//nolint:wsl_v5 // Strict full-snapshot validation keeps identity checks adjacent.
package teamstate

import (
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/internal/coding/session"
)

var safeIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

const maximumAttemptDependencies = 10_000

func validateMutation(value Mutation, limits Limits) (Mutation, error) {
	if !safeIDPattern.MatchString(string(value.CommandID)) {
		return Mutation{}, fmt.Errorf("%w: invalid command ID", ErrInvalid)
	}
	if value.ExpectedRevision == ^team.Revision(0) ||
		value.Snapshot.Revision != value.ExpectedRevision+1 {
		return Mutation{}, fmt.Errorf("%w: snapshot revision must follow expected revision", ErrInvalid)
	}

	value.Snapshot = cloneSnapshot(value.Snapshot)
	normalizeSnapshot(&value.Snapshot)
	if err := validateSnapshot(value.Snapshot, limits); err != nil {
		return Mutation{}, err
	}

	return value, nil
}

//nolint:gocyclo // Full durable snapshot validation intentionally keeps every identity edge explicit.
func validateSnapshot(value Snapshot, limits Limits) error {
	if !safeIDPattern.MatchString(string(value.TeamID)) {
		return fmt.Errorf("%w: invalid Team ID", ErrInvalid)
	}
	if value.Revision == 0 || !validState(value.State) || !validCleanup(value.Cleanup) {
		return fmt.Errorf("%w: invalid Team resource lifecycle", ErrInvalid)
	}
	if err := validateParent(value.Parent); err != nil {
		return err
	}
	if err := validateRepository(value.Repository); err != nil {
		return err
	}
	if err := validateTimes(value.CreatedAt, value.UpdatedAt); err != nil {
		return err
	}
	if len(value.Members) == 0 || len(value.Members) > limits.MaxMembers ||
		len(value.Attempts) > limits.MaxAttempts || len(value.Integrations) > limits.MaxIntegrations {
		return fmt.Errorf("%w: snapshot collection size", ErrLimit)
	}

	members := make(map[team.MemberID]struct{}, len(value.Members))
	for index, member := range value.Members {
		if !safeIDPattern.MatchString(string(member.MemberID)) ||
			!validDigest(member.CapabilityProfileFingerprint) {
			return fmt.Errorf("%w: invalid member resource %d", ErrInvalid, index)
		}
		if _, duplicate := members[member.MemberID]; duplicate {
			return fmt.Errorf("%w: duplicate member %q", ErrInvalid, member.MemberID)
		}
		members[member.MemberID] = struct{}{}
	}

	attempts := make(map[team.AttemptID]struct{}, len(value.Attempts))
	for index, attempt := range value.Attempts {
		if err := validateAttempt(attempt, members); err != nil {
			return fmt.Errorf("coding team state: attempt %d: %w", index, err)
		}
		if _, duplicate := attempts[attempt.AttemptID]; duplicate {
			return fmt.Errorf("%w: duplicate attempt %q", ErrInvalid, attempt.AttemptID)
		}
		attempts[attempt.AttemptID] = struct{}{}
	}

	integrations := make(map[string]struct{}, len(value.Integrations))
	for index, integration := range value.Integrations {
		if err := validateIntegration(integration, attempts); err != nil {
			return fmt.Errorf("coding team state: integration %d: %w", index, err)
		}
		if _, duplicate := integrations[integration.ID]; duplicate {
			return fmt.Errorf("%w: duplicate integration %q", ErrInvalid, integration.ID)
		}
		integrations[integration.ID] = struct{}{}
	}

	return nil
}

func validateParent(value ParentResource) error {
	if session.ValidateID(value.SessionID) != nil || !validWorkspaceID(value.WorkspaceID) {
		return fmt.Errorf("%w: invalid parent identity", ErrInvalid)
	}
	if err := validateFileIdentity(value.Workspace, true); err != nil {
		return fmt.Errorf("%w: invalid parent Workspace: %w", ErrInvalid, err)
	}

	return nil
}

func validateRepository(value RepositoryResource) error {
	if err := validateFileIdentity(value.CommonDir, true); err != nil {
		return fmt.Errorf("%w: invalid repository common dir: %w", ErrInvalid, err)
	}
	if !validOID(value.BaseOID) || !validRef(value.BranchRef, true) ||
		value.Admission != AdmissionClean && value.Admission != AdmissionHEADOnly {
		return fmt.Errorf("%w: invalid repository admission", ErrInvalid)
	}

	return nil
}

func validateAttempt(value AttemptResource, members map[team.MemberID]struct{}) error {
	if !safeIDPattern.MatchString(string(value.TaskID)) ||
		!safeIDPattern.MatchString(string(value.AttemptID)) ||
		!safeIDPattern.MatchString(string(value.MemberID)) ||
		!safeIDPattern.MatchString(string(value.ContinuationID)) {
		return fmt.Errorf("%w: incomplete Attempt lineage", ErrInvalid)
	}
	if _, exists := members[value.MemberID]; !exists {
		return fmt.Errorf("%w: Attempt references unknown member", ErrInvalid)
	}
	if !validAttemptState(value.State) || !validCleanup(value.Cleanup) {
		return fmt.Errorf("%w: invalid Attempt lifecycle", ErrInvalid)
	}
	if value.Session != (WorkerSessionResource{}) {
		if session.ValidateID(value.Session.SessionID) != nil ||
			!validWorkspaceID(value.Session.WorkspaceID) {
			return fmt.Errorf("%w: invalid Worker Session binding", ErrInvalid)
		}
	}
	if err := validateAttemptBase(value.Base); err != nil {
		return err
	}
	if err := validateWorktree(value.Worktree); err != nil {
		return err
	}

	return nil
}

//nolint:gocyclo // Closed optional/generated binding invariants are kept in one validator.
func validateAttemptBase(value AttemptBaseResource) error {
	if value == (AttemptBaseResource{}) {
		return nil
	}
	if !validOID(value.OID) || value.TreeOID != "" && !validOID(value.TreeOID) ||
		!validRef(value.OwnedRef, true) || value.DependencyCount < 0 ||
		value.DependencyCount > maximumAttemptDependencies {
		return fmt.Errorf("%w: invalid Attempt base binding", ErrInvalid)
	}
	if value.DependencyDigest != "" && !validDigest(value.DependencyDigest) ||
		value.CompositionDigest != "" && !validDigest(value.CompositionDigest) {
		return fmt.Errorf("%w: invalid Attempt base digest", ErrInvalid)
	}
	generated := value.OwnedRef != "" || value.CompositionDigest != ""
	if value.DependencyCount > 0 && value.DependencyDigest == "" ||
		generated && (value.DependencyCount < 2 || value.OwnedRef == "" ||
			value.CompositionDigest == "" || value.TreeOID == "") {
		return fmt.Errorf("%w: incomplete Attempt base binding", ErrInvalid)
	}

	return nil
}

func validateIntegration(
	value IntegrationResource,
	attempts map[team.AttemptID]struct{},
) error {
	if !safeIDPattern.MatchString(value.ID) || !validIntegrationState(value.State) ||
		!validCleanup(value.Cleanup) || len(value.AttemptIDs) == 0 {
		return fmt.Errorf("%w: invalid integration resource", ErrInvalid)
	}
	seen := make(map[team.AttemptID]struct{}, len(value.AttemptIDs))
	for _, id := range value.AttemptIDs {
		if _, exists := attempts[id]; !exists {
			return fmt.Errorf("%w: integration references unknown Attempt", ErrInvalid)
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("%w: duplicate integration Attempt", ErrInvalid)
		}
		seen[id] = struct{}{}
	}
	if err := validateWorktree(value.Worktree); err != nil {
		return err
	}
	for _, digest := range []string{
		value.DiffDigest, value.ManifestDigest, value.VerificationDigest,
		value.JournalDigest, value.ApprovalTokenHash,
	} {
		if digest != "" && !validDigest(digest) {
			return fmt.Errorf("%w: invalid integration digest", ErrInvalid)
		}
	}
	if value.TreeOID != "" && !validOID(value.TreeOID) ||
		value.CommitOID != "" && !validOID(value.CommitOID) {
		return fmt.Errorf("%w: invalid integration digest", ErrInvalid)
	}

	return nil
}

func validateWorktree(value WorktreeResource) error {
	if value == (WorktreeResource{}) {
		return nil
	}
	if !safeIDPattern.MatchString(value.ID) || !validRef(value.BranchRef, true) ||
		!validRef(value.ResultRef, true) || value.BaseOID != "" && !validOID(value.BaseOID) ||
		value.ResultCommitOID != "" && !validOID(value.ResultCommitOID) ||
		value.ObjectFormat != "sha1" && value.ObjectFormat != "sha256" ||
		strings.TrimSpace(value.LockReason) == "" || len(value.LockReason) > 4096 ||
		value.LeaseGeneration == 0 {
		return fmt.Errorf("%w: invalid Worktree binding", ErrInvalid)
	}
	for _, identity := range []FileIdentity{
		value.Workspace, value.Directory, value.GitDir, value.CommonDir,
	} {
		if err := validateFileIdentity(identity, false); err != nil {
			return fmt.Errorf("%w: invalid Worktree filesystem identity", ErrInvalid)
		}
	}

	return nil
}

func validateFileIdentity(value FileIdentity, required bool) error {
	if value == (FileIdentity{}) && !required {
		return nil
	}
	if value.Path == "" || !filepath.IsAbs(value.Path) || filepath.Clean(value.Path) != value.Path ||
		strings.ContainsRune(value.Path, '\x00') {
		return fmt.Errorf("%w: invalid absolute path", ErrInvalid)
	}

	return nil
}

func validateTimes(created, updated time.Time) error {
	if created.IsZero() || updated.IsZero() || updated.Before(created) {
		return fmt.Errorf("%w: invalid snapshot timestamps", ErrInvalid)
	}
	_, createdOffset := created.Zone()
	_, updatedOffset := updated.Zone()
	if createdOffset != 0 || updatedOffset != 0 {
		return fmt.Errorf("%w: snapshot timestamps must be UTC", ErrInvalid)
	}

	return nil
}

func validWorkspaceID(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= 512 &&
		!strings.ContainsRune(value, '\x00')
}

func validDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && value == strings.ToLower(value)
}

func validOID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)

	return err == nil && value == strings.ToLower(value)
}

func validRef(value string, optional bool) bool {
	if value == "" {
		return optional
	}
	return len(value) <= 512 && strings.TrimSpace(value) == value &&
		!strings.ContainsAny(value, "\x00\r\n")
}

func validState(value State) bool {
	return slices.Contains([]State{
		StateAdmitted, StateProvisioning, StateActive, StateWorkComplete,
		StateIntegrationPending, StateIntegrated, StateClosedWithoutIntegration,
		StateCanceling, StateCancelled, StateInterrupted, StateBlockedIdentity,
		StateBlockedConflict, StateFailed,
	}, value)
}

func validAttemptState(value AttemptState) bool {
	return slices.Contains([]AttemptState{
		AttemptPlanned, AttemptBasePrepared, AttemptWorktreeReady, AttemptSessionReady,
		AttemptRunning, AttemptCapturing, AttemptCaptured, AttemptTerminal,
		AttemptInterrupted, AttemptRecoverable, AttemptFailed, AttemptCancelled,
		AttemptConflicted, AttemptCaptureFailed, AttemptOrphaned,
	}, value)
}

func validIntegrationState(value IntegrationState) bool {
	return slices.Contains([]IntegrationState{
		IntegrationPlanned, IntegrationReady, IntegrationVerified, IntegrationApproved,
		IntegrationApplied, IntegrationConflict, IntegrationApplying, IntegrationInterrupted,
		IntegrationRolledBack, IntegrationFailed, IntegrationRetained,
	}, value)
}

func validCleanup(value CleanupClass) bool {
	return slices.Contains([]CleanupClass{
		CleanupRetain, CleanupRecoverable, CleanupEligible, CleanupPending,
		CleanupComplete, CleanupOrphaned,
	}, value)
}

func normalizeSnapshot(value *Snapshot) {
	if value == nil {
		return
	}
	value.CreatedAt = value.CreatedAt.UTC()
	value.UpdatedAt = value.UpdatedAt.UTC()
	slices.SortFunc(value.Members, func(left, right MemberResource) int {
		return strings.Compare(string(left.MemberID), string(right.MemberID))
	})
	slices.SortFunc(value.Attempts, func(left, right AttemptResource) int {
		return strings.Compare(string(left.AttemptID), string(right.AttemptID))
	})
	for index := range value.Integrations {
		slices.SortFunc(value.Integrations[index].AttemptIDs, func(left, right team.AttemptID) int {
			return strings.Compare(string(left), string(right))
		})
	}
	slices.SortFunc(value.Integrations, func(left, right IntegrationResource) int {
		return strings.Compare(left.ID, right.ID)
	})
}

func cloneSnapshot(value Snapshot) Snapshot {
	value.Members = slices.Clone(value.Members)
	value.Attempts = slices.Clone(value.Attempts)
	value.Integrations = slices.Clone(value.Integrations)
	for index := range value.Integrations {
		value.Integrations[index].AttemptIDs = slices.Clone(value.Integrations[index].AttemptIDs)
	}

	return value
}
