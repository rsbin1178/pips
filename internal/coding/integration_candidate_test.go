package coding

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rsbin/pips/internal/coding/agentplugin"
	"github.com/rsbin/pips/internal/coding/agentprofile"
	codingmcp "github.com/rsbin/pips/internal/coding/mcp"
	"github.com/rsbin/pips/internal/coding/resource"
	"github.com/rsbin/pips/internal/coding/skillsettings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegrationCandidateAbortClosesResourcesInReverseOrder(t *testing.T) {
	t.Parallel()

	firstErr := errors.New("first cleanup failed")
	secondErr := errors.New("second cleanup failed")
	order := []string{}
	candidate := &integrationCandidate{}
	candidate.cleanup.add(func(context.Context) error {
		order = append(order, "first")

		return firstErr
	})
	candidate.cleanup.add(func(context.Context) error {
		order = append(order, "second")

		return secondErr
	})

	err := candidate.abort(t.Context())
	require.ErrorIs(t, err, firstErr)
	require.ErrorIs(t, err, secondErr)
	assert.Equal(t, []string{"second", "first"}, order)
	require.NoError(t, candidate.abort(t.Context()))
	assert.Equal(t, []string{"second", "first"}, order)
}

func TestIntegrationCandidatePublishTransfersCleanupOwnership(t *testing.T) {
	t.Parallel()

	generation := newIntegrationGeneration(
		1,
		nil,
		nil,
		resource.Result{},
		agentprofile.Registry{},
		skillsettings.Empty(),
		"",
		agentplugin.Result{},
		nil,
	)
	cleaned := false
	candidate := &integrationCandidate{generation: generation}
	candidate.cleanup.add(func(context.Context) error {
		cleaned = true

		return nil
	})

	published, err := candidate.publish()
	require.NoError(t, err)
	assert.Same(t, generation, published)
	require.NoError(t, candidate.abort(t.Context()))
	assert.False(t, cleaned)

	_, err = candidate.publish()
	assert.ErrorIs(t, err, ErrRuntimeInvalid)
}

func TestMergeAgentPluginDefinitionsIsolatesDuplicateAndLimit(t *testing.T) {
	t.Parallel()

	definition := func(id string, scope codingmcp.Scope) codingmcp.Definition {
		return codingmcp.Definition{
			ID: id, Scope: scope, Transport: codingmcp.TransportStreamableHTTP,
			URL: "https://example.com/mcp", ConnectTimeout: time.Second,
		}
	}
	base, err := codingmcp.NewDefinitions([]codingmcp.Definition{
		definition("shared", codingmcp.ScopeUser),
	}, 2)
	require.NoError(t, err)
	plugins, err := codingmcp.NewDefinitions([]codingmcp.Definition{
		definition("shared", codingmcp.ScopeAgentPlugin),
		definition("plugin-one", codingmcp.ScopeAgentPlugin),
		definition("plugin-two", codingmcp.ScopeAgentPlugin),
	}, 3)
	require.NoError(t, err)

	merged, diagnostics, err := mergeAgentPluginDefinitions(base, plugins, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"shared", "plugin-one"}, definitionIDs(merged.List()))
	require.Len(t, diagnostics, 2)
	assert.Equal(t, "definition_duplicate", diagnostics[0].Code)
	assert.Equal(t, "definition_limit", diagnostics[1].Code)
}

func definitionIDs(definitions []codingmcp.Definition) []string {
	values := make([]string, len(definitions))
	for index, definition := range definitions {
		values[index] = definition.ID
	}

	return values
}
