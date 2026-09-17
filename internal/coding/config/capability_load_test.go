package config_test

import (
	"path/filepath"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadCapabilityDeclarationInheritance(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
[providers.zhipu]
protocol = "openai/chat_completions"

[providers.zhipu.capabilities]
tools = true
vision = false

[providers.zhipu.models."glm-4v-plus"]
default = true

[providers.zhipu.models."glm-4v-plus".capabilities]
vision = true
structured_output = false
`)

	result, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.NoError(t, err)

	provider := result.Config.Providers[ai.ProviderZhipu]
	require.NotNil(t, provider.Capabilities.Tools)
	assert.True(t, *provider.Capabilities.Tools)

	require.NotNil(t, provider.Capabilities.Vision)
	assert.False(t, *provider.Capabilities.Vision, "an explicit false must survive decoding")

	assert.Nil(t, provider.Capabilities.StructuredOutput, "an undeclared field stays nil")

	require.Len(t, result.Config.Models, 1)
	model := result.Config.Models[0]
	require.Equal(t, "glm-4v-plus", model.Ref.Model)

	require.NotNil(t, model.Capabilities.Vision)
	assert.True(t, *model.Capabilities.Vision)

	require.NotNil(t, model.Capabilities.StructuredOutput)
	assert.False(t, *model.Capabilities.StructuredOutput)

	assert.Nil(t, model.Capabilities.Tools, "a model declares only what it overrides")
}

func TestLoadRejectsUnknownCapabilityKey(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
[providers.openai.models."gpt-x"]

[providers.openai.models."gpt-x".capabilities]
visoin = true
`)

	_, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.ErrorIs(t, err, config.ErrDecode)
	assert.Contains(t, err.Error(), "strict mode", "an unknown capability key must be rejected")
}

func TestLoadRejectsToolsFalseDeclaration(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, `
[providers.openai.models."gpt-x"]

[providers.openai.models."gpt-x".capabilities]
tools = false
`)

	_, err := config.Load(config.LoadOptions{ConfigFile: path})
	require.ErrorIs(t, err, config.ErrInvalid)
	assert.Contains(t, err.Error(), "requires native tool calling")
}

func TestValidateRuntimeRejectsToolsFalseDeclaration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		providers map[ai.Provider]config.ProviderConfig
		models    []config.ModelConfig
	}{
		{
			name: "provider level",
			providers: map[ai.Provider]config.ProviderConfig{
				ai.ProviderOpenAI: {Capabilities: ai.CapabilityOverride{Tools: ai.Ptr(false)}},
			},
		},
		{
			name: "model level",
			models: []config.ModelConfig{{
				Ref:          config.ModelRef{Provider: ai.ProviderOpenAI, Model: "gpt"},
				Capabilities: ai.CapabilityOverride{Tools: ai.Ptr(false)},
			}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cfg := config.Config{
				Model:     config.ModelRef{Provider: ai.ProviderOpenAI, Model: "gpt"},
				Providers: test.providers,
				Models:    test.models,
				Sandbox:   config.SandboxWorkspaceWrite,
				Approval:  config.ApprovalOnRequest,
			}
			if cfg.Providers == nil {
				cfg.Providers = map[ai.Provider]config.ProviderConfig{}
			}

			err := cfg.ValidateRuntime()
			require.ErrorIs(t, err, config.ErrInvalid)
			assert.Contains(t, err.Error(), "requires native tool calling")
		})
	}
}

func TestValidateRuntimeAcceptsToolsTrue(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		Model: config.ModelRef{Provider: ai.ProviderOpenAI, Model: "gpt"},
		Providers: map[ai.Provider]config.ProviderConfig{
			ai.ProviderOpenAI: {Capabilities: ai.CapabilityOverride{Tools: ai.Ptr(true)}},
		},
		Sandbox:  config.SandboxWorkspaceWrite,
		Approval: config.ApprovalOnRequest,
	}

	require.NoError(t, cfg.ValidateRuntime())
}
