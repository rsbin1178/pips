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
	assert.Empty(t, cfg.Model.Provider)
	assert.Empty(t, cfg.Model.ID)
	assert.Equal(t, openai.APIAuto, cfg.Model.API)
	assert.False(t, cfg.ToolSearch)
	assert.Equal(t, config.SandboxWorkspaceWrite, cfg.Sandbox)
	assert.Equal(t, config.ApprovalOnRequest, cfg.Approval)

	for _, field := range config.Fields() {
		source, ok := cfg.Source(field)
		require.True(t, ok)
		assert.Equal(t, config.SourceDefault, source.Kind)
		assert.Equal(t, "built-in", source.Detail)
	}

	fields := config.Fields()
	fields[0] = "changed"

	assert.Equal(t, config.FieldProvider, config.Fields()[0])
}

func TestValidateRuntime(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  config.Config
		ok   bool
	}{
		{name: "valid", cfg: config.Config{Model: config.ModelConfig{Provider: ai.ProviderOpenAI, ID: "model", API: openai.APIResponses}, Sandbox: config.SandboxWorkspaceWrite, Approval: config.ApprovalOnRequest}, ok: true},
		{name: "zero api is auto", cfg: config.Config{Model: config.ModelConfig{Provider: ai.ProviderAnthropic, ID: "model"}, Sandbox: config.SandboxWorkspaceWrite, Approval: config.ApprovalOnRequest}, ok: true},
		{name: "non openai auto", cfg: config.Config{Model: config.ModelConfig{Provider: ai.ProviderGemini, ID: "model", API: openai.APIAuto}, Sandbox: config.SandboxWorkspaceWrite, Approval: config.ApprovalOnRequest}, ok: true},
		{name: "non openai explicit api", cfg: config.Config{Model: config.ModelConfig{Provider: ai.ProviderAnthropic, ID: "model", API: openai.APIChatCompletions}, Sandbox: config.SandboxWorkspaceWrite, Approval: config.ApprovalOnRequest}},
		{name: "unknown api", cfg: config.Config{Model: config.ModelConfig{Provider: ai.ProviderOpenAI, ID: "model", API: "unknown"}, Sandbox: config.SandboxWorkspaceWrite, Approval: config.ApprovalOnRequest}},
		{name: "missing provider", cfg: config.Config{Model: config.ModelConfig{ID: "model"}, Sandbox: config.SandboxWorkspaceWrite, Approval: config.ApprovalOnRequest}},
		{name: "unsupported provider", cfg: config.Config{Model: config.ModelConfig{Provider: ai.ProviderDeepSeek, ID: "model"}, Sandbox: config.SandboxWorkspaceWrite, Approval: config.ApprovalOnRequest}},
		{name: "missing model", cfg: config.Config{Model: config.ModelConfig{Provider: ai.ProviderOpenAI}, Sandbox: config.SandboxWorkspaceWrite, Approval: config.ApprovalOnRequest}},
		{name: "bad sandbox", cfg: config.Config{Model: config.ModelConfig{Provider: ai.ProviderOpenAI, ID: "model"}, Sandbox: "bad", Approval: config.ApprovalOnRequest}},
		{name: "bad approval", cfg: config.Config{Model: config.ModelConfig{Provider: ai.ProviderOpenAI, ID: "model"}, Sandbox: config.SandboxWorkspaceWrite, Approval: "bad"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.cfg.ValidateRuntime()
			if tt.ok {
				require.NoError(t, err)
				return
			}

			require.Error(t, err)
			assert.ErrorIs(t, err, config.ErrInvalid)
		})
	}
}

func TestParsers(t *testing.T) {
	t.Parallel()

	provider, err := config.ParseProvider(" anthropic ")
	require.NoError(t, err)
	assert.Equal(t, ai.ProviderAnthropic, provider)

	sandbox, err := config.ParseSandboxMode("full-access")
	require.NoError(t, err)
	assert.Equal(t, config.SandboxFullAccess, sandbox)

	approval, err := config.ParseApprovalMode("never")
	require.NoError(t, err)
	assert.Equal(t, config.ApprovalNever, approval)

	api, err := config.ParseModelAPI(" responses ")
	require.NoError(t, err)
	assert.Equal(t, openai.APIResponses, api)

	api, err = config.ParseModelAPI("")
	require.NoError(t, err)
	assert.Equal(t, openai.APIAuto, api)

	_, err = config.ParseProvider("deepseek")
	require.ErrorIs(t, err, config.ErrInvalid)
	_, err = config.ParseSandboxMode("unknown")
	require.ErrorIs(t, err, config.ErrInvalid)
	_, err = config.ParseApprovalMode("unknown")
	require.ErrorIs(t, err, config.ErrInvalid)
	_, err = config.ParseModelAPI("unknown")
	require.ErrorIs(t, err, config.ErrInvalid)
}
