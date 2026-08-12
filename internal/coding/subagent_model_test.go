//nolint:wsl_v5 // Model fixture configuration remains adjacent to its assertions.
package coding

import (
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/credential"
	"github.com/rsbin1178/pips/internal/coding/generation"
	"github.com/rsbin1178/pips/internal/coding/modelcatalog"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChildModelResolverAllowsOnlyConfiguredModelReferences(t *testing.T) {
	t.Parallel()

	baseRef := config.ModelRef{Provider: ai.ProviderOpenAI, Model: "base"}
	secondRef := config.ModelRef{Provider: ai.ProviderOpenAI, Model: "specialist"}
	configured := config.Defaults()
	configured.Model = baseRef
	configured.Providers[ai.ProviderOpenAI] = config.ProviderConfig{
		BaseURL: "https://api.openai.com/v1", Protocol: config.ProtocolOpenAIResponses,
	}
	configured.Models = []config.ModelConfig{
		{Ref: baseRef, ContextWindow: 128_000, Variants: map[string]config.VariantConfig{}},
		{Ref: secondRef, ContextWindow: 128_000, Variants: map[string]config.VariantConfig{}},
	}
	catalog, err := modelcatalog.New(configured)
	require.NoError(t, err)
	base, err := catalog.Resolve(modelcatalog.Selection{Ref: baseRef})
	require.NoError(t, err)
	policy, err := generation.Compile(base)
	require.NoError(t, err)

	resolver, err := newChildModelResolver(
		catalog,
		base,
		newRuntimeModelFor(baseRef.Provider, baseRef.Model),
		policy,
		nil,
		nil,
	)
	require.NoError(t, err)

	inherited, err := resolver.planModel("inherit")
	require.NoError(t, err)
	assert.Equal(t, baseRef.String(), inherited)

	_, err = resolver.planModel(secondRef.String())
	require.ErrorIs(t, err, subagent.ErrInvalid)
	assert.Contains(t, err.Error(), "credentials")

	_, err = resolver.planModel("openai/not-configured")
	require.ErrorIs(t, err, subagent.ErrInvalid)

	store, err := credential.NewEnvironmentStore(func(string) (string, bool) { return "test-key", true })
	require.NoError(t, err)
	resolver.credentials = store
	selected, err := resolver.planModel(secondRef.String())
	require.NoError(t, err)
	assert.Equal(t, secondRef.String(), selected)
}
