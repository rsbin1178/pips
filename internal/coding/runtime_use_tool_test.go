//nolint:wsl_v5 // The echo fixture keeps its handler, record, and accessor adjacent.
package coding

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// echoMCPServer is a Streamable HTTP MCP server whose one tool answers with the
// arguments it received, so a dispatch can be shown to hand the model's own
// object over and to return the target's own result.
type echoMCPServer struct {
	http *httptest.Server

	mu       sync.Mutex
	received []string
}

func newEchoMCPServer(t *testing.T, toolName string) *echoMCPServer {
	t.Helper()

	server := sdk.NewServer(&sdk.Implementation{Name: "echo", Version: "v1"}, nil)
	echo := &echoMCPServer{}
	server.AddTool(&sdk.Tool{
		Name: toolName, InputSchema: json.RawMessage(`{"type":"object","additionalProperties":true}`),
	}, func(_ context.Context, request *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		arguments := string(request.Params.Arguments)
		echo.record(arguments)

		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "echo " + arguments}}}, nil
	})
	handler := sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{JSONResponse: true},
	)
	echo.http = httptest.NewServer(handler)
	t.Cleanup(echo.http.Close)

	return echo
}

func (s *echoMCPServer) record(arguments string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.received = append(s.received, arguments)
}

func (s *echoMCPServer) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.received)
}

// openUseToolRuntime opens a Runtime whose search surface is on and whose one MCP
// tool is deferred, so that target stays hidden until the model activates it. It
// returns the workspace root beside the Runtime for the file assertions.
func openUseToolRuntime(t *testing.T, model *runtimeModel, server *echoMCPServer) (*Runtime, string) {
	t.Helper()

	base := t.TempDir()
	writeTestMCPDefinitions(t, base, fmt.Sprintf(
		`{"id":"docs","type":"streamable_http","url":%q,"connect_timeout":"30s"}`, server.http.URL,
	))

	runtime := openTestRuntimeConfiguredWithSandboxAndConfig(
		t, base, SessionTarget{}, model, nil, nil, nil, false,
		config.SandboxWorkspaceWrite,
		func(cfg *config.Config) {
			cfg.ToolSearch = true
			cfg.Approval = config.ApprovalNever
		},
	)

	return runtime, filepath.Join(base, "workspace")
}

// TestRuntimeUseToolDispatchesToAToolTheSnapshotHides runs the whole path: the
// model calls the wrapper for a deferred tool the snapshot hides, and the
// wrapper's result is the target's own. The third call activates the same tool
// through the search surface and calls it directly, so the two results can be
// compared as the same call.
func TestRuntimeUseToolDispatchesToAToolTheSnapshotHides(t *testing.T) {
	t.Parallel()

	server := newEchoMCPServer(t, "lookup")
	model := newRuntimeModel(
		runtimeToolResponse("call-1", "use_tool",
			`{"tool_name":"docs_lookup","tool_input":{"query":"parity","limit":3}}`),
		runtimeToolResponse("call-2", catalog.DefaultToolSearchName, `{"tools":["docs_lookup"]}`),
		runtimeToolResponse("call-3", "docs_lookup", `{"query":"parity","limit":3}`),
		runtimeTextResponse("done"),
	)
	runtime, _ := openUseToolRuntime(t, model, server)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), testUserMessage("look the docs up")))
	results := toolResultsByCallID(events)

	require.Contains(t, results, "call-1")
	require.Contains(t, results, "call-3")
	assert.False(t, results["call-1"].IsError, toolResultTextOf(t, results["call-1"]))
	assert.Contains(t, toolResultTextOf(t, results["call-1"]), `"query":"parity"`)
	// The wrapper returns the target's own result: the same text the same tool
	// produced when it was called directly with the same arguments.
	assert.Equal(t, toolResultTextOf(t, results["call-3"]), toolResultTextOf(t, results["call-1"]))

	// The target received the model's own object, unchanged, on both calls.
	calls := server.calls()
	require.Len(t, calls, 2)
	for _, call := range calls {
		assert.JSONEq(t, `{"query":"parity","limit":3}`, call)
	}

	requests := model.Requests()
	require.GreaterOrEqual(t, len(requests), 3)
	assert.Contains(t, toolNamesFromRequest(requests[0]), "use_tool")
	assert.NotContains(t, toolNamesFromRequest(requests[0]), "docs_lookup")
	assert.Contains(t, toolNamesFromRequest(requests[2]), "docs_lookup",
		"the activated target is direct from the next request on")
}

