// Package teamstate persists Coding Team application resource bindings.
package teamstate

import (
	"time"

	"github.com/rsbin/pips/agent/continuation"
	"github.com/rsbin/pips/agent/team"
)

// State is the application lifecycle of one Team's resources.
type State string

// Team application resource states.
const (
	StateAdmitted                 State = "admitted"
	StateProvisioning             State = "provisioning"
	StateActive                   State = "active"
	StateWorkComplete             State = "work_complete"
	StateIntegrationPending       State = "integration_pending"
	StateIntegrated               State = "integrated"
	StateClosedWithoutIntegration State = "closed_without_integration"
	StateCanceling                State = "canceling"
	StateCancelled                State = "cancelled"
	StateInterrupted              State = "interrupted"
	StateBlockedIdentity          State = "blocked_identity"
	StateBlockedConflict          State = "blocked_conflict"
	StateFailed                   State = "failed"
)

// Admission identifies the workspace baseline admitted by the user.
type Admission string

// Supported Team admission modes.
const (
	AdmissionClean    Admission = "clean"
	AdmissionHEADOnly Admission = "head_only"
)

// AttemptState classifies the application resources of one Team Attempt.
type AttemptState string

// Attempt application resource states.
const (
	AttemptPlanned       AttemptState = "planned"
	AttemptBasePrepared  AttemptState = "base_prepared"
	AttemptWorktreeReady AttemptState = "worktree_ready"
	AttemptSessionReady  AttemptState = "session_ready"
	AttemptRunning       AttemptState = "running"
	AttemptCapturing     AttemptState = "capturing"
	AttemptCaptured      AttemptState = "captured"
	AttemptTerminal      AttemptState = "terminal"
	AttemptInterrupted   AttemptState = "interrupted"
	AttemptRecoverable   AttemptState = "recoverable"
	AttemptFailed        AttemptState = "failed"
	AttemptCancelled     AttemptState = "cancelled"
	AttemptConflicted    AttemptState = "conflicted"
	AttemptCaptureFailed AttemptState = "capture_failed"
	AttemptOrphaned      AttemptState = "orphaned"
)

// IntegrationState classifies one integration resource.
type IntegrationState string

// Integration resource states.
const (
	IntegrationPlanned  IntegrationState = "planned"
	IntegrationReady    IntegrationState = "ready"
	IntegrationVerified IntegrationState = "verified"
	IntegrationApproved IntegrationState = "approved"
	IntegrationApplied  IntegrationState = "applied"
	IntegrationFailed   IntegrationState = "failed"
	IntegrationRetained IntegrationState = "retained"
)

// CleanupClass states what may happen to retained resources. It is a
// classification only; this package never performs cleanup.
type CleanupClass string

// Cleanup classifications.
const (
	CleanupRetain      CleanupClass = "retain"
	CleanupRecoverable CleanupClass = "recoverable"
	CleanupEligible    CleanupClass = "eligible"
	CleanupPending     CleanupClass = "pending"
	CleanupComplete    CleanupClass = "complete"
	CleanupOrphaned    CleanupClass = "orphaned"
)

