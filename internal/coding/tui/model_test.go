//nolint:wsl_v5 // Model transitions and assertions stay locally paired.
package tui

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrustDefaultsToDenyAndBootstrapsSelection(t *testing.T) {
	t.Parallel()

	var decisions []bool
	model := newModel(t.Context(), Options{
		Workspace: "/workspace",
		Bootstrap: func(_ context.Context, trusted bool) (Controller, error) {
			decisions = append(decisions, trusted)

			return stubController{state: readyState()}, nil
		},
	})

	view := model.View()
	assert.True(t, view.AltScreen)
	assert.Equal(t, tea.MouseModeCellMotion, view.MouseMode)
	assert.Contains(t, view.Content, "Selected: Deny")

	updated, command := model.Update(key("enter"))
	require.Same(t, model, updated)
	require.NotNil(t, command)
	result := command()
	updated, command = model.Update(result)
	require.Same(t, model, updated)
	assert.Nil(t, command)
	assert.Equal(t, []bool{false}, decisions)
	assert.Contains(t, model.View().Content, "openai/test-model")
}

func TestTrustAllowsExplicitKeyboardSelection(t *testing.T) {
	t.Parallel()

	trusted := false
	model := newModel(t.Context(), Options{
		Workspace: "/workspace",
		Bootstrap: func(_ context.Context, selected bool) (Controller, error) {
			trusted = selected

			return stubController{state: readyState()}, nil
		},
	})

	_, command := model.Update(key("a"))
	require.NotNil(t, command)
	model.Update(command())
	assert.True(t, trusted)
}

func TestBootstrapErrorBecomesFatalView(t *testing.T) {
	t.Parallel()

	model := newModel(t.Context(), Options{
		Workspace: "/workspace",
		Trusted:   true,
		Bootstrap: func(context.Context, bool) (Controller, error) {
			return nil, errors.New("configuration is invalid")
		},
	})

	command := model.bootstrap(true)
	require.NotNil(t, command)
	model.Update(command())
	assert.Contains(t, model.View().Content, "Pips could not start")
	assert.Contains(t, model.View().Content, "configuration is invalid")
}

func TestTrustPersistenceFailureRemainsRetryable(t *testing.T) {
	t.Parallel()

	model := newModel(t.Context(), Options{
		Workspace: "/workspace",
		Bootstrap: func(context.Context, bool) (Controller, error) {
			return nil, &TrustError{Err: errors.New("read-only store")}
		},
	})

	_, command := model.Update(key("a"))
	require.NotNil(t, command)
	model.Update(command())
	assert.Equal(t, lifecycleTrust, model.lifecycle)
	assert.Contains(t, model.View().Content, "Retry or choose Deny")
}

func TestReadyLayoutSupportsResizeMultilineAndNoColor(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	model.composer.SetValue("one\ntwo\nthree")
	model.setLayout()
	view := model.View()

	assert.Equal(t, 40, model.viewport.Width())
	assert.GreaterOrEqual(t, model.composer.Height(), 3)
	assert.LessOrEqual(t, model.composer.Height(), composerMaxLines)
	assert.True(t, view.AltScreen)
	assert.Equal(t, tea.MouseModeCellMotion, view.MouseMode)
	assert.NotContains(t, view.Content, "\x1b[")
}

func TestReadyOnlyMouseWheelChangesTranscriptScroll(t *testing.T) {
	t.Parallel()

	model := readyModel(t, false)
	model.Update(tea.WindowSizeMsg{Width: 30, Height: 8})
	for index := range 30 {
		model.state.Transcript = append(
			model.state.Transcript,
			ai.UserText(strings.Repeat("line ", index+1)),
		)
	}
	model.renderTranscript(true)
	require.True(t, model.viewport.AtBottom())

	before := model.composer.Value()
	beforeOffset := model.viewport.YOffset()
	ignored := []tea.MouseMsg{
		tea.MouseClickMsg{X: 3, Y: 3, Button: tea.MouseLeft},
		tea.MouseReleaseMsg{X: 3, Y: 3, Button: tea.MouseLeft},
		tea.MouseMotionMsg{X: 4, Y: 3, Button: tea.MouseLeft},
	}
	for _, message := range ignored {
		updated, command := model.Update(message)
		require.Same(t, model, updated)
		assert.Nil(t, command)
		assert.Equal(t, beforeOffset, model.viewport.YOffset())
		assert.Equal(t, before, model.composer.Value())
	}

	updated, command := model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	require.Same(t, model, updated)
	assert.Nil(t, command)
	assert.Less(t, model.viewport.YOffset(), beforeOffset)
}

