//nolint:wsl_v5 // Model transitions and assertions stay locally paired.
package tui

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/changes"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/runtimecontrol"
	"github.com/rsbin1178/pips/internal/coding/statusline"
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
	assert.False(t, view.AltScreen)
	assert.Equal(t, tea.MouseModeNone, view.MouseMode)
	assert.Contains(t, view.Content, "  Allow")
	assert.Contains(t, view.Content, "> Deny")
	assert.NotContains(t, view.Content, "Selected:")

	updated, command := model.Update(key("enter"))
	require.Same(t, model, updated)
	require.NotNil(t, command)
	result := command()
	updated, command = model.Update(result)
	require.Same(t, model, updated)
	require.NotNil(t, command)
	assert.Contains(t, driveModelCommandsCapture(t, model, command), "Pips")
	assert.Equal(t, []bool{false}, decisions)
	assert.Contains(t, model.View().Content, "openai/test-model")
}

func TestTrustListSupportsArrowNavigationAndNarrowNoColor(t *testing.T) {
	t.Parallel()

	model := newModel(t.Context(), Options{
		Workspace: "/workspace/with/a/long/name",
		NoColor:   true,
		Bootstrap: func(context.Context, bool) (Controller, error) { return nil, nil },
	})
	model.Update(tea.WindowSizeMsg{Width: 28, Height: 12})

	model.Update(key("up"))
	assert.True(t, model.allow)
	assert.Contains(t, model.View().Content, "> Allow")
	assert.Contains(t, model.View().Content, "  Deny")

	model.Update(key("down"))
	assert.False(t, model.allow)
	assert.Contains(t, model.View().Content, "> Deny")
	assert.NotContains(t, model.View().Content, "\x1b[")
	for line := range strings.SplitSeq(model.View().Content, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), 28)
	}
}

func TestTrustListHighlightsColorSelection(t *testing.T) {
	t.Parallel()

	model := newModel(t.Context(), Options{
		Workspace: "/workspace",
		Bootstrap: func(context.Context, bool) (Controller, error) { return nil, nil },
	})

	content := model.View().Content
	assert.Contains(t, content, "> Deny")
	assert.Contains(t, content, "\x1b[")
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

	assert.GreaterOrEqual(t, model.composer.Height(), 3)
	assert.LessOrEqual(t, model.composer.Height(), composerMaxLines)
	assert.False(t, view.AltScreen)
	assert.Equal(t, tea.MouseModeNone, view.MouseMode)
	assert.NotContains(t, view.Content, "\x1b[")
	assert.NotContains(t, view.Content, strings.Repeat("-", 40))
	assert.NotContains(t, view.Content, "ctrl+j newline")
	assert.Equal(t, 1, strings.Count(view.Content, inputArrow))
	lines := strings.Split(view.Content, "\n")
	for index := range lines {
		lines[index] = strings.TrimRight(lines[index], " ")
	}
	assert.NotEqual(t, -1, lineContaining(lines, "│ "+inputArrow+" one"))
	assert.NotEqual(t, -1, lineContaining(lines, "│   two"))
	assert.NotEqual(t, -1, lineContaining(lines, "│   three"))
	require.NotNil(t, view.Cursor)
	composerLine := lineContaining(lines, inputArrow+" one")
	require.NotEqual(t, -1, composerLine)
	assert.Equal(t, composerLine+model.composer.Cursor().Y, view.Cursor.Y)
	assert.Equal(t, model.composer.Cursor().X+2, view.Cursor.X)
	assert.Equal(t, "┌"+strings.Repeat("─", model.width-2)+"┐", lines[composerLine-1])
	assert.Empty(t, lines[composerLine-2])
}

func TestReadyStatusLineUsesProvisionalLabelAndStyledSegments(t *testing.T) {
	t.Parallel()

	model := readyModel(t, false)
	status := model.statusLine()
	plain := ansi.Strip(status)
	assert.LessOrEqual(t, ansi.StringWidth(status), model.statusLineWidth())
	assert.Contains(t, plain, "workspace  ·  new  ·  openai/test-model  ·  idle")
	assert.NotContains(t, plain, "Agent mode")
	assert.NotContains(t, plain, "shift+tab to cycle")
	assert.Contains(t, status, "\x1b[")

	model.state.Interaction.ID = "interaction-1"
	assert.Contains(t, ansi.Strip(model.statusLine()), "session-1")
	assert.NotContains(t, ansi.Strip(model.statusLine()), " ·  new  · ")
}

func TestReadyStatusLineKeepsModeAtRightEdgeOnNarrowTerminal(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.Mode = coding.ModePlan
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 10})

	status := model.statusLine()
	assert.Equal(t, model.statusLineWidth(), ansi.StringWidth(status))
	assert.True(t, strings.HasSuffix(status, "Plan mode"))
	assert.NotContains(t, status, " ·  plan  · ")
}

