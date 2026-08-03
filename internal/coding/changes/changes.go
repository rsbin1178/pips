// Package changes defines the application boundary for read-only workspace
// change attribution. Concrete adapters live in subpackages; this package
// deliberately contains no process execution.
package changes

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rsbin/pips/internal/coding/workspace"
)

// ErrInvalid means a snapshot or report violates the change contract.
var ErrInvalid = errors.New("coding changes: invalid value")

// Inspector captures an opaque baseline and later reports changes relative to
// it. Implementations own the snapshot format and must remain read-only.
type Inspector interface {
	Capture(context.Context) (Snapshot, error)
	Changes(context.Context, Snapshot) (Report, error)
}

// WorktreeInspector reports bounded point-in-time repository truth. It is
// deliberately separate from Inspector, whose reports attribute changes to
// one interaction baseline.
type WorktreeInspector interface {
	Status(context.Context) (WorktreeStatus, error)
}

// PathState is one Git index or worktree status column.
type PathState string

// Normalized Git path states.
const (
	PathUnchanged   PathState = ""
	PathAdded       PathState = "added"
	PathModified    PathState = "modified"
	PathDeleted     PathState = "deleted"
	PathRenamed     PathState = "renamed"
	PathCopied      PathState = "copied"
	PathTypeChanged PathState = "type_changed"
	PathUnmerged    PathState = "unmerged"
	PathUntracked   PathState = "untracked"
)

// Branch describes the current repository head without resolving or changing
// refs.
type Branch struct {
	Head     string
	OID      string
	Upstream string
	Ahead    int
	Behind   int
	Detached bool
	Unborn   bool
}

// StatusEntry preserves Git's separate index and worktree columns.
type StatusEntry struct {
	Path         string
	PreviousPath string
	Index        PathState
	Worktree     PathState
	Conflict     bool
	Submodule    string
}

// DiffSummary is a bounded section's aggregate.
type DiffSummary struct {
	Files     int
	Additions int
	Deletions int
	Binary    int
	Omitted   int
}

// DiffSection is one immutable-by-convention staged, unstaged, or untracked
// presentation payload.
type DiffSection struct {
	Summary   DiffSummary
	Diff      string
	Truncated bool
}

// WorktreeStatus is a clone-safe point-in-time repository snapshot.
type WorktreeStatus struct {
	repository       bool
	branch           Branch
	entries          []StatusEntry
	staged           DiffSection
	unstaged         DiffSection
	untracked        DiffSection
	protectedOmitted int
}

// NewWorktreeStatus validates and defensively copies a repository snapshot.
//
//nolint:gocyclo // Boundary validation deliberately keeps every malformed field fail-closed.
func NewWorktreeStatus(
	repository bool,
	branch Branch,
	entries []StatusEntry,
	staged, unstaged, untracked DiffSection,
	protectedOmitted int,
) (WorktreeStatus, error) {
	if protectedOmitted < 0 || branch.Ahead < 0 || branch.Behind < 0 {
		return WorktreeStatus{}, fmt.Errorf("%w: invalid worktree summary", ErrInvalid)
	}

	if !repository && (len(entries) != 0 || branch != (Branch{}) ||
		staged != (DiffSection{}) || unstaged != (DiffSection{}) ||
		untracked != (DiffSection{}) || protectedOmitted != 0) {
		return WorktreeStatus{}, fmt.Errorf("%w: non-repository contains status", ErrInvalid)
	}

	copyEntries := slices.Clone(entries)

	if !validStatusText(branch.Head) || !validStatusText(branch.OID) ||
		!validStatusText(branch.Upstream) {
		return WorktreeStatus{}, fmt.Errorf("%w: invalid branch metadata", ErrInvalid)
	}

	seen := make(map[string]struct{}, len(copyEntries))
	for index, entry := range copyEntries {
		normalized, err := workspace.NormalizePath(entry.Path, false)
		if err != nil || normalized != entry.Path || !validPathState(entry.Index) ||
			!validPathState(entry.Worktree) ||
			(entry.Index == PathUnchanged && entry.Worktree == PathUnchanged) ||
			!validSubmoduleState(entry.Submodule) {
			return WorktreeStatus{}, fmt.Errorf("%w: invalid status entry %d", ErrInvalid, index)
		}

		if entry.PreviousPath != "" {
			previous, previousErr := workspace.NormalizePath(entry.PreviousPath, false)
			if previousErr != nil || previous != entry.PreviousPath || previous == entry.Path {
				return WorktreeStatus{}, fmt.Errorf("%w: invalid previous path %d", ErrInvalid, index)
			}
		}

		if _, duplicate := seen[entry.Path]; duplicate {
			return WorktreeStatus{}, fmt.Errorf("%w: duplicate status path", ErrInvalid)
		}

		seen[entry.Path] = struct{}{}
	}

	for _, section := range []DiffSection{staged, unstaged, untracked} {
		if section.Summary.Files < 0 || section.Summary.Additions < 0 ||
			section.Summary.Deletions < 0 || section.Summary.Binary < 0 ||
			section.Summary.Omitted < 0 {
			return WorktreeStatus{}, fmt.Errorf("%w: invalid diff summary", ErrInvalid)
		}
	}

	return WorktreeStatus{
		repository: repository, branch: branch, entries: copyEntries,
		staged: staged, unstaged: unstaged, untracked: untracked,
		protectedOmitted: protectedOmitted,
	}, nil
}

// Repository reports whether the workspace is inside a Git work tree.
func (s WorktreeStatus) Repository() bool { return s.repository }

// Branch returns the captured branch state.
func (s WorktreeStatus) Branch() Branch { return s.branch }

