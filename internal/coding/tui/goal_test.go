package tui

import (
	"context"
	"iter"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseGoalCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     string
		action    string
		condition string
		budget    int
		invalid   bool
	}{
		{name: "bare", action: "status"},
		{name: "status", input: " status ", action: "status"},
		{name: "pause", input: "pause", action: "pause"},
		{name: "resume", input: "resume", action: "resume"},
		{name: "clear", input: "clear", action: "clear"},
		{name: "condition", input: "测试通过\nand lint clean", action: "start", condition: "测试通过\nand lint clean"},
		{name: "budget", input: "tests pass\t--budget\t1200", action: "start", condition: "tests pass", budget: 1200},
		{name: "equals budget", input: "tests pass --budget=1200", action: "start", condition: "tests pass", budget: 1200},
		{name: "zero", input: "tests --budget 0", invalid: true},
		{name: "negative", input: "tests --budget -1", invalid: true},
		{name: "overflow", input: "tests --budget 999999999999999999999", invalid: true},
		{name: "missing amount", input: "tests --budget", invalid: true},
		{name: "missing condition", input: "--budget 100", invalid: true},
		{name: "duplicate budget", input: "tests --budget 100 --budget 200", invalid: true},
		{name: "control arguments", input: "pause other", invalid: true},
		{name: "oversized condition", input: strings.Repeat("字", 4001), invalid: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			command, err := parseGoalCommand(test.input)
			if test.invalid {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, test.action, command.action)
			assert.Equal(t, test.condition, command.request.Condition)
			assert.Equal(t, test.budget, command.request.MaxTokens)
		})
	}
}

func TestGoalControlsAreReachableFromRunningKeyboard(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Phase = coding.PhaseRunning
	state.Goal = coding.GoalState{ID: "goal-1", Status: coding.GoalRunning}
	controller := &goalTestController{overlayController: newOverlayController(state)}
	model := readyModelWithController(t, controller, true)
	model.Update(tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl})
	require.Equal(t, pickerCommand, model.picker.kind)
	model.Update(tea.KeyPressMsg{Text: "pause"})
	_, command := model.Update(key("enter"))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)
	assert.Equal(t, 1, controller.pauseCalls)
}

func TestGoalPendingResumeIsExplicitAndReachable(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Phase = coding.PhasePaused
	state.Goal = coding.GoalState{ID: "goal-1", Status: coding.GoalPaused}
	controller := &goalTestController{overlayController: newOverlayController(state)}
	model := readyModelWithController(t, controller, true)
	assert.Nil(t, model.continueIfPaused(), "bootstrap must not resume a recovered Goal")
	model.Update(tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl})
	require.Equal(t, pickerCommand, model.picker.kind)
	model.Update(tea.KeyPressMsg{Text: "resume"})
	_, command := model.Update(key("enter"))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)
	assert.Equal(t, 1, controller.resumeCalls)
}

func TestGoalCommandStartsThroughController(t *testing.T) {
	t.Parallel()

	controller := &goalTestController{overlayController: newOverlayController(readyState())}
	model := readyModelWithController(t, controller, true)
	model.openCommandPicker()
	model.picker.query = "goal tests pass --budget 1200"
	_, command := model.Update(key("enter"))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	assert.Equal(t, coding.GoalRequest{Condition: "tests pass", MaxTokens: 1200}, controller.request)
	assert.Equal(t, 1, controller.startCalls)
	assert.Equal(t, pickerNone, model.picker.kind)
}

func TestGoalCommandPreservesInvalidArguments(t *testing.T) {
	t.Parallel()

	controller := &goalTestController{overlayController: newOverlayController(readyState())}
	model := readyModelWithController(t, controller, true)
	model.openCommandPicker()
	model.picker.query = "goal tests --budget -1"
	model.syncCommandInput()
	_, command := model.Update(key("enter"))
	assert.Nil(t, command)
	require.Error(t, model.picker.err)
	assert.Equal(t, "goal tests --budget -1", model.picker.query)
	assert.Equal(t, "/goal tests --budget -1", model.composer.Value())
	assert.Zero(t, controller.startCalls)
}

