package catalog_test

import (
	"context"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearchTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  []string
	}{
		{name: "snake case", value: "web_search", want: []string{"web", "search"}},
		{name: "kebab and camel", value: "list-openIssues", want: []string{"list", "open", "issue"}},
		{name: "acronym boundary", value: "HTTPServer", want: []string{"http", "server"}},
		{name: "plural stem", value: "search tools", want: []string{"search", "tool"}},
		{name: "short plural kept", value: "gas", want: []string{"gas"}},
		{name: "double s kept", value: "class", want: []string{"class"}},
		{name: "punctuation and digits", value: "s3.Get_Object2", want: []string{"s3", "get", "object2"}},
		{name: "empty", value: "  ", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, catalog.SearchTokens(tt.value))
		})
	}
}

func TestSearchRanksNameAboveParameterAboveDescription(t *testing.T) {
	t.Parallel()

	byName := agent.NewTool("web_search", "Look things up online", func(context.Context, struct{}) (string, error) { return "", nil })
	byParam := agent.NewTool("fetch_page", "Download a page", func(_ context.Context, _ struct {
		SearchQuery string `json:"search_query" description:"Query for the fetch"`
	},
	) (string, error) {
		return "", nil
	})
	byDescription := agent.NewTool("read_doc", "Search a document for text", func(context.Context, struct{}) (string, error) { return "", nil })
	unrelated := agent.NewTool("deploy", "Ship a release", func(context.Context, struct{}) (string, error) { return "", nil })

	c, err := catalog.New(
		catalog.Local("app", catalog.RiskRead, unrelated, byDescription, byParam, byName)...,
	)
	require.NoError(t, err)

	policy := catalog.AllowAll("test", catalog.RiskRead)

	ranked, err := c.Search(t.Context(), policy, "web search")
	require.NoError(t, err)
	assert.Equal(t, []string{"web_search", "fetch_page", "read_doc"}, descriptorNames(ranked))

	// The empty query keeps registration order and every authorized entry.
	all, err := c.Search(t.Context(), policy, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"deploy", "read_doc", "fetch_page", "web_search"}, descriptorNames(all))
}

func TestSearchMatchesParameterDescriptionsAndPrefixes(t *testing.T) {
	t.Parallel()

	typed := agent.NewTool("lookup", "Find records", func(_ context.Context, _ struct {
		Q string `json:"q" description:"query string to search"`
	},
	) (string, error) {
		return "", nil
	})
	rawSchema, err := ai.ParseSchema([]byte(`{
		"type": "object",
		"properties": {
			"repository": {"type": "string", "description": "Owner and repository name"},
			"flag": true
		}
	}`))
	require.NoError(t, err)

	remote := rawSchemaTool{decl: ai.Tool{Name: "list_issues", Description: "Enumerate open items", InputSchema: rawSchema}}
	searching := agent.NewTool("grep_files", "Searching workspace files", func(context.Context, struct{}) (string, error) { return "", nil })

	c, err := catalog.New(
		catalog.Local("app", catalog.RiskRead, typed, searching)[0],
		catalog.Local("app", catalog.RiskRead, typed, searching)[1],
		catalog.MCP("github", catalog.RiskRead, remote)[0],
	)
	require.NoError(t, err)

	policy := catalog.AllowAll("test", catalog.RiskRead)

	found, err := c.Search(t.Context(), policy, "search")
	require.NoError(t, err)
	assert.Equal(t, []string{"grep_files", "lookup"}, descriptorNames(found))

	found, err = c.Search(t.Context(), policy, "repository")
	require.NoError(t, err)
	assert.Equal(t, []string{"list_issues"}, descriptorNames(found))

	found, err = c.Search(t.Context(), policy, "github")
	require.NoError(t, err)
	assert.Equal(t, []string{"list_issues"}, descriptorNames(found))

	found, err = c.Search(t.Context(), policy, "nothing_here")
	require.NoError(t, err)
	assert.Empty(t, found)
}

func TestSearchRequiresOnlyOneTermAndBreaksTiesByName(t *testing.T) {
	t.Parallel()

	beta := agent.NewTool("beta_sync", "Sync data", func(context.Context, struct{}) (string, error) { return "", nil })
	alpha := agent.NewTool("alpha_sync", "Sync data", func(context.Context, struct{}) (string, error) { return "", nil })
	c, err := catalog.New(catalog.Local("app", catalog.RiskRead, beta, alpha)...)
	require.NoError(t, err)

	found, err := c.Search(t.Context(), catalog.AllowAll("test", catalog.RiskRead), "sync unrelated")
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha_sync", "beta_sync"}, descriptorNames(found))
}

type rawSchemaTool struct {
	decl ai.Tool
}

func (t rawSchemaTool) Decl() ai.Tool { return t.decl }

func (rawSchemaTool) Exec(context.Context, agent.ToolCall) ([]ai.Part, error) {
	return nil, nil
}

func descriptorNames(descriptors []catalog.Descriptor) []string {
	names := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		names = append(names, descriptor.Name)
	}

	return names
}
