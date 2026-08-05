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
	assert.Equal(t, config.ModeAgent, cfg.Mode)
	assert.Equal(t, config.SandboxWorkspaceWrite, cfg.Sandbox)
	assert.Equal(t, config.SandboxNetworkOnRequest, cfg.SandboxWorkspaceWrite.Network)
	assert.Equal(t, config.ApprovalOnRequest, cfg.Approval)
	assert.Equal(t, config.DefaultCompactionConfig(), cfg.Compaction)
	for _, field := range config.Fields() {
		source, ok := cfg.Source(field)
		require.True(t, ok)
		assert.Equal(t, config.SourceDefault, source.Kind)
	}
}

func TestParseSandboxMode(t *testing.T) {
	t.Parallel()

	for _, value := range []config.SandboxMode{
		config.SandboxReadOnly,
		config.SandboxWorkspaceWrite,
		config.SandboxFullAccess,
	} {
		parsed, err := config.ParseSandboxMode(" " + string(value) + " ")
		require.NoError(t, err)
		assert.Equal(t, value, parsed)
	}

	for _, value := range []string{"", "sandboxed", "FULL-ACCESS"} {
		_, err := config.ParseSandboxMode(value)
		require.ErrorIs(t, err, config.ErrInvalid)
	}
}

func TestParseSandboxNetworkMode(t *testing.T) {
	t.Parallel()

	for _, value := range []config.SandboxNetworkMode{
		config.SandboxNetworkDeny,
		config.SandboxNetworkOnRequest,
		config.SandboxNetworkAllow,
	} {
		parsed, err := config.ParseSandboxNetworkMode(" " + string(value) + " ")
		require.NoError(t, err)
		assert.Equal(t, value, parsed)
	}

	for _, value := range []string{"", "always", "ALLOW"} {
		_, err := config.ParseSandboxNetworkMode(value)
		require.ErrorIs(t, err, config.ErrInvalid)
	}
}

func TestPermissionSourceHelpersAreDetachedAndSafe(t *testing.T) {
	t.Parallel()

	base := config.Defaults()
	base = base.RestoreSourceFrom(base, config.FieldSandbox)
	changed := base.WithSessionOverride(config.FieldSandbox)

	source, ok := changed.Source(config.FieldSandbox)
	require.True(t, ok)
	assert.Equal(t, config.SourceSessionOverride, source.Kind)
	assert.Empty(t, source.Detail)

	restored := changed.RestoreSourceFrom(base, config.FieldSandbox)
	source, ok = restored.Source(config.FieldSandbox)
	require.True(t, ok)
	assert.Equal(t, config.SourceDefault, source.Kind)
	assert.Equal(t, "built-in", source.Detail)
	assert.True(t, base.Equal(restored))
	assert.False(t, base.Equal(changed))
	assert.True(t, base.Equal(base.WithSessionOverride(config.FieldModel)))
}

func TestValidateRuntimeAllowsSandboxProfiles(t *testing.T) {
	t.Parallel()

	for _, sandbox := range []config.SandboxMode{
		config.SandboxReadOnly,
		config.SandboxWorkspaceWrite,
		config.SandboxFullAccess,
	} {
		t.Run(string(sandbox), func(t *testing.T) {
			t.Parallel()

			cfg := config.Defaults()
			cfg.Mode = ""
			cfg.Model = config.ModelRef{Provider: ai.ProviderOpenAI, Model: "test"}
			cfg.Sandbox = sandbox
			require.NoError(t, cfg.ValidateRuntime())
		})
	}
}

func TestParseOperatingMode(t *testing.T) {
	t.Parallel()

	for _, value := range []config.OperatingMode{config.ModeAgent, config.ModePlan} {
		parsed, err := config.ParseOperatingMode(" " + string(value) + " ")
		require.NoError(t, err)
		assert.Equal(t, value, parsed)
	}

	for _, value := range []string{"", "build", "PLAN"} {
		_, err := config.ParseOperatingMode(value)
		require.ErrorIs(t, err, config.ErrInvalid)
	}
}

func TestValidateCompactionConfig(t *testing.T) {
	t.Parallel()

	cfg := config.Defaults()
	cfg.Model = config.ModelRef{Provider: ai.ProviderOpenAI, Model: "gpt"}
	require.NoError(t, cfg.ValidateRuntime())

	cfg.Compaction.SummaryMaxTokens = cfg.Compaction.KeepRecentTokens + 1
	require.ErrorIs(t, cfg.ValidateRuntime(), config.ErrInvalid)
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

func TestParseProtocol(t *testing.T) {
	t.Parallel()

	values := []config.Protocol{
		config.ProtocolOpenAIAuto,
		config.ProtocolOpenAIChatCompletions,
		config.ProtocolOpenAIResponses,
		config.ProtocolAnthropicMessages,
		config.ProtocolGeminiGenerateContent,
	}
	for _, value := range values {
		parsed, err := config.ParseProtocol(string(value))
		require.NoError(t, err)
		assert.Equal(t, value, parsed)
	}

	_, err := config.ParseProtocol("chat_completions")
	require.ErrorIs(t, err, config.ErrInvalid)
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
		Mode:     config.ModeAgent,
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
