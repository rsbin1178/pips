//nolint:wsl_v5 // Picker actions and their observable assertions stay adjacent.
package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommandPickerDisablesRuntimeReplacementUnlessIdle(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Phase = coding.PhaseRunning
	state.Interaction.Active = true
	controller := newOverlayController(state)
	model := readyModelWithController(t, controller, true)
	model.openCommandPicker()

	_, command := model.executeCommand(commands[0])
	assert.Nil(t, command)
	require.Error(t, model.picker.err)
	assert.Contains(t, model.picker.err.Error(), "idle")
	assert.Zero(t, controller.newCalls)
}

func TestCommandPickerRendersBelowComposerAndFiltersInPlace(t *testing.T) {
	t.Parallel()

	model := readyModel(t, false)
	model.Update(tea.WindowSizeMsg{Width: 64, Height: 18})
	model.Update(key("/"))
	model.Update(tea.KeyPressMsg{Text: "res"})

	view := model.View()
	content := ansi.Strip(view.Content)
	lines := strings.Split(content, "\n")
	composerLine := lineContaining(lines, inputArrow+" /res")
	resumeLine := lineContaining(lines, "› /resume")

	require.GreaterOrEqual(t, composerLine, 0)
	require.Greater(t, resumeLine, composerLine)
	assert.True(t, strings.HasPrefix(lines[composerLine-1], "╭"))
	assert.True(t, strings.HasPrefix(lines[composerLine+1], "╰"))
	assert.Less(t, composerLine+1, resumeLine)
	assert.NotContains(t, content, "Commands\n")
	assert.NotContains(t, content, "openai/test-model")
	assert.Contains(t, view.Content, "\x1b[")
	assert.Equal(t, pickerCommand, model.picker.kind)
	require.NotNil(t, view.Cursor)
}

func TestCommandPickerUsesRemainingHeightAndLightweightSelection(t *testing.T) {
	t.Parallel()

	model := readyModel(t, false)
	model.Update(tea.WindowSizeMsg{Width: 72, Height: 30})
	model.openCommandPicker()

	content := ansi.Strip(model.View().Content)
	for _, command := range commands {
		assert.Contains(t, content, "/"+command.name)
	}
	selected := model.renderCommandPickerRow(commands[0], true)
	assert.Contains(t, selected, "› /new")
	assert.NotContains(t, selected, "\x1b[48;")
}

func TestCommandPickerWrapsDescriptionWithAlignedContinuation(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.width = 32
	row := model.renderCommandPickerRow(commandDescriptor{
		name:        "long-command",
		description: "explain a deliberately long command description",
	}, true)
	lines := strings.Split(row, "\n")

	require.Greater(t, len(lines), 1)
	assert.Contains(t, lines[0], "› /long-command")
	for index, line := range lines {
		assert.LessOrEqual(t, ansi.StringWidth(line), model.width)
		if index > 0 {
			assert.NotContains(t, line, "›")
			assert.Greater(t, len(line)-len(strings.TrimLeft(line, " ")), 2)
		}
	}
}

func TestCommandPickerRestoresDraftAndUsesBoundedNoColorWindow(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 32, Height: 9})
	model.composer.SetValue("keep this draft")
	model.openCommandPicker()

	for range 8 {
		model.Update(key("down"))
	}

	content := model.View().Content
	assert.NotContains(t, content, "\x1b[")
	assert.Contains(t, content, "› /")
	assert.LessOrEqual(t, strings.Count(content, "\n"), 9)

	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, pickerNone, model.picker.kind)
	assert.Equal(t, "keep this draft", model.composer.Value())

	model.openCommandPicker()
	model.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	assert.Equal(t, pickerNone, model.picker.kind)
	assert.Equal(t, "keep this draft", model.composer.Value())
}

