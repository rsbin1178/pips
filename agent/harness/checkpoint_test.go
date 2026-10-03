//nolint:wsl_v5 // Fixtures group durable actions with their observable assertions.
package harness

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testCheckpoint() ContextCheckpoint {
	return ContextCheckpoint{
		Version: ContextCheckpointVersion, ArchiveID: "archive_0123456789-abcd",
		Messages: ai.Messages{ai.UserText("Historical summary."), ai.UserText("Continue the current task.")},
	}
}

func appendCheckpointMessage(t *testing.T, session *Session, message ai.Message, usage *ai.Usage) string {
	t.Helper()
	id, err := session.AppendMessage(message, usage)
	require.NoError(t, err)
	return id
}

func TestContextCheckpointReplayAndNavigation(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore("checkpoint")
	session, err := NewSession(store)
	require.NoError(t, err)
	old := appendCheckpointMessage(t, session, ai.UserText("old private history"), nil)
	keep := appendCheckpointMessage(t, session, ai.AssistantText("legacy retained"), nil)
	_, err = session.AppendCompaction("legacy summary", keep, 1000)
	require.NoError(t, err)
	legacy, err := session.Context()
	require.NoError(t, err)
	require.Equal(t, ai.Messages{ai.UserText(CompactionPrefix + "legacy summary"), ai.AssistantText("legacy retained")}, legacy.Messages)

	seed := testCheckpoint()
	first, err := session.AppendContextCheckpoint(session.LeafID(), seed, 1000)
	require.NoError(t, err)
	appendCheckpointMessage(t, session, ai.AssistantText("new answer"), nil)
	visible, err := session.Context()
	require.NoError(t, err)
	assert.Equal(t, append(seed.Messages, ai.AssistantText("new answer")), visible.Messages)
	assert.Len(t, session.Entries(), 5, "raw ancestors remain stored")

	secondSeed := testCheckpoint()
	secondSeed.Messages = ai.Messages{ai.UserText("Only the second seed survives.")}
	second, err := session.AppendContextCheckpoint(session.LeafID(), secondSeed, 100)
	require.NoError(t, err)
	reopened, err := NewSession(store)
	require.NoError(t, err)
	visible, err = reopened.Context()
	require.NoError(t, err)
	assert.Equal(t, secondSeed.Messages, visible.Messages)

	tree, err := reopened.Tree(TreeLimits{})
	require.NoError(t, err)
	node := nodeByID(t, tree.Nodes, second)
	assert.True(t, node.Compacted)
	assert.True(t, node.HasSummary)
	assert.Equal(t, ContextCheckpointVersion, node.CheckpointVersion)
	assert.Equal(t, KindContextCheckpoint, node.Kind)

	require.NoError(t, reopened.MoveTo(old, ""))
	visible, err = reopened.Context()
	require.NoError(t, err)
	assert.Equal(t, ai.Messages{ai.UserText("old private history")}, visible.Messages)
	appendCheckpointMessage(t, reopened, ai.AssistantText("sibling answer"), nil)
	visible, err = reopened.Context()
	require.NoError(t, err)
	assert.Equal(t, ai.Messages{ai.UserText("old private history"), ai.AssistantText("sibling answer")}, visible.Messages)

	require.NoError(t, reopened.MoveTo(first, ""))
	visible, err = reopened.Context()
	require.NoError(t, err)
	assert.Equal(t, seed.Messages, visible.Messages)
}

