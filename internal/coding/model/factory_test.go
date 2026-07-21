//nolint:wsl_v5 // Adapter fixtures keep acquisition and assertion steps adjacent.
package model_test

import (
	"context"
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/credential"
	"github.com/rsbin/pips/internal/coding/model"
	"github.com/rsbin/pips/internal/coding/modelcatalog"
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
		api      config.API
		baseURL  string
	}{
		{provider: ai.ProviderOpenAI, api: config.APIResponses, baseURL: "https://api.openai.com/v1"},
		{provider: ai.ProviderAnthropic, api: config.APIAnthropicMessages, baseURL: "https://api.anthropic.com/v1"},
		{provider: ai.ProviderGemini, api: config.APIGenerateContent, baseURL: "https://generativelanguage.googleapis.com/v1beta"},
		{provider: "local", api: config.APIChatCompletions, baseURL: "http://127.0.0.1:11434/v1"},
	}
	for _, tt := range tests {
		t.Run(string(tt.provider), func(t *testing.T) {
			t.Parallel()
			store := &credentialStore{}
			bound, err := model.New(t.Context(), modelcatalog.ResolvedModel{
				Ref: config.ModelRef{Provider: tt.provider, Model: "model"},
				API: tt.api,
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
		API:      config.APIResponses,
		Endpoint: modelcatalog.Endpoint{BaseURL: "https://api.openai.com/v1"},
	}
	_, err := model.New(t.Context(), resolved, nil)
	require.ErrorIs(t, err, model.ErrInvalid)

	resolved.API = "unknown"
	_, err = model.New(t.Context(), resolved, &credentialStore{})
	require.ErrorIs(t, err, model.ErrInvalid)
}
