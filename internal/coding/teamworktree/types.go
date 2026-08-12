package teamworktree

import (
	"time"

	"github.com/rsbin1178/pips/agent/team"
	"github.com/rsbin1178/pips/internal/coding/teamstate"
)

const (
	maximumFiles       = 200_000
	maximumBytes       = int64(4 << 30)
	maximumFileBytes   = int64(1 << 30)
	maximumPathBytes   = 4096
	maximumGitBytes    = int64(64 << 20)
	maximumGitDuration = 5 * time.Minute
)

// Limits bounds Worktree materialization, scanning, and trusted Git execution.
type Limits struct {
	Files       int
	Bytes       int64
	FileBytes   int64
	PathBytes   int
	GitBytes    int64
	GitDuration time.Duration
}

// DefaultLimits returns conservative production control-plane budgets.
func DefaultLimits() Limits {
	return Limits{
		Files: 100_000, Bytes: 1 << 30, FileBytes: 256 << 20,
		PathBytes: 1024, GitBytes: 16 << 20, GitDuration: 45 * time.Second,
	}
}

// Options configures the trusted Worktree Manager.
type Options struct {
	GitPath       string
	ProductRoot   string
	WorktreesRoot string
	LeasesRoot    string
	Limits        Limits
}

// Owner is the stable application identity of one writable Attempt resource.
type Owner struct {
	TeamID          team.ID
	MemberID        team.MemberID
	AttemptID       team.AttemptID
	LeaseGeneration uint64
}

// CreateRequest identifies an exact parent Workspace and immutable base commit.
type CreateRequest struct {
	Owner     Owner
	Workspace string
	BaseOID   string
}

// FileIdentity binds a canonical absolute path to one filesystem object.
type FileIdentity struct {
	Path   string
	Device uint64
	Inode  uint64
}

// Resource is the exact external identity of one Attempt Worktree.
type Resource struct {
	ID              string
	Owner           Owner
	Workspace       FileIdentity
	Directory       FileIdentity
	GitDir          FileIdentity
	CommonDir       FileIdentity
	ObjectFormat    string
	BranchRef       string
	ResultRef       string
	BaseOID         string
	ResultCommitOID string
	LockReason      string
}

// TeamState projects the resource into the durable application resource schema.
func (r Resource) TeamState() teamstate.WorktreeResource {
	return teamstate.WorktreeResource{
		ID: r.ID,
		Workspace: teamstate.FileIdentity{
			Path: r.Workspace.Path, Device: r.Workspace.Device, Inode: r.Workspace.Inode,
		},
		Directory: teamstate.FileIdentity{
			Path: r.Directory.Path, Device: r.Directory.Device, Inode: r.Directory.Inode,
		},
		GitDir: teamstate.FileIdentity{
			Path: r.GitDir.Path, Device: r.GitDir.Device, Inode: r.GitDir.Inode,
		},
		CommonDir: teamstate.FileIdentity{
			Path: r.CommonDir.Path, Device: r.CommonDir.Device, Inode: r.CommonDir.Inode,
		},
		ObjectFormat: r.ObjectFormat,
		BranchRef:    r.BranchRef, BaseOID: r.BaseOID, ResultRef: r.ResultRef,
		ResultCommitOID: r.ResultCommitOID, LeaseGeneration: r.Owner.LeaseGeneration,
		LockReason: r.LockReason,
	}
}

// State classifies exact current Worktree state.
type State string

// Supported Worktree state classifications.
const (
	StateUnchangedClean    State = "unchanged_clean"
	StateChanged           State = "changed"
	StateCapturedClean     State = "captured_clean"
	StateDirtyAfterCapture State = "dirty_after_capture"
)

// Status is one bounded read-only resource projection.
type Status struct {
	State          State
	HeadOID        string
	TreeOID        string
	ManifestDigest string
	Files          int
	Bytes          int64
}

// CaptureRequest supplies bounded generated commit metadata.
type CaptureRequest struct {
	Message string
	Time    time.Time
}

// Capture is the durable result evidence published by Capture.
type Capture struct {
	Resource       Resource
	CommitOID      string
	TreeOID        string
	ManifestDigest string
	Files          int
	Bytes          int64
	Added          int
	Modified       int
	Deleted        int
}

// Cleanup reports exact resource deletion while preserving a captured result ref.
type Cleanup struct {
	BranchDeleted   bool
	WorktreeRemoved bool
	ResultRetained  bool
}
