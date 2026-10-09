package tui

import (
	tea "charm.land/bubbletea/v2"
)

// Wheel scrolling is the only mouse gesture the managed viewport consumes. Clicks,
// drags and selection stay with the terminal, except in the transcript escape
// hatch where the whole gesture set goes back to it.
const (
	// wheelLinesDefault is one notch of viewport scrolling outside a multiplexer.
	wheelLinesDefault = 3
	// wheelLinesMultiplexer is the conservative value inside TMUX/Zellij/screen:
	// every event crosses an extra process boundary, and a notch that scrolls too
	// far is harder to aim than one that scrolls too little. A per-terminal profile
	// table lives here if a terminal is ever reported as feeling wrong.
	wheelLinesMultiplexer = 1
	// wheelHintNavigation names the scroll inputs in a route's hint line, so a list
	// that scrolls its viewport says so once, in the same words everywhere.
	wheelHintNavigation = "wheel/PgUp/PgDn scroll"
)

// scrollWheel routes one wheel notch to whichever surface owns the pointer: the
// managed transcript, a picker dropdown over the ready view, or a full-area route
// that has its own scrolling body.
func (m *Model) scrollWheel(message tea.MouseWheelMsg) tea.Cmd {
	if m.viewportOwnsPointer() {
		return m.scrollViewport(message)
	}

	lines, ok := m.wheelLines(message)
	if !ok {
		return nil
	}

	if m.picker.kind != pickerNone {
		return moveSelection(lines, m.updatePickerKey)
	}

	// The read-only plan preview lives in the prompt band rather than in a route,
	// so its own scrolling keys get the notch here.
	if m.prompt.kind == promptPlanView {
		return m.scrollPlanView(lines)
	}

	return m.scrollRoute(lines)
}

// scrollViewport moves the managed transcript by one wheel notch. A gesture that
// arrives while an overlay owns the screen is dropped: forwarding it to the region
// underneath would move content the user cannot see.
func (m *Model) scrollViewport(message tea.MouseWheelMsg) tea.Cmd {
	if !m.viewportOwnsPointer() {
		return nil
	}

	lines, ok := m.wheelLines(message)
	if !ok {
		return nil
	}

	m.transcriptScroll.scrollBy(lines)

	return nil
}

// scrollRoute moves the active full-area route by one wheel notch, using whatever
// that route's own scrolling keys use:
//
//   - a route with an independent viewport — the Ctrl+T panels, the Agent list,
//     the full-area Team stages — moves that viewport, exactly like its PgUp/PgDn
//     and shift-arrow keys, clamped at the route's own ends;
//   - a route whose window follows its selection — the tree and the pickers — moves
//     the selection, because that is the only way that route scrolls.
func (m *Model) scrollRoute(lines int) tea.Cmd {
	if m.lifecycle != lifecycleReady || !m.sizeReady {
		return nil
	}

	switch m.route.kind {
	case routeToolDetail:
		m.route.offset = min(max(0, m.route.offset+lines), m.toolDetailMaximumOffset())
	case routeChild:
		m.route.offset = min(max(0, m.route.offset+lines), m.subagentRouteMaximumOffset())
	case routeAgents:
		m.route.offset = min(
			max(0, m.route.offset+lines),
			max(0, m.routeScrollLineCount()-m.routeScrollRows()),
		)
	case routeTeam:
		// An inline Team stage lives in the composer's band, not in the full-area
		// viewport its offset is measured against; its keys still scroll it.
		if m.teamRouteIsInline() {
			return nil
		}

		m.route.offset = min(max(0, m.route.offset+lines), m.teamRouteMaximumOffset())
	case routeTree, routeMCP, routeSkills, routeSessions:
		return moveSelection(lines, m.updateRouteKey)
	case routeStatus:
		m.scrollStatusRoute(lines)
	default:
	}

	return nil
}

// moveSelection moves a list's cursor the way pressing its own up/down key would,
// so the wheel cannot drift from the keyboard: wrapping, clamping and any command a
// step triggers (a preview load, a snapshot refresh) follow one path instead of a
// second cursor model.
func moveSelection(
	lines int,
	dispatch func(tea.KeyPressMsg) (tea.Model, tea.Cmd),
) tea.Cmd {
	step := tea.KeyPressMsg{Code: tea.KeyUp, Text: "up"}
	if lines > 0 {
		step = tea.KeyPressMsg{Code: tea.KeyDown, Text: keyDown}
	}

	steps := max(lines, -lines)

	commands := make([]tea.Cmd, 0, steps)
	for range steps {
		if _, command := dispatch(step); command != nil {
			commands = append(commands, command)
		}
	}

	if len(commands) == 0 {
		return nil
	}

	return tea.Batch(commands...)
}

// wheelLines converts a wheel notch into signed rows. A multiplexer reports fewer
// lines per notch because every event crosses an extra process boundary there.
func (m *Model) wheelLines(message tea.MouseWheelMsg) (int, bool) {
	lines := wheelLinesDefault
	if m.inMultiplexer() {
		lines = wheelLinesMultiplexer
	}

	switch message.Button {
	case tea.MouseWheelUp:
		return -lines, true
	case tea.MouseWheelDown:
		return lines, true
	default:
		return 0, false
	}
}

// readyViewOwnsPointer reports whether the ready frame is the one on screen: the
// layout's own transcript band is what the pointer acts on, and nothing else
// (a route, a prompt, a picker, the transcript escape hatch, an inline Team stage)
// has taken the pointer or the keyboard.
func (m *Model) readyViewOwnsPointer() bool {
	return m.lifecycle == lifecycleReady && m.sizeReady &&
		m.route.kind == routeNone && !m.teamRouteIsInline() &&
		m.prompt.kind == promptNone && m.picker.kind == pickerNone &&
		!m.transcriptMode.active
}

// viewportOwnsPointer reports whether the managed transcript is the surface a
// pointer may act on as a selection: the ready viewport is showing, nothing else
// owns input, and the escape hatch is not holding the screen open for the
// terminal. The reserved band's arrow needs less than this — it is answered
// wherever the ready frame is the one on screen — so it asks
// [readyViewOwnsPointer] instead.
func (m *Model) viewportOwnsPointer() bool {
	return m.readyViewOwnsPointer() && m.fullscreen()
}

// mouseReportingEnabled reports whether this frame asked the terminal for mouse
// events. It is the resolved policy rather than the negotiated mode: the terminal
// either honours the request or ignores it, and the help text has to describe the
// request.
func (m *Model) mouseReportingEnabled() bool {
	_, mode := m.terminalModes()

	return mode != tea.MouseModeNone
}