func TestCommandPickerReplacementIsSingleFlightAndClearsCommandInput(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	model := readyModelWithController(t, controller, true)
	model.openCommandPicker()

	_, command := model.Update(key("enter"))
	require.NotNil(t, command)
	assert.True(t, model.picker.controlling)
	assert.Equal(t, "/new", model.composer.Value())
	_, duplicate := model.Update(key("enter"))
	assert.Nil(t, duplicate)

	_, commit := model.Update(commandMessage(t, command))
	printed := driveModelCommandsCapture(t, model, commit)
	assert.Equal(t, 1, controller.newCalls)
	assert.Equal(t, pickerNone, model.picker.kind)
	assert.Empty(t, model.composer.Value())
	assert.Contains(t, printed, "✻ Pips")
}

func TestResumeCommandOpensFullWidthSessionPicker(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.sessions = []session.Metadata{{ID: "alpha", Preview: "first question"}}
	model := readyModelWithController(t, controller, true)
	model.Update(key("/"))
	model.Update(tea.KeyPressMsg{Text: "res"})

	_, load := model.Update(key("enter"))
	require.NotNil(t, load)
	driveModelCommands(t, model, load)

	assert.Equal(t, routeSessions, model.route.kind)
	assert.Contains(t, model.View().Content, "Resume session")
}

func TestAgentsCommandOpensCurrentSessionListAndDetail(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	summary := subagent.Summary{
		ChildSessionID: "child-1", Role: subagent.RoleExplore,
		State: subagent.StateSucceeded, TaskPreview: "Locate runtime wiring",
		Model: "openai/test", CreatedAt: time.Now().Add(-time.Minute),
		Duration: 2 * time.Second, Code: "ok",
	}
	controller.agents = []subagent.Summary{summary}
	controller.agentDetail = subagent.Detail{
		Summary: summary,
		Transcript: []ai.Message{
			ai.UserText("Locate runtime wiring."),
			ai.AssistantText(`{"summary":"Found it.","evidence":[],"unknowns":[]}`),
		},
		Result: subagent.ExploreResult{Summary: "Found it."},
	}
	model := readyModelWithController(t, controller, true)
	model.openCommandPicker()
	for _, character := range "agents" {
		model.Update(tea.KeyPressMsg{Text: string(character)})
	}

	_, load := model.Update(key("enter"))
	require.NotNil(t, load)
	driveModelCommands(t, model, load)
	assert.Equal(t, routeAgents, model.route.kind)
	assert.Contains(t, model.View().Content, "Locate runtime wiring")

	_, inspect := model.Update(key("enter"))
	require.NotNil(t, inspect)
	driveModelCommands(t, model, inspect)
	content := model.View().Content
	assert.Equal(t, routeSubagent, model.route.kind)
	assert.Contains(t, content, "❯ Locate runtime wiring.")
	assert.Contains(t, content, "Found it.")
	assert.Contains(t, content, "▣ openai/test · 2s")
	assert.NotContains(t, content, "Subagent · explore · succeeded")
	assert.NotContains(t, content, "Activity")
	assert.NotContains(t, content, "Details")

	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, routeAgents, model.route.kind)
}

func TestCtrlTTogglesSubagentDetailFromDurableToolEnvelope(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Tools = []coding.ToolState{{
		RunID:  "parent-run",
		Call:   coding.ToolCall{ID: "call-1", Name: subagent.ToolName},
		Status: coding.ToolStatusCompleted,
		Result: ai.ToolResultText(
			"call-1",
			subagent.ToolName,
			`{"schema":"`+subagent.ResultSchema+`","child_session_id":"child-durable"}`,
		),
	}}
	controller := newOverlayController(state)
	controller.agentDetail = subagent.Detail{Summary: subagent.Summary{
		ChildSessionID: "child-durable", Role: subagent.RoleReview,
	}}
	model := readyModelWithController(t, controller, true)

	_, command := model.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	require.NotNil(t, command)
	driveModelCommands(t, model, command)
	assert.Equal(t, []string{"child-durable"}, controller.agentInspections)
	assert.Equal(t, routeSubagent, model.route.kind)
	assert.NotNil(t, model.route.detail)
	assert.NotContains(t, model.View().Content, "Subagent · review")

	model.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	assert.Equal(t, routeNone, model.route.kind)
	assert.Nil(t, model.route.detail)
	assert.True(t, model.composer.Focused())
}
