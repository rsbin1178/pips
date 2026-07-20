package catalog_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/catalog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMergePreservesCatalogOrderAndMetadata(t *testing.T) {
	t.Parallel()

	read := agent.NewTool("read_doc", "Read a document", func(context.Context, struct{}) (string, error) { return "", nil })
	lookup := agent.NewTool("remote_lookup", "Look up remote documentation", func(context.Context, struct{}) (string, error) { return "", nil })
	review := agent.NewTool("review_code", "Review source code", func(context.Context, struct{}) (string, error) { return "", nil })

	local, err := catalog.New(catalog.Entry{
		Tool: read, Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding"},
		Risk: catalog.RiskRead, Tags: []string{"builtin", "filesystem"},
	})
	require.NoError(t, err)

	remote, err := catalog.New(catalog.MCP("docs", catalog.RiskRead, lookup)[0])
	require.NoError(t, err)

	extension, err := catalog.New(catalog.Entry{
		Tool: review, Source: catalog.Source{Kind: catalog.SourceExtension, ID: "review"},
		Risk: catalog.RiskWrite, Tags: []string{"quality"},
	})
	require.NoError(t, err)

	merged, err := catalog.Merge(local, remote, extension)
	require.NoError(t, err)

	policy := catalog.AllowAll("test", catalog.RiskPrivileged)
	tools, err := merged.Snapshot(t.Context(), policy)
	require.NoError(t, err)
	require.Len(t, tools, 3)
	assert.Equal(t, []string{"read_doc", "remote_lookup", "review_code"}, []string{
		tools[0].Decl().Name,
		tools[1].Decl().Name,
		tools[2].Decl().Name,
	})

	descriptors, err := merged.Search(t.Context(), policy, "")
	require.NoError(t, err)
	require.Len(t, descriptors, 3)
	assert.Equal(t, catalog.Descriptor{
		Name: "read_doc", Description: "Read a document",
		Source: catalog.Source{Kind: catalog.SourceLocal, ID: "coding"},
		Risk:   catalog.RiskRead, Tags: []string{"builtin", "filesystem"},
	}, descriptors[0])
	assert.Equal(t, catalog.Source{Kind: catalog.SourceMCP, ID: "docs"}, descriptors[1].Source)
	assert.Equal(t, catalog.Source{Kind: catalog.SourceExtension, ID: "review"}, descriptors[2].Source)
	assert.Equal(t, catalog.RiskWrite, descriptors[2].Risk)
	assert.Equal(t, []string{"quality"}, descriptors[2].Tags)
}

func TestMergeValidatesInputsAndCopiesMetadata(t *testing.T) {
	t.Parallel()

	tags := []string{"original"}
	tool := agent.NewTool("read_doc", "Read a document", func(context.Context, struct{}) (string, error) { return "", nil })
	source, err := catalog.New(catalog.Entry{
		Tool: tool, Source: catalog.Source{Kind: catalog.SourceLocal, ID: "app"},
		Risk: catalog.RiskRead, Tags: tags,
	})
	require.NoError(t, err)

	tags[0] = "mutated source"
	merged, err := catalog.Merge(source)
	require.NoError(t, err)

	descriptors, err := merged.Search(t.Context(), catalog.AllowAll("test", catalog.RiskRead), "")
	require.NoError(t, err)
	require.Len(t, descriptors, 1)
	assert.Equal(t, []string{"original"}, descriptors[0].Tags)

	descriptors[0].Tags[0] = "mutated result"
	again, err := merged.Search(t.Context(), catalog.AllowAll("test", catalog.RiskRead), "")
	require.NoError(t, err)
	assert.Equal(t, []string{"original"}, again[0].Tags)

	empty, err := catalog.Merge()
	require.NoError(t, err)
	tools, err := empty.Snapshot(t.Context(), catalog.AllowAll("test", catalog.RiskPrivileged))
	require.NoError(t, err)
	assert.Empty(t, tools)

	_, err = catalog.Merge(source, nil)
	require.ErrorContains(t, err, "catalog 1 is nil")

	_, err = catalog.Merge(source, source)
	require.ErrorContains(t, err, `duplicate tool name "read_doc"`)
}

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

func ExampleMerge() {
	read := agent.NewTool("read_doc", "Read a document", func(context.Context, struct{}) (string, error) {
		return "", nil
	})
	lookup := agent.NewTool("remote_lookup", "Look up documentation", func(context.Context, struct{}) (string, error) {
		return "", nil
	})

	local, _ := catalog.New(catalog.Local("app", catalog.RiskRead, read)...)
	remote, _ := catalog.New(catalog.MCP("docs", catalog.RiskRead, lookup)...)
	merged, _ := catalog.Merge(local, remote)

	tools, _ := merged.Snapshot(context.Background(), catalog.AllowAll("example", catalog.RiskRead))
	for _, tool := range tools {
		fmt.Println(tool.Decl().Name)
	}
	// Output:
	// read_doc
	// remote_lookup
}