func TestContextCheckpointLegacyBoundaryCannotResurrectAncestors(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore("mixed")
	session, err := NewSession(store)
	require.NoError(t, err)
	old := appendCheckpointMessage(t, session, ai.UserText("must stay hidden"), nil)
	checkpoint, err := session.AppendContextCheckpoint(old, testCheckpoint(), 100)
	require.NoError(t, err)
	kept := appendCheckpointMessage(t, session, ai.UserText("post-checkpoint request"), nil)
	before := session.Entries()
	_, err = session.AppendCompaction("unsafe", old, 100)
	require.ErrorIs(t, err, ErrInvalidEntry)
	assert.Equal(t, before, session.Entries())

	_, err = session.AppendCompaction("new legacy summary", kept, 100)
	require.NoError(t, err)
	visible, err := session.Context()
	require.NoError(t, err)
	assert.Equal(t, ai.Messages{ai.UserText(CompactionPrefix + "new legacy summary"), ai.UserText("post-checkpoint request")}, visible.Messages)
	_, err = NewSession(store)
	require.NoError(t, err)

	require.NoError(t, session.MoveTo(checkpoint, ""))
	_, err = session.AppendCompaction("retain seed", checkpoint, 100)
	require.NoError(t, err)
	visible, err = session.Context()
	require.NoError(t, err)
	assert.Equal(t, append(ai.Messages{ai.UserText(CompactionPrefix + "retain seed")}, testCheckpoint().Messages...), visible.Messages)

	bad := Entry{Kind: KindCompaction, ID: "bad", ParentID: checkpoint, Time: time.Now().UTC(), Summary: "bad", FirstKeptID: old}
	require.NoError(t, store.Append(bad))
	_, err = NewSession(store)
	require.ErrorIs(t, err, ErrSessionCorrupt)
}

func TestContextCheckpointAppendGuardsAndStoreFailure(t *testing.T) {
	t.Parallel()
	store := &integrityStore{}
	session, err := NewSession(store)
	require.NoError(t, err)
	old := appendCheckpointMessage(t, session, ai.UserText("request"), nil)
	current := appendCheckpointMessage(t, session, ai.AssistantText("response"), nil)
	before := session.Entries()
	_, err = session.AppendContextCheckpoint(old, testCheckpoint(), 10)
	require.ErrorIs(t, err, ErrStaleContextCheckpoint)
	assert.Equal(t, before, session.Entries())

	store.fail = true
	_, err = session.AppendContextCheckpoint(current, testCheckpoint(), 10)
	require.ErrorIs(t, err, errIntegrityAppend)
	assert.Equal(t, current, session.LeafID())
	assert.Equal(t, before, session.Entries())
	store.fail = false
	// A failed checkpoint append fences this writer even when this test Store
	// happens to know that nothing was written. Revalidate before continuing.
	session, err = NewSession(store)
	require.NoError(t, err)

	call := appendCheckpointMessage(t, session, ai.Assistant(ai.ToolCallPart{ID: "call", Name: "read", Args: ai.JSON(`{}`)}), nil)
	_, err = session.AppendContextCheckpoint(call, testCheckpoint(), 10)
	require.ErrorIs(t, err, agent.ErrPendingToolCalls)
	assert.Equal(t, call, session.LeafID())
	seed := testCheckpoint()
	require.NoError(t, store.Append(Entry{Kind: KindContextCheckpoint, ID: "unsafe", ParentID: call, Time: time.Now().UTC(), Checkpoint: &seed}))
	_, err = NewSession(store)
	require.ErrorIs(t, err, ErrSessionCorrupt)
	require.ErrorIs(t, err, agent.ErrPendingToolCalls)
}

func TestContextCheckpointConcurrentExpectedLeaf(t *testing.T) {
	t.Parallel()
	session, err := NewSession(NewMemoryStore("concurrent"))
	require.NoError(t, err)
	leaf := appendCheckpointMessage(t, session, ai.UserText("request"), nil)
	results := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Go(func() {
			_, appendErr := session.AppendContextCheckpoint(leaf, testCheckpoint(), 10)
			results <- appendErr
		})
	}
	group.Wait()
	close(results)
	succeeded := 0
	for appendErr := range results {
		if appendErr == nil {
			succeeded++
		} else {
			require.ErrorIs(t, appendErr, ErrStaleContextCheckpoint)
		}
	}
	assert.Equal(t, 1, succeeded)
	assert.Len(t, session.Entries(), 2)
}

func TestContextCheckpointValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(*ContextCheckpoint)
	}{
		{"unknown version", func(c *ContextCheckpoint) { c.Version++ }},
		{"missing archive", func(c *ContextCheckpoint) { c.ArchiveID = "" }},
		{"traversal", func(c *ContextCheckpoint) { c.ArchiveID = "../archive" }},
		{"absolute archive", func(c *ContextCheckpoint) { c.ArchiveID = "/private/archive" }},
		{"no messages", func(c *ContextCheckpoint) { c.Messages = nil }},
		{"blank", func(c *ContextCheckpoint) { c.Messages = ai.Messages{ai.UserText(" \n ")} }},
		{"system", func(c *ContextCheckpoint) { c.Messages = ai.Messages{ai.SystemText("privilege")} }},
		{"pointer", func(c *ContextCheckpoint) { m := ai.UserText("text"); c.Messages = ai.Messages{&m} }},
		{"call", func(c *ContextCheckpoint) {
			c.Messages = ai.Messages{ai.Assistant(ai.ToolCallPart{ID: "c", Name: "tool", Args: ai.JSON(`{}`)})}
		}},
		{"result", func(c *ContextCheckpoint) { c.Messages = ai.Messages{ai.ToolResultText("c", "tool", "result")} }},
		{"reasoning", func(c *ContextCheckpoint) { c.Messages = ai.Messages{ai.Assistant(ai.ReasoningPart{Text: "private"})} }},
		{"too many", func(c *ContextCheckpoint) { c.Messages = make(ai.Messages, MaxContextCheckpointMessages+1) }},
		{"invalid utf8", func(c *ContextCheckpoint) { c.Messages = ai.Messages{ai.UserText("\xff")} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			seed := testCheckpoint()
			test.edit(&seed)
			session, err := NewSession(NewMemoryStore("invalid"))
			require.NoError(t, err)
			_, err = session.AppendContextCheckpoint("", seed, 0)
			require.ErrorIs(t, err, ErrInvalidEntry)
			assert.Empty(t, session.Entries())
		})
	}
}

func TestContextCheckpointEncodedBudget(t *testing.T) {
	t.Parallel()
	seed := testCheckpoint()
	// HTML escaping expands each rune to six JSON bytes. Raw text length is
	// deliberately below the payload limit; the actual encoded size is not.
	seed.Messages = ai.Messages{ai.UserText(strings.Repeat("<", MaxContextCheckpointBytes/6))}
	require.Error(t, seed.Validate())
	session, err := NewSession(NewMemoryStore("oversized"))
	require.NoError(t, err)
	_, err = session.AppendContextCheckpoint("", seed, 0)
	require.ErrorIs(t, err, ErrInvalidEntry)
	assert.Empty(t, session.Entries())
}

func TestContextCheckpointDefensiveCopies(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore("copies")
	session, err := NewSession(store)
	require.NoError(t, err)
	seed := testCheckpoint()
	media := []byte{1, 2, 3}
	seed.Messages = append(seed.Messages, ai.User(ai.ImagePart{Source: ai.MediaSource{Data: media, MIMEType: "image/png"}}))
	id, err := session.AppendContextCheckpoint("", seed, 10)
	require.NoError(t, err)
	seed.Messages[0] = ai.UserText("changed")
	media[0] = 9
	entry, ok := session.Entry(id)
	require.True(t, ok)
	assert.Equal(t, testCheckpoint().Messages[0], entry.Checkpoint.Messages[0])
	user, ok := entry.Checkpoint.Messages[2].(ai.UserMessage)
	require.True(t, ok)
	image, ok := user.Parts[0].(ai.ImagePart)
	require.True(t, ok)
	assert.Equal(t, byte(1), image.Source.Data[0])
	image.Source.Data[0] = 8
	entry.Checkpoint.Messages[0] = ai.UserText("changed again")
	visible, err := session.Context()
	require.NoError(t, err)
	assert.Equal(t, testCheckpoint().Messages[0], visible.Messages[0])
	visible.Messages[0] = ai.UserText("changed context")
	reopened, err := NewSession(store)
	require.NoError(t, err)
	visible, err = reopened.Context()
	require.NoError(t, err)
	assert.Equal(t, testCheckpoint().Messages[0], visible.Messages[0])
}