// TestRuntimeUseToolRefusesADirectlyVisibleToolWithoutRunningIt pins the rule
// that makes the wrapper safe: a controlled local tool is visible, so the
// correction refuses it before any dispatch, and its own write never lands.
func TestRuntimeUseToolRefusesADirectlyVisibleToolWithoutRunningIt(t *testing.T) {
	t.Parallel()

	server := newEchoMCPServer(t, "lookup")
	model := newRuntimeModel(
		runtimeToolResponse("call-1", "use_tool",
			`{"tool_name":"Edit","tool_input":{"file_path":"use-tool-proof.txt","old_string":"","new_string":"proof\n"}}`),
		runtimeTextResponse("done"),
	)
	runtime, workspacePath := openUseToolRuntime(t, model, server)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), testUserMessage("write the file")))
	results := toolResultsByCallID(events)

	require.Contains(t, results, "call-1")
	assert.True(t, results["call-1"].IsError)
	assert.Equal(t,
		`tool "Edit" is directly visible in this session; `+
			"call it as its own tool call instead of routing it through use_tool",
		toolResultTextOf(t, results["call-1"]))
	// The controlled tool never ran: its own write would have landed.
	_, err := os.Stat(filepath.Join(workspacePath, "use-tool-proof.txt"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// TestRuntimeUseToolRefusesAnUnknownNameByName keeps a guessed name legible: the
// refusal names the tool instead of dispatching or reporting a generic failure.
func TestRuntimeUseToolRefusesAnUnknownNameByName(t *testing.T) {
	t.Parallel()

	server := newEchoMCPServer(t, "lookup")
	model := newRuntimeModel(
		runtimeToolResponse("call-1", "use_tool", `{"tool_name":"docs_missing","tool_input":{}}`),
		runtimeTextResponse("done"),
	)
	runtime, _ := openUseToolRuntime(t, model, server)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), testUserMessage("guess a tool")))
	results := toolResultsByCallID(events)

	require.Contains(t, results, "call-1")
	assert.True(t, results["call-1"].IsError)
	assert.Equal(t, `catalog: unknown tool "docs_missing"`, toolResultTextOf(t, results["call-1"]))
	assert.Empty(t, server.calls())
}

// TestRuntimeOffersNoUseToolWithoutTheSearchSurface keeps the registration rule:
// a Runtime whose search surface is off offers no dispatch Tool at all.
func TestRuntimeOffersNoUseToolWithoutTheSearchSurface(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(runtimeTextResponse("done"))
	runtime := openTestRuntimeConfiguredWithSandboxAndConfig(
		t, t.TempDir(), SessionTarget{}, model, nil, nil, nil, false,
		config.SandboxWorkspaceWrite, nil,
	)

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), testUserMessage("hello")))

	requests := model.Requests()
	require.Len(t, requests, 1)
	assert.NotContains(t, toolNamesFromRequest(requests[0]), "use_tool")
	assert.NotContains(t, toolNamesFromRequest(requests[0]), catalog.DefaultToolSearchName)
}

// TestRuntimeUseToolIsAnErrorResultTheModelCanRead pins the shape the model
// receives for a refusal: an error result whose text is the sentence, not a
// success carrying it.
func TestRuntimeUseToolIsAnErrorResultTheModelCanRead(t *testing.T) {
	t.Parallel()

	server := newEchoMCPServer(t, "lookup")
	model := newRuntimeModel(
		runtimeToolResponse("call-1", "use_tool", `{"tool_name":"shell","tool_input":{"command":"echo hi"}}`),
		runtimeTextResponse("done"),
	)
	runtime, _ := openUseToolRuntime(t, model, server)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), testUserMessage("run it through the wrapper")))
	results := toolResultsByCallID(events)

	require.Contains(t, results, "call-1")
	assert.True(t, results["call-1"].IsError)
	assert.Contains(t, toolResultTextOf(t, results["call-1"]), `tool "shell" is directly visible`)
}

