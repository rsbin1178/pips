package tui

import (
	"context"
	"iter"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentLibraryStartsSelectedProfileWithoutParentPrompt(t *testing.T) {
	t.Parallel()

	controller := &directAgentTestController{overlayController: newOverlayController(readyState())}
	controller.agentLibrary = coding.AgentLibrary{Entries: []coding.AgentLibraryEntry{{
		ID: "go-checker", Name: "Go checker", Kind: "custom", Scope: "user:pips",
		Source: "user:pips/go-checker.md", Status: "available", Available: true,
	}}}
	model := readyModelWithController(t, controller, true)
	_, supportsDirectRun := model.controller.(agentRunController)
	require.True(t, supportsDirectRun)
	driveModelCommands(t, model, model.openAgentsRoute())
	require.Equal(t, agentsTabRuns, model.route.agentsTab)
	require.Len(t, model.route.agentLibrary, 1)

	_, command := model.Update(tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
	require.Nil(t, command)
	require.Equal(t, routeAgents, model.route.kind)
	require.Equal(t, agentsTabAgents, model.route.agentsTab)
	require.Len(t, model.filteredAgentLibrary(), 1)
	_, command = model.Update(key("enter"))
	require.NoError(t, model.route.err)
	driveModelCommands(t, model, command)
	require.NotNil(t, model.directAgent)
	assert.True(t, model.composer.Focused())
	assert.Equal(t, "go-checker", model.directAgent.id)
	assert.Equal(t, routeNone, model.route.kind)
	assert.Contains(t, model.View().Content, "Selected Agent · Go checker")

	model.composer.SetValue("inspect the Go package")
	_, command = model.Update(key("enter"))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	require.Equal(t, []coding.AgentRunRequest{{
		AgentID: "go-checker", Task: "inspect the Go package",
	}}, controller.runs)
	assert.Empty(t, controller.prompts)
	assert.Nil(t, model.directAgent)
}

func TestAgentLibraryReportsDisabledTypeAsUnavailable(t *testing.T) {
	t.Parallel()

	controller := &directAgentTestController{overlayController: newOverlayController(readyState())}
	controller.agentLibrary = coding.AgentLibrary{Entries: []coding.AgentLibraryEntry{{
		ID: "explore", Name: "Explore", Kind: "builtin", Scope: "builtin",
		Source: "builtin:explore", Status: "available", Available: false,
		Unavailable: "disabled by configuration",
	}}}
	model := readyModelWithController(t, controller, true)
	driveModelCommands(t, model, model.openAgentsRoute())
	_, command := model.Update(tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
	require.Nil(t, command)
	require.Equal(t, agentsTabAgents, model.route.agentsTab)
	require.Len(t, model.filteredAgentLibrary(), 1)
	assert.Contains(t, model.View().Content, "disabled by configuration")

	// A type disabled by configuration cannot be started directly, and the
	// refusal carries the same reason the tab shows.
	_, _ = model.Update(key("enter"))
	require.Error(t, model.route.err)
	assert.Contains(t, model.route.err.Error(), "disabled by configuration")
	assert.Nil(t, model.directAgent)
	assert.Empty(t, controller.runs)
}

func TestSelectedDirectAgentClearsWithEscapeBeforeExecution(t *testing.T) {
	t.Parallel()

	model := readyModelWithController(t, newOverlayController(readyState()), true)
	model.directAgent = &directAgentSelection{id: "go-checker", name: "Go checker"}

	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})

	assert.Nil(t, model.directAgent)
}

type directAgentTestController struct {
	*overlayController
	runs []coding.AgentRunRequest
}

func (c *directAgentTestController) RunAgentEvents(
	_ context.Context,
	request coding.AgentRunRequest,
) iter.Seq2[coding.Event, error] {
	c.runs = append(c.runs, request)

	return func(func(coding.Event, error) bool) {}
}
