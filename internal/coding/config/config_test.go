//nolint:wsl_v5 // Test setup and mutation assertions stay adjacent.
package config_test

import (
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/openai"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaults(t *testing.T) {
	t.Parallel()

	cfg := config.Defaults()
	assert.Empty(t, cfg.Model.String())
	assert.Empty(t, cfg.Providers)
	assert.Empty(t, cfg.Models)
	assert.False(t, cfg.ToolSearch)
	assert.Equal(t, config.SandboxWorkspaceWrite, cfg.Sandbox)
	assert.Equal(t, config.ApprovalOnRequest, cfg.Approval)
	for _, field := range config.Fields() {
		source, ok := cfg.Source(field)
		require.True(t, ok)
		assert.Equal(t, config.SourceDefault, source.Kind)
	}
}

func TestParseModelRef(t *testing.T) {
	t.Parallel()

	ref, err := config.ParseModelRef(" openrouter/anthropic/claude ")
	require.NoError(t, err)
	assert.Equal(t, ai.ProviderOpenRouter, ref.Provider)
	assert.Equal(t, "anthropic/claude", ref.Model)
	assert.Equal(t, "openrouter/anthropic/claude", ref.String())

	for _, value := range []string{"", "openai", "/model", "OpenAI/model", "openai/"} {
		_, err := config.ParseModelRef(value)
		require.ErrorIs(t, err, config.ErrInvalid)
	}
}

func TestValidateRuntimeRegistry(t *testing.T) {
	t.Parallel()

	level := config.ReasoningLevel("high")
	valid := config.Config{
		Model: config.ModelRef{Provider: ai.ProviderOpenAI, Model: "gpt"},
		Models: []config.ModelConfig{{
			Ref:                   config.ModelRef{Provider: ai.ProviderOpenAI, Model: "gpt"},
			ReasoningLevels:       []config.ReasoningLevel{"low", "high"},
			DefaultReasoningLevel: &level,
			Variants:              map[string]config.VariantConfig{},
		}},
		Sandbox:  config.SandboxWorkspaceWrite,
		Approval: config.ApprovalOnRequest,
	}
	require.NoError(t, valid.ValidateRuntime())

	duplicate := valid.Clone()
	duplicate.Models = append(duplicate.Models, duplicate.Models[0])
	require.ErrorIs(t, duplicate.ValidateRuntime(), config.ErrInvalid)

	badDefault := valid.Clone()
	unsupported := config.ReasoningLevel("xhigh")
	badDefault.Models[0].DefaultReasoningLevel = &unsupported
	require.ErrorIs(t, badDefault.ValidateRuntime(), config.ErrInvalid)
}

func TestCloneDetachesNestedValues(t *testing.T) {
	t.Parallel()

	stops := []string{"end"}
	providerMaxTokens := openai.MaxTokensFieldLegacy
	modelStreamUsage := openai.StreamUsageOmit
	cfg := config.Config{
		Providers: map[ai.Provider]config.ProviderConfig{
			ai.ProviderOpenAI: {
				Compatibility: config.CompatibilityConfig{MaxTokensField: &providerMaxTokens},
			},
		},
		Models: []config.ModelConfig{{
			Ref:           config.ModelRef{Provider: ai.ProviderOpenAI, Model: "gpt"},
			Compatibility: config.CompatibilityConfig{StreamUsage: &modelStreamUsage},
			Options: config.ModelOptions{
				Stop:      &stops,
				ExtraBody: map[string]any{"nested": map[string]any{"value": true}},
			},
			Variants: map[string]config.VariantConfig{},
		}},
	}

	cloned := cfg.Clone()
	assert.True(t, cfg.Equal(cloned))
	provider := cloned.Providers[ai.ProviderOpenAI]
	*provider.Compatibility.MaxTokensField = openai.MaxTokensFieldCompletion
	cloned.Providers[ai.ProviderOpenAI] = provider
	*cloned.Models[0].Compatibility.StreamUsage = openai.StreamUsageInclude
	(*cloned.Models[0].Options.Stop)[0] = "changed"
	clonedNested, ok := cloned.Models[0].Options.ExtraBody["nested"].(map[string]any)
	require.True(t, ok)
	clonedNested["value"] = false

	assert.Equal(t, "end", (*cfg.Models[0].Options.Stop)[0])
	assert.Equal(t, openai.MaxTokensFieldLegacy, *cfg.Providers[ai.ProviderOpenAI].Compatibility.MaxTokensField)
	assert.Equal(t, openai.StreamUsageOmit, *cfg.Models[0].Compatibility.StreamUsage)
	assert.False(t, cfg.Equal(cloned))
	originalNested, ok := cfg.Models[0].Options.ExtraBody["nested"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, originalNested["value"])
}