func TestReadyViewUsesFullTerminalWidthWithInsetStatus(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.Mode = coding.ModePlan
	model.Update(tea.WindowSizeMsg{Width: 200, Height: 20})
	view := model.View()
	lines := strings.Split(view.Content, "\n")

	assert.Equal(t, 200, model.width)
	require.NotEqual(t, -1, lineContaining(lines, "┌"))
	composerLine := lines[lineContaining(lines, "┌")]
	statusLine := lines[lineContaining(lines, "Plan mode")]
	visibleStatusLine := strings.TrimRight(statusLine, " ")
	assert.Equal(t, 200, ansi.StringWidth(composerLine))
	assert.Equal(t, 198, ansi.StringWidth(visibleStatusLine))
	assert.True(t, strings.HasPrefix(composerLine, "┌"))
	assert.True(t, strings.HasPrefix(statusLine, "  "))
	assert.True(t, strings.HasSuffix(visibleStatusLine, "Plan mode (shift+tab to cycle)"))
}

func TestReadyComposerUsesArrowWithoutPlaceholder(t *testing.T) {
	t.Parallel()

	assert.Equal(t, inputPromptWidth, ansi.StringWidth(inputArrow)+1)

	model := readyModel(t, false)
	styles := model.composer.Styles()
	palette := paletteFor(themeDark)
	assert.Equal(t, palette.composerPrompt, styles.Focused.Prompt.GetForeground())
	assert.Empty(t, model.composer.Placeholder)

	view := ansi.Strip(model.composer.View())
	assert.NotContains(t, view, "Ask Pips")
	assert.Contains(t, view, inputArrow)
	assert.Equal(t, 1, strings.Count(view, inputArrow))
	assert.Contains(t, ansi.Strip(model.View().Content), "╭")

	model.Update(tea.WindowSizeMsg{Width: 23, Height: 10})
	assert.NotContains(t, ansi.Strip(model.View().Content), "╭")
}

func TestReadyLongCompletionMarkerStaysVisibleAtBottom(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	model.state.Transcript = []ai.Message{
		ai.UserText("explain it"),
		ai.AssistantText(strings.Repeat("long response line\n\n", 20)),
	}
	model.renderTranscript(true)
	model.recordCompletion(coding.Event{
		InteractionID: "interaction-1",
		Type:          coding.EventInteractionCompleted,
		Payload: coding.InteractionCompleted{
			Outcome: coding.InteractionSucceeded, DurationMillis: 7_000,
		},
	})
	model.renderTranscript(false)

	assert.Contains(t, model.View().Content, "▣ openai/test-model · 7s")
}

func TestReadyBoundsLongLiveTailToTerminalHeight(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	model.state.Phase = coding.PhaseRunning
	model.state.Interaction.Active = true
	lines := make([]string, 30)
	for index := range lines {
		lines[index] = fmt.Sprintf("stream line %02d", index)
	}
	model.state.Draft = []coding.MessageDelta{{
		Kind: ai.StreamTextDelta,
		Text: strings.Join(lines, "\n"),
	}}
	model.rerenderTranscript(false)

	view := model.View()
	assert.LessOrEqual(t, lipgloss.Height(view.Content), model.height)
	assert.Contains(t, ansi.Strip(view.Content), "stream line 29")
	assert.NotContains(t, ansi.Strip(view.Content), "stream line 00")
	require.NotNil(t, view.Cursor)
	assert.Less(t, view.Cursor.Y, model.height)

	model.state.Transcript = []ai.Message{ai.AssistantText(strings.Join(lines, "\n"))}
	model.state.Draft = nil
	committed := model.takeStableTimeline()
	assert.Contains(t, committed, "stream line 00")
	assert.Contains(t, committed, "stream line 29")
}

func TestReadyRecordsCompletionMarkerOnceAndClearsInvalidAnchors(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.state.Transcript = []ai.Message{
		ai.UserText("question"),
		ai.AssistantText("answer"),
	}
	event := coding.Event{
		InteractionID: "interaction-1",
		Type:          coding.EventInteractionCompleted,
		Payload: coding.InteractionCompleted{
			Outcome: coding.InteractionSucceeded, DurationMillis: 7_000,
		},
	}
	model.recordCompletion(event)
	model.recordCompletion(event)

	require.Len(t, model.completionMarkers, 1)
	rendered := renderTimeline(
		model.timelineBlocks(), model.markdown, 80, themeDark, true,
	)
	assert.Equal(t, 1, strings.Count(rendered, "▣ openai/test-model · 7s"))

	model.recordCompletion(coding.Event{Type: coding.EventSessionNavigated})
	assert.Empty(t, model.completionMarkers)

	for index := range maxCompletionMarkers + 1 {
		bounded := event
		bounded.InteractionID = fmt.Sprintf("interaction-%d", index)
		model.recordCompletion(bounded)
	}
	require.Len(t, model.completionMarkers, maxCompletionMarkers)
	assert.Equal(t, "interaction-1", model.completionMarkers[0].interactionID)

	detail := newToolDetailView(timelineBlock{kind: blockTool, tools: []toolActivity{{
		id: "tool-1", name: "read", class: toolClassExplore,
	}}})
	driveModelCommands(t, model, model.openToolDetailRoute(detail))
	model.Update(controlResultMsg{operation: operationNew})
	assert.Empty(t, model.completionMarkers)
	assert.Equal(t, routeToolDetail, model.route.kind)
}

