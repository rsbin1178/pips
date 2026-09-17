//nolint:wsl_v5 // Adapter fixtures keep acquisition and assertion steps adjacent.
package model_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/openai"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/credential"
	"github.com/rsbin1178/pips/internal/coding/generation"
	"github.com/rsbin1178/pips/internal/coding/model"
	"github.com/rsbin1178/pips/internal/coding/modelcatalog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type credentialStore struct {
	provider ai.Provider
}

func (s *credentialStore) Get(
	_ context.Context,
	provider ai.Provider,
) (credential.Credential, error) {
	s.provider = provider
	store, err := credential.NewEnvironmentStore(func(string) (string, bool) {
		return "secret", true
	})
	if err != nil {
		return credential.Credential{}, err
	}

	return store.Get(context.Background(), provider)
}

func TestNewSelectsAdapterAndPreservesProviderIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		provider ai.Provider
		protocol config.Protocol
		baseURL  string
	}{
		{provider: ai.ProviderOpenAI, protocol: config.ProtocolOpenAIResponses, baseURL: "https://api.openai.com/v1"},
		{provider: ai.ProviderAnthropic, protocol: config.ProtocolAnthropicMessages, baseURL: "https://api.anthropic.com/v1"},
		{provider: ai.ProviderGemini, protocol: config.ProtocolGeminiGenerateContent, baseURL: "https://generativelanguage.googleapis.com/v1beta"},
		{provider: "local", protocol: config.ProtocolOpenAIChatCompletions, baseURL: "http://127.0.0.1:11434/v1"},
	}
	for _, tt := range tests {
		t.Run(string(tt.provider), func(t *testing.T) {
			t.Parallel()
			store := &credentialStore{}
			bound, err := model.New(t.Context(), modelcatalog.ResolvedModel{
				Ref:      config.ModelRef{Provider: tt.provider, Model: "model"},
				Protocol: tt.protocol,
				Endpoint: modelcatalog.Endpoint{
					BaseURL: tt.baseURL, AllowHTTP: tt.provider == "local",
					AllowPrivateIPs: tt.provider == "local",
				},
			}, store)
			require.NoError(t, err)
			assert.Equal(t, tt.provider, bound.Provider())
			assert.Equal(t, "model", bound.ModelID())
			assert.Equal(t, tt.provider, store.provider)
		})
	}
}

func TestNewRejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	resolved := modelcatalog.ResolvedModel{
		Ref:      config.ModelRef{Provider: ai.ProviderOpenAI, Model: "model"},
		Protocol: config.ProtocolOpenAIResponses,
		Endpoint: modelcatalog.Endpoint{BaseURL: "https://api.openai.com/v1"},
	}
	_, err := model.New(t.Context(), resolved, nil)
	require.ErrorIs(t, err, model.ErrInvalid)

	resolved.Protocol = "unknown"
	_, err = model.New(t.Context(), resolved, &credentialStore{})
	require.ErrorIs(t, err, model.ErrInvalid)
}

// TestNewAppliesCapabilityDeclaration proves the declared override reaches the
// runtime for every protocol, layering on top of whatever the adapter reports.
func TestNewAppliesCapabilityDeclaration(t *testing.T) {
	t.Parallel()

	override := ai.CapabilityOverride{
		Vision:           ai.Ptr(true),
		StructuredOutput: ai.Ptr(false),
		Reasoning:        ai.Ptr(true),
	}

	tests := []struct {
		provider ai.Provider
		protocol config.Protocol
		baseURL  string
	}{
		{provider: ai.ProviderOpenAI, protocol: config.ProtocolOpenAIResponses, baseURL: "https://api.openai.com/v1"},
		{provider: ai.ProviderAnthropic, protocol: config.ProtocolAnthropicMessages, baseURL: "https://api.anthropic.com/v1"},
		{provider: ai.ProviderGemini, protocol: config.ProtocolGeminiGenerateContent, baseURL: "https://generativelanguage.googleapis.com/v1beta"},
	}

	for _, test := range tests {
		t.Run(string(test.provider), func(t *testing.T) {
			t.Parallel()

			resolved := modelcatalog.ResolvedModel{
				Ref:      config.ModelRef{Provider: test.provider, Model: "model"},
				Protocol: test.protocol,
				Endpoint: modelcatalog.Endpoint{BaseURL: test.baseURL},
			}

			baseline, err := model.New(t.Context(), resolved, &credentialStore{})
			require.NoError(t, err)

			resolved.Capabilities = override
			declared, err := model.New(t.Context(), resolved, &credentialStore{})
			require.NoError(t, err)

			want := override.Apply(baseline.Capabilities())
			assert.Equal(t, want, declared.Capabilities())
			assert.True(t, declared.Capabilities().Vision)
			assert.False(t, declared.Capabilities().StructuredOutput)
		})
	}
}

