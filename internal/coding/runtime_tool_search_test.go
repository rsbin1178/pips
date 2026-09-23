package coding

import (
	"encoding/json"
	"testing"

	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRuntimeToolSearchAnswersBlindProbeWithoutDeferredTools replays the
// failure mode from session s-2c8d952c…: a proxy rewrote the discovery tool
// name, the model called the rewritten name, and then probed a registry that
// had nothing deferred. With ranking hints and did-you-mean, the first call
// is answered with the correct name and the second with the empty-registry
// hint, so the model does not keep retrying.
func TestRuntimeToolSearchAnswersBlindProbeWithoutDeferredTools(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeToolResponse("probe-1", "search_tools", `{"query":"web search tool"}`),
		runtimeToolResponse("probe-2", catalog.DefaultToolSearchName, `{"query":"web search tool"}`),
		runtimeTextResponse("no deferred tools are registered"),
	)
	runtime := openTestRuntimeConfiguredWithSandboxAndConfig(
		t, t.TempDir(), SessionTarget{}, model, nil, nil, nil, false,
		config.SandboxWorkspaceWrite,
		func(cfg *config.Config) { cfg.ToolSearch = true },
	)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), testUserMessage("find a web search tool")))
	assert.Contains(t, eventTypes(events), EventInteractionCompleted)

	requests := model.Requests()
	require.Len(t, requests, 3)

	for _, request := range requests {
		assert.NotContains(t, toolNamesFromRequest(request), catalog.DefaultToolSearchName)
		assert.NotContains(t, requestSystemText(request), "discover deferred tools")
	}

	results := toolResultsByCallID(events)
	first, ok := results["probe-1"]
	require.True(t, ok)
	assert.True(t, first.IsError)
	assert.Equal(t, `unknown tool "search_tools"; did you mean "pips_tool_search"?`, toolResultTextOf(t, first))

	second, ok := results["probe-2"]
	require.True(t, ok)
	assert.False(t, second.IsError, toolResultTextOf(t, second))

	var decoded struct {
		Matches   []json.RawMessage       `json:"matches"`
		Activated []string                `json:"activated"`
		Sources   []catalog.SourceSummary `json:"sources"`
		Hint      string                  `json:"hint"`
	}
	require.NoError(t, json.Unmarshal([]byte(toolResultTextOf(t, second)), &decoded))
	assert.Empty(t, decoded.Matches)
	assert.Empty(t, decoded.Activated)
	assert.Empty(t, decoded.Sources)
	assert.Equal(t, "No MCP or Extension tools are registered in this session; nothing can be discovered.", decoded.Hint)
}

func TestRuntimeToolSearchUsesConfiguredProviderName(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(
		runtimeToolResponse("probe-1", "tool_search", `{"query":"anything"}`),
		runtimeToolResponse("probe-2", "proxy_tool_search", `{"query":"anything"}`),
		runtimeTextResponse("done"),
	)
	runtime := openTestRuntimeConfiguredWithSandboxAndConfig(
		t, t.TempDir(), SessionTarget{}, model, nil, nil, nil, false,
		config.SandboxWorkspaceWrite,
		func(cfg *config.Config) {
			cfg.ToolSearch = true
			cfg.ToolSearchName = "global_tool_search"
			cfg.Providers[model.Provider()] = config.ProviderConfig{ToolSearchName: "proxy_tool_search"}
		},
	)

	events := collectRuntimeEvents(t, runtime.Prompt(t.Context(), testUserMessage("probe")))
	results := toolResultsByCallID(events)

	// Only the provider override is a known name; the global value is not.
	first, ok := results["probe-1"]
	require.True(t, ok)
	assert.True(t, first.IsError)
	assert.Equal(t, `unknown tool "tool_search"; did you mean "proxy_tool_search"?`, toolResultTextOf(t, first))

	second, ok := results["probe-2"]
	require.True(t, ok)
	assert.False(t, second.IsError, toolResultTextOf(t, second))
	assert.Contains(t, toolResultTextOf(t, second), "nothing can be discovered")
}

func TestRuntimeRejectsToolSearchNameCollidingWithBuiltIn(t *testing.T) {
	t.Parallel()

	model := newRuntimeModel(runtimeTextResponse("unreachable"))
	runtime := openTestRuntimeConfiguredWithSandboxAndConfig(
		t, t.TempDir(), SessionTarget{}, model, nil, nil, nil, false,
		config.SandboxWorkspaceWrite,
		func(cfg *config.Config) {
			cfg.ToolSearch = true
			cfg.ToolSearchName = "read"
		},
	)

	_, err := collectRuntimeEventsAndError(t, runtime.Prompt(t.Context(), testUserMessage("probe")))
	require.ErrorIs(t, err, ErrRuntimeInvalid)
	require.ErrorContains(t, err, `"read" is reserved`)
	assert.Empty(t, model.Requests())
}

func toolResultsByCallID(events []Event) map[string]ai.ToolResultPart {
	results := make(map[string]ai.ToolResultPart)

	for _, payload := range payloadsOfType[ToolCompleted](events, EventToolCompleted) {
		for _, part := range payload.Result.Parts {
			results[part.ToolCallID] = part
		}
	}

	return results
}

func toolResultTextOf(t *testing.T, part ai.ToolResultPart) string {
	t.Helper()

	require.Len(t, part.Content, 1)
	text, ok := part.Content[0].(ai.TextPart)
	require.True(t, ok)

	return text.Text
}
