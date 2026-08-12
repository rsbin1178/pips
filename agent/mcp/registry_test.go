package agentmcp_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	agentmcp "github.com/rsbin1178/pips/agent/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type registrySource struct {
	tools   []agent.Tool
	err     error
	changed chan struct{}
}

func (s *registrySource) Tools(context.Context) ([]agent.Tool, error) { return s.tools, s.err }
func (s *registrySource) ToolListChanged() <-chan struct{}            { return s.changed }

func TestRegistryRefreshIsAtomicAndVersioned(t *testing.T) {
	t.Parallel()

	source := &registrySource{changed: make(chan struct{}, 1)}
	source.tools = []agent.Tool{agent.NewTool("lookup", "Lookup", func(context.Context, struct{}) (string, error) { return "", nil })}
	registry, err := agentmcp.NewRegistry(agentmcp.RegistryServer{ID: "docs", Source: source, Risk: catalog.RiskRead})
	require.NoError(t, err)

	first, err := registry.Refresh(t.Context())
	require.NoError(t, err)
	assert.Equal(t, uint64(1), first.Version)
	require.Len(t, first.Entries, 1)
	assert.Equal(t, catalog.SourceMCP, first.Entries[0].Source.Kind)

	select {
	case <-registry.Changed():
	default:
		t.Fatal("missing changed signal")
	}

	second, err := registry.Refresh(t.Context())
	require.NoError(t, err)
	assert.Equal(t, first.Version, second.Version)

	source.err = errors.New("offline")
	failed, err := registry.Refresh(t.Context())
	require.ErrorContains(t, err, "offline")
	assert.Equal(t, first.Version, failed.Version)
	assert.Equal(t, first.Version, registry.Snapshot().Version)
}

func TestRegistryRefreshChanged(t *testing.T) {
	t.Parallel()

	source := &registrySource{changed: make(chan struct{}, 1), tools: []agent.Tool{agent.NewTool("lookup", "Lookup", func(context.Context, struct{}) (string, error) { return "", nil })}}
	registry, err := agentmcp.NewRegistry(agentmcp.RegistryServer{ID: "docs", Source: source, Risk: catalog.RiskRead})
	require.NoError(t, err)
	_, err = registry.Refresh(t.Context())
	require.NoError(t, err)

	_, changed, err := registry.RefreshChanged(t.Context())
	require.NoError(t, err)
	assert.False(t, changed)

	source.changed <- struct{}{}

	_, changed, err = registry.RefreshChanged(t.Context())
	require.NoError(t, err)
	assert.True(t, changed)
}
