package credential_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding/credential"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnvironmentStoreUsesOneProviderNeutralVariable(t *testing.T) {
	t.Parallel()

	store, err := credential.NewEnvironmentStore(func(key string) (string, bool) {
		if key != credential.APIKeyEnv {
			return "", false
		}

		return "selected-provider-secret", true
	})
	require.NoError(t, err)

	for _, provider := range []ai.Provider{
		ai.ProviderOpenAI,
		ai.ProviderAnthropic,
		ai.ProviderGemini,
	} {
		t.Run(string(provider), func(t *testing.T) {
			t.Parallel()

			got, err := store.Get(t.Context(), provider)
			require.NoError(t, err)
			assert.Equal(t, "selected-provider-secret", got.APIKey())
		})
	}
}

func TestEnvironmentStoreDoesNotReadProviderSDKVariables(t *testing.T) {
	t.Parallel()

	lookups := make([]string, 0, 1)
	store, err := credential.NewEnvironmentStore(func(key string) (string, bool) {
		lookups = append(lookups, key)
		values := map[string]string{
			"OPENAI_API_KEY":    "openai-secret",
			"ANTHROPIC_API_KEY": "anthropic-secret",
			"GEMINI_API_KEY":    "gemini-secret",
			"GOOGLE_API_KEY":    "google-secret",
		}
		value, ok := values[key]

		return value, ok
	})
	require.NoError(t, err)

	_, err = store.Get(t.Context(), ai.ProviderGemini)
	require.ErrorIs(t, err, credential.ErrNotFound)
	assert.Equal(t, []string{credential.APIKeyEnv}, lookups)
}

func TestEnvironmentStoreErrors(t *testing.T) {
	t.Parallel()

	_, err := credential.NewEnvironmentStore(nil)
	require.Error(t, err)

	store, err := credential.NewEnvironmentStore(func(string) (string, bool) { return " ", true })
	require.NoError(t, err)

	_, err = store.Get(t.Context(), ai.ProviderOpenAI)
	require.Error(t, err)
	require.ErrorIs(t, err, credential.ErrNotFound)
	assert.Contains(t, err.Error(), credential.APIKeyEnv)

	_, err = store.Get(t.Context(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty provider")

	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = store.Get(canceled, ai.ProviderOpenAI)
	require.ErrorIs(t, err, context.Canceled)
}

func TestCredentialFormattingAndJSONAreRedacted(t *testing.T) {
	t.Parallel()

	const secret = "sentinel-secret-value"

	store, err := credential.NewEnvironmentStore(func(string) (string, bool) { return secret, true })
	require.NoError(t, err)
	value, err := store.Get(t.Context(), ai.ProviderOpenAI)
	require.NoError(t, err)

	for _, formatted := range []string{
		fmt.Sprint(value),
		fmt.Sprintf("%v", value),
		fmt.Sprintf("%+v", value),
		fmt.Sprintf("%#v", value),
		fmt.Sprintf("%s", value),
	} {
		assert.NotContains(t, formatted, secret)
		assert.Contains(t, formatted, "redacted")
	}

	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), secret)
}

func TestMissingErrorNeverContainsLookedUpValue(t *testing.T) {
	t.Parallel()

	const secret = "sentinel-secret-value"

	store, err := credential.NewEnvironmentStore(func(string) (string, bool) { return secret, false })
	require.NoError(t, err)
	_, err = store.Get(t.Context(), ai.ProviderAnthropic)
	require.Error(t, err)
	require.NotErrorIs(t, err, context.Canceled)
	assert.NotContains(t, err.Error(), secret)
}
