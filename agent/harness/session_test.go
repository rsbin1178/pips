package harness_test

import (
	"os"
	"testing"

	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

func TestContextReconstruction(t *testing.T) {
	t.Parallel()

	sess := buildSession(t)

	m1 := appendText(t, sess, ai.RoleUser, "old question", nil)
	appendText(t, sess, ai.RoleAssistant, "old answer", nil)
	keep := appendText(t, sess, ai.RoleUser, "recent question", nil)
	appendText(t, sess, ai.RoleAssistant, "recent answer", nil)

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

	text, ok := cctx.Messages[0].Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Contains(t, text.Text, harness.CompactionPrefix+"first summary")

	// A later compaction wins outright.
	tail := appendText(t, sess, ai.RoleUser, "newest", nil)
	_, err = sess.AppendCompaction("second summary", tail, 200)
	require.NoError(t, err)

	cctx, err = sess.Context()
	require.NoError(t, err)
	require.Len(t, cctx.Messages, 2) // second summary + newest

	second, ok := cctx.Messages[0].Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Contains(t, second.Text, "second summary")
}

func TestContextModelDerivation(t *testing.T) {
	t.Parallel()

	sess := buildSession(t)
	appendText(t, sess, ai.RoleUser, "hi", nil)

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

	root := appendText(t, sess, ai.RoleUser, "start", nil)
	a := appendText(t, sess, ai.RoleAssistant, "branch a", nil)

	// Branch from the root entry with a summary of the abandoned path.
	require.NoError(t, sess.MoveTo(root, "tried branch a, dead end"))
	b := appendText(t, sess, ai.RoleAssistant, "branch b", nil)

	// The active branch: start → (summary) → branch b; branch a is off-path.
	cctx, err := sess.Context()
	require.NoError(t, err)
	require.Len(t, cctx.Messages, 3)

	summary, ok := cctx.Messages[1].Parts[0].(ai.TextPart)
	require.True(t, ok)
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
