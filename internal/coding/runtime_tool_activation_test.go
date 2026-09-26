package coding

import (
	"fmt"
	"testing"

	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openExaToolSearchRuntime(t *testing.T, model *runtimeModel) *Runtime {
	t.Helper()

	server := newGatedMCPServer(t, "web_search_exa")
	server.release()
	base := t.TempDir()
	writeTestMCPDefinitions(t, base, fmt.Sprintf(
		`{"id":"exa","type":"streamable_http","url":%q,"connect_timeout":"30s"}`, server.http.URL,
	))

	return openTestRuntimeConfiguredWithSandboxAndConfig(
		t, base, SessionTarget{}, model, nil, nil, nil, false,
		config.SandboxWorkspaceWrite,
		func(cfg *config.Config) {
			cfg.ToolSearch = true
			cfg.Approval = config.ApprovalNever
		},
	)
}

// TestRuntimeToolSearchActivationPersistsAcrossInteractions replays session
// s-25e04fba…: a tool activated in one interaction must stay callable in the
// next without another discovery round-trip.
func TestRuntimeToolSearchActivationPersistsAcrossInteractions(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeToolResponse("search-1", catalog.DefaultToolSearchName, `{"query":"web search","tools":["mcp/exa"]}`),
		runtimeToolResponse("call-1", "exa_web_search_exa", `{}`),
		runtimeTextResponse("first answer"),
		runtimeToolResponse("call-2", "exa_web_search_exa", `{}`),
		runtimeTextResponse("second answer"),
	)
	runtime := openExaToolSearchRuntime(t, model)

	first := collectRuntimeEvents(t, runtime.Prompt(t.Context(), testUserMessage("search the web")))
	results := toolResultsByCallID(first)
	require.Contains(t, results, "call-1")
	assert.False(t, results["call-1"].IsError, toolResultTextOf(t, results["call-1"]))

	second := collectRuntimeEvents(t, runtime.Prompt(t.Context(), testUserMessage("search again")))
	results = toolResultsByCallID(second)
	require.Contains(t, results, "call-2")
	assert.False(t, results["call-2"].IsError, toolResultTextOf(t, results["call-2"]))

	requests := model.Requests()
	require.Len(t, requests, 5)
	assert.NotContains(t, toolNamesFromRequest(requests[0]), "exa_web_search_exa")
	assert.Contains(t, toolNamesFromRequest(requests[3]), "exa_web_search_exa",
		"the first request of the next interaction already declares the activated tool")
}

func TestRuntimeInactiveDeferredToolNamesDiscoveryTool(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeToolResponse("call-1", "exa_web_search_exa", `{}`),
		runtimeTextResponse("done"),
	)
	runtime := openExaToolSearchRuntime(t, model)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), testUserMessage("search the web")))
	results := toolResultsByCallID(events)
	require.Contains(t, results, "call-1")
	assert.True(t, results["call-1"].IsError)
	assert.Equal(t,
		`tool "exa_web_search_exa" is not active; call pips_tool_search with tools:["exa_web_search_exa"] first`,
		toolResultTextOf(t, results["call-1"]))
}
