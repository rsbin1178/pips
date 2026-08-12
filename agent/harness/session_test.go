package harness_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionPendingUsesActiveBranchAndCopiesArguments(t *testing.T) {
	t.Parallel()

	sess := buildSession(t)
	root := appendText(t, sess, ai.UserMessage{}, "start", nil)
	_, err := sess.AppendMessage(ai.Assistant(
		ai.ToolCallPart{ID: "c1", Name: "read_file", Args: ai.JSON(`{"path":"a.go"}`)},
		ai.ToolCallPart{ID: "c2", Name: "search_text", Args: ai.JSON(`{"query":"TODO"}`)},
	), nil)
	require.NoError(t, err)

	_, err = sess.AppendCustom("approval", ai.JSON(`{"state":"pending"}`))
	require.NoError(t, err)

	pending, err := sess.Pending()
	require.NoError(t, err)
	require.Len(t, pending, 2)
	assert.Equal(t, []string{"c1", "c2"}, []string{pending[0].ID, pending[1].ID})

	pending[0].Args[0] = 'x'
	again, err := sess.Pending()
	require.NoError(t, err)
	assert.JSONEq(t, `{"path":"a.go"}`, string(again[0].Args))

	resolved, err := sess.AppendMessage(ai.ToolResultText("c1", "read_file", "ok"), nil)
	require.NoError(t, err)
	pending, err = sess.Pending()
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "c2", pending[0].ID)

	require.NoError(t, sess.MoveTo(root, ""))
	_, err = sess.AppendMessage(ai.Assistant(
		ai.ToolCallPart{ID: "c3", Name: "review_code", Args: ai.JSON(`{}`)},
	), nil)
	require.NoError(t, err)
	pending, err = sess.Pending()
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "c3", pending[0].ID)

	require.NoError(t, sess.MoveTo(resolved, ""))
	pending, err = sess.Pending()
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "c2", pending[0].ID)

	_, err = sess.AppendMessage(ai.ToolResultText("c2", "search_text", "ok"), nil)
	require.NoError(t, err)
	pending, err = sess.Pending()
	require.NoError(t, err)
	assert.Empty(t, pending)
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

func TestContextReconstruction(t *testing.T) {
	t.Parallel()

	sess := buildSession(t)

	m1 := appendText(t, sess, ai.UserMessage{}, "old question", nil)
	appendText(t, sess, ai.AssistantMessage{}, "old answer", nil)
	keep := appendText(t, sess, ai.UserMessage{}, "recent question", nil)
	appendText(t, sess, ai.AssistantMessage{}, "recent answer", nil)

	// Bookkeeping entries never enter context.
	_, err := sess.AppendCustom("note", ai.JSON(`{}`))
	require.NoError(t, err)
	require.NoError(t, sess.SetLabel(m1, "x"))
	require.NoError(t, sess.SetName("n"))

	// No compaction: everything message-like is visible.
	cctx, err := sess.Context()
	require.NoError(t, err)
	require.Len(t, cctx.Messages, 4)

	// First compaction folds the first exchange.
	_, err = sess.AppendCompaction("first summary", keep, 100)
	require.NoError(t, err)

	cctx, err = sess.Context()
	require.NoError(t, err)
	require.Len(t, cctx.Messages, 3) // summary, recent question, recent answer

	text := firstTextPart(t, cctx.Messages[0])
	assert.Contains(t, text.Text, harness.CompactionPrefix+"first summary")

	// A later compaction wins outright.
	tail := appendText(t, sess, ai.UserMessage{}, "newest", nil)
	_, err = sess.AppendCompaction("second summary", tail, 200)
	require.NoError(t, err)

	cctx, err = sess.Context()
	require.NoError(t, err)
	require.Len(t, cctx.Messages, 2) // second summary + newest

	second := firstTextPart(t, cctx.Messages[0])
	assert.Contains(t, second.Text, "second summary")
}

func TestContextModelDerivation(t *testing.T) {
	t.Parallel()

	sess := buildSession(t)
	appendText(t, sess, ai.UserMessage{}, "hi", nil)

	_, err := sess.AppendModelChange(ai.ProviderAnthropic, "claude-sonnet-4-5")
	require.NoError(t, err)

	cctx, err := sess.Context()
	require.NoError(t, err)
	assert.Equal(t, ai.ProviderAnthropic, cctx.Provider)
	assert.Equal(t, "claude-sonnet-4-5", cctx.ModelID)
	assert.Len(t, cctx.Messages, 1, "model_change is not a message")
}

func TestMoveToBranches(t *testing.T) {
	t.Parallel()

	sess := buildSession(t)

	root := appendText(t, sess, ai.UserMessage{}, "start", nil)
	a := appendText(t, sess, ai.AssistantMessage{}, "branch a", nil)

	// Branch from the root entry with a summary of the abandoned path.
	require.NoError(t, sess.MoveTo(root, "tried branch a, dead end"))
	b := appendText(t, sess, ai.AssistantMessage{}, "branch b", nil)

	// The active branch: start → (summary) → branch b; branch a is off-path.
	cctx, err := sess.Context()
	require.NoError(t, err)
	require.Len(t, cctx.Messages, 3)

	summary := firstTextPart(t, cctx.Messages[1])
	assert.Contains(t, summary.Text, harness.BranchSummaryPrefix)

	// Tree structure: both branches share the root.
	ancestor, err := sess.CommonAncestor(a, b)
	require.NoError(t, err)
	assert.Equal(t, root, ancestor)

	// MoveTo("") returns to the root.
	require.NoError(t, sess.MoveTo("", ""))
	assert.Empty(t, sess.LeafID())

	cctx, err = sess.Context()
	require.NoError(t, err)
	assert.Empty(t, cctx.Messages)
}

func TestMoveToUnknownEntry(t *testing.T) {
	t.Parallel()

	sess := buildSession(t)
	require.ErrorIs(t, sess.MoveTo("nope", ""), harness.ErrEntryNotFound)
}

func ExampleSession_Pending() {
	store := harness.NewMemoryStore("example")
	sess, _ := harness.NewSession(store)
	_, _ = sess.AppendMessage(ai.Assistant(
		ai.ToolCallPart{ID: "call-1", Name: "write_file", Args: ai.JSON(`{"path":"main.go"}`)},
	), nil)

	pending, _ := sess.Pending()
	fmt.Println(pending[0].Name)
	// Output: write_file
}
