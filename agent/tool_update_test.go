package agent_test

import (
	"context"
	"testing"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareTurnReplacesRunScopedToolsForDeferredSearch(t *testing.T) {
	t.Parallel()

	read := agent.NewTool("read_status", "Read status", func(context.Context, struct{}) (string, error) {
		return "ready", nil
	})
	lookup := agent.NewTool("lookup_docs", "Look up documentation", func(context.Context, struct{}) (string, error) {
		return "documentation", nil
	})
	catalogue, err := catalog.New(
		catalog.Local("app", catalog.RiskRead, read)[0],
		catalog.MCP("docs", catalog.RiskRead, lookup)[0],
	)
	require.NoError(t, err)
	search, err := catalog.NewToolSearch(catalogue, catalog.AllowAll("tenant", catalog.RiskRead), catalog.ToolSearchOptions{Enabled: true})
	require.NoError(t, err)
	opts, err := search.AgentOptions(t.Context())
	require.NoError(t, err)

	model := newScriptedModel(
		respond(callResponse(call("search", "tool_search", `{"query":"documentation"}`))),
		respond(callResponse(call("lookup", "lookup_docs", `{}`))),
		respond(textResponse("done")),
	)
	a, err := agent.New(model, opts...)
	require.NoError(t, err)

	_, err = a.Run(t.Context(), agent.NewSession(), ai.UserText("find docs"))
	require.NoError(t, err)

	requests := model.Requests()
	require.Len(t, requests, 3)
	assert.ElementsMatch(t, []string{"read_status", "tool_search"}, declaredToolNames(requests[0].Tools))
	assert.ElementsMatch(t, []string{"read_status", "tool_search", "lookup_docs"}, declaredToolNames(requests[1].Tools))
	assert.ElementsMatch(t, []string{"read_status", "tool_search", "lookup_docs"}, declaredToolNames(requests[2].Tools))
}

func TestToolSearchCanBeDisabledToExposeAuthorizedTools(t *testing.T) {
	t.Parallel()

	local := agent.NewTool("read_status", "Read status", func(context.Context, struct{}) (string, error) { return "", nil })
	remote := agent.NewTool("lookup_docs", "Look up documentation", func(context.Context, struct{}) (string, error) { return "", nil })
	catalogue, err := catalog.New(
		catalog.Local("app", catalog.RiskRead, local)[0],
		catalog.MCP("docs", catalog.RiskRead, remote)[0],
	)
	require.NoError(t, err)

	search, err := catalog.NewToolSearch(catalogue, catalog.AllowAll("tenant", catalog.RiskRead), catalog.ToolSearchOptions{})
	require.NoError(t, err)
	tools, err := search.Tools(t.Context())
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"read_status", "lookup_docs"}, declaredToolNamesFromTools(tools))
}

func declaredToolNames(tools []ai.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}

	return names
}

func declaredToolNamesFromTools(tools []agent.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Decl().Name)
	}

	return names
}
