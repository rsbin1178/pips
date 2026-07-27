package gitcontrol

import "time"

const (
	maximumOutputBytes = 64 << 20
	maximumInputBytes  = 512 << 20
	maximumTimeout     = 5 * time.Minute
	maximumTreeEntries = 200_000
)

// Limits bounds every trusted Git process and parsed collection.
type Limits struct {
	OutputBytes int64
	InputBytes  int64
	Timeout     time.Duration
	TreeEntries int
}

// DefaultLimits returns conservative production Git-control budgets.
func DefaultLimits() Limits {
	return Limits{
		OutputBytes: 16 << 20,
		InputBytes:  256 << 20,
		Timeout:     45 * time.Second,
		TreeEntries: 100_000,
	}
}

// Repository is one resolved Git repository/worktree identity projection.
type Repository struct {
	TopLevel     string
	GitDir       string
	CommonDir    string
	ObjectFormat string
	HeadOID      string
	BranchRef    string
}

// Worktree is one record from worktree list --porcelain -z.
type Worktree struct {
	Path       string
	HeadOID    string
	BranchRef  string
	Detached   bool
	Bare       bool
	Locked     bool
	LockReason string
	Prunable   bool
}

// TreeEntry is one recursive Git tree leaf.
type TreeEntry struct {
	Mode string
	Type string
	OID  string
	Path string
}

// IndexEntry is one exact temporary-index cache entry. Mode "0" deletes Path.
type IndexEntry struct {
	Mode string
	OID  string
	Path string
}

// RefUpdate is one update-ref transaction item.
type RefUpdate struct {
	Ref    string
	NewOID string
	OldOID string
	Create bool
	Delete bool
}

// Commit identifies one commit-tree result.
type Commit struct {
	TreeOID   string
	ParentOID string
	Message   string
	Timestamp time.Time
}

type executableIdentity struct {
	path   string
	device uint64
	inode  uint64
}

type commandResult struct {
	stdout []byte
	stderr []byte
}
