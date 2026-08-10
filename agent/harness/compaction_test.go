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

	appendText(t, sess, ai.UserMessage{}, bigText(1000), nil)
	appendText(t, sess, ai.AssistantMessage{}, "answer", &ai.Usage{InputTokens: 900, OutputTokens: 100})
	appendText(t, sess, ai.UserMessage{}, bigText(200), nil) // trailing, estimated

	tokens := harness.EstimateContext(sess.Path())
	assert.InDelta(t, 1200, tokens, 10, "usage (1000) + trailing estimate (~200)")
}

func TestShouldCompact(t *testing.T) {
	t.Parallel()

	s := harness.CompactionSettings{ContextTokens: 100_000, ReserveTokens: 16_384}
	assert.False(t, harness.ShouldCompact(50_000, s))
	assert.True(t, harness.ShouldCompact(90_000, s))
	assert.False(t, harness.ShouldCompact(90_000, harness.CompactionSettings{}), "no window, never auto-compacts")
	assert.False(t, harness.ShouldCompact(1, harness.CompactionSettings{ContextTokens: 1000, ReserveTokens: 1000}))
}

func TestPrepareCutsAtUserMessage(t *testing.T) {
	t.Parallel()

	sess := buildSession(t)

	appendText(t, sess, ai.UserMessage{}, bigText(500), nil)
	appendText(t, sess, ai.AssistantMessage{}, bigText(500), nil)
	keep := appendText(t, sess, ai.UserMessage{}, bigText(500), nil)
	appendText(t, sess, ai.AssistantMessage{}, bigText(500), nil)

	// keepRecent ≈ the last exchange: the cut lands on the second user turn.
	prep := harness.PlanCompaction(sess.Path(), harness.CompactionSettings{KeepRecentTokens: 800})
	require.NotNil(t, prep)

	assert.Equal(t, keep, prep.FirstKeptID)
	assert.False(t, prep.SplitTurn)
	assert.Len(t, prep.ToSummarize, 2)
	assert.Empty(t, prep.Previous)
}

func TestPrepareNeverCutsToolResults(t *testing.T) {
	t.Parallel()

	sess := buildSession(t)

	appendText(t, sess, ai.UserMessage{}, bigText(100), nil)

	// A turn: assistant tool call + tool result, then the final answer.
	callMsg := ai.Assistant(ai.ToolCallPart{ID: "c1", Name: "search", Args: ai.JSON(`{}`)})
	_, err := sess.AppendMessage(callMsg, nil)
	require.NoError(t, err)

	resultMsg := ai.ToolResultText("c1", "search", bigText(2000))
	_, err = sess.AppendMessage(resultMsg, nil)
	require.NoError(t, err)

	appendText(t, sess, ai.AssistantMessage{}, bigText(100), nil)

	// keepRecent lands inside the tool result — the cut must move to a
	// message boundary, never between the call and its result.
	prep := harness.PlanCompaction(sess.Path(), harness.CompactionSettings{KeepRecentTokens: 500})
	require.NotNil(t, prep)

	kept, ok := sess.Entry(prep.FirstKeptID)
	require.True(t, ok)
	require.Equal(t, harness.KindMessage, kept.Kind)
	_, isTool := kept.Message.(ai.ToolMessage)
	assert.False(t, isTool)
}

func TestPrepareSplitTurn(t *testing.T) {
	t.Parallel()

	sess := buildSession(t)

	appendText(t, sess, ai.UserMessage{}, "old", nil)
	appendText(t, sess, ai.AssistantMessage{}, "old answer", nil)
	appendText(t, sess, ai.UserMessage{}, bigText(100), nil) // turn start
	appendText(t, sess, ai.AssistantMessage{}, bigText(3000), nil)
	appendText(t, sess, ai.AssistantMessage{}, bigText(400), nil) // kept suffix

	prep := harness.PlanCompaction(sess.Path(), harness.CompactionSettings{KeepRecentTokens: 500})
	require.NotNil(t, prep)

	// The cut lands on the giant mid-turn assistant message → split turn.
	assert.True(t, prep.SplitTurn)
	assert.NotEmpty(t, prep.TurnPrefix, "prefix of the split turn is summarized separately")
	assert.Len(t, prep.ToSummarize, 2, "history before the split turn")
}

