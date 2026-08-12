package coding

import (
	"slices"
	"time"

	"github.com/rsbin1178/pips/agent/harness"
)

// SessionNodeKind is the product-level kind of one durable Session tree node.
// It deliberately does not expose Harness types to frontends.
type SessionNodeKind string

// Session tree node kinds.
const (
	SessionNodeMessage       SessionNodeKind = "message"
	SessionNodeModelChange   SessionNodeKind = "model_change"
	SessionNodeCompaction    SessionNodeKind = "compaction"
	SessionNodeBranchSummary SessionNodeKind = "branch_summary"
	SessionNodeCustom        SessionNodeKind = "custom"
	SessionNodeLabel         SessionNodeKind = "label"
	SessionNodeName          SessionNodeKind = "name"
)

// SessionNode is one flat, bounded Session tree node.
type SessionNode struct {
	ID           string          `json:"id"`
	ParentID     string          `json:"parent_id,omitempty"`
	Kind         SessionNodeKind `json:"kind"`
	CreatedAt    time.Time       `json:"created_at"`
	Depth        int             `json:"depth"`
	Label        string          `json:"label,omitempty"`
	Current      bool            `json:"current"`
	OnActivePath bool            `json:"on_active_path"`
	HasSummary   bool            `json:"has_summary"`
	Compacted    bool            `json:"compacted"`
}

// SessionTree is the frontend-safe, flat projection of one durable Session.
type SessionTree struct {
	SessionID  string        `json:"session_id"`
	Name       string        `json:"name,omitempty"`
	LeafID     string        `json:"leaf_id,omitempty"`
	Nodes      []SessionNode `json:"nodes"`
	TotalNodes int           `json:"total_nodes"`
	MaxDepth   int           `json:"max_depth"`
	Truncated  bool          `json:"truncated"`
}

// Clone returns a detached Session tree.
func (tree SessionTree) Clone() SessionTree {
	tree.Nodes = slices.Clone(tree.Nodes)

	return tree
}

// CompactionMode identifies why compaction runs.
type CompactionMode string

// Compaction modes.
const (
	CompactionManual    CompactionMode = "manual"
	CompactionAutomatic CompactionMode = "automatic"
)

// CompactionPreview is a content-free, point-in-time compaction plan. Token
// binds confirmation to the exact durable leaf and budget snapshot.
type CompactionPreview struct {
	Available          bool   `json:"available"`
	DisabledReason     string `json:"disabled_reason,omitempty"`
	Token              string `json:"token,omitempty"`
	EstimatedTokens    int    `json:"estimated_tokens"`
	ThresholdTokens    int    `json:"threshold_tokens"`
	SummarizedMessages int    `json:"summarized_messages"`
	KeptMessages       int    `json:"kept_messages"`
	FirstKeptID        string `json:"first_kept_id,omitempty"`
	SplitTurn          bool   `json:"split_turn"`
}

// CompactionRequest confirms a preview. Instructions are content-bearing and
// are never copied into Coding events or telemetry.
type CompactionRequest struct {
	PreviewToken string
	Instructions string
}

// CompactionState is the latest reducer projection of compaction lifecycle.
type CompactionState struct {
	Active         bool              `json:"active"`
	Mode           CompactionMode    `json:"mode,omitempty"`
	Preview        CompactionPreview `json:"preview"`
	TokensBefore   int               `json:"tokens_before"`
	TokensAfter    int               `json:"tokens_after"`
	FirstKeptID    string            `json:"first_kept_id,omitempty"`
	DurationMillis int64             `json:"duration_ms"`
}

func sessionTreeFromHarness(value harness.TreeSnapshot) (SessionTree, error) {
	tree := SessionTree{
		SessionID: value.SessionID, Name: value.Name, LeafID: value.LeafID,
		TotalNodes: value.TotalNodes, MaxDepth: value.MaxDepth, Truncated: value.Truncated,
		Nodes: make([]SessionNode, 0, len(value.Nodes)),
	}
	for _, node := range value.Nodes {
		kind, err := sessionNodeKindFromHarness(node.Kind)
		if err != nil {
			return SessionTree{}, err
		}

		tree.Nodes = append(tree.Nodes, SessionNode{
			ID: node.ID, ParentID: node.ParentID, Kind: kind, CreatedAt: node.CreatedAt,
			Depth: node.Depth, Label: node.Label, Current: node.Current,
			OnActivePath: node.OnActivePath, HasSummary: node.HasSummary, Compacted: node.Compacted,
		})
	}

	return tree, nil
}

func sessionNodeKindFromHarness(kind harness.Kind) (SessionNodeKind, error) {
	switch kind {
	case harness.KindMessage:
		return SessionNodeMessage, nil
	case harness.KindModelChange:
		return SessionNodeModelChange, nil
	case harness.KindCompaction:
		return SessionNodeCompaction, nil
	case harness.KindBranchSummary:
		return SessionNodeBranchSummary, nil
	case harness.KindCustom:
		return SessionNodeCustom, nil
	case harness.KindLabel:
		return SessionNodeLabel, nil
	case harness.KindName:
		return SessionNodeName, nil
	default:
		return "", protocolError("unknown Harness tree node kind %q", kind)
	}
}

func validSessionNodeKind(kind SessionNodeKind) bool {
	switch kind {
	case SessionNodeMessage, SessionNodeModelChange, SessionNodeCompaction,
		SessionNodeBranchSummary, SessionNodeCustom, SessionNodeLabel, SessionNodeName:
		return true
	default:
		return false
	}
}

func validCompactionMode(mode CompactionMode) bool {
	return mode == CompactionManual || mode == CompactionAutomatic
}
