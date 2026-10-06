package tui

import (
	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/internal/coding/config"
)

// ScreenMode selects the interactive render layout.
//
// The render layout (transcript viewport vs. native-history tail) and the
// terminal buffer (alternate screen vs. main screen) are independent decisions:
// a full layout drawn inline in the main buffer is a supported combination, not
// a fallback.
type ScreenMode uint8

const (
	// ScreenInline keeps the terminal's own scrollback and the mutable tail.
	ScreenInline ScreenMode = iota
	// ScreenFullscreen owns a fixed-height layout and its own scrolling.
	ScreenFullscreen
)

// AltScreenPolicy selects the terminal buffer independently of the layout.
type AltScreenPolicy uint8

const (
	// AltScreenAuto uses the alternate screen unless the environment is known to
	// limit it.
	AltScreenAuto AltScreenPolicy = iota
	// AltScreenAlways forces the alternate screen.
	AltScreenAlways
	// AltScreenNever draws the layout inline in the main buffer.
	AltScreenNever
)

// presentationSnapshot is what the last rendered frame resolved to. The exit path
// needs it to decide what to leave on the main screen without asking a Controller
// that has already been released.
type presentationSnapshot struct {
	screen ScreenMode
	exit   string
}

// presentationView is the single place that decides terminal buffer and mouse
// reporting for a frame. Every view constructor routes through it so a surface
// cannot leave the alternate screen mid-session by omission.
func (m *Model) presentationView(content string) tea.View {
	view := tea.NewView(content)
	view.SetContent(content)
	view.WindowTitle = appTitle
	view.AltScreen, view.MouseMode = m.terminalModes()
	// A frame that captures the pointer resolves gestures against what it painted,
	// so every surface gets the same handler: it decides what its own frame affords.
	if m.mouseReportingEnabled() {
		view.OnMouse = m.handleMouse
	}

	return view
}

// presentationPolicy resolves the layout, buffer and mouse policy for this frame
// and records it for the exit path.
//
// Explicit [Options] win, which is what tests and direct callers use. Otherwise
// the persisted [tui] configuration applies once a Controller is available; before
// that (the trust screen and the loading state) the inline layout is used, because
// a fullscreen frame drawn before the first WindowSizeMsg has no geometry to
// honour.
func (m *Model) presentationPolicy() (ScreenMode, AltScreenPolicy, bool) {
	screen, policy, mouse, exit := m.resolvePresentation()
	m.presentationSnapshot = presentationSnapshot{screen: screen, exit: exit}

	return screen, policy, mouse
}

// resolvePresentation computes the policy without recording it.
func (m *Model) resolvePresentation() (ScreenMode, AltScreenPolicy, bool, string) {
	if m.presentationExplicit() {
		return m.options.Screen, m.options.AltScreen, m.options.MouseReporting,
			exitOutputOr(m.options.ExitOutput)
	}

	if m.controller == nil || m.lifecycle != lifecycleReady || !m.sizeReady {
		return ScreenInline, AltScreenAuto, false, config.DefaultExitOutput
	}

	settings := m.controller.Config().TUI

	screen := ScreenFullscreen
	if settings.Screen == config.ScreenInline {
		screen = ScreenInline
	}

	policy := AltScreenAuto

	switch settings.AltScreen {
	case config.AltScreenAlways:
		policy = AltScreenAlways
	case config.AltScreenNever:
		policy = AltScreenNever
	}

	return screen, policy, settings.Mouse, exitOutputOr(settings.ExitOutput)
}

// exitOutputOr resolves an unset exit policy to the shipped default.
func exitOutputOr(value string) string {
	if value == "" {
		return config.DefaultExitOutput
	}

	return value
}

// presentationExplicit reports whether the caller pinned the presentation policy
// rather than leaving it to configuration.
func (m *Model) presentationExplicit() bool {
	return m.options.PinPresentation
}

// fullscreen reports whether the transcript viewport owns presentation.
func (m *Model) fullscreen() bool {
	screen, _, _ := m.presentationPolicy()

	return screen == ScreenFullscreen
}

// showThinkingBlocks reports whether the viewport renders visible reasoning as
// Thinking blocks. The persisted [tui] setting decides; before a controller is
// attached the shipped default applies.
func (m *Model) showThinkingBlocks() bool {
	if m.controller == nil {
		return config.DefaultShowThinkingBlocks
	}

	return m.controller.Config().TUI.ShowThinkingBlocks
}

// transcriptModeActive reports whether the in-session escape hatch owns the
// terminal. While it does, the app draws a compact frame in the main buffer and
// leaves scrolling, selection and search to the terminal.
func (m *Model) transcriptModeActive() bool {
	return m.transcriptMode.active
}

// terminalModes resolves the buffer and mouse modes for the current frame.
//
// Mouse capture is a fullscreen-mode capability only. The inline layout leaves
// scrolling and selection to the terminal, so capturing the mouse there would take
// drag-select away without offering a replacement.
func (m *Model) terminalModes() (bool, tea.MouseMode) {
	screen, policy, mouse := m.presentationPolicy()
	if m.transcriptModeActive() {
		return false, tea.MouseModeNone
	}

	alt := m.useAltScreen(screen, policy)

	// Ctrl+R releases the capture for a session: while it is off the terminal does
	// its own selection and copy, and the viewport receives no wheel, click or drag.
	if !mouse || !alt || m.mouseCaptureOff {
		return alt, tea.MouseModeNone
	}

	return alt, m.mouseMode()
}

// useAltScreen reports whether this frame is drawn in the alternate buffer.
func (m *Model) useAltScreen(screen ScreenMode, policy AltScreenPolicy) bool {
	if screen != ScreenFullscreen {
		return false
	}

	switch policy {
	case AltScreenAlways:
		return true
	case AltScreenNever:
		return false
	default:
		return m.altScreenSupported()
	}
}

// altScreenSupported reports whether the automatic policy should use the
// alternate screen for this environment.
//
// There is no portable capability query for the alternate screen, so the
// decision uses the environments known to limit it: Zellij and tmux control mode
// restrict the alternate buffer, and a nested multiplexer session is the case
// where a full-screen frame is most likely to be clipped or refused. Everything
// else defaults to the alternate screen, which is the same default comparable
// agents ship.
func (m *Model) altScreenSupported() bool {
	return !m.restrictedAltScreenEnvironment()
}

// restrictedAltScreenEnvironment reports whether the environment is known to
// limit the alternate screen. This is deliberately narrow: an unrecognized
// environment keeps the default rather than silently degrading.
func (m *Model) restrictedAltScreenEnvironment() bool {
	environment := m.options.Environment

	return hasEnvironmentValue(environment, "ZELLIJ") ||
		hasEnvironmentPrefix(environment, "TERM", "screen") && hasEnvironmentValue(environment, "TMUX")
}

// mouseMode selects a mouse reporting mode for this frame. A multiplexer forwards
// every pointer movement across an extra process boundary, so all-motion
// reporting is measurably more expensive there; cell motion (press, release,
// wheel, drag) is enough for scrolling and click hit-testing.
func (m *Model) mouseMode() tea.MouseMode {
	if m.inMultiplexer() {
		return tea.MouseModeCellMotion
	}

	return tea.MouseModeAllMotion
}

// inMultiplexer reports whether the program runs under a terminal multiplexer.
func (m *Model) inMultiplexer() bool {
	return inMultiplexerEnvironment(m.options.Environment)
}
