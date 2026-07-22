//nolint:wsl_v5 // Flat tree projection keeps each bounded node step visible in order.
package harness

import (
	"fmt"
	"slices"
	"time"
)

const (
	// DefaultTreeMaxNodes bounds one tree projection while retaining useful
	// interactive histories without exposing an unbounded allocation surface.
	DefaultTreeMaxNodes = 4096
	// DefaultTreeMaxDepth bounds parent traversal for one projected node.
	DefaultTreeMaxDepth = 256
)

// TreeLimits bound a [Session.Tree] projection. Zero values select defaults.
type TreeLimits struct {
	MaxNodes int
	MaxDepth int
}

// TreeSnapshot is an immutable, append-ordered projection of a Session graph.
// Nodes are flat so renderers never recurse over untrusted depth.
type TreeSnapshot struct {
	SessionID  string
	Name       string
	LeafID     string
	Nodes      []TreeNode
	TotalNodes int
	MaxDepth   int
	Truncated  bool
}

// TreeNode is one durable non-leaf-marker entry in a [TreeSnapshot].
type TreeNode struct {
	ID           string
	ParentID     string
	Kind         Kind
	CreatedAt    time.Time
	Depth        int
	Label        string
	Current      bool
	OnActivePath bool
	HasSummary   bool
	Compacted    bool
}

// Tree returns a bounded defensive projection of the durable Session graph.
func (s *Session) Tree(limits TreeLimits) (TreeSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	limits = withTreeDefaults(limits)
	path, err := s.pathLocked(s.leaf)
	if err != nil {
		return TreeSnapshot{}, err
	}

	onPath := make(map[string]struct{}, len(path))
	for _, entry := range path {
		onPath[entry.ID] = struct{}{}
	}
	labels := labelsFromEntries(s.entries)
	depths := make(map[string]int, len(s.entries))
	snapshot := TreeSnapshot{
		SessionID: s.store.Metadata().ID,
		Name:      nameFromEntries(s.entries),
		LeafID:    s.leaf,
	}

	for _, entry := range s.entries {
		if entry.Kind == KindLeaf {
			continue
		}
		snapshot.TotalNodes++
		depth := 0
		if entry.ParentID != "" {
			parentDepth, ok := depths[entry.ParentID]
			if !ok {
				return TreeSnapshot{}, fmt.Errorf("%w: missing projected parent %q", ErrSessionCorrupt, entry.ParentID)
			}
			depth = parentDepth + 1
		}
		depths[entry.ID] = depth
		snapshot.MaxDepth = max(snapshot.MaxDepth, depth)
		if depth > limits.MaxDepth || len(snapshot.Nodes) >= limits.MaxNodes {
			snapshot.Truncated = true
			continue
		}
		_, active := onPath[entry.ID]
		snapshot.Nodes = append(snapshot.Nodes, TreeNode{
			ID: entry.ID, ParentID: entry.ParentID, Kind: entry.Kind,
			CreatedAt: entry.Time, Depth: depth, Label: labels[entry.ID],
			Current: entry.ID == s.leaf, OnActivePath: active,
			HasSummary: entry.Kind == KindCompaction || entry.Kind == KindBranchSummary,
			Compacted:  entry.Kind == KindCompaction,
		})
	}

	snapshot.Nodes = slices.Clone(snapshot.Nodes)

	return snapshot, nil
}

func withTreeDefaults(limits TreeLimits) TreeLimits {
	if limits.MaxNodes <= 0 {
		limits.MaxNodes = DefaultTreeMaxNodes
	}
	if limits.MaxDepth <= 0 {
		limits.MaxDepth = DefaultTreeMaxDepth
	}

	return limits
}

func labelsFromEntries(entries []Entry) map[string]string {
	labels := make(map[string]string)
	for _, entry := range entries {
		if entry.Kind != KindLabel {
			continue
		}
		if entry.Label == "" {
			delete(labels, entry.TargetID)
		} else {
			labels[entry.TargetID] = entry.Label
		}
	}

	return labels
}

func nameFromEntries(entries []Entry) string {
	name := ""
	for _, entry := range entries {
		if entry.Kind == KindName {
			name = entry.Name
		}
	}

	return name
}