func TestReadyCoalescesRenderTicks(t *testing.T) {
	t.Parallel()

	model := readyModel(t, false)

	first := model.requestRender()
	second := model.requestRender()
	assert.NotNil(t, first)
	assert.Nil(t, second)
	model.Update(renderTickMsg{})
	assert.False(t, model.renderWait)
	assert.NotNil(t, model.requestRender())
}

func TestReadyScrollingDoesNotForceBottom(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 30, Height: 8})
	messages := make([]ai.Message, 0, 30)
	for index := range 30 {
		messages = append(messages, ai.UserText(strings.Repeat("line ", index+1)))
	}
	model.state.Transcript = messages
	model.renderTranscript(true)
	require.True(t, model.viewport.AtBottom())
	model.viewport.PageUp()
	require.False(t, model.viewport.AtBottom())

	model.state.Transcript = append(model.state.Transcript, ai.Assistant(ai.Text("new")))
	model.renderTranscript(false)
	assert.False(t, model.viewport.AtBottom())
	assert.Equal(t, 1, model.unseen)

	model.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	assert.True(t, model.viewport.AtBottom())
	assert.Zero(t, model.unseen)
}

func TestReadyPromptUsesOneBridgeAndFinishes(t *testing.T) {
	t.Parallel()

	controller := &interactionController{stubController: stubController{state: readyState()}}
	model := readyModelWithController(t, controller, true)
	model.composer.SetValue("hello")

	_, start := model.Update(key("enter"))
	require.NotNil(t, start)
	_, wait := model.Update(start())
	require.NotNil(t, wait)
	_, command := model.Update(wait())
	assert.Nil(t, command)
	assert.Nil(t, model.bridge)
	require.Len(t, controller.prompts, 1)
	assert.Equal(t, "hello", visibleMessageText(controller.prompts[0]))
}

func TestReadyRunningEnterSteersAndTabFollowsUp(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Phase = coding.PhaseRunning
	state.Interaction.Active = true
	controller := &interactionController{stubController: stubController{state: state}}
	model := readyModelWithController(t, controller, true)

	model.composer.SetValue("steer this")
	_, steer := model.Update(key("enter"))
	require.NotNil(t, steer)
	model.Update(steer())
	model.composer.SetValue("then this")
	_, follow := model.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	require.NotNil(t, follow)
	model.Update(follow())

	require.Len(t, controller.steered, 1)
	require.Len(t, controller.followed, 1)
	assert.Equal(t, "steer this", visibleMessageText(controller.steered[0]))
	assert.Equal(t, "then this", visibleMessageText(controller.followed[0]))
	assert.Equal(t, 2, model.queued)
}

func TestStartingBridgeAlreadyUsesRunningInputSemantics(t *testing.T) {
	t.Parallel()

	controller := &interactionController{stubController: stubController{state: readyState()}}
	model := readyModelWithController(t, controller, true)
	model.starting = true
	model.composer.SetValue("redirect before first event")

	_, steer := model.Update(key("enter"))
	require.NotNil(t, steer)
	model.Update(steer())
	require.Len(t, controller.steered, 1)
	assert.Equal(t, "redirect before first event", visibleMessageText(controller.steered[0]))
	assert.Empty(t, controller.prompts)
}

func TestStartingBridgeCancellationWaitsForLateBridge(t *testing.T) {
	t.Parallel()

	controller := &cancelController{
		interactionController: interactionController{
			stubController: stubController{state: readyState()},
		},
	}
	model := readyModelWithController(t, controller, true)
	model.starting = true

	_, cancel := model.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	require.NotNil(t, cancel)
	model.Update(cancel())
	assert.True(t, model.starting)
	assert.True(t, model.cancelStart)

	late := startBridge(t.Context(), func(context.Context) iter.Seq2[coding.Event, error] {
		return func(func(coding.Event, error) bool) {}
	})
	_, stop := model.Update(bridgeStartedMsg{bridge: late})
	require.NotNil(t, stop)
	model.Update(stop())
	assert.Nil(t, model.bridge)
	assert.False(t, model.starting)
	assert.False(t, model.cancelStart)
	assert.Equal(t, 1, controller.canceled)
}

func TestReadyCtrlCRequiresConfirmationWhenIdle(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.composer.SetValue("draft")
	_, command := model.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	assert.Nil(t, command)
	assert.Empty(t, model.composer.Value())
	assert.False(t, model.exitArmed)

	_, command = model.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	require.NotNil(t, command)
	assert.True(t, model.exitArmed)
	_, quit := model.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	require.NotNil(t, quit)
	assert.IsType(t, tea.Quit(), quit())
}

