// Package changes defines the application boundary for read-only workspace
// change attribution. The production Git adapter is added with the P0-4
// sandbox executor; this package deliberately contains no process execution.
package changes

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

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
	Path string
	Kind Kind
}

// Report is an immutable-by-API change list and optional bounded diff.
type Report struct {
	entries   []Entry
	diff      string
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

		if _, duplicate := seen[entry.Path]; duplicate {
			return Report{}, fmt.Errorf("%w: duplicate path %q", ErrInvalid, entry.Path)
		}

		seen[entry.Path] = struct{}{}
	}

	return Report{entries: copyEntries, diff: diff, truncated: truncated}, nil
}

// Entries returns a defensive copy of changed paths.
func (r Report) Entries() []Entry { return slices.Clone(r.entries) }

// Diff returns the implementation-bounded human-readable diff.
func (r Report) Diff() string { return r.diff }

// Truncated reports whether the Inspector bounded the diff or change list.
func (r Report) Truncated() bool { return r.truncated }

func validKind(kind Kind) bool {
	switch kind {
	case KindAdded, KindModified, KindDeleted, KindRenamed, KindUntracked, KindConflict:
		return true
	default:
		return false
	}
}
