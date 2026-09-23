package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewToolSearchValidatesName(t *testing.T) {
	t.Parallel()

	read := agent.NewTool("read_doc", "Read a document", func(context.Context, struct{}) (string, error) { return "", nil })
	c, err := catalog.New(catalog.Local("app", catalog.RiskRead, read)...)
	require.NoError(t, err)

	policy := catalog.AllowAll("test", catalog.RiskRead)

	search, err := catalog.NewToolSearch(c, policy, catalog.ToolSearchOptions{Enabled: true})
	require.NoError(t, err)
	assert.Equal(t, catalog.DefaultToolSearchName, search.Name())

	search, err = catalog.NewToolSearch(c, policy, catalog.ToolSearchOptions{Enabled: true, Name: "find-Tools_2"})
	require.NoError(t, err)
	assert.Equal(t, "find-Tools_2", search.Name())

	for _, invalid := range []string{"bad name", "tool.search", strings.Repeat("a", 65), "tool/search"} {
		_, err = catalog.NewToolSearch(c, policy, catalog.ToolSearchOptions{Enabled: true, Name: invalid})
		require.Error(t, err, invalid)
		require.Error(t, catalog.ValidateToolSearchName(invalid), invalid)
	}

	_, err = catalog.NewToolSearch(c, policy, catalog.ToolSearchOptions{Enabled: true, Name: "read_doc"})
	require.ErrorContains(t, err, `"read_doc" is reserved`)

	initial := agent.NewTool("status", "", func(context.Context, struct{}) (string, error) { return "", nil })
	_, err = catalog.NewToolSearch(c, policy, catalog.ToolSearchOptions{
		Enabled: true, Name: "status", Initial: []agent.Tool{initial},
	})
	require.ErrorContains(t, err, `duplicate initial tool "status"`)
}

func TestToolSearchIsHiddenWithoutDeferredTools(t *testing.T) {
	t.Parallel()

	read := agent.NewTool("read_doc", "Read a document", func(context.Context, struct{}) (string, error) { return "", nil })
	lookup := agent.NewTool("lookup_docs", "Look up documentation", func(context.Context, struct{}) (string, error) { return "", nil })
	policy := catalog.AllowAll("test", catalog.RiskRead)

	localOnly, err := catalog.New(catalog.Local("app", catalog.RiskRead, read)...)
	require.NoError(t, err)
	search, err := catalog.NewToolSearch(localOnly, policy, catalog.ToolSearchOptions{Enabled: true})
	require.NoError(t, err)
	tools, err := search.Tools(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"read_doc"}, toolNames(tools))
	sources, err := search.DeferredSources(t.Context())
	require.NoError(t, err)
	assert.Empty(t, sources)

	// The executable set keeps a hidden copy so a blind call is answered.
	executable, err := search.ExecutableTools(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"read_doc", catalog.DefaultToolSearchName}, toolNames(executable))
	assert.True(t, executable[1].Decl().Disabled)

	withMCP, err := catalog.New(
		catalog.Local("app", catalog.RiskRead, read)[0],
		catalog.MCP("docs", catalog.RiskRead, lookup)[0],
	)
	require.NoError(t, err)
	search, err = catalog.NewToolSearch(withMCP, policy, catalog.ToolSearchOptions{Enabled: true})
	require.NoError(t, err)
	tools, err = search.Tools(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"read_doc", catalog.DefaultToolSearchName}, toolNames(tools))
	assert.False(t, tools[1].Decl().Disabled)
	executable, err = search.ExecutableTools(t.Context())
	require.NoError(t, err)
	assert.Equal(t, toolNames(tools), toolNames(executable))
	sources, err = search.DeferredSources(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []catalog.SourceSummary{{Kind: catalog.SourceMCP, ID: "docs", Tools: 1}}, sources)

	// A deferred tool the policy does not authorize does not surface the tool.
	denied := catalog.Policy{TenantID: "test", Allowlist: []string{"read_doc"}, MaxRisk: catalog.RiskRead}
	search, err = catalog.NewToolSearch(withMCP, denied, catalog.ToolSearchOptions{Enabled: true})
	require.NoError(t, err)
	tools, err = search.Tools(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"read_doc"}, toolNames(tools))

	// Disabled search never reports discoverable sources.
	search, err = catalog.NewToolSearch(withMCP, policy, catalog.ToolSearchOptions{})
	require.NoError(t, err)
	sources, err = search.DeferredSources(t.Context())
	require.NoError(t, err)
	assert.Empty(t, sources)
}

