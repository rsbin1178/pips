//nolint:wsl_v5 // Integrity fixtures keep actions and assertions in transaction groups.
package harness

import (
	"errors"
	"testing"
	"time"

	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewSessionRejectsCorruptGraphs(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	message := ai.UserText("hello")
	valid := Entry{Kind: KindMessage, ID: "one", Time: now, Message: &message}

	tests := []struct {
		name    string
		entries []Entry
	}{
		{name: "duplicate id", entries: []Entry{valid, {Kind: KindName, ID: "one", ParentID: "one", Time: now}}},
		{name: "forward parent", entries: []Entry{{Kind: KindName, ID: "one", ParentID: "later", Time: now}}},
		{name: "unknown kind", entries: []Entry{{Kind: Kind("future"), ID: "one", Time: now}}},
		{name: "missing message", entries: []Entry{{Kind: KindMessage, ID: "one", Time: now}}},
		{name: "dangling leaf", entries: []Entry{{Kind: KindLeaf, ID: "one", Time: now, LeafID: "missing"}}},
		{name: "dangling label", entries: []Entry{{Kind: KindLabel, ID: "one", Time: now, TargetID: "missing"}}},
		{name: "off path compaction", entries: []Entry{
			valid,
			{Kind: KindName, ID: "other", Time: now},
			{
				Kind: KindCompaction, ID: "compact", ParentID: "other", Time: now,
				Summary: "summary", FirstKeptID: "one", TokensBefore: 10,
			},
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewSession(&integrityStore{entries: test.entries})
			require.ErrorIs(t, err, ErrSessionCorrupt)
		})
	}
}

func TestSessionDefensiveCopiesInputsAndOutputs(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore("copies")
	session, err := NewSession(store)
	require.NoError(t, err)

	args := ai.JSON(`{"path":"main.go"}`)
	message := ai.Assistant(ai.ToolCallPart{ID: "call", Name: "read_file", Args: args})
	usage := &ai.Usage{InputTokens: 12}
	_, err = session.AppendMessage(message, usage)
	require.NoError(t, err)
	args[2] = 'x'
	usage.InputTokens = 99

	entries := session.Entries()
	call, ok := entries[0].Message.Parts[0].(ai.ToolCallPart)
	require.True(t, ok)
	assert.JSONEq(t, `{"path":"main.go"}`, string(call.Args))
	assert.Equal(t, 12, entries[0].Usage.InputTokens)

	call.Args[2] = 'y'
	entries[0].Usage.InputTokens = 77
	again, ok := session.Entry(entries[0].ID)
	require.True(t, ok)
	againCall, ok := again.Message.Parts[0].(ai.ToolCallPart)
	require.True(t, ok)
	assert.JSONEq(t, `{"path":"main.go"}`, string(againCall.Args))
	assert.Equal(t, 12, again.Usage.InputTokens)
}

func TestMoveToWithSummaryIsOneFailureAtomicAppend(t *testing.T) {
	t.Parallel()

	store := &integrityStore{}
	session, err := NewSession(store)
	require.NoError(t, err)
	root, err := session.AppendMessage(ai.UserText("root"), nil)
	require.NoError(t, err)
	branch, err := session.AppendMessage(ai.AssistantText("branch"), nil)
	require.NoError(t, err)
	before := session.Entries()
	store.fail = true

	err = session.MoveTo(root, "abandoned")
	require.ErrorIs(t, err, errIntegrityAppend)
	assert.Equal(t, branch, session.LeafID())
	assert.Equal(t, before, session.Entries())

	store.fail = false
	require.NoError(t, session.MoveTo(root, "abandoned"))
	entries := session.Entries()
	require.Len(t, entries, len(before)+1)
	assert.Equal(t, KindBranchSummary, entries[len(entries)-1].Kind)
	assert.Equal(t, root, entries[len(entries)-1].ParentID)
	assert.Equal(t, branch, entries[len(entries)-1].FromID)
}

func TestSessionTreeProjectsActivePathAndBounds(t *testing.T) {
	t.Parallel()

	session, err := NewSession(NewMemoryStore("tree"))
	require.NoError(t, err)
	root, err := session.AppendMessage(ai.UserText("root"), nil)
	require.NoError(t, err)
	first, err := session.AppendMessage(ai.AssistantText("first"), nil)
	require.NoError(t, err)
	require.NoError(t, session.SetLabel(first, "attempt"))
	require.NoError(t, session.MoveTo(root, "branch summary"))
	current, err := session.AppendMessage(ai.AssistantText("second"), nil)
	require.NoError(t, err)

	tree, err := session.Tree(TreeLimits{})
	require.NoError(t, err)
	assert.Equal(t, current, tree.LeafID)
	assert.Equal(t, "attempt", nodeByID(t, tree.Nodes, first).Label)
	assert.False(t, nodeByID(t, tree.Nodes, first).OnActivePath)
	assert.True(t, nodeByID(t, tree.Nodes, current).Current)

	bounded, err := session.Tree(TreeLimits{MaxNodes: 2, MaxDepth: 1})
	require.NoError(t, err)
	assert.True(t, bounded.Truncated)
	assert.Equal(t, tree.TotalNodes, bounded.TotalNodes)
	assert.LessOrEqual(t, len(bounded.Nodes), 2)
}

func nodeByID(t *testing.T, nodes []TreeNode, id string) TreeNode {
	t.Helper()

	for _, node := range nodes {
		if node.ID == id {
			return node
		}
	}
	t.Fatalf("missing tree node %q", id)

	return TreeNode{}
}

var errIntegrityAppend = errors.New("append failed")

type integrityStore struct {
	entries []Entry
	fail    bool
}

func (s *integrityStore) Metadata() SessionMetadata {
	return SessionMetadata{ID: "integrity", CreatedAt: time.Now().UTC()}
}

func (s *integrityStore) Append(entry Entry) error {
	if s.fail {
		return errIntegrityAppend
	}
	s.entries = append(s.entries, cloneEntry(entry))

	return nil
}

func (s *integrityStore) Entries() ([]Entry, error) { return cloneEntries(s.entries), nil }
