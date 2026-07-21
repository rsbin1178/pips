package modelcatalog_test

import (
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/openai"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/modelcatalog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveInheritanceVariantAndReasoning(t *testing.T) {
	t.Parallel()

	medium := config.ReasoningLevel("medium")
	high := config.ReasoningLevel("high")
	baseMax := 4096
	variantMax := 8192
	cfg := config.Config{
		Model: config.ModelRef{Provider: ai.ProviderOpenAI, Model: "gpt"},
		Models: []config.ModelConfig{{
			Ref:                   config.ModelRef{Provider: ai.ProviderOpenAI, Model: "gpt"},
			ContextWindow:         200000,
			MaxOutputTokens:       100000,
			ReasoningLevels:       []config.ReasoningLevel{"low", medium, high},
			DefaultReasoningLevel: &medium,
			ReasoningBudgets:      map[config.ReasoningLevel]int{high: 16000},
			DefaultVariant:        "balanced",
			Options:               config.ModelOptions{MaxOutputTokens: &baseMax},
			Variants: map[string]config.VariantConfig{
				"balanced": {ReasoningLevel: &medium},
				"deep":     {ReasoningLevel: &high, Options: config.ModelOptions{MaxOutputTokens: &variantMax}},
			},
		}},
		Providers: map[ai.Provider]config.ProviderConfig{},
		Sandbox:   config.SandboxWorkspaceWrite,
		Approval:  config.ApprovalOnRequest,
	}
	catalog, err := modelcatalog.New(cfg)
	require.NoError(t, err)

	resolved, err := catalog.Resolve(modelcatalog.Selection{Ref: cfg.Model, Variant: "deep"})
	require.NoError(t, err)
	assert.Equal(t, config.APIResponses, resolved.API)
	assert.Equal(t, "https://api.openai.com/v1", resolved.Endpoint.BaseURL)
	assert.Equal(t, high, *resolved.ReasoningLevel)
	assert.Equal(t, 16000, *resolved.Options.ReasoningBudget)
	assert.Equal(t, variantMax, *resolved.Options.MaxOutputTokens)
	assert.Equal(t, 200000, resolved.Limits.ContextWindow)

	adaptive := config.ReasoningAdaptive
	adaptiveConfig := cfg.Clone()
	adaptiveConfig.Models[0].Options.ReasoningMode = &adaptive
	adaptiveCatalog, err := modelcatalog.New(adaptiveConfig)
	require.NoError(t, err)
	adaptiveResolved, err := adaptiveCatalog.Resolve(modelcatalog.Selection{
		Ref: cfg.Model, ReasoningOverride: &high,
	})
	require.NoError(t, err)
	assert.Nil(t, adaptiveResolved.Options.ReasoningBudget)
}

func TestCatalogCustomProviderAndUnknownCurrent(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		Model: config.ModelRef{Provider: "local", Model: "qwen"},
		Providers: map[ai.Provider]config.ProviderConfig{
			"local": {
				BaseURL: "http://127.0.0.1:11434/v1", API: config.APIChatCompletions,
				AllowHTTP: true, AllowPrivateIPs: true,
			},
		},
		Sandbox:  config.SandboxWorkspaceWrite,
		Approval: config.ApprovalOnRequest,
	}
	catalog, err := modelcatalog.New(cfg)
	require.NoError(t, err)
	assert.Len(t, catalog.List(), 1)

	resolved, err := catalog.Resolve(modelcatalog.Selection{Ref: cfg.Model})
	require.NoError(t, err)
	assert.Zero(t, resolved.Limits.ContextWindow)
	assert.Equal(t, ai.Provider("local"), resolved.Ref.Provider)
}

func TestCatalogDropsOpenAICompatibilityForNonOpenAIModelOverride(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		Model: config.ModelRef{Provider: ai.ProviderDeepSeek, Model: "native"},
		Models: []config.ModelConfig{{
			Ref: config.ModelRef{Provider: ai.ProviderDeepSeek, Model: "native"},
			API: config.APIAnthropicMessages,
		}},
		Providers: map[ai.Provider]config.ProviderConfig{},
		Sandbox:   config.SandboxWorkspaceWrite,
		Approval:  config.ApprovalOnRequest,
	}
	catalog, err := modelcatalog.New(cfg)
	require.NoError(t, err)

	resolved, err := catalog.Resolve(modelcatalog.Selection{Ref: cfg.Model})
	require.NoError(t, err)
	assert.Equal(t, config.APIAnthropicMessages, resolved.API)
	assert.Equal(t, openai.Compatibility{}, resolved.Compatibility)
}

func TestCatalogRejectsUnsafeCustomEndpointAndUnknownSelection(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		Model: config.ModelRef{Provider: "local", Model: "qwen"},
		Providers: map[ai.Provider]config.ProviderConfig{
			"local": {BaseURL: "http://127.0.0.1:11434/v1", API: config.APIChatCompletions},
		},
		Sandbox:  config.SandboxWorkspaceWrite,
		Approval: config.ApprovalOnRequest,
	}
	_, err := modelcatalog.New(cfg)
	require.ErrorIs(t, err, modelcatalog.ErrInvalid)

	cfg.Providers["local"] = config.ProviderConfig{
		BaseURL: "http://127.0.0.1:11434/v1", API: config.APIChatCompletions,
		AllowHTTP: true, AllowPrivateIPs: true,
	}
	catalog, err := modelcatalog.New(cfg)
	require.NoError(t, err)
	_, err = catalog.Resolve(modelcatalog.Selection{Ref: config.ModelRef{Provider: "local", Model: "other"}})
	require.ErrorIs(t, err, modelcatalog.ErrInvalid)
}

func TestCatalogEndpointValidationMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		baseURL      string
		allowHTTP    bool
		allowPrivate bool
		wantError    bool
	}{
		{name: "public https", baseURL: "https://example.com/v1"},
		{name: "public http denied", baseURL: "http://example.com/v1", wantError: true},
		{name: "public http allowed", baseURL: "http://example.com/v1", allowHTTP: true},
		{name: "private https denied", baseURL: "https://127.0.0.1/v1", wantError: true},
		{name: "private http needs both opts", baseURL: "http://127.0.0.1/v1", allowHTTP: true, wantError: true},
		{name: "private http allowed", baseURL: "http://127.0.0.1/v1", allowHTTP: true, allowPrivate: true},
		{name: "userinfo", baseURL: "https://user@example.com/v1", wantError: true},
		{name: "query", baseURL: "https://example.com/v1?token=secret", wantError: true},
		{name: "fragment", baseURL: "https://example.com/v1#fragment", wantError: true},
		{name: "relative", baseURL: "/v1", wantError: true},
		{name: "unsupported scheme", baseURL: "ftp://example.com/v1", allowHTTP: true, wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := config.Config{
				Model: config.ModelRef{Provider: "custom", Model: "model"},
				Providers: map[ai.Provider]config.ProviderConfig{
					"custom": {
						BaseURL: tt.baseURL, API: config.APIChatCompletions,
						AllowHTTP: tt.allowHTTP, AllowPrivateIPs: tt.allowPrivate,
					},
				},
				Sandbox: config.SandboxWorkspaceWrite, Approval: config.ApprovalOnRequest,
			}

			_, err := modelcatalog.New(cfg)
			if tt.wantError {
				require.ErrorIs(t, err, modelcatalog.ErrInvalid)
				assert.NotContains(t, err.Error(), "secret")

				return
			}

			require.NoError(t, err)
		})
	}
}
