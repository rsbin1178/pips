//nolint:wsl_v5 // Route action and rendering assertions stay grouped.
package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/internal/coding"
	codingmcp "github.com/rsbin1178/pips/internal/coding/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testMCPSnapshot(settled bool) coding.MCPSnapshot {
	started := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	servers := []codingmcp.ServerStatus{
		{
			ID: "docs", Scope: codingmcp.ScopeUser, Transport: codingmcp.TransportStreamableHTTP,
			Visibility: codingmcp.VisibilityAmbient, State: codingmcp.ServerStateConnected,
			Tools:     []string{"docs_lookup", "docs_search", "docs_summarize", "docs_translate"},
			StartedAt: started, SettledAt: started.Add(1200 * time.Millisecond),
		},
		{
			ID: "slow", Scope: codingmcp.ScopeUser, Transport: codingmcp.TransportStdio,
			Visibility: codingmcp.VisibilityAgentPrivate, State: codingmcp.ServerStateConnecting,
			StartedAt: started,
		},
		{
			ID: "broken", Scope: codingmcp.ScopeProject, Transport: codingmcp.TransportStreamableHTTP,
			Visibility: codingmcp.VisibilityAmbient, State: codingmcp.ServerStateFailed,
			Stage: "connect", Code: "connect_failed",
			Message:   "server connection could not be initialized",
			StartedAt: started, SettledAt: started.Add(5 * time.Second),
		},
		{
			ID: "triage", Scope: codingmcp.ScopeProject, Transport: codingmcp.TransportStdio,
			Visibility: codingmcp.VisibilityAmbient, State: codingmcp.ServerStatePending,
		},
		{
			ID: "denied", Scope: codingmcp.ScopeProject, Transport: codingmcp.TransportStdio,
			Visibility: codingmcp.VisibilityAmbient, State: codingmcp.ServerStateDisabled,
		},
	}
	if settled {
		servers[1].State = codingmcp.ServerStateConnected
		servers[1].Tools = []string{"slow_search"}
		servers[1].SettledAt = started.Add(3 * time.Second)
	}

	return coding.MCPSnapshot{GenerationID: 1, Settled: settled, Servers: servers}
}

func TestMCPCommandOpensReadOnlyStatusRoute(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.mcpSnapshot = testMCPSnapshot(true)
	model := readyModelWithController(t, controller, true)
	model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	model.Update(key("/"))
	for _, character := range "mcp" {
		model.Update(tea.KeyPressMsg{Text: string(character)})
	}
	require.Equal(t, pickerCommand, model.picker.kind)
	_, load := model.Update(key("enter"))
	require.NotNil(t, load)
	driveModelCommands(t, model, load)

	assert.Equal(t, routeMCP, model.route.kind)
	assert.False(t, model.route.loading)
	content := ansi.Strip(model.View().Content)
	assert.Contains(t, content, "MCP Servers")
	assert.Contains(t, content, "2 connected · 1 failed · 1 pending · 1 disabled")
	assert.Contains(t, content, "› ● docs · connected · 4 tools · user/streamable_http")
	assert.Contains(t, content, "user · streamable_http · ambient")
	assert.Contains(t, content, "connected in 1.2s")
	assert.Contains(t, content, "tools: docs_lookup, docs_search, docs_summarize (+1 more · Ctrl+D)")
	assert.Contains(t, content, "● slow · connected · 1 tool · user/stdio")
	assert.Contains(t, content, "✗ broken · failed · project/streamable_http")
	assert.Contains(t, content, "○ triage · pending · project/stdio")
	assert.Contains(t, content, "○ denied · disabled · project/stdio")
	assert.NotContains(t, content, "docs_translate")
	assert.NotContains(t, content, "connecting…")
	assert.Contains(t, content, "Ctrl+D details")

	model.Update(tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl})
	content = ansi.Strip(model.View().Content)
	assert.Contains(t, content, "Details · docs")
	assert.Contains(t, content, "docs_translate")

	model.Update(key("down"))
	model.Update(key("down"))
	content = ansi.Strip(model.View().Content)
	assert.Contains(t, content, "› ✗ broken · failed · project/streamable_http")
	assert.Contains(t, content, "project · streamable_http · ambient")
	assert.Contains(t, content, "failed at connect: connect_failed — server connection could not be initialized")
	assert.Contains(t, content, "Details · broken")
	assert.Contains(t, content, "stage: connect")
	assert.NotContains(t, content, "connected in 1.2s")

	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, routeNone, model.route.kind)
	assert.Empty(t, model.composer.Value())
	assert.Equal(t, 1, controller.mcpCalls)
	require.NotNil(t, model.View().Cursor)
}