// TestRuntimeCustomChildDispatchesThroughTheSearchSurface keeps a child's own
// search surface complete: a child whose definition enables it offers the
// dispatch Tool and reaches a deferred tool through the child's own catalog and
// policy, not the parent's.
func TestRuntimeCustomChildDispatchesThroughTheSearchSurface(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	layout, err := paths.New(filepath.Join(base, "home"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.AgentsDir(), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(layout.AgentsDir(), "docs-reader.md"),
		[]byte(`---
schema: pips.agent/v1alpha1
name: Docs reader
description: Look one fact up through the deferred documentation tool.
model: openai/runtime-test
tools:
  allow: ["source:mcp/docs"]
  tool_search: true
---
Use the search surface to reach the documentation tool.
`),
		0o600,
	))

	server := newEchoMCPServer(t, "lookup")
	writeTestMCPDefinitions(t, base, fmt.Sprintf(
		`{"id":"docs","type":"streamable_http","url":%q,"connect_timeout":"30s"}`, server.http.URL,
	))

	model := newRuntimeModel(
		runtimeToolResponse("parent-delegate", subagent.ToolName,
			`{"prompt":"Look the fact up.","description":"delegate","subagent_type":"docs-reader","run_in_background":false}`),
		runtimeToolResponse("child-dispatch", "use_tool", `{"tool_name":"docs_lookup","tool_input":{"query":"parity"}}`),
		runtimeTextResponse("looked up"),
		runtimeTextResponse("delegation complete"),
	)
	runtime := openTestRuntimeConfiguredWithSandboxAndConfig(
		t, base, SessionTarget{}, model, nil, nil, nil, false,
		config.SandboxWorkspaceWrite,
		func(cfg *config.Config) {
			cfg.ToolSearch = true
			cfg.Approval = config.ApprovalNever
		},
	)

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), testUserMessage("delegate the lookup")))

	// The child reached the hidden target through the wrapper, with the model's
	// own argument object.
	calls := server.calls()
	require.Len(t, calls, 1)
	assert.JSONEq(t, `{"query":"parity"}`, calls[0])

	// The child's own request offers the dispatch Tool and keeps the target hidden.
	requests := model.Requests()
	require.GreaterOrEqual(t, len(requests), 2)
	assert.Contains(t, toolNamesFromRequest(requests[1]), "use_tool")
	assert.NotContains(t, toolNamesFromRequest(requests[1]), "docs_lookup")

	summaries, err := runtime.ListSubagents(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 1)

	// The dispatch call succeeded inside the child, so it was never denied by the
	// child's own lease.
	assert.Equal(t, subagent.StateSucceeded, summaries[0].State)
	assert.Equal(t, 1, summaries[0].ToolCalls)

	// The child's call came back with the target's own result, not a summary.
	detail, err := runtime.InspectSubagent(t.Context(), summaries[0].ChildSessionID)
	require.NoError(t, err)

	results := make([]string, 0, 1)
	for _, message := range detail.Transcript {
		toolMessage, ok := message.(ai.ToolMessage)
		if !ok {
			continue
		}
		for _, part := range toolMessage.Parts {
			results = append(results, toolResultTextOf(t, part))
		}
	}
	assert.Equal(t, []string{`echo {"query":"parity"}`}, results)
}

