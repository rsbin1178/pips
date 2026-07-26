package coding

import (
	"fmt"
	"testing"
	"time"

	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBootstrapStateAcceptsCustomProviderMetadata(t *testing.T) {
	t.Parallel()

	provider := ai.Provider("opencode-go")
	result, err := BootstrapState(BootstrapOptions{
		SessionID: "session-1",
		Provider:  provider,
		ModelID:   "deepseek-v4-flash",
		Mode:      ModeAgent,
		Path: []harness.Entry{{
			Kind: harness.KindModelChange, ID: "entry-1",
			Provider: provider, ModelID: "deepseek-v4-flash",
		}},
	})
	require.NoError(t, err)
	assert.Equal(t, provider, result.State.Provider)
	assert.Equal(t, "deepseek-v4-flash", result.State.ModelID)
}

func TestBootstrapStateRejectsInvalidCustomProviderMetadata(t *testing.T) {
	t.Parallel()

	_, err := BootstrapState(BootstrapOptions{
		SessionID: "session-1", Provider: "OpenCode", ModelID: "model", Mode: ModeAgent,
	})
	require.ErrorIs(t, err, ErrInvalidEvent)
}

func TestBootstrapStateRebuildsBoundedTranscriptTreeAndCompaction(t *testing.T) {
	t.Parallel()

	path := make([]harness.Entry, 0, maxEventItems+6)
	for index := range maxEventItems + 5 {
		message := ai.UserText(fmt.Sprintf("message-%d", index))
		path = append(path, harness.Entry{
			Kind: harness.KindMessage, ID: fmt.Sprintf("entry-%d", index), Message: &message,
		})
	}

	path = append(path, harness.Entry{
		Kind: harness.KindCompaction, ID: "compact", Summary: "summary",
		FirstKeptID: "entry-4090", TokensBefore: 9000,
	})
	tree := harness.TreeSnapshot{
		SessionID: "session-1", LeafID: "compact", TotalNodes: 1,
		Nodes: []harness.TreeNode{{
			ID: "compact", Kind: harness.KindCompaction, CreatedAt: time.Now().UTC(),
			Current: true, OnActivePath: true, HasSummary: true, Compacted: true,
		}},
	}

	result, err := BootstrapState(BootstrapOptions{
		SessionID: "session-1", Provider: ai.ProviderOpenAI, ModelID: "gpt-test",
		Mode: ModePlan, Path: path, Tree: tree,
	})
	require.NoError(t, err)
	require.Len(t, result.State.Transcript, maxEventItems)
	assert.Equal(t, ai.UserText("message-5"), result.State.Transcript[0])
	assert.Equal(t, tree.LeafID, result.State.Tree.LeafID)
	assert.Equal(t, 9000, result.State.Compaction.TokensBefore)
	assert.Equal(t, "entry-4090", result.State.Compaction.FirstKeptID)
	assert.Equal(t, ModePlan, result.State.Mode)
}