// FileIdentity binds a canonical absolute path to one filesystem object.
type FileIdentity struct {
	Path   string `json:"path"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

// ParentResource identifies the Lead conversation and its Workspace.
type ParentResource struct {
	SessionID   string       `json:"session_id"`
	WorkspaceID string       `json:"workspace_id"`
	Workspace   FileIdentity `json:"workspace"`
}

// RepositoryResource records the immutable Team admission baseline.
type RepositoryResource struct {
	CommonDir FileIdentity `json:"common_dir"`
	BaseOID   string       `json:"base_oid"`
	BranchRef string       `json:"branch_ref,omitempty"`
	Admission Admission    `json:"admission"`
}

// MemberResource binds one logical Team member to a capability profile.
type MemberResource struct {
	MemberID                     team.MemberID `json:"member_id"`
	CapabilityProfileFingerprint string        `json:"capability_profile_fingerprint"`
}

// WorkerSessionResource binds an Attempt to its real Worker Workspace.
type WorkerSessionResource struct {
	SessionID   string `json:"session_id"`
	WorkspaceID string `json:"workspace_id"`
}

// WorktreeResource records application-observed Worktree identity. Empty
// fields are valid before the Worktree is provisioned.
type WorktreeResource struct {
	ID              string       `json:"id,omitempty"`
	Workspace       FileIdentity `json:"workspace,omitzero"`
	Directory       FileIdentity `json:"directory,omitzero"`
	GitDir          FileIdentity `json:"git_dir,omitzero"`
	CommonDir       FileIdentity `json:"common_dir,omitzero"`
	ObjectFormat    string       `json:"object_format,omitempty"`
	BranchRef       string       `json:"branch_ref,omitempty"`
	BaseOID         string       `json:"base_oid,omitempty"`
	ResultRef       string       `json:"result_ref,omitempty"`
	ResultCommitOID string       `json:"result_commit_oid,omitempty"`
	LockReason      string       `json:"lock_reason,omitempty"`
	LeaseGeneration uint64       `json:"lease_generation,omitempty"`
}

// AttemptResource binds one Team task attempt to all external resources.
type AttemptResource struct {
	TaskID         team.TaskID           `json:"task_id"`
	AttemptID      team.AttemptID        `json:"attempt_id"`
	MemberID       team.MemberID         `json:"member_id"`
	ContinuationID continuation.ID       `json:"continuation_id"`
	Session        WorkerSessionResource `json:"session,omitzero"`
	Worktree       WorktreeResource      `json:"worktree,omitzero"`
	State          AttemptState          `json:"state"`
	Cleanup        CleanupClass          `json:"cleanup"`
}

// IntegrationResource binds one integration attempt to selected Team results.
type IntegrationResource struct {
	ID                string           `json:"id"`
	AttemptIDs        []team.AttemptID `json:"attempt_ids"`
	Worktree          WorktreeResource `json:"worktree,omitzero"`
	State             IntegrationState `json:"state"`
	DiffDigest        string           `json:"diff_digest,omitempty"`
	ApprovalTokenHash string           `json:"approval_token_hash,omitempty"`
	Cleanup           CleanupClass     `json:"cleanup"`
}

// Snapshot is the latest full application resource truth for one Team.
type Snapshot struct {
	TeamID       team.ID               `json:"team_id"`
	Revision     team.Revision         `json:"revision"`
	State        State                 `json:"state"`
	Parent       ParentResource        `json:"parent"`
	Repository   RepositoryResource    `json:"repository"`
	Members      []MemberResource      `json:"members"`
	Attempts     []AttemptResource     `json:"attempts"`
	Integrations []IntegrationResource `json:"integrations,omitempty"`
	Cleanup      CleanupClass          `json:"cleanup"`
	CreatedAt    time.Time             `json:"created_at"`
	UpdatedAt    time.Time             `json:"updated_at"`
}

// Mutation is one idempotent optimistic update. Snapshot.Revision must equal
// ExpectedRevision + 1.
type Mutation struct {
	CommandID        team.CommandID `json:"command_id"`
	ExpectedRevision team.Revision  `json:"expected_revision"`
	Snapshot         Snapshot       `json:"snapshot"`
}

// Limits bound resource journals and reconciliation projections.
type Limits struct {
	MaxFileBytes    int64
	MaxRecordBytes  int
	MaxRecords      int
	MaxTeams        int
	MaxMembers      int
	MaxAttempts     int
	MaxIntegrations int
	MaxListResults  int
}

// DefaultLimits returns production resource journal limits.
func DefaultLimits() Limits {
	return Limits{
		MaxFileBytes:    16 << 20,
		MaxRecordBytes:  1 << 20,
		MaxRecords:      10_000,
		MaxTeams:        10_000,
		MaxMembers:      128,
		MaxAttempts:     10_000,
		MaxIntegrations: 1_000,
		MaxListResults:  1_000,
	}
}