func TestNewWithoutDeclarationKeepsAdapterDefaults(t *testing.T) {
	t.Parallel()

	resolved := modelcatalog.ResolvedModel{
		Ref:      config.ModelRef{Provider: ai.ProviderOpenAI, Model: "gpt-4o"},
		Protocol: config.ProtocolOpenAIResponses,
		Endpoint: modelcatalog.Endpoint{BaseURL: "https://api.openai.com/v1"},
	}
	bound, err := model.New(t.Context(), resolved, &credentialStore{})
	require.NoError(t, err)
	assert.Equal(
		t,
		openai.New("gpt-4o").Capabilities(),
		bound.Capabilities(),
		"with no declaration the adapter's own table still applies",
	)
}

// TestConfiguredReasoningLevelReachesTheWire walks the whole coding path for
// the families the compatibility policy stopped omitting: configuration ->
// catalog -> generation policy -> adapter -> HTTP body. It proves a configured
// level both starts the agent and is actually sent, rather than being withheld
// by a capability guess (.trellis/spec/backend/provider-compatibility-policy.md
// R1).
func TestConfiguredReasoningLevelReachesTheWire(t *testing.T) {
	t.Parallel()

	tests := []struct {
		provider ai.Provider
		model    string
	}{
		{provider: ai.ProviderQwen, model: "qwen3.8-max"},
		{provider: ai.ProviderKimi, model: "kimi-k3"},
		{provider: ai.ProviderMiniMax, model: "MiniMax-M3"},
	}

	for _, tt := range tests {
		t.Run(string(tt.provider), func(t *testing.T) {
			t.Parallel()

			var captured map[string]any

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&captured)) {
					return
				}

				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(
					`{"id":"chat_1","model":"` + tt.model +
						`","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
				))
			}))
			t.Cleanup(server.Close)

			high := config.ReasoningLevel(ai.ReasoningHigh)
			ref := config.ModelRef{Provider: tt.provider, Model: tt.model}
			cfg := config.Config{
				Model: ref,
				Models: []config.ModelConfig{{
					Ref: ref,
					ReasoningLevels: []config.ReasoningLevel{
						config.ReasoningLevel(ai.ReasoningLow),
						config.ReasoningLevel(ai.ReasoningMedium),
						high,
					},
					DefaultReasoningLevel: &high,
				}},
				Providers: map[ai.Provider]config.ProviderConfig{
					tt.provider: {
						BaseURL:         server.URL + "/v1",
						AllowHTTP:       true,
						AllowPrivateIPs: true,
					},
				},
				Sandbox:  config.SandboxWorkspaceWrite,
				Approval: config.ApprovalOnRequest,
			}

			catalog, err := modelcatalog.New(cfg)
			require.NoError(t, err)

			resolved, err := catalog.Resolve(modelcatalog.Selection{Ref: ref})
			require.NoError(t, err)

			policy, err := generation.Compile(resolved)
			require.NoError(t, err)

			bound, err := model.New(t.Context(), resolved, &credentialStore{})
			require.NoError(t, err)

			request := ai.Request{Messages: []ai.Message{ai.UserText("hello")}}
			policy(&request)

			_, err = bound.Generate(t.Context(), request)
			require.NoError(t, err)

			assert.Equal(t, "high", captured["reasoning_effort"])
		})
	}
}