func TestMCPRouteRestoresDraftOnEscape(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.mcpSnapshot = testMCPSnapshot(true)
	model := readyModelWithController(t, controller, true)
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model.composer.SetValue("keep this draft")
	load := model.openMCPRoute()
	driveModelCommands(t, model, load)
	require.Equal(t, routeMCP, model.route.kind)
	assert.Empty(t, model.composer.Value())

	model.Update(tea.KeyPressMsg{Text: "broken"})
	assert.Equal(t, "broken", model.route.search.Value())
	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, routeNone, model.route.kind)
	assert.Equal(t, "keep this draft", model.composer.Value())
	require.NotNil(t, model.View().Cursor)
}

func TestMCPRouteFiltersAndStaysAvailableWhileBusy(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Phase = coding.PhaseRunning
	controller := newOverlayController(state)
	controller.mcpSnapshot = testMCPSnapshot(true)
	model := readyModelWithController(t, controller, true)
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	require.NotEqual(t, contextIdle, model.actionContext())

	model.openCommandPicker()
	require.Equal(t, pickerCommand, model.picker.kind)
	for _, character := range "mcp" {
		model.Update(tea.KeyPressMsg{Text: string(character)})
	}
	_, load := model.Update(key("enter"))
	require.NoError(t, model.picker.err, "/mcp must not be idle-only")
	require.NotNil(t, load)
	driveModelCommands(t, model, load)
	require.Equal(t, routeMCP, model.route.kind)

	model.Update(tea.KeyPressMsg{Text: "failed"})
	assert.Equal(t, "failed", model.route.search.Value())
	values := model.filteredMCPRouteValues()
	require.Len(t, values, 1)
	assert.Equal(t, "broken", values[0].ID)
	content := ansi.Strip(model.View().Content)
	assert.Contains(t, content, "broken")
	assert.NotContains(t, content, "docs ·")

	model.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	for range 5 {
		model.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	model.Update(tea.KeyPressMsg{Text: "slow_search"})
	values = model.filteredMCPRouteValues()
	require.Len(t, values, 1)
	assert.Equal(t, "slow", values[0].ID)

	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, routeNone, model.route.kind)
}

func TestMCPRoutePollsWhileConnectingAndStopsWhenSettled(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.mcpSnapshot = testMCPSnapshot(false)
	model := readyModelWithController(t, controller, true)
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	load := model.openMCPRoute()
	require.NotNil(t, load)

	_, poll := model.Update(commandMessage(t, load))
	require.NotNil(t, poll, "an unsettled snapshot schedules a poll")
	content := ansi.Strip(model.View().Content)
	assert.Contains(t, content, "1 connected · 1 connecting · 1 failed · 1 pending · 1 disabled")
	assert.Contains(t, content, "◌ slow · connecting · user/stdio")
	assert.Contains(t, content, "connecting…")
	assert.Equal(t, 1, controller.mcpCalls)

	model.Update(key("down"))
	content = ansi.Strip(model.View().Content)
	assert.Contains(t, content, "› ◌ slow · connecting · user/stdio")
	assert.Contains(t, content, "connecting for")

	generation := model.route.generation
	_, reload := model.Update(mcpRoutePollMsg{generation: generation - 1})
	assert.Nil(t, reload, "a stale poll never reloads")
	_, reload = model.Update(mcpRoutePollMsg{generation: generation})
	require.NotNil(t, reload)
	assert.True(t, model.route.refreshing)

	controller.mcpSnapshot = testMCPSnapshot(true)
	_, next := model.Update(commandMessage(t, reload))
	assert.Nil(t, next, "a settled snapshot schedules no further poll")
	assert.False(t, model.route.refreshing)
	assert.Equal(t, 2, controller.mcpCalls)
	assert.Equal(t, 1, model.route.cursor, "cursor survives a background refresh")
	content = ansi.Strip(model.View().Content)
	assert.Contains(t, content, "2 connected · 1 failed · 1 pending · 1 disabled")
	assert.Contains(t, content, "› ● slow · connected · 1 tool · user/stdio")
	assert.NotContains(t, content, "connecting…")

	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Equal(t, routeNone, model.route.kind)
	_, reload = model.Update(mcpRoutePollMsg{generation: generation})
	assert.Nil(t, reload, "a poll after the route closed is a no-op")
	assert.Equal(t, 2, controller.mcpCalls)
}

