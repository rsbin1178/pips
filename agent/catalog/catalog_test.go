package catalog_test

import (
	"context"
	"testing"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/catalog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCatalogSnapshotsEnforceAllowlistRiskAndTenant(t *testing.T) {
	t.Parallel()

	read := agent.NewTool("read_doc", "Read a document", func(context.Context, struct{}) (string, error) { return "", nil })
	write := agent.NewTool("write_doc", "Write a document", func(context.Context, struct{}) (string, error) { return "", nil })
	remote := agent.NewTool("remote_lookup", "MCP lookup", func(context.Context, struct{}) (string, error) { return "", nil })
	c, err := catalog.New(
		catalog.Local("app", catalog.RiskRead, read)[0],
		catalog.Local("app", catalog.RiskWrite, write)[0],
		catalog.MCP("docs", catalog.RiskRead, remote)[0],
	)
	require.NoError(t, err)

	policy := catalog.Policy{
		TenantID:  "tenant-a",
		Allowlist: []string{"read_doc", "write_doc", "remote_lookup"},
		MaxRisk:   catalog.RiskRead,
		Authorize: func(_ context.Context, tenant string, descriptor catalog.Descriptor) (bool, error) {
			return tenant == "tenant-a" && descriptor.Source.Kind != catalog.SourceMCP, nil
		},
	}

	snapshot, err := c.Snapshot(t.Context(), policy)
	require.NoError(t, err)
	require.Len(t, snapshot, 1)
	assert.Equal(t, "read_doc", snapshot[0].Decl().Name)

	result, err := c.Search(t.Context(), policy, "document")
	require.Len(t, result, 1)
	require.NoError(t, err)
	assert.Equal(t, "read_doc", result[0].Name)
}

func TestCatalogIsDenyByDefault(t *testing.T) {
	t.Parallel()

	tool := agent.NewTool("read_doc", "Read a document", func(context.Context, struct{}) (string, error) { return "", nil })
	c, err := catalog.New(catalog.Local("app", catalog.RiskRead, tool)[0])
	require.NoError(t, err)

	snapshot, err := c.Snapshot(t.Context(), catalog.Policy{})
	require.NoError(t, err)
	assert.Empty(t, snapshot)

	_, err = c.Tools(t.Context(), catalog.Policy{}, "read_doc")
	require.ErrorContains(t, err, "not authorized")
}