// Entries returns a defensive copy of changed paths.
func (s WorktreeStatus) Entries() []StatusEntry { return slices.Clone(s.entries) }

// Staged returns the bounded index diff.
func (s WorktreeStatus) Staged() DiffSection { return s.staged }

// Unstaged returns the bounded worktree diff.
func (s WorktreeStatus) Unstaged() DiffSection { return s.unstaged }

// Untracked returns bounded previews of untracked text files.
func (s WorktreeStatus) Untracked() DiffSection { return s.untracked }

// ProtectedOmitted reports hidden product-metadata paths.
func (s WorktreeStatus) ProtectedOmitted() int { return s.protectedOmitted }

// Snapshot is immutable opaque state produced and consumed by one Inspector
// implementation. Format versions the private payload.
type Snapshot struct {
	format  string
	payload []byte
}

// NewSnapshot returns a defensively copied inspector snapshot.
func NewSnapshot(format string, payload []byte) (Snapshot, error) {
	if strings.TrimSpace(format) == "" {
		return Snapshot{}, fmt.Errorf("%w: empty snapshot format", ErrInvalid)
	}

	return Snapshot{format: format, payload: slices.Clone(payload)}, nil
}

// Format returns the implementation-owned snapshot format.
func (s Snapshot) Format() string { return s.format }

// Payload returns a defensive copy of opaque snapshot data.
func (s Snapshot) Payload() []byte { return slices.Clone(s.payload) }

// Kind classifies one observed workspace change.
type Kind string

// Change kinds shared by read-only inspectors.
const (
	KindAdded     Kind = "added"
	KindModified  Kind = "modified"
	KindDeleted   Kind = "deleted"
	KindRenamed   Kind = "renamed"
	KindUntracked Kind = "untracked"
	KindConflict  Kind = "conflict"
)

// Entry is one normalized workspace-relative changed path.
type Entry struct {
	Path         string
	PreviousPath string
	Kind         Kind
}

// Report is an immutable-by-API change list and optional bounded diff.
type Report struct {
	entries   []Entry
	diff      string
	summary   DiffSummary
	truncated bool
}

// NewReport validates and defensively copies a change report.
func NewReport(entries []Entry, diff string, truncated bool) (Report, error) {
	seen := make(map[string]struct{}, len(entries))

	copyEntries := slices.Clone(entries)
	for index, entry := range copyEntries {
		normalized, err := workspace.NormalizePath(entry.Path, false)
		if err != nil {
			return Report{}, fmt.Errorf("%w: entry %d: %w", ErrInvalid, index, err)
		}

		if normalized != entry.Path {
			return Report{}, fmt.Errorf("%w: entry %d path is not normalized", ErrInvalid, index)
		}

		if !validKind(entry.Kind) {
			return Report{}, fmt.Errorf("%w: entry %d has unknown kind %q", ErrInvalid, index, entry.Kind)
		}

		if entry.Kind == KindRenamed {
			previous, err := workspace.NormalizePath(entry.PreviousPath, false)
			if err != nil || previous != entry.PreviousPath || previous == entry.Path {
				return Report{}, fmt.Errorf("%w: entry %d has invalid previous path", ErrInvalid, index)
			}
		} else if entry.PreviousPath != "" {
			return Report{}, fmt.Errorf("%w: entry %d has unexpected previous path", ErrInvalid, index)
		}

		if _, duplicate := seen[entry.Path]; duplicate {
			return Report{}, fmt.Errorf("%w: duplicate path %q", ErrInvalid, entry.Path)
		}

		seen[entry.Path] = struct{}{}
	}

	additions, deletions := CountUnifiedDiffLines(diff)

	return Report{
		entries: copyEntries,
		diff:    diff,
		summary: DiffSummary{
			Files: len(copyEntries), Additions: additions, Deletions: deletions,
		},
		truncated: truncated,
	}, nil
}

// Entries returns a defensive copy of changed paths.
func (r Report) Entries() []Entry { return slices.Clone(r.entries) }

// Diff returns the implementation-bounded human-readable diff.
func (r Report) Diff() string { return r.diff }

// Summary returns aggregate file and visible unified-diff line counts.
func (r Report) Summary() DiffSummary { return r.summary }

// Truncated reports whether the Inspector bounded the diff or change list.
func (r Report) Truncated() bool { return r.truncated }

// CountUnifiedDiffLines counts added and deleted content lines inside unified
// diff hunks. File headers and no-newline markers are not content changes.
func CountUnifiedDiffLines(diff string) (additions, deletions int) {
	inHunk := false

	for line := range strings.SplitSeq(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case strings.HasPrefix(line, "diff --git "):
			inHunk = false
		case inHunk && strings.HasPrefix(line, "+"):
			additions++
		case inHunk && strings.HasPrefix(line, "-"):
			deletions++
		}
	}

	return additions, deletions
}

func validKind(kind Kind) bool {
	switch kind {
	case KindAdded, KindModified, KindDeleted, KindRenamed, KindUntracked, KindConflict:
		return true
	default:
		return false
	}
}

func validPathState(state PathState) bool {
	switch state {
	case PathUnchanged, PathAdded, PathModified, PathDeleted, PathRenamed,
		PathCopied, PathTypeChanged, PathUnmerged, PathUntracked:
		return true
	default:
		return false
	}
}

func validStatusText(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}

	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}

	return true
}

func validSubmoduleState(value string) bool {
	if value == "" || value == "N..." {
		return true
	}

	return len(value) == 4 && value[0] == 'S' &&
		(value[1] == 'C' || value[1] == '.') &&
		(value[2] == 'M' || value[2] == '.') &&
		(value[3] == 'U' || value[3] == '.')
}