func TestReadyLeavesSelectionAndScrollbackToTerminal(t *testing.T) {
	t.Parallel()

	model := readyModel(t, false)
	model.Update(tea.WindowSizeMsg{Width: 30, Height: 8})
	assert.False(t, model.View().AltScreen)
	assert.Equal(t, tea.MouseModeNone, model.View().MouseMode)

	before := model.composer.Value()
	ignored := []tea.MouseMsg{
		tea.MouseClickMsg{X: 3, Y: 3, Button: tea.MouseLeft},
		tea.MouseReleaseMsg{X: 3, Y: 3, Button: tea.MouseLeft},
		tea.MouseMotionMsg{X: 4, Y: 3, Button: tea.MouseLeft},
	}
	for _, message := range ignored {
		updated, command := model.Update(message)
		require.Same(t, model, updated)
		assert.Nil(t, command)
		assert.Equal(t, before, model.composer.Value())
	}

	updated, command := model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	require.Same(t, model, updated)
	assert.Nil(t, command)

	model.composer.SetValue("first\nsecond")
	model.setLayout()
	require.Equal(t, 1, model.composer.Line())
	updated, command = model.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	require.Same(t, model, updated)
	assert.Nil(t, command)
	assert.Equal(t, 0, model.composer.Line())
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

func TestReadyCommitsLongHistoryOutsideManagedView(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	model.Update(tea.WindowSizeMsg{Width: 30, Height: 8})
	messages := make([]ai.Message, 0, 30)
	for index := range 30 {
		messages = append(messages, ai.UserText(strings.Repeat("line ", index+1)))
	}
	model.state.Transcript = messages
	committed := model.takeStableTimeline()
	model.rerenderTranscript(false)

	assert.Contains(t, committed, "line ")
	assert.NotContains(t, model.View().Content, "line ")
	assert.NotContains(t, ansi.Strip(model.statusLine()), " new")
}

func TestReadyPromptUsesOneBridgeAndFinishes(t *testing.T) {
	t.Parallel()

	controller := &interactionController{stubController: stubController{state: readyState()}}
	model := readyModelWithController(t, controller, true)
	model.composer.SetValue("hello")

	_, start := model.Update(key("enter"))
	require.NotNil(t, start)
	driveModelCommands(t, model, start)
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
	commands, ok := stop().(tea.BatchMsg)
	require.True(t, ok)
	require.NotEmpty(t, commands)
	model.Update(commands[0]())
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
	assert.True(t, model.canceling)
	assert.Contains(t, model.View().Content, "Interrupting…")
	model.Update(cancel())
	assert.Equal(t, 1, controller.canceled)
	assert.Nil(t, model.bridge)
	assert.False(t, model.waiting)
	assert.False(t, model.canceling)
	assert.Equal(t, coding.PhaseIdle, model.state.Phase)
}

func TestReadyToolDetailsToggleNeverShowsReasoning(t *testing.T) {
	t.Parallel()

	const secret = "tool-reasoning-secret"
	state := readyState()
	state.Tools = []coding.ToolState{{
		Call: coding.ToolCall{
			ID: "call-1", Name: "read", Arguments: ai.JSON(`{"path":"main.go"}`),
		},
		Status: coding.ToolStatusCompleted,
		Result: ai.ToolResults(
			ai.ToolResultPart{
				ToolCallID: "call-1",
				Name:       "read",
				Content: []ai.Part{
					ai.TextPart{Text: "visible result"},
					ai.ReasoningPart{Text: secret, Signature: secret},
				},
			},
		),
	}}
	model := readyModelWithController(t, stubController{state: state}, true)
	assert.NotContains(t, model.View().Content, "visible result")
	scrollbackOutput := model.scrollbackOutput

	_, command := model.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	assert.Nil(t, command)
	assert.Equal(t, scrollbackOutput, model.scrollbackOutput)
	assert.Equal(t, routeToolDetail, model.route.kind)
	detail := model.toolDetailRouteContent()
	assert.Contains(t, detail, "visible result")
	assert.NotContains(t, detail, secret)
	_, command = model.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	assert.Nil(t, command)
	assert.Equal(t, routeNone, model.route.kind)
	assert.Equal(t, scrollbackOutput, model.scrollbackOutput)
	assert.True(t, model.composer.Focused())
}

func TestReadyToolDetailsToggleOpensLatestExplorationGroup(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Tools = []coding.ToolState{
		{
			Call: coding.ToolCall{
				ID: "call-1", Name: "read", Arguments: ai.JSON(`{"path":"model.go"}`),
			},
			Status: coding.ToolStatusCompleted,
			Result: codingToolResultFor("call-1", "read", "model contents"),
		},
		{
			Call: coding.ToolCall{
				ID: "call-2", Name: "grep",
				Arguments: ai.JSON(`{"pattern":"toggleLatestTool","path":"internal/coding/tui"}`),
			},
			Status: coding.ToolStatusCompleted,
			Result: codingToolResultFor("call-2", "grep", "one match"),
		},
	}
	model := readyModelWithController(t, stubController{state: state}, true)

	_, command := model.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	assert.Nil(t, command)
	assert.Equal(t, routeToolDetail, model.route.kind)
	detail := model.toolDetailRouteContent()
	assert.Contains(t, detail, "Read model.go")
	assert.Contains(t, detail, "Search toggleLatestTool in internal/coding/tui")
}

func TestSubscriptionSnapshotCommitsMissedStableTimeline(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	state := readyState()
	state.Transcript = []ai.Message{
		ai.UserText("Inspect the event bridge."),
		ai.AssistantText("The subscription snapshot recovered the missing output."),
	}

	_, command := model.Update(subscriptionStartedMsg{
		supported: true,
		observation: coding.EventObservation{
			State: state,
		},
		bridge: &subscriptionBridge{},
	})

	require.NotNil(t, command)
	assert.Equal(t, len(state.Transcript), model.scrollback.messages)
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
	} else {
		// Most unit tests intentionally skip the startup command. Discard the
		// matching presentation transaction as well so later route assertions
		// do not wait for a command this harness chose not to execute.
		model.presentation = presentationState{}
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
		Mode:        coding.ModeAgent,
		Phase:       coding.PhaseIdle,
	}
}