func TestContextEstimateReplacementAndFreshUsage(t *testing.T) {
	t.Parallel()
	session, err := NewSession(NewMemoryStore("estimate"))
	require.NoError(t, err)
	appendCheckpointMessage(t, session, ai.UserText(strings.Repeat("word", 1000)), nil)
	appendCheckpointMessage(t, session, ai.AssistantText("old"), &ai.Usage{InputTokens: 9000, OutputTokens: 1000})
	assert.Equal(t, 10000, EstimateContext(session.Path()))
	seed := testCheckpoint()
	seed.Messages = ai.Messages{ai.UserText(strings.Repeat("word", 1000))}
	checkpoint, err := session.AppendContextCheckpoint(session.LeafID(), seed, 10000)
	require.NoError(t, err)
	assert.Equal(t, ContextEstimate{Tokens: 1000, BoundaryID: checkpoint}, EstimateContextUsage(session.Path()))

	usageID := appendCheckpointMessage(t, session, ai.Assistant(ai.ToolCallPart{ID: "c", Name: "read", Args: ai.JSON(`{}`)}), &ai.Usage{
		InputTokens: 1000, OutputTokens: 200, CachedInputTokens: 800, CacheWriteTokens: 100, ReasoningTokens: 50,
	})
	assert.Equal(t, ContextEstimate{Tokens: 1200, ProviderBaseline: true, BoundaryID: checkpoint, UsageEntryID: usageID}, EstimateContextUsage(session.Path()))
	appendCheckpointMessage(t, session, ai.ToolResultText("c", "read", strings.Repeat("word", 200)), nil)
	assert.Equal(t, 1400, EstimateContext(session.Path()))
	appendCheckpointMessage(t, session, ai.AssistantText("fresh"), &ai.Usage{InputTokens: 1500, OutputTokens: 200})
	assert.Equal(t, 1700, EstimateContext(session.Path()), "fresh usage replaces, never adds to the old gauge")
	appendCheckpointMessage(t, session, ai.AssistantText("tail"), &ai.Usage{})
	assert.Equal(t, 1701, EstimateContext(session.Path()), "empty usage does not erase a usable baseline")
	boundary, err := session.AppendModelChange(ai.Provider("new"), "new-model")
	require.NoError(t, err)
	estimate := EstimateContextUsage(session.Path())
	assert.False(t, estimate.ProviderBaseline)
	assert.Equal(t, boundary, estimate.BoundaryID)
	assert.Empty(t, estimate.UsageEntryID)
}

func TestContextEstimateInvalidatesRetainedLegacyUsage(t *testing.T) {
	t.Parallel()
	session, err := NewSession(NewMemoryStore("legacy-estimate"))
	require.NoError(t, err)
	appendCheckpointMessage(t, session, ai.UserText(strings.Repeat("word", 1000)), nil)
	keep := appendCheckpointMessage(t, session, ai.UserText(strings.Repeat("word", 100)), nil)
	appendCheckpointMessage(t, session, ai.AssistantText("retained"), &ai.Usage{InputTokens: 9000, OutputTokens: 1000})
	boundary, err := session.AppendCompaction("small summary", keep, 10000)
	require.NoError(t, err)
	visible, err := session.Context()
	require.NoError(t, err)
	want := 0
	for _, message := range visible.Messages {
		want += EstimateTokens(message)
	}
	estimate := EstimateContextUsage(session.Path())
	assert.Equal(t, ContextEstimate{Tokens: want, BoundaryID: boundary}, estimate)
	assert.Less(t, estimate.Tokens, 1000)
}