func TestReadyComposerPreservesPasteAndExplicitNewlines(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.KeyPressMsg{Text: "pasted one\npasted two"})
	assert.Equal(t, "pasted one\npasted two", model.composer.Value())

	model.Update(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
	assert.Equal(t, "pasted one\npasted two\n", model.composer.Value())
	model.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
	assert.Equal(t, "pasted one\npasted two\n\n", model.composer.Value())
}

func TestReadyRunningCtrlCCancelsAndWaitsForBridge(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Phase = coding.PhaseRunning
	state.Interaction.Active = true
	controller := &cancelController{
		interactionController: interactionController{
			stubController: stubController{state: state},
		},
	}
	model := readyModelWithController(t, controller, true)
	start := model.startStream(func(ctx context.Context) iter.Seq2[coding.Event, error] {
		return func(func(coding.Event, error) bool) { <-ctx.Done() }
	})
	_, wait := model.Update(start())
	require.NotNil(t, wait)

	_, cancel := model.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	require.NotNil(t, cancel)
	model.Update(cancel())
	assert.Equal(t, 1, controller.canceled)
	assert.Nil(t, model.bridge)
	assert.False(t, model.waiting)
	assert.Equal(t, coding.PhaseIdle, model.state.Phase)
}

func TestReadyToolDetailsToggleNeverShowsReasoning(t *testing.T) {
	t.Parallel()

	const secret = "tool-reasoning-secret"
	state := readyState()
	state.Tools = []coding.ToolState{{
		Call: coding.ToolCall{
			ID: "call-1", Name: "read_file", Arguments: ai.JSON(`{"path":"main.go"}`),
		},
		Status: coding.ToolStatusCompleted,
		Result: ai.Message{
			Role: ai.RoleTool,
			Parts: []ai.Part{ai.ToolResultPart{
				ToolCallID: "call-1",
				Name:       "read_file",
				Content: []ai.Part{
					ai.TextPart{Text: "visible result"},
					ai.ReasoningPart{Text: secret, Signature: secret},
				},
			}},
		},
	}}
	model := readyModelWithController(t, stubController{state: state}, true)
	assert.NotContains(t, model.View().Content, "visible result")

	model.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	assert.Contains(t, model.View().Content, "visible result")
	assert.NotContains(t, model.View().Content, secret)
	model.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	assert.NotContains(t, model.View().Content, "visible result")
}

func readyModel(t *testing.T, noColor bool) *Model {
	t.Helper()

	return readyModelWithController(
		t,
		stubController{state: readyState()},
		noColor,
	)
}

func readyModelWithController(
	t *testing.T,
	controller Controller,
	noColor bool,
) *Model {
	t.Helper()

	model := newModel(t.Context(), Options{
		Workspace: "/workspace",
		NoColor:   noColor,
		Bootstrap: func(context.Context, bool) (Controller, error) {
			return controller, nil
		},
	})
	_, command := model.Update(bootstrapResult{controller: controller})
	if model.starting {
		driveModelCommands(t, model, command)
	}

	return model
}

func key(value string) tea.KeyPressMsg {
	if value == "enter" {
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	}

	return tea.KeyPressMsg{Code: []rune(value)[0], Text: value}
}

func readyState() coding.State {
	return coding.State{
		SessionID:   "session-1",
		SessionOpen: true,
		Provider:    ai.ProviderOpenAI,
		ModelID:     "test-model",
		Phase:       coding.PhaseIdle,
	}
}

type stubController struct {
	Controller
	state coding.State
}

func (c stubController) Snapshot() coding.State { return c.state.Clone() }
func (c stubController) Config() config.Config {
	value := config.Defaults()
	value.Model.Provider = c.state.Provider
	value.Model.Model = c.state.ModelID

	return value
}

func (stubController) Close(context.Context) error {
	return nil
}

type interactionController struct {
	stubController
	prompts  []ai.Message
	steered  []ai.Message
	followed []ai.Message
}

func (c *interactionController) Prompt(
	_ context.Context,
	messages ...ai.Message,
) iter.Seq2[coding.Event, error] {
	c.prompts = append(c.prompts, messages...)

	return func(func(coding.Event, error) bool) {}
}

func (c *interactionController) Steer(messages ...ai.Message) error {
	c.steered = append(c.steered, messages...)

	return nil
}

func (c *interactionController) FollowUp(messages ...ai.Message) error {
	c.followed = append(c.followed, messages...)

	return nil
}

type cancelController struct {
	interactionController
	canceled int
}

func (c *cancelController) Cancel() error {
	c.canceled++
	c.state.Phase = coding.PhaseIdle
	c.state.Interaction.Active = false

	return nil
}