type stubController struct {
	Controller
	state            coding.State
	configStatusLine []statusline.Item
}

func (c stubController) Snapshot() coding.State { return c.state.Clone() }
func (c stubController) Mode() runtimecontrol.ModeState {
	mode := c.state.Mode
	if mode == "" {
		mode = coding.ModeAgent
	}

	return runtimecontrol.ModeState{Current: mode, Configured: mode}
}
func (c stubController) SetMode(context.Context, coding.OperatingMode) error { return nil }
func (c stubController) Permissions() runtimecontrol.PermissionState {
	value := c.Config()
	state := runtimecontrol.PermissionState{
		Sandbox:            value.Sandbox,
		ConfiguredSandbox:  value.Sandbox,
		Approval:           value.Approval,
		ConfiguredApproval: value.Approval,
		Network:            value.SandboxWorkspaceWrite.Network,
		ConfiguredNetwork:  value.SandboxWorkspaceWrite.Network,
	}
	if source, ok := value.Source(config.FieldSandbox); ok {
		state.SandboxSource = source.Kind
	}
	if source, ok := value.Source(config.FieldApproval); ok {
		state.ApprovalSource = source.Kind
	}
	if source, ok := value.Source(config.FieldSandboxNetwork); ok {
		state.NetworkSource = source.Kind
	}

	return state
}

func (stubController) NewFullAccessConfirmation(
	context.Context,
	runtimecontrol.PermissionUpdate,
) (*runtimecontrol.FullAccessConfirmation, error) {
	return nil, nil
}

func (stubController) SetPermissions(
	context.Context,
	runtimecontrol.PermissionUpdate,
	...*runtimecontrol.FullAccessConfirmation,
) error {
	return nil
}

func (stubController) WorkspaceStatus(context.Context) (changes.WorktreeStatus, error) {
	return changes.NewWorktreeStatus(
		false,
		changes.Branch{},
		nil,
		changes.DiffSection{},
		changes.DiffSection{},
		changes.DiffSection{},
		0,
	)
}

func (stubController) DiscoverTeamRecovery(
	context.Context,
) ([]coding.TeamRecoveryCandidate, error) {
	return nil, nil
}

func (c stubController) Config() config.Config {
	value := config.Defaults()
	value.Model.Provider = c.state.Provider
	value.Model.Model = c.state.ModelID
	if c.configStatusLine != nil {
		value.TUI.StatusLine = make([]statusline.Item, len(c.configStatusLine))
		copy(value.TUI.StatusLine, c.configStatusLine)
	}

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
