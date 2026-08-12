//nolint:wsl_v5 // Adapter fixtures keep acquisition and assertion steps adjacent.
package model_test

import (
	"context"
	"testing"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/credential"
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
