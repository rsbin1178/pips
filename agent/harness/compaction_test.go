package harness_test

import (
	"strings"
	"testing"

	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bigText returns text estimating to roughly n tokens (4 chars per token).
func bigText(n int) string {
	return strings.Repeat("word", n)
}

func TestEstimateContextUsesUsageAndTail(t *testing.T) {
	t.Parallel()

	sess := buildSession(t)

	appendText(t, sess, ai.RoleUser, bigText(1000), nil)
	appendText(t, sess, ai.RoleAssistant, "answer", &ai.Usage{InputTokens: 900, OutputTokens: 100})
	appendText(t, sess, ai.RoleUser, bigText(200), nil) // trailing, estimated

	tokens := harness.EstimateContext(sess.Path())
	assert.InDelta(t, 1200, tokens, 10, "usage (1000) + trailing estimate (~200)")
}

func TestShouldCompact(t *testing.T) {
	t.Parallel()

	s := harness.Settings{ContextTokens: 100_000, ReserveTokens: 16_384}
	assert.False(t, harness.ShouldCompact(50_000, s))
	assert.True(t, harness.ShouldCompact(90_000, s))
	assert.False(t, harness.ShouldCompact(90_000, harness.Settings{}), "no window, never auto-compacts")
}

func TestPrepareCutsAtUserMessage(t *testing.T) {
	t.Parallel()

	sess := buildSession(t)

	appendText(t, sess, ai.RoleUser, bigText(500), nil)
	appendText(t, sess, ai.RoleAssistant, bigText(500), nil)
	keep := appendText(t, sess, ai.RoleUser, bigText(500), nil)
	appendText(t, sess, ai.RoleAssistant, bigText(500), nil)

	// keepRecent ≈ the last exchange: the cut lands on the second user turn.
	prep := harness.Prepare(sess.Path(), harness.Settings{KeepRecentTokens: 800})
	require.NotNil(t, prep)

	assert.Equal(t, keep, prep.FirstKeptID)
	assert.False(t, prep.SplitTurn)
	assert.Len(t, prep.ToSummarize, 2)
	assert.Empty(t, prep.Previous)
}

func TestPrepareNeverCutsToolResults(t *testing.T) {
	t.Parallel()

	sess := buildSession(t)

	appendText(t, sess, ai.RoleUser, bigText(100), nil)

	// A turn: assistant tool call + tool result, then the final answer.
	callMsg := ai.Assistant(ai.ToolCallPart{ID: "c1", Name: "search", Args: ai.JSON(`{}`)})
	_, err := sess.AppendMessage(callMsg, nil)
	require.NoError(t, err)

	resultMsg := ai.ToolResultText("c1", "search", bigText(2000))
	_, err = sess.AppendMessage(resultMsg, nil)
	require.NoError(t, err)

	appendText(t, sess, ai.RoleAssistant, bigText(100), nil)

	// keepRecent lands inside the tool result — the cut must move to a
	// message boundary, never between the call and its result.
	prep := harness.Prepare(sess.Path(), harness.Settings{KeepRecentTokens: 500})
	require.NotNil(t, prep)

	kept, ok := sess.Entry(prep.FirstKeptID)
	require.True(t, ok)
	require.Equal(t, harness.KindMessage, kept.Kind)
	assert.NotEqual(t, ai.RoleTool, kept.Message.Role)
}

func TestPrepareSplitTurn(t *testing.T) {
	t.Parallel()

	sess := buildSession(t)

	appendText(t, sess, ai.RoleUser, "old", nil)
	appendText(t, sess, ai.RoleAssistant, "old answer", nil)
	appendText(t, sess, ai.RoleUser, bigText(100), nil) // turn start
	appendText(t, sess, ai.RoleAssistant, bigText(3000), nil)
	appendText(t, sess, ai.RoleAssistant, bigText(400), nil) // kept suffix

	prep := harness.Prepare(sess.Path(), harness.Settings{KeepRecentTokens: 500})
	require.NotNil(t, prep)

	// The cut lands on the giant mid-turn assistant message → split turn.
	assert.True(t, prep.SplitTurn)
	assert.NotEmpty(t, prep.TurnPrefix, "prefix of the split turn is summarized separately")
	assert.Len(t, prep.ToSummarize, 2, "history before the split turn")
}

func TestPrepareIterativeCarriesPrevious(t *testing.T) {
	t.Parallel()

	sess := buildSession(t)

	appendText(t, sess, ai.RoleUser, bigText(500), nil)
	keep := appendText(t, sess, ai.RoleUser, bigText(500), nil)
	_, err := sess.AppendCompaction("earlier summary", keep, 100)
	require.NoError(t, err)

	appendText(t, sess, ai.RoleUser, bigText(800), nil)
	appendText(t, sess, ai.RoleUser, bigText(200), nil)

	prep := harness.Prepare(sess.Path(), harness.Settings{KeepRecentTokens: 100})
	require.NotNil(t, prep)
	assert.Equal(t, "earlier summary", prep.Previous)
}

func TestPrepareNothingToCompact(t *testing.T) {
	t.Parallel()

	sess := buildSession(t)
	assert.Nil(t, harness.Prepare(sess.Path(), harness.Settings{}), "empty session")

	keep := appendText(t, sess, ai.RoleUser, "hi", nil)
	_, err := sess.AppendCompaction("s", keep, 1)
	require.NoError(t, err)
	assert.Nil(t, harness.Prepare(sess.Path(), harness.Settings{}), "ends at a compaction")
}

func TestSummarizeEndToEnd(t *testing.T) {
	t.Parallel()

	sess := buildSession(t)

	appendText(t, sess, ai.RoleUser, bigText(500), nil)
	appendText(t, sess, ai.RoleAssistant, bigText(500), nil)
	appendText(t, sess, ai.RoleUser, bigText(200), nil)

	prep := harness.Prepare(sess.Path(), harness.Settings{KeepRecentTokens: 200})
	require.NotNil(t, prep)

	summarizer := newFakeModel("sum", textResponse("## Goal\nDo the thing.", 10))

	summary, err := harness.Summarize(t.Context(), summarizer, prep, harness.Settings{}, "focus on decisions")
	require.NoError(t, err)
	assert.Equal(t, "## Goal\nDo the thing.", summary)

	// The summarization request carried the serialized conversation, the
	// structured prompt, and the custom instructions.
	reqs := summarizer.Requests()
	require.Len(t, reqs, 1)

	prompt, ok := reqs[0].Messages[0].Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Contains(t, prompt.Text, "<conversation>")
	assert.Contains(t, prompt.Text, "## Goal")
	assert.Contains(t, prompt.Text, "Additional focus: focus on decisions")
	require.NotNil(t, reqs[0].MaxTokens)

	// Commit and verify reconstruction shrinks.
	_, err = sess.AppendCompaction(summary, prep.FirstKeptID, prep.TokensBefore)
	require.NoError(t, err)

	cctx, err := sess.Context()
	require.NoError(t, err)
	require.Len(t, cctx.Messages, 2) // summary + kept user message
}

func TestSummarizeBranch(t *testing.T) {
	t.Parallel()

	sess := buildSession(t)

	root := appendText(t, sess, ai.RoleUser, "start", nil)
	tip := appendText(t, sess, ai.RoleAssistant, "abandoned work", nil)

	summarizer := newFakeModel("sum", textResponse("## What was attempted\nBranch work.", 10))

	summary, err := harness.SummarizeBranch(t.Context(), summarizer, sess, tip, root)
	require.NoError(t, err)
	assert.Contains(t, summary, "Branch work")

	prompt, ok := summarizer.Requests()[0].Messages[0].Parts[0].(ai.TextPart)
	require.True(t, ok)
	assert.Contains(t, prompt.Text, "abandoned work")
	assert.NotContains(t, prompt.Text, "[user]\nstart", "segment before the ancestor stays out")
}
