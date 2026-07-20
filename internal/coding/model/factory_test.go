package model_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/credential"
	"github.com/rsbin/pips/internal/coding/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type credentialStore struct {
	credential credential.Credential
	err        error
	provider   ai.Provider
}

func (s *credentialStore) Get(_ context.Context, provider ai.Provider) (credential.Credential, error) {
	s.provider = provider
	return s.credential, s.err
}

func TestNewProviderModels(t *testing.T) {
	t.Parallel()

	environment, err := credential.NewEnvironmentStore(func(string) (string, bool) {
		return "sentinel-secret", true
	})
	require.NoError(t, err)

	for _, provider := range []ai.Provider{
		ai.ProviderOpenAI,
		ai.ProviderAnthropic,
		ai.ProviderGemini,
	} {
		t.Run(string(provider), func(t *testing.T) {
			t.Parallel()

			got, err := model.New(t.Context(), config.ModelConfig{
				Provider: provider,
				ID:       "test-model",
			}, environment)
			require.NoError(t, err)
			assert.Equal(t, provider, got.Provider())
			assert.Equal(t, "test-model", got.ModelID())
		})
	}
}

func TestNewRejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	validStore, err := credential.NewEnvironmentStore(func(string) (string, bool) { return "secret", true })
	require.NoError(t, err)

	_, err = model.New(t.Context(), config.ModelConfig{}, validStore)
	require.ErrorIs(t, err, model.ErrInvalid)

	_, err = model.New(t.Context(), config.ModelConfig{Provider: ai.ProviderOpenAI, ID: "model"}, nil)
	require.ErrorIs(t, err, model.ErrInvalid)

	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = model.New(canceled, config.ModelConfig{Provider: ai.ProviderOpenAI, ID: "model"}, validStore)
	require.ErrorIs(t, err, context.Canceled)
}

func TestNewPropagatesCredentialErrorWithoutSecret(t *testing.T) {
	t.Parallel()

	const secret = "sentinel-secret-value"

	cause := errors.New("credential backend unavailable")
	store := &credentialStore{err: cause}

	_, err := model.New(t.Context(), config.ModelConfig{
		Provider: ai.ProviderOpenAI,
		ID:       "model",
	}, store)
	require.Error(t, err)
	require.ErrorIs(t, err, cause)
	assert.Equal(t, ai.ProviderOpenAI, store.provider)
	assert.NotContains(t, err.Error(), secret)
}