// TestRuntimeUseToolRefusesAToolTheChildAlreadyActivated covers the per-run half
// of the correction. A child keeps run-scoped activation and passes no shared
// ActivationSet, so a tool the child activated this run is directly visible in
// its own snapshot even though the surface's own Tools omits it; the wrapper
// must refuse it there too, or it becomes a second way to call a visible tool.
func TestRuntimeUseToolRefusesAToolTheChildAlreadyActivated(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	layout, err := paths.New(filepath.Join(base, "home"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(layout.AgentsDir(), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(layout.AgentsDir(), "docs-reader.md"),
		[]byte(`---
schema: pips.agent/v1alpha1
name: Docs reader
description: Look one fact up through the deferred documentation tool.
model: openai/runtime-test
tools:
  allow: ["source:mcp/docs"]
  tool_search: true
---
Use the search surface to reach the documentation tool.
`),
		0o600,
	))

	server := newEchoMCPServer(t, "lookup")
	writeTestMCPDefinitions(t, base, fmt.Sprintf(
		`{"id":"docs","type":"streamable_http","url":%q,"connect_timeout":"30s"}`, server.http.URL,
	))

	model := newRuntimeModel(
		runtimeToolResponse("parent-delegate", subagent.ToolName,
			`{"prompt":"Look the fact up.","description":"delegate","subagent_type":"docs-reader","run_in_background":false}`),
		runtimeToolResponse("child-activate", catalog.DefaultToolSearchName, `{"tools":["docs_lookup"]}`),
		runtimeToolResponse("child-dispatch", "use_tool", `{"tool_name":"docs_lookup","tool_input":{"query":"parity"}}`),
		runtimeTextResponse("looked up"),
		runtimeTextResponse("delegation complete"),
	)
	runtime := openTestRuntimeConfiguredWithSandboxAndConfig(
		t, base, SessionTarget{}, model, nil, nil, nil, false,
		config.SandboxWorkspaceWrite,
		func(cfg *config.Config) {
			cfg.ToolSearch = true
			cfg.Approval = config.ApprovalNever
		},
	)

	collectRuntimeEvents(t, runtime.Prompt(t.Context(), testUserMessage("delegate the lookup")))

	// The child activated the target this run, so its own snapshot shows it
	// directly on the request that routed the call through the wrapper.
	requests := model.Requests()
	require.GreaterOrEqual(t, len(requests), 3)
	assert.Contains(t, toolNamesFromRequest(requests[2]), "docs_lookup")

	// The wrapper refused it, so the target never ran.
	assert.Empty(t, server.calls())

	summaries, err := runtime.ListSubagents(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 1)

	detail, err := runtime.InspectSubagent(t.Context(), summaries[0].ChildSessionID)
	require.NoError(t, err)

	results := make([]string, 0, 2)
	for _, message := range detail.Transcript {
		toolMessage, ok := message.(ai.ToolMessage)
		if !ok {
			continue
		}
		for _, part := range toolMessage.Parts {
			results = append(results, toolResultTextOf(t, part))
		}
	}
	require.Len(t, results, 2)
	assert.Equal(t,
		`tool "docs_lookup" is directly visible in this session; `+
			"call it as its own tool call instead of routing it through use_tool",
		results[1])
}

// TestRuntimeUseToolRefusesEveryControlledTool pins the permit rule end to end:
// every approval-controlled handler and the stop Tool are local tools, so each
// is directly visible and the correction refuses it before its Exec could run
// without the permit its BeforeTool gate issues.
func TestRuntimeUseToolRefusesEveryControlledTool(t *testing.T) {
	t.Parallel()

	server := newEchoMCPServer(t, "lookup")
	model := newRuntimeModel(
		runtimeToolResponse("call-1", "use_tool", `{"tool_name":"shell","tool_input":{"command":"echo hi"}}`),
		runtimeToolResponse("call-2", "use_tool", `{"tool_name":"monitor","tool_input":{"command":"echo hi"}}`),
		runtimeToolResponse("call-3", "use_tool", `{"tool_name":"kill_background_task","tool_input":{"task_id":"bg-1"}}`),
		runtimeTextResponse("done"),
	)
	runtime, _ := openUseToolRuntime(t, model, server)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), testUserMessage("route them all through the wrapper")))
	results := toolResultsByCallID(events)

	for _, test := range []struct {
		id   string
		name string
	}{
		{id: "call-1", name: "shell"},
		{id: "call-2", name: "monitor"},
		{id: "call-3", name: "kill_background_task"},
	} {
		require.Contains(t, results, test.id)
		assert.True(t, results[test.id].IsError)
		assert.Equal(t, fmt.Sprintf(
			`tool %q is directly visible in this session; call it as its own tool call instead of routing it through use_tool`,
			test.name,
		), toolResultTextOf(t, results[test.id]))
	}

	// No controlled handler ran: the MCP server is the only dispatchable target
	// and it was never called.
	assert.Empty(t, server.calls())
}
