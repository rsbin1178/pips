//nolint:wsl_v5 // Panel assertions follow the pages they read.
package tui

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pageLinesText renders panel lines the way their rows read, so a test can assert
// what a panel discloses without composing a whole view.
func pageLinesText(lines []statusPageLine) string {
	rows := make([]string, 0, len(lines))
	for _, line := range lines {
		switch line.kind {
		case statusLineHeading:
			rows = append(rows, line.text)
		case statusLineField:
			rows = append(rows, line.label+": "+line.value)
		case statusLinePair:
			rows = append(rows, line.label+": "+line.value, line.label2+": "+line.value2)
		case statusLineText:
			rows = append(rows, line.text)
		case statusLineBlank:
			// A blank row carries no text: it is spacing the renderer draws itself.
		}
	}

	return strings.Join(rows, "\n")
}

// helpPageText is one help page as its rows read.
func helpPageText(model *Model, tab helpTab) string {
	return pageLinesText(model.helpRoutePageLines(tab))
}

// TestHelpPanelOpensOnCtrlX is the panel contract: the shortcut list is a full-area
// route like /status, so it owns the screen instead of printing into a transcript the
// reader is not looking at.
func TestHelpPanelOpensOnCtrlX(t *testing.T) {
	t.Parallel()

	model := fullscreenModel(t, stubController{state: readyState()}, true)

	_, _ = model.Update(key("ctrl+x"))
	require.Equal(t, routeHelp, model.route.kind)

	frame := ansi.Strip(model.View().Content)
	assert.Contains(t, frame, "/help")
	assert.Contains(t, frame, "Shortcuts")
	assert.Contains(t, frame, "Commands")
	assert.Contains(t, frame, "Esc to close")

	// The panel is a route in either presentation, exactly like /status.
	inline := readyModel(t, true)
	_, _ = inline.Update(key("ctrl+x"))
	assert.Equal(t, routeHelp, inline.route.kind)
	assert.Contains(t, ansi.Strip(inline.View().Content), "Esc to close")
}

// The /help command opens the same panel as the key, so the two ways to ask for help
// are one surface.
func TestHelpCommandOpensThePanel(t *testing.T) {
	t.Parallel()

	model := fullscreenModel(t, stubController{state: readyState()}, true)

	_, command := model.executeCommand(commandDescriptor{name: string(actionHelp)})
	driveModelCommands(t, model, command)

	require.Equal(t, routeHelp, model.route.kind)
	assert.Contains(t, ansi.Strip(model.View().Content), "Esc to close")
}

// The panel answers the same keys the status panel does: tabs switch pages, the
// scrolling keys and the wheel move the page, and Escape returns to the Composer.
func TestHelpPanelSwitchesPagesScrollsAndCloses(t *testing.T) {
	t.Parallel()

	model := fullscreenModel(t, stubController{state: readyState()}, true)
	_, _ = model.Update(key("ctrl+x"))
	require.Equal(t, routeHelp, model.route.kind)

	// Commands is its own page, and it lists what the Composer's picker offers.
	_, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Equal(t, helpTabCommands, model.route.helpTab)
	assert.Contains(t, ansi.Strip(model.View().Content), "/resume")

	// A wheel notch moves the page, and Escape gives the Composer back.
	before := model.route.offset
	model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown, Y: 4})
	assert.Greater(t, model.route.offset, before)

	_, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, routeNone, model.route.kind)
}

// The shortcut page is generated from the binding table, so every key the ready view
// answers to is listed, and a key that moves cannot leave the panel behind.
func TestHelpPanelListsEveryBinding(t *testing.T) {
	t.Parallel()

	model := fullscreenModel(t, stubController{state: readyState()}, true)
	page := helpPageText(model, helpTabShortcuts)

	for _, binding := range defaultActions {
		if !slices.Contains(binding.Contexts, contextIdle) {
			continue
		}
		assert.Contains(t, page, strings.Join(binding.Keys, " / ")+": "+binding.Label,
			"the panel lists %s", binding.ID)
	}
}

// The two facts a reader cannot get anywhere else: which surface owns the
// conversation, and the modifier that selects text while the mouse is captured.
func TestHelpPanelStatesTheOwningSurfaceAndMouseFallback(t *testing.T) {
	t.Parallel()

	fullscreen := fullscreenModel(t, stubController{state: readyState()}, true)
	fullscreen.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	page := helpPageText(fullscreen, helpTabShortcuts)
	assert.Contains(t, page, "this session owns it")
	assert.Contains(t, page, "Shift")

	inline := readyModel(t, true)
	inlinePage := helpPageText(inline, helpTabShortcuts)
	assert.Contains(t, inlinePage, "the terminal owns it")
	assert.NotContains(t, inlinePage, "Shift",
		"inline mode never captures the mouse, so there is nothing to bypass")
	assert.Contains(t, inlinePage, "dragging selects text")
}

// TestRouteViewsKeepTheAlternateScreen is the invariant the help panel leaked
// through: a full-area route that composes its frame with a raw tea.NewView leaves
// view.AltScreen false, so Bubble Tea takes that frame off the alternate screen and
// paints the panel into the terminal's scrollback, where it outlives the session.
// Every full-area route must compose through presentationView.
func TestRouteViewsKeepTheAlternateScreen(t *testing.T) {
	t.Parallel()

	// The overlay controller answers the status panel's runtime reads; the stub
	// controller does not.
	model := fullscreenModel(t, newOverlayController(readyState()), true)
	require.True(t, model.presentationView("").AltScreen, "the harness must be on the alternate screen")

	// The shortcut panel, opened by its key.
	_, _ = model.Update(key("ctrl+x"))
	require.Equal(t, routeHelp, model.route.kind)
	assert.True(t, model.routeView().AltScreen, "the help panel must not leave the alternate screen")

	_, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Equal(t, routeNone, model.route.kind)

	// The status panel, which the report compared against and which was already
	// correct.
	_, command := model.executeCommand(commandDescriptor{name: commandStatus})
	driveModelCommands(t, model, command)
	require.Equal(t, routeStatus, model.route.kind)
	assert.True(t, model.routeView().AltScreen, "the status panel must not leave the alternate screen")
}