func TestGoalCommandResumeAndClear(t *testing.T) {
	t.Parallel()

	for _, action := range []string{"resume", "clear"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()

			controller := &goalTestController{overlayController: newOverlayController(readyState())}
			model := readyModelWithController(t, controller, true)
			model.openCommandPicker()
			model.picker.query = "goal " + action
			_, command := model.Update(key("enter"))
			require.NotNil(t, command)
			driveModelCommands(t, model, command)

			if action == "resume" {
				assert.Equal(t, 1, controller.resumeCalls)
				assert.Zero(t, controller.clearCalls)
			} else {
				assert.Equal(t, 1, controller.clearCalls)
				assert.Zero(t, controller.resumeCalls)
			}

			assert.Zero(t, controller.startCalls)
		})
	}
}

func TestGoalCommandPauseIsAvailableWhileRunning(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Phase = coding.PhaseRunning
	state.Interaction.Active = true
	controller := &goalTestController{overlayController: newOverlayController(state)}
	model := readyModelWithController(t, controller, true)
	model.openCommandPicker()
	model.picker.query = "goal pause"
	_, command := model.Update(key("enter"))
	require.NotNil(t, command)
	assert.True(t, model.picker.controlling)
	_, duplicate := model.Update(key("enter"))
	assert.Nil(t, duplicate)
	driveModelCommands(t, model, command)
	assert.Equal(t, 1, controller.pauseCalls)
	assert.Equal(t, pickerNone, model.picker.kind)
}

func TestGoalCommandRejectsStartWhileRunning(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Phase = coding.PhaseRunning
	controller := &goalTestController{overlayController: newOverlayController(state)}
	model := readyModelWithController(t, controller, true)
	model.openCommandPicker()
	model.picker.query = "goal new objective"
	_, command := model.Update(key("enter"))
	assert.Nil(t, command)
	require.Error(t, model.picker.err)
	assert.Zero(t, controller.startCalls)
}

func TestGoalControlIgnoresStaleSessionAndPickerResults(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.openCommandPicker()
	model.picker.generation = 4
	model.picker.controlling = true
	state := readyState()

	state.Goal = coding.GoalState{ID: "old", Status: "completed"}
	for _, message := range []goalControlResultMsg{
		{generation: 4, sessionID: "old-session", state: state},
		{generation: 3, sessionID: model.state.SessionID, state: state},
	} {
		_, command := model.Update(message)
		assert.Nil(t, command)
		assert.Empty(t, model.state.Goal.ID)
		assert.True(t, model.picker.controlling)
	}
}

func TestGoalStatusDisplaysStateAndSanitizesInspection(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Goal = coding.GoalState{
		ID: "goal-1", Condition: "tests pass\x1b[2J", Status: "paused", Reason: "missing evidence",
		Attempts: 2, Evaluations: 1, Tokens: 400, MaxTokens: 1200,
	}
	model := readyModelWithController(t, newOverlayController(state), true)
	model.openCommandPicker()
	model.picker.query = "goal status"
	_, command := model.Update(key("enter"))
	content := driveModelCommandsCapture(t, model, command)
	assert.Contains(t, content, "Status: paused")
	assert.Contains(t, content, "Tokens: 400")
	assert.Contains(t, content, "missing evidence")
	assert.NotContains(t, content, "\x1b[2J")
	assert.Contains(t, ansi.Strip(model.statusLine()), "goal paused")
}

type goalTestController struct {
	*overlayController
	request     coding.GoalRequest
	startCalls  int
	pauseCalls  int
	resumeCalls int
	clearCalls  int
}

func (c *goalTestController) StartGoal(_ context.Context, request coding.GoalRequest) iter.Seq2[coding.Event, error] {
	return func(func(coding.Event, error) bool) {
		c.request = request
		c.startCalls++
	}
}

func (c *goalTestController) ResumeGoal(context.Context) iter.Seq2[coding.Event, error] {
	return func(func(coding.Event, error) bool) { c.resumeCalls++ }
}

func (c *goalTestController) PauseGoal(context.Context) error {
	c.pauseCalls++

	return nil
}

func (c *goalTestController) ClearGoal(context.Context) error {
	c.clearCalls++

	return nil
}
