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

func TestActivationSetIsBoundedByRecency(t *testing.T) {
	t.Parallel()

	set := catalog.NewActivationSet(2)
	set.Add("a", "b")
	set.Add("a") // refresh a; b is now least recent
	set.Add("c")
	assert.Equal(t, []string{"a", "c"}, set.Names())
	assert.False(t, set.Contains("b"))

	var nilSet *catalog.ActivationSet
	nilSet.Add("x")
	assert.Nil(t, nilSet.Names())
}

func exaCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()

	noop := func(context.Context, struct{}) (string, error) { return "ok", nil }
	c, err := catalog.New(
		catalog.Local("app", catalog.RiskRead, agent.NewTool("read_doc", "Read a document", noop))[0],
		catalog.MCP("exa", catalog.RiskPrivileged, agent.NewTool("exa_web_search_exa", "Search the web", noop))[0],
		catalog.MCP("exa", catalog.RiskPrivileged, agent.NewTool("exa_crawling_exa", "Crawl a page", noop))[0],
		catalog.MCP("slack", catalog.RiskPrivileged, agent.NewTool("slack_post", "Post a Slack message", noop))[0],
	)
	require.NoError(t, err)

	return c
}

func TestToolSearchActivationsPersistAcrossInstances(t *testing.T) {
	t.Parallel()

	c := exaCatalog(t)
	policy := catalog.AllowAll("test", catalog.RiskPrivileged)
	set := catalog.NewActivationSet(0)

	first, err := catalog.NewToolSearch(c, policy, catalog.ToolSearchOptions{Enabled: true, Activations: set})
	require.NoError(t, err)
	runToolSearch(t, first, `{"query":"web search","tools":["exa_web_search_exa"]}`)
	assert.Equal(t, []string{"exa_web_search_exa"}, set.Names())

	// A later interaction builds a fresh ToolSearch; its first snapshot
	// already contains the activated tool.
	second, err := catalog.NewToolSearch(c, policy, catalog.ToolSearchOptions{Enabled: true, Activations: set})
	require.NoError(t, err)
	tools, err := second.Tools(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"read_doc", "exa_web_search_exa", catalog.DefaultToolSearchName}, toolNames(tools))
	assert.Equal(t, agent.ToolDecision{}, second.BeforeTool(t.Context(), agent.ToolCallInfo{
		ToolCall: agent.ToolCall{Name: "exa_web_search_exa"},
	}))

	// Activations are re-authorized: a narrower policy drops the name.
	narrow := catalog.Policy{TenantID: "test", Allowlist: []string{"read_doc"}, MaxRisk: catalog.RiskPrivileged}
	third, err := catalog.NewToolSearch(c, narrow, catalog.ToolSearchOptions{Enabled: true, Activations: set})
	require.NoError(t, err)
	tools, err = third.Tools(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"read_doc"}, toolNames(tools))

	// Without a shared set, activation stays per run.
	perRun, err := catalog.NewToolSearch(c, policy, catalog.ToolSearchOptions{Enabled: true})
	require.NoError(t, err)
	tools, err = perRun.Tools(t.Context())
	require.NoError(t, err)
	assert.NotContains(t, toolNames(tools), "exa_web_search_exa")
}

func TestToolSearchDeniesInactiveDeferredTool(t *testing.T) {
	t.Parallel()

	search, err := catalog.NewToolSearch(exaCatalog(t), catalog.AllowAll("test", catalog.RiskPrivileged),
		catalog.ToolSearchOptions{Enabled: true, Activations: catalog.NewActivationSet(0)})
	require.NoError(t, err)

	decision := search.BeforeTool(t.Context(), agent.ToolCallInfo{ToolCall: agent.ToolCall{Name: "exa_web_search_exa"}})
	assert.Equal(t, agent.ToolDecisionDeny, decision.Action)
	assert.Contains(t, decision.Reason, `tool "exa_web_search_exa" is not active`)
	assert.Contains(t, decision.Reason, catalog.DefaultToolSearchName)

	// Direct and unknown tools are someone else's decision.
	for _, name := range []string{"read_doc", "nope"} {
		assert.Equal(t, agent.ToolDecision{}, search.BeforeTool(t.Context(), agent.ToolCallInfo{ToolCall: agent.ToolCall{Name: name}}))
	}
}

func TestToolSearchSourceSelectors(t *testing.T) {
	t.Parallel()

	policy := catalog.AllowAll("test", catalog.RiskPrivileged)

	t.Run("selector activates the whole source", func(t *testing.T) {
		t.Parallel()

		set := catalog.NewActivationSet(0)
		search, err := catalog.NewToolSearch(exaCatalog(t), policy, catalog.ToolSearchOptions{Enabled: true, Activations: set})
		require.NoError(t, err)
		result := runToolSearch(t, search, `{"query":"","tools":["mcp/exa"]}`)
		assert.Contains(t, result, `"activated":["exa_web_search_exa","exa_crawling_exa"]`)
		assert.Equal(t, []string{"exa_web_search_exa", "exa_crawling_exa"}, set.Names())
	})

	t.Run("exact name need not match the query", func(t *testing.T) {
		t.Parallel()

		search, err := catalog.NewToolSearch(exaCatalog(t), policy, catalog.ToolSearchOptions{Enabled: true})
		require.NoError(t, err)
		result := runToolSearch(t, search, `{"query":"slack","tools":["exa_crawling_exa"]}`)
		assert.Contains(t, result, `"activated":["exa_crawling_exa"]`)
	})

	t.Run("unknown selector lists sources", func(t *testing.T) {
		t.Parallel()

		search, err := catalog.NewToolSearch(exaCatalog(t), policy, catalog.ToolSearchOptions{Enabled: true})
		require.NoError(t, err)
		text, isError := runToolSearchResult(t, search, `{"tools":["mcp/nope"]}`)
		assert.True(t, isError)
		assert.Contains(t, text, `unknown tool source "mcp/nope"`)
		assert.Contains(t, text, "mcp/exa (2), mcp/slack (1)")

		text, isError = runToolSearchResult(t, search, `{"tools":["read_doc"]}`)
		assert.True(t, isError)
		assert.Contains(t, text, "not a searchable deferred tool")
		assert.Contains(t, text, "exa_web_search_exa")
	})
}

// runToolSearchResult is runToolSearch without the success requirement.
func runToolSearchResult(t *testing.T, search *catalog.ToolSearch, args string) (string, bool) {
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

			text, _ := part.Content[0].(ai.TextPart)

			return text.Text, part.IsError
		}
	}

	t.Fatal("tool result not found")

	return "", false
}
