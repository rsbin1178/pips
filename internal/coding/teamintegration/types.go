package teamintegration

import (
	"errors"
	"time"
)

const (
	maximumEntries   = 100_000
	maximumPathBytes = 1024
	maximumBlobBytes = int64(64 << 20)
	maximumTreeBytes = int64(1 << 30)
)

var (
	// ErrInvalid reports malformed or incomplete integration evidence.
	ErrInvalid = errors.New("coding team integration: invalid value")
	// ErrConflict reports captured deltas that cannot be composed exactly.
	ErrConflict = errors.New("coding team integration: conflict")
	// ErrLimit reports a configured integration evidence budget exhaustion.
	ErrLimit = errors.New("coding team integration: limit exceeded")
	// ErrStale reports a changed approval or repository binding.
	ErrStale = errors.New("coding team integration: stale approval")
	// ErrConsumed reports a one-shot approval token that is no longer usable.
	ErrConsumed = errors.New("coding team integration: approval consumed")
	// ErrRecovery reports an apply state that cannot be recovered automatically.
	ErrRecovery = errors.New("coding team integration: recovery required")
)

// Limits bounds tree composition, manifest construction, and preview evidence.
type Limits struct {
	Entries   int
	PathBytes int
	BlobBytes int64
	TreeBytes int64
}

// DefaultLimits returns conservative production integration budgets.
func DefaultLimits() Limits {
	return Limits{
		Entries:   maximumEntries,
		PathBytes: maximumPathBytes,
		BlobBytes: maximumBlobBytes,
		TreeBytes: maximumTreeBytes,
	}
}

// Entry is one exact Git tree leaf supported by the integration gate.
type Entry struct {
	Mode string `json:"mode"`
	OID  string `json:"oid"`
	Path string `json:"path"`
}

// Artifact is one captured Attempt delta and its immutable identity.
type Artifact struct {
	TaskID    string  `json:"task_id"`
	AttemptID string  `json:"attempt_id"`
	BaseOID   string  `json:"base_oid"`
	ResultOID string  `json:"result_oid"`
	ResultRef string  `json:"result_ref"`
	Base      []Entry `json:"base"`
	Result    []Entry `json:"result"`
}

// Selection is the stable dependency-closed result order chosen for one gate.
type Selection struct {
	TeamID           string     `json:"team_id"`
	ResourceRevision uint64     `json:"resource_revision"`
	BaseOID          string     `json:"base_oid"`
	Base             []Entry    `json:"base"`
	Artifacts        []Artifact `json:"artifacts"`
}

// ConflictKind classifies an exact entry-level composition conflict.
type ConflictKind string

// Supported conflict classifications.
const (
	ConflictUnknown      ConflictKind = "unknown"
	ConflictAddAdd       ConflictKind = "add_add"
	ConflictModifyModify ConflictKind = "modify_modify"
	ConflictDeleteModify ConflictKind = "delete_modify"
	ConflictMode         ConflictKind = "mode"
	ConflictSymlink      ConflictKind = "symlink"
	ConflictPath         ConflictKind = "file_directory"
)

// Conflict is bounded evidence that a Task delta could not be applied exactly.
type Conflict struct {
	TaskID    string       `json:"task_id"`
	AttemptID string       `json:"attempt_id"`
	Path      string       `json:"path"`
	Kind      ConflictKind `json:"kind"`
}

// Composition is an immutable-by-convention composed tree and its evidence.
type Composition struct {
	Entries    []Entry    `json:"entries"`
	Conflicts  []Conflict `json:"conflicts"`
	Duplicates int        `json:"duplicates"`
	Digest     string     `json:"digest"`
}

// FileKind is the materialized filesystem object type.
type FileKind string

// Supported file kinds.
const (
	FileAbsent  FileKind = "absent"
	FileRegular FileKind = "regular"
	FileSymlink FileKind = "symlink"
)

// FileState is one content-addressed manifest side.
type FileState struct {
	Kind   FileKind `json:"kind"`
	Mode   string   `json:"mode,omitempty"`
	OID    string   `json:"oid,omitempty"`
	Size   int64    `json:"size,omitempty"`
	SHA256 string   `json:"sha256,omitempty"`
	Binary bool     `json:"binary,omitempty"`
}

// Operation classifies one parent Workspace path mutation.
type Operation string

// Supported manifest operations.
const (
	OperationAdd     Operation = "add"
	OperationReplace Operation = "replace"
	OperationDelete  Operation = "delete"
)

// ManifestEntry is one exact parent Workspace path transition.
type ManifestEntry struct {
	Path      string    `json:"path"`
	Operation Operation `json:"operation"`
	Base      FileState `json:"base"`
	Target    FileState `json:"target"`
}

// Manifest is the canonical Team-base to Integration-tree transition.
type Manifest struct {
	Entries []ManifestEntry `json:"entries"`
	Added   int             `json:"added"`
	Changed int             `json:"changed"`
	Deleted int             `json:"deleted"`
	Binary  int             `json:"binary"`
	Digest  string          `json:"digest"`
}

// VerificationStatus is the normalized validation outcome.
type VerificationStatus string

// Supported verification outcomes.
const (
	VerificationNotRun         VerificationStatus = "not_run"
	VerificationPassed         VerificationStatus = "passed"
	VerificationFailed         VerificationStatus = "failed"
	VerificationTimeout        VerificationStatus = "timeout"
	VerificationApprovalNeeded VerificationStatus = "approval_required"
)

// Verification is bounded evidence for one immutable Integration tree.
type Verification struct {
	Status             VerificationStatus `json:"status"`
	TreeOID            string             `json:"tree_oid"`
	CommandFingerprint string             `json:"command_fingerprint,omitempty"`
	OutputDigest       string             `json:"output_digest,omitempty"`
	ExitCode           int                `json:"exit_code,omitempty"`
	Signal             string             `json:"signal,omitempty"`
	DurationMillis     int64              `json:"duration_millis,omitempty"`
	Truncated          bool               `json:"truncated,omitempty"`
	Digest             string             `json:"digest"`
}

// ApprovalBinding contains every value authorized by one preview.
type ApprovalBinding struct {
	IntegrationID     string       `json:"integration_id"`
	WorkspaceIdentity string       `json:"workspace_identity"`
	CommonIdentity    string       `json:"common_identity"`
	BranchRef         string       `json:"branch_ref"`
	HeadOID           string       `json:"head_oid"`
	StatusDigest      string       `json:"status_digest"`
	IndexDigest       string       `json:"index_digest"`
	ResourceRevision  uint64       `json:"resource_revision"`
	SelectionDigest   string       `json:"selection_digest"`
	IntegrationCommit string       `json:"integration_commit"`
	IntegrationTree   string       `json:"integration_tree"`
	ManifestDigest    string       `json:"manifest_digest"`
	DiffDigest        string       `json:"diff_digest"`
	Verification      Verification `json:"verification"`
	ExpiresAt         time.Time    `json:"expires_at"`
}