func TestPrepareIterativeCarriesPrevious(t *testing.T) {
	t.Parallel()

	sess := buildSession(t)

	appendText(t, sess, ai.UserMessage{}, bigText(500), nil)
	keep := appendText(t, sess, ai.UserMessage{}, bigText(500), nil)
	_, err := sess.AppendCompaction("earlier summary", keep, 100)
	require.NoError(t, err)

	appendText(t, sess, ai.UserMessage{}, bigText(800), nil)
	appendText(t, sess, ai.UserMessage{}, bigText(200), nil)

	prep := harness.PlanCompaction(sess.Path(), harness.CompactionSettings{KeepRecentTokens: 100})
	require.NotNil(t, prep)
	assert.Equal(t, "earlier summary", prep.Previous)
}

func TestEstimateContextDropsUsageHiddenByCompaction(t *testing.T) {
	t.Parallel()

	sess := buildSession(t)
	appendText(t, sess, ai.UserMessage{}, bigText(2000), nil)
	appendText(t, sess, ai.AssistantMessage{}, "old", &ai.Usage{InputTokens: 9000, OutputTokens: 1000})
	keep := appendText(t, sess, ai.UserMessage{}, bigText(100), nil)
	_, err := sess.AppendCompaction("small summary", keep, 10000)
	require.NoError(t, err)

	assert.Less(t, harness.EstimateContext(sess.Path()), 1000)
}

func TestPrepareNothingToCompact(t *testing.T) {
	t.Parallel()

	sess := buildSession(t)
	assert.Nil(t, harness.PlanCompaction(sess.Path(), harness.CompactionSettings{}), "empty session")

	keep := appendText(t, sess, ai.UserMessage{}, "hi", nil)
	_, err := sess.AppendCompaction("s", keep, 1)
	require.NoError(t, err)
	assert.Nil(t, harness.PlanCompaction(sess.Path(), harness.CompactionSettings{}), "ends at a compaction")
}

func TestSummarizeEndToEnd(t *testing.T) {
	t.Parallel()

	sess := buildSession(t)

	appendText(t, sess, ai.UserMessage{}, bigText(500), nil)
	appendText(t, sess, ai.AssistantMessage{}, bigText(500), nil)
	appendText(t, sess, ai.UserMessage{}, bigText(200), nil)

	prep := harness.PlanCompaction(sess.Path(), harness.CompactionSettings{KeepRecentTokens: 200})
	require.NotNil(t, prep)

	summarizer := newScriptedModel("sum", textResponse("## Goal\nDo the thing.", 10))

	summary, err := harness.SummarizeCompaction(t.Context(), summarizer, prep, harness.CompactionSettings{}, "focus on decisions")
	require.NoError(t, err)
	assert.Equal(t, "## Goal\nDo the thing.", summary)

	// The summarization request carried the serialized conversation, the
	// structured prompt, and the custom instructions.
	reqs := summarizer.Requests()
	require.Len(t, reqs, 1)

	prompt := firstTextPart(t, reqs[0].Messages[1])
	assert.Contains(t, prompt.Text, "<conversation>")
	assert.Contains(t, prompt.Text, "## Goal")
	assert.Contains(t, prompt.Text, "Additional focus: focus on decisions")
	require.NotNil(t, reqs[0].MaxTokens)
	assert.Equal(t, harness.DefaultCompactionSummaryTokens, *reqs[0].MaxTokens)

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

	root := appendText(t, sess, ai.UserMessage{}, "start", nil)
	tip := appendText(t, sess, ai.AssistantMessage{}, "abandoned work", nil)

	summarizer := newScriptedModel("sum", textResponse("## What was attempted\nBranch work.", 10))

	summary, err := harness.SummarizeBranch(t.Context(), summarizer, sess, tip, root)
	require.NoError(t, err)
	assert.Contains(t, summary, "Branch work")

	prompt := firstTextPart(t, summarizer.Requests()[0].Messages[1])
	assert.Contains(t, prompt.Text, "abandoned work")
	assert.NotContains(t, prompt.Text, "[user]\nstart", "segment before the ancestor stays out")
}