func TestToolSearchReportsSourcesAndHints(t *testing.T) {
	t.Parallel()

	read := agent.NewTool("read_doc", "Read a document", func(context.Context, struct{}) (string, error) { return "", nil })
	issues := agent.NewTool("list_issues", "List GitHub issues", func(context.Context, struct{}) (string, error) { return "", nil })
	pulls := agent.NewTool("list_pulls", "List GitHub pull requests", func(context.Context, struct{}) (string, error) { return "", nil })
	post := agent.NewTool("post_message", "Post a Slack message", func(context.Context, struct{}) (string, error) { return "", nil })
	policy := catalog.AllowAll("test", catalog.RiskRead)

	t.Run("nothing deferred", func(t *testing.T) {
		t.Parallel()

		c, err := catalog.New(catalog.Local("app", catalog.RiskRead, read)...)
		require.NoError(t, err)
		search, err := catalog.NewToolSearch(c, policy, catalog.ToolSearchOptions{Enabled: true})
		require.NoError(t, err)

		result := runToolSearch(t, search, `{"query":"web search tool"}`)
		assert.JSONEq(
			t,
			`{"matches":[],"activated":[],"sources":[],"hint":"No MCP or Extension tools are registered in this session; nothing can be discovered."}`,
			result,
		)

		// The hint answers even an argument-less probe.
		assert.Contains(t, runToolSearch(t, search, `{}`), "nothing can be discovered")
	})

	t.Run("miss lists sources", func(t *testing.T) {
		t.Parallel()

		c, err := catalog.New(
			catalog.Local("app", catalog.RiskRead, read)[0],
			catalog.MCP("github", catalog.RiskRead, issues)[0],
			catalog.MCP("github", catalog.RiskRead, pulls)[0],
			catalog.MCP("slack", catalog.RiskRead, post)[0],
		)
		require.NoError(t, err)
		search, err := catalog.NewToolSearch(c, policy, catalog.ToolSearchOptions{Enabled: true})
		require.NoError(t, err)

		type decodedResult struct {
			Matches   []json.RawMessage       `json:"matches"`
			Activated []string                `json:"activated"`
			Sources   []catalog.SourceSummary `json:"sources"`
			Hint      string                  `json:"hint"`
		}

		var decoded decodedResult
		require.NoError(t, json.Unmarshal([]byte(runToolSearch(t, search, `{"query":"deploy kubernetes"}`)), &decoded))
		assert.Empty(t, decoded.Matches)
		assert.Empty(t, decoded.Activated)
		assert.Equal(t, []catalog.SourceSummary{
			{Kind: catalog.SourceMCP, ID: "github", Tools: 2},
			{Kind: catalog.SourceMCP, ID: "slack", Tools: 1},
		}, decoded.Sources)
		assert.Equal(
			t,
			"No deferred tool matched. Searchable sources: mcp/github (2), mcp/slack (1). Try broader or different terms.",
			decoded.Hint,
		)

		var hit decodedResult
		require.NoError(t, json.Unmarshal([]byte(runToolSearch(t, search, `{"query":"github issues"}`)), &hit))
		assert.Empty(t, hit.Hint)
		assert.Equal(t, []string{"list_issues", "list_pulls"}, hit.Activated)
		assert.Len(t, hit.Sources, 2)
	})
}

// runToolSearch runs one scripted agent turn that calls the discovery tool by
// name and returns the tool result text the model would see. AgentOptions
// installs ExecutableTools, so the call succeeds even when the tool is hidden.
func runToolSearch(t *testing.T, search *catalog.ToolSearch, args string) string {
	t.Helper()

	opts, err := search.AgentOptions(t.Context())
	require.NoError(t, err)

	model := &generateOnlyModel{responses: []*ai.Response{
		{
			Message:      ai.Assistant(ai.ToolCallPart{ID: "call-1", Name: search.Name(), Args: ai.JSON(args)}),
			FinishReason: ai.FinishToolCalls,
		},
		{Message: ai.AssistantText("done"), FinishReason: ai.FinishStop},
	}}
	a, err := agent.New(model, opts...)
	require.NoError(t, err)

	sess := agent.NewSession()
	_, err = a.Run(t.Context(), sess, ai.UserText("find tools"))
	require.NoError(t, err)

	for _, message := range sess.Messages() {
		toolMessage, ok := message.(ai.ToolMessage)
		if !ok {
			continue
		}

		for _, part := range toolMessage.Parts {
			if part.ToolCallID != "call-1" {
				continue
			}

			require.False(t, part.IsError, "tool result: %v", part.Content)
			require.Len(t, part.Content, 1)
			text, ok := part.Content[0].(ai.TextPart)
			require.True(t, ok)

			return text.Text
		}
	}

	t.Fatal("tool result not found")

	return ""
}

type generateOnlyModel struct {
	responses []*ai.Response
	position  int
}

func (m *generateOnlyModel) Generate(_ context.Context, _ ai.Request) (*ai.Response, error) {
	if m.position >= len(m.responses) {
		return nil, errors.New("script exhausted")
	}

	response := m.responses[m.position]
	m.position++

	return response, nil
}

func (m *generateOnlyModel) Stream(context.Context, ai.Request) ai.Stream {
	return func(yield func(ai.StreamEvent, error) bool) {
		yield(ai.StreamEvent{}, errors.New("streaming is not scripted"))
	}
}

func (m *generateOnlyModel) Provider() ai.Provider { return ai.Provider("scripted") }
func (m *generateOnlyModel) ModelID() string       { return "scripted" }
func (m *generateOnlyModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Text: true, Tools: true}
}

func toolNames(tools []agent.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Decl().Name)
	}

	return names
}
