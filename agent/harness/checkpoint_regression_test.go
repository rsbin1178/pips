package harness_test

import (
	"math"
	"testing"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContextEstimateSaturatesMalformedHugeUsage(t *testing.T) {
	t.Parallel()

	path := []harness.Entry{
		{ID: "response", Kind: harness.KindMessage, Message: ai.AssistantText("answer"), Usage: &ai.Usage{InputTokens: math.MaxInt, OutputTokens: 1}},
		{ID: "next", Kind: harness.KindMessage, Message: ai.UserText("another question")},
	}
	assert.Equal(t, math.MaxInt, harness.EstimateContext(path))
}

func TestSummaryCleaningUsesOriginalUnicodeOffsets(t *testing.T) {
	t.Parallel()

	value, err := harness.ValidateCompactionSummary("<analysis>İK</AnAlYsIs><summary>Useful checkpoint</summary>", 1)
	require.NoError(t, err)
	assert.Equal(t, "Useful checkpoint", value)
}

func TestLegacySummaryRejectsNilModelResponse(t *testing.T) {
	t.Parallel()

	model := newScriptedModel("summary", nil)
	_, err := harness.SummarizeCompaction(t.Context(), model, &harness.CompactionPlan{
		ToSummarize: ai.Messages{ai.UserText("work")},
	}, harness.CompactionSettings{}, "")
	require.ErrorIs(t, err, harness.ErrInvalidCompactionSummary)
}

func TestSnapshotContextIsDetachedAndConsistent(t *testing.T) {
	t.Parallel()
	sess := buildSession(t)
	id := appendText(t, sess, ai.UserMessage{}, "original", nil)
	snapshot, err := sess.SnapshotContext()
	require.NoError(t, err)
	assert.Equal(t, id, snapshot.LeafID)
	require.Len(t, snapshot.Entries, 1)
	require.Len(t, snapshot.Context.Messages, 1)
	snapshot.Entries[0].ID = "changed"
	snapshot.Context.Messages[0] = ai.UserText("changed")
	actual, err := sess.Context()
	require.NoError(t, err)
	assert.Equal(t, ai.UserText("original"), actual.Messages[0])
	assert.Equal(t, id, sess.LeafID())
}
