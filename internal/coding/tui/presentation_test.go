package tui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/stretchr/testify/assert"
)

func TestPresentationPolicyUsesAltScreenOnlyForFullscreen(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name     string
		screen   ScreenMode
		policy   AltScreenPolicy
		alt      bool
		mode     tea.MouseMode
		mouse    bool
		env      []string
		wantAlt  bool
		wantMode tea.MouseMode
	}{
		{name: "inline ignores the buffer", screen: ScreenInline, policy: AltScreenAlways, mouse: true, wantAlt: false},
		{name: "always", screen: ScreenFullscreen, policy: AltScreenAlways, mouse: true, wantAlt: true, wantMode: tea.MouseModeAllMotion},
		{name: "never stays inline", screen: ScreenFullscreen, policy: AltScreenNever, mouse: true, wantAlt: false, wantMode: tea.MouseModeNone},
		{name: "auto defaults to alternate", screen: ScreenFullscreen, policy: AltScreenAuto, mouse: true, wantAlt: true, wantMode: tea.MouseModeAllMotion},
		{
			name: "auto degrades inside zellij", screen: ScreenFullscreen, policy: AltScreenAuto, mouse: true,
			env: []string{"ZELLIJ=1"}, wantAlt: false, wantMode: tea.MouseModeNone,
		},
		{
			name: "auto degrades in tmux control mode", screen: ScreenFullscreen, policy: AltScreenAuto, mouse: true,
			env: []string{"TERM=screen-256color", "TMUX=/tmp/tmux,1,0"}, wantAlt: false,
			wantMode: tea.MouseModeNone,
		},
		{
			name: "cell motion under tmux", screen: ScreenFullscreen, policy: AltScreenAlways, mouse: true,
			env: []string{"TMUX=/tmp/tmux,1,0", "TERM=tmux-256color"}, wantAlt: true,
			wantMode: tea.MouseModeCellMotion,
		},
		{
			name: "mouse disabled", screen: ScreenFullscreen, policy: AltScreenAlways, mouse: false,
			wantAlt: true, wantMode: tea.MouseModeNone,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			model := newModel(t.Context(), Options{
				Workspace:       "/workspace",
				Environment:     testCase.env,
				PinPresentation: true,
				Screen:          testCase.screen,
				AltScreen:       testCase.policy,
				MouseReporting:  testCase.mouse,
			})
			alt, mode := model.terminalModes()
			assert.Equal(t, testCase.wantAlt, alt)
			assert.Equal(t, testCase.wantMode, mode)

			view := model.presentationView("frame")
			assert.Equal(t, testCase.wantAlt, view.AltScreen)
			assert.Equal(t, testCase.wantMode, view.MouseMode)
			assert.Equal(t, appTitle, view.WindowTitle)
		})
	}
}

func TestMultiplexerEnvironmentDetection(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		env  []string
		want bool
	}{
		{env: nil, want: false},
		{env: []string{"TERM=xterm-256color"}, want: false},
		{env: []string{"TMUX=/tmp/tmux,1,0"}, want: true},
		{env: []string{"STY=12345.pts-0.host"}, want: true},
		{env: []string{"ZELLIJ=0"}, want: true},
		{env: []string{"TERM=tmux-256color"}, want: true},
		{env: []string{"TERM=screen.xterm"}, want: true},
		{env: []string{"TERM="}, want: false},
	} {
		assert.Equal(t, testCase.want, inMultiplexerEnvironment(testCase.env), "%v", testCase.env)
	}
}

// TestPresentationFollowsPersistedConfiguration asserts the interactive default
// comes from the persisted [tui] settings rather than from the zero value of
// Options, so a real CLI session starts in the configured layout.
func TestPresentationFollowsPersistedConfiguration(t *testing.T) {
	t.Parallel()

	state := readyState()

	for _, testCase := range []struct {
		name       string
		tui        config.TUIConfig
		wantAlt    bool
		wantMouse  tea.MouseMode
		wantScreen ScreenMode
	}{
		{
			name: "defaults are fullscreen with the alternate screen",
			tui: config.TUIConfig{
				Screen: config.ScreenFullscreen, AltScreen: config.AltScreenAuto, Mouse: true,
			},
			wantAlt: true, wantMouse: tea.MouseModeAllMotion, wantScreen: ScreenFullscreen,
		},
		{
			name: "inline keeps the terminal history",
			tui: config.TUIConfig{
				Screen: config.ScreenInline, AltScreen: config.AltScreenAuto, Mouse: true,
			},
			wantAlt: false, wantMouse: tea.MouseModeNone, wantScreen: ScreenInline,
		},
		{
			name: "fullscreen can stay on the main buffer",
			tui: config.TUIConfig{
				Screen: config.ScreenFullscreen, AltScreen: config.AltScreenNever, Mouse: true,
			},
			wantAlt: false, wantMouse: tea.MouseModeNone, wantScreen: ScreenFullscreen,
		},
		{
			name: "mouse can be turned off",
			tui: config.TUIConfig{
				Screen: config.ScreenFullscreen, AltScreen: config.AltScreenAlways, Mouse: false,
			},
			wantAlt: true, wantMouse: tea.MouseModeNone, wantScreen: ScreenFullscreen,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			controller := stubController{state: state, tui: testCase.tui}

			model := newModel(t.Context(), Options{
				Workspace: "/workspace",
				NoColor:   true,
				Bootstrap: func(context.Context, bool) (Controller, error) {
					return controller, nil
				},
			})
			// Before bootstrap the model has no configuration, so it stays safe.
			_, _ = model.terminalModes()

			model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			_, _ = model.Update(bootstrapResult{controller: controller})

			screen, _, _ := model.presentationPolicy()
			assert.Equal(t, testCase.wantScreen, screen)

			alt, mouse := model.terminalModes()
			assert.Equal(t, testCase.wantAlt, alt)
			assert.Equal(t, testCase.wantMouse, mouse)
			assert.Equal(t, testCase.wantAlt, model.View().AltScreen)
		})
	}
}
