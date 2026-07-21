package coding

import (
	"testing"

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
		SessionID: "session-1", Provider: "OpenCode", ModelID: "model",
	})
	require.ErrorIs(t, err, ErrInvalidEvent)
}
