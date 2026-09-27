//nolint:wsl_v5 // Expansion fixtures keep each call adjacent to its assertion.
package mcp

import (
	"testing"

	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExpandCredentialReferencesIsSinglePassAndLiteralOtherwise(t *testing.T) {
	t.Parallel()

	environment := map[string]string{
		"TOKEN": "t0k", "NESTED": "${env:TOKEN}", "EMPTY": "",
	}
	lookup := func(name string) (string, bool) {
		value, ok := environment[name]

		return value, ok
	}

	expanded, err := expandCredentialReferences("Bearer ${env:TOKEN}; ${HOME} ${env:TOKEN}", lookup)
	require.NoError(t, err)
	assert.Equal(t, "Bearer t0k; ${HOME} t0k", expanded)

	expanded, err = expandCredentialReferences("${env:NESTED}", lookup)
	require.NoError(t, err)
	assert.Equal(t, "${env:TOKEN}", expanded, "replacement text is never rescanned")

	for _, missing := range []string{"${env:UNSET}", "${env:EMPTY}"} {
		_, err = expandCredentialReferences(missing, lookup)
		var credential credentialError
		require.ErrorAs(t, err, &credential)
		assert.NotContains(t, err.Error(), "t0k")
	}

	_, err = expandCredentialReferences("${env:TOKEN}", nil)
	require.ErrorAs(t, err, new(credentialError))
}

func TestResolveCredentialsOnlyForNativeScopesAndRejectsUnsafeValues(t *testing.T) {
	t.Parallel()

	lookup := func(name string) (string, bool) {
		values := map[string]string{"TOKEN": "secret", "SPLIT": "a\r\nInjected: yes"}
		value, ok := values[name]

		return value, ok
	}
	definition := Definition{
		Scope:       ScopeUser,
		Environment: []execution.EnvVar{{Name: "SERVICE_TOKEN", Value: "${env:TOKEN}"}},
		Headers:     []HTTPHeader{{Name: "Authorization", Value: "Bearer ${env:TOKEN}"}},
	}
	environment, err := resolveEnvironment(definition, lookup)
	require.NoError(t, err)
	assert.Equal(t, []execution.EnvVar{{Name: "SERVICE_TOKEN", Value: "secret"}}, environment)
	headers, err := resolveHeaders(definition, lookup)
	require.NoError(t, err)
	assert.Equal(t, []HTTPHeader{{Name: "Authorization", Value: "Bearer secret"}}, headers)
	assert.Equal(t, "${env:TOKEN}", definition.Environment[0].Value, "templates stay in the definition")

	definition.Headers = []HTTPHeader{{Name: "X-Key", Value: "${env:SPLIT}"}}
	_, err = resolveHeaders(definition, lookup)
	require.ErrorAs(t, err, new(credentialError))
	assert.NotContains(t, err.Error(), "Injected")
	assert.Contains(t, err.Error(), "SPLIT")

	for _, scope := range []Scope{ScopeAgentPlugin, ScopeSession} {
		literal := Definition{Scope: scope, Headers: []HTTPHeader{{Name: "X-Key", Value: "${env:TOKEN}"}}}
		headers, err := resolveHeaders(literal, lookup)
		require.NoError(t, err)
		assert.Equal(t, "${env:TOKEN}", headers[0].Value)
		assert.False(t, literal.hasCredentialReferences())
	}
}