func TestMCPRouteIgnoresStaleLoadReportsErrorsAndFitsNarrowTerminal(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	model.route = routeState{
		kind: routeMCP, generation: 2, loading: true,
		search: newRouteSearch(themeDark, true),
	}
	model.setLayout()
	content := ansi.Strip(model.View().Content)
	assert.Contains(t, content, "Loading MCP servers")
	assert.NotContains(t, content, "No MCP servers are configured.")

	model.Update(mcpRouteDataMsg{generation: 1, snapshot: testMCPSnapshot(true)})
	assert.True(t, model.route.loading)
	assert.Empty(t, model.route.mcp.Servers)

	model.Update(mcpRouteDataMsg{generation: 2, err: assert.AnError})
	assert.False(t, model.route.loading)
	content = ansi.Strip(model.View().Content)
	assert.Contains(t, content, "Error:")
	assert.NotContains(t, content, "No MCP servers are configured.")

	model.Update(mcpRouteDataMsg{generation: 2, snapshot: testMCPSnapshot(true)})
	require.NoError(t, model.route.err)
	content = ansi.Strip(model.View().Content)
	assert.Contains(t, content, "docs")
	assert.Contains(t, content, "Ctrl+D details")
	lines := strings.Split(content, "\n")
	assert.LessOrEqual(t, len(lines), model.height)
	for _, line := range lines {
		assert.LessOrEqual(t, ansi.StringWidth(line), model.width)
	}
	require.NotNil(t, model.View().Cursor)
}

func TestMCPRouteDetailPaneCoversEveryState(t *testing.T) {
	t.Parallel()

	snapshot := testMCPSnapshot(false)
	controller := newOverlayController(readyState())
	controller.mcpSnapshot = snapshot
	model := readyModelWithController(t, controller, false)
	model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	load := model.openMCPRoute()
	model.Update(commandMessage(t, load))
	model.route.showDetails = true

	expected := map[string][]string{
		"docs":   {"Details · docs", "docs_lookup", "docs_translate"},
		"slow":   {"Details · slow", "connecting for"},
		"broken": {"Details · broken", "stage: connect", "code: connect_failed", "server connection could not be initialized"},
		"triage": {"Details · triage", "awaiting project approval"},
		"denied": {"Details · denied", "denied by project permissions"},
	}
	for index, server := range snapshot.Servers {
		model.route.cursor = index
		content := ansi.Strip(model.View().Content)
		for _, fragment := range expected[server.ID] {
			assert.Contains(t, content, fragment, server.ID)
		}
		assert.NotContains(t, content, "example.test")
	}
	assert.Equal(t, "No MCP servers are configured.", mcpRouteSummary(coding.MCPSnapshot{}))
	assert.Equal(t, "0ms", mcpDuration(-time.Second))
	assert.Equal(t, "750ms", mcpDuration(750*time.Millisecond))
	assert.Equal(t, "2.5s", mcpDuration(2490*time.Millisecond))
}
