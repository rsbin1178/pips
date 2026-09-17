package capability_test

import (
	"context"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/middleware/capability"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubModel struct {
	capabilities ai.Capabilities
}

func (m *stubModel) Generate(context.Context, ai.Request) (*ai.Response, error) {
	return &ai.Response{Message: ai.AssistantText("ok")}, nil
}

func (m *stubModel) Stream(context.Context, ai.Request) ai.Stream {
	return func(func(ai.StreamEvent, error) bool) {}
}

func (m *stubModel) Provider() ai.Provider         { return ai.ProviderOpenAI }
func (m *stubModel) ModelID() string               { return "stub-model" }
func (m *stubModel) Capabilities() ai.Capabilities { return m.capabilities }

func TestZeroOverrideReturnsTheModelUnchanged(t *testing.T) {
	t.Parallel()

	base := &stubModel{capabilities: ai.Capabilities{Text: true, Tools: true}}

	wrapped := capability.New(ai.CapabilityOverride{})(base)

	assert.Same(t, base, wrapped)
}

func TestOverridePatchesCapabilitiesAndPassesThrough(t *testing.T) {
	t.Parallel()

	no := false
	yes := true
	base := &stubModel{capabilities: ai.Capabilities{Text: true, Tools: true}}

	wrapped := capability.New(ai.CapabilityOverride{
		Vision:           &yes,
		StructuredOutput: &no,
	})(base)

	assert.Equal(t, ai.ProviderOpenAI, wrapped.Provider())
	assert.Equal(t, "stub-model", wrapped.ModelID())
	assert.Equal(t, ai.Capabilities{
		Text:             true,
		Tools:            true,
		Vision:           true,
		StructuredOutput: false,
	}, wrapped.Capabilities())

	assert.Equal(t, ai.Capabilities{Text: true, Tools: true}, base.Capabilities(),
		"the wrapped model must not be mutated")

	resp, err := wrapped.Generate(t.Context(), ai.Request{})
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Text())
}
