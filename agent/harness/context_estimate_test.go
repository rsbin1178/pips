package harness

import (
	"strings"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContextCheckpointFixedOverheadSurvivesReplay(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore("fixed-overhead")
	session, err := NewSession(store)
	require.NoError(t, err)
	appendCheckpointMessage(t, session, ai.UserText("old request"), nil)
	appendCheckpointMessage(t, session, ai.AssistantText("old response"), &ai.Usage{InputTokens: 9000, OutputTokens: 1000})

	seed := testCheckpoint()
	seed.Messages = ai.Messages{ai.UserText(strings.Repeat("word", 800))}
	seed.FixedTokens = 200
	checkpoint, err := session.AppendContextCheckpoint(session.LeafID(), seed, 10000)
	require.NoError(t, err)

	want := ContextEstimate{Tokens: 1000, FixedTokens: 200, BoundaryID: checkpoint}
	assert.Equal(t, want, EstimateContextUsage(session.Path()))

	entry, ok := session.Entry(checkpoint)
	require.True(t, ok)

	encoded, err := MarshalEntry(entry)
	require.NoError(t, err)
	decoded, err := UnmarshalEntry(encoded)
	require.NoError(t, err)
	require.NotNil(t, decoded.Checkpoint)
	assert.Equal(t, 200, decoded.Checkpoint.FixedTokens)

	reopened, err := NewSession(store)
	require.NoError(t, err)
	assert.Equal(t, want, EstimateContextUsage(reopened.Path()))
	appendCheckpointMessage(t, reopened, ai.AssistantText("tail"), &ai.Usage{})
	assert.Equal(t, 1001, EstimateContext(reopened.Path()))
	appendCheckpointMessage(t, reopened, ai.AssistantText("tail"), &ai.Usage{OutputTokens: 200})
	assert.Equal(t, 1002, EstimateContext(reopened.Path()), "output-only usage cannot measure the input context")
	appendCheckpointMessage(t, reopened, ai.AssistantText("fresh response"), &ai.Usage{InputTokens: 1500, OutputTokens: 200})
	estimate := EstimateContextUsage(reopened.Path())
	assert.Equal(t, 1700, estimate.Tokens)
	assert.True(t, estimate.ProviderBaseline)
	assert.Zero(t, estimate.FixedTokens, "provider input already includes overhead")

	_, err = reopened.AppendModelChange(ai.Provider("new-provider"), "new-model")
	require.NoError(t, err)

	estimate = EstimateContextUsage(reopened.Path())
	assert.False(t, estimate.ProviderBaseline)
	assert.Zero(t, estimate.FixedTokens, "changed model requires a fresh application overhead estimate")
}

func TestContextCheckpointFixedOverheadBounds(t *testing.T) {
	t.Parallel()

	for _, tokens := range []int{-1, MaxContextCheckpointFixedTokens + 1} {
		seed := testCheckpoint()
		seed.FixedTokens = tokens
		require.Error(t, seed.Validate())
	}

	seed := testCheckpoint()
	seed.FixedTokens = MaxContextCheckpointFixedTokens
	require.NoError(t, seed.Validate())
}
