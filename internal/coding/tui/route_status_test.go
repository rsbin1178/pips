//nolint:wsl_v5 // Panel pages and their assertions stay adjacent.
package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// statusPageText renders one status panel page the way its rows read, so a test
// can assert the disclosed facts without composing a whole view.
func statusPageText(model *Model, tab statusPanelTab) string {
	lines := make([]string, 0, 64)
	for _, line := range model.statusPageLines(tab) {
		switch line.kind {
		case statusLineHeading:
			lines = append(lines, line.text)
		case statusLineField:
			lines = append(lines, line.label+": "+line.value)
		case statusLinePair:
			lines = append(lines,
				line.label+": "+line.value,
				line.label2+": "+line.value2,
			)
		case statusLineText:
			lines = append(lines, line.text)
		}
	}

	return strings.Join(lines, "\n")
}

func TestStatusPanelShowsTheRuntimeReport(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	model := readyModelWithController(t, controller, false)
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	_, command := model.executeCommand(commandDescriptor{name: commandStatus})
	require.NotNil(t, command)
	assert.Equal(t, routeStatus, model.route.kind)
	driveModelCommands(t, model, command)

	chrome, _, _ := model.statusRouteChrome(model.width)
	header := ansi.Strip(strings.Join(chrome, "\n"))
	for _, title := range statusPanelTabTitles {
		assert.Contains(t, header, title, "the tab bar names every page")
	}
	panel := statusPageText(model, statusTabStatus)
	assert.Contains(t, panel, "Workspace: /workspace")
	assert.Contains(t, panel, "Model: openai/test-model")
	assert.Contains(t, panel, "Permissions")
	assert.Contains(t, panel, "Sandbox: Workspace write")
	assert.NotContains(t, panel, "/status is available only while idle")

	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, routeNone, model.route.kind)
}

// TestStatusPanelSwitchesTabsAndSearchesSettings pins the panel's navigation: the
// tab keys move between pages, and the Config page's search filters its rows.
func TestStatusPanelSwitchesTabsAndSearchesSettings(t *testing.T) {
	t.Parallel()

	model := readyModelWithController(t, newOverlayController(readyState()), true)
	model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	_, command := model.executeCommand(commandDescriptor{name: commandStatus})
	driveModelCommands(t, model, command)
	require.Equal(t, statusTabStatus, model.route.statusTab)

	model.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	assert.Equal(t, statusTabConfig, model.route.statusTab)
	config := statusPageText(model, statusTabConfig)
	assert.Contains(t, config, "Sandbox: Workspace write")
	assert.Contains(t, config, "Status line: ")
	// NO_COLOR keeps the active page visible without styling.
	assert.Contains(t, model.statusRouteTabBar(80), "[Config]")

	model.Update(tea.KeyPressMsg{Text: "/"})
	require.True(t, model.statusSearching())
	model.Update(tea.KeyPressMsg{Text: "theme"})
	filtered := model.statusVisiblePageLines()
	labels := make([]string, 0, len(filtered))
	for _, line := range filtered {
		if line.kind == statusLineField {
			labels = append(labels, line.label)
		}
	}
	assert.Contains(t, labels, "Theme")
	assert.NotContains(t, labels, "Sandbox")

	model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	assert.False(t, model.statusSearching(), "Down returns to the list")
	model.Update(tea.KeyPressMsg{Text: "h"})
	assert.Equal(t, statusTabStatus, model.route.statusTab, "the back key leaves the Config page")
}

// TestStatusPanelStatsSummarisesTheSessionStore pins the Stats page: the grid and
// the numbers come from the Session store's headers, and the range key cycles.
func TestStatusPanelStatsSummarisesTheSessionStore(t *testing.T) {
	t.Parallel()

	now := time.Now()
	controller := newOverlayController(readyState())
	for day := range 4 {
		controller.sessions = append(controller.sessions, session.Metadata{
			ID:        fmt.Sprintf("s-%d", day),
			CreatedAt: now.AddDate(0, 0, -day),
		})
	}
	model := readyModelWithController(t, controller, true)
	model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	_, command := model.executeCommand(commandDescriptor{name: commandStatus})
	driveModelCommands(t, model, command)
	driveModelCommands(t, model, model.selectStatusTab(statusTabStats))

	page := statusPageText(model, statusTabStats)
	assert.Contains(t, page, "Activity")
	assert.Contains(t, page, "Less")
	assert.Contains(t, page, "All time")
	assert.Contains(t, page, "Sessions: 4")
	assert.Contains(t, page, "Current streak: 4 days")
	assert.Contains(t, page, "Active days: 4/4")

	model.Update(tea.KeyPressMsg{Text: "r"})
	assert.Equal(t, statusStatsLast30, model.route.statusRange)
	model.Update(tea.KeyPressMsg{Text: "r"})
	model.Update(tea.KeyPressMsg{Text: "r"})
	assert.Equal(t, statusStatsAllTime, model.route.statusRange)
}

// TestStatusPanelUsageReportsWhatPipsMeasured pins the Usage page: it states the
// last reported usage instead of inventing a lifetime total or a price.
func TestStatusPanelUsageReportsWhatPipsMeasured(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.ContextWindow = 1_000
	state.ContextTokens = 250
	state.Interaction = coding.InteractionState{
		ID: "interaction-1", Active: true,
		Usage: coding.TokenUsage{InputTokens: 400, OutputTokens: 20, CachedInputTokens: 300},
	}
	model := readyModelWithController(t, newOverlayController(state), true)
	model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	_, command := model.executeCommand(commandDescriptor{name: commandStatus})
	driveModelCommands(t, model, command)
	model.selectStatusTab(statusTabUsage)

	page := statusPageText(model, statusTabUsage)
	assert.Contains(t, page, "Context: 25% · 250 of 1,000 tokens")
	assert.Contains(t, page, "Cache: 75% of the last turn's prompt")
	assert.Contains(t, page, "400 input · 20 output · 300 cache read · 0 cache write")
	assert.NotContains(t, page, "$")
}

// TestStatusRouteScrollsWithKeysAndWheel pins the report window: the keys and the
// wheel move the same offset, and neither scrolls past the report.
func TestStatusRouteScrollsWithKeysAndWheel(t *testing.T) {
	t.Parallel()

	model := readyModelWithController(t, newOverlayController(readyState()), true)
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	_, command := model.executeCommand(commandDescriptor{name: commandStatus})
	driveModelCommands(t, model, command)
	require.Equal(t, routeStatus, model.route.kind)
	require.Greater(t, model.statusRouteMaximumOffset(), 0, "the report is taller than the window")

	model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	assert.Equal(t, wheelLinesDefault, model.route.offset)
	model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	assert.Zero(t, model.route.offset)

	model.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	assert.Equal(t, model.statusRouteMaximumOffset(), model.route.offset)
	model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	assert.Equal(t, model.statusRouteMaximumOffset(), model.route.offset, "the wheel clamps at the end")
	model.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	assert.Zero(t, model.route.offset)
}

// TestStatusRouteDefersTheRepositoryReadWhileRunning pins /status over a running
// turn: the runtime refuses a workspace read then, so the report says the summary
// waits instead of showing a busy failure.
func TestStatusRouteDefersTheRepositoryReadWhileRunning(t *testing.T) {
	t.Parallel()

	model := runningCommandModel(t)
	model.executeCommand(commandDescriptor{name: commandStatus})
	require.Equal(t, routeStatus, model.route.kind)

	assert.False(t, model.route.loading)
	assert.False(t, model.worktreeLoading)
	report := statusPageText(model, statusTabStatus)
	assert.Contains(t, report, "Repository: available once the current turn finishes")
	assert.NotContains(t, report, "unavailable")
	assert.NotContains(t, model.statusRouteFooterText(model.width), "r re-read")
}

// TestStatusPanelReadsTheStoreOnlyWhenAPageNeedsIt pins the lazy load: opening
// the panel reads no Session history, and the first page that reduces it starts
// the read and says so while it is in flight.
func TestStatusPanelReadsTheStoreOnlyWhenAPageNeedsIt(t *testing.T) {
	t.Parallel()

	model := readyModelWithController(t, newOverlayController(readyState()), true)
	model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	model.executeCommand(commandDescriptor{name: commandStatus})

	assert.False(t, model.route.statusDataRequested, "the Status page reads no history")
	assert.False(t, model.route.statusDataLoading)

	load := model.selectStatusTab(statusTabStats)
	require.NotNil(t, load, "the Stats page asks for the store")
	require.True(t, model.route.statusDataLoading)
	assert.Contains(t, statusPageText(model, statusTabStats), "Reading the session store…")

	model.selectStatusTab(statusTabUsage)
	assert.Contains(t, statusPageText(model, statusTabUsage), "Started: reading…")

	driveModelCommands(t, model, load)
	assert.False(t, model.route.statusDataLoading)
	assert.NotContains(t, statusPageText(model, statusTabStats), "Reading the session store…")
	assert.Nil(t, model.selectStatusTab(statusTabStatus), "switching back reads nothing")
	assert.Nil(t, model.selectStatusTab(statusTabStats), "a page that already asked does not read again")
}

// TestStatusPanelCountsUnreadableSessions pins the failure policy: a Session the
// listing cannot read is counted and reported instead of blanking the page.
func TestStatusPanelCountsUnreadableSessions(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.unreadableSessions = 2
	model := readyModelWithController(t, controller, true)
	model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	model.executeCommand(commandDescriptor{name: commandStatus})
	driveModelCommands(t, model, model.selectStatusTab(statusTabStats))

	assert.Contains(t,
		statusPageText(model, statusTabStats),
		"2 sessions could not be read and are not counted.",
	)
}

// TestStatusPanelClicksSwitchTabsRangeAndSearch pins the pointer map: a left press
// on a painted tab switches pages, a press on the Stats range row selects that
// window, a press on the Config search box takes the keyboard, and the panel never
// hands a gesture to the transcript.
func TestStatusPanelClicksSwitchTabsRangeAndSearch(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	now := time.Now()
	for day := range 3 {
		controller.sessions = append(controller.sessions, session.Metadata{
			ID: fmt.Sprintf("s-%d", day), CreatedAt: now.AddDate(0, 0, -day),
		})
	}
	model := fullscreenModel(t, controller, true)
	model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	require.True(t, model.mouseReportingEnabled(), "the panel answers the pointer in fullscreen")
	model.executeCommand(commandDescriptor{name: commandStatus})
	model.View()

	hits := model.route.statusHits
	require.True(t, hits.painted, "the frame records what it affords")
	require.Len(t, hits.tabs, int(statusPanelTabCount))

	// Click the Stats tab exactly where the painted bar drew it.
	command := model.handleMouse(tea.MouseClickMsg{
		X: hits.tabs[statusTabStats].start + 1, Y: hits.tabRow, Button: tea.MouseLeft,
	})
	require.Equal(t, statusTabStats, model.route.statusTab)
	driveModelCommands(t, model, command)
	model.View()

	hits = model.route.statusHits
	require.GreaterOrEqual(t, hits.rangeRow, 0, "the range row is on screen")
	require.Len(t, hits.windows, int(statusStatsRangeCount))
	model.handleMouse(tea.MouseClickMsg{
		X: hits.windows[statusStatsLast7].start + 1, Y: hits.rangeRow, Button: tea.MouseLeft,
	})
	assert.Equal(t, statusStatsLast7, model.route.statusRange)

	// The Config page's search box takes the keyboard from a click.
	model.selectStatusTab(statusTabConfig)
	model.View()
	hits = model.route.statusHits
	require.True(t, hits.hasSearch)
	model.handleMouse(tea.MouseClickMsg{X: 4, Y: hits.search.top, Button: tea.MouseLeft})
	assert.True(t, model.statusSearching())

	// The panel owns the pointer, so a drag across a page row selects nothing.
	model.handleMouse(tea.MouseClickMsg{X: 4, Y: hits.search.bottom + 1, Button: tea.MouseLeft})
	model.handleMouse(tea.MouseMotionMsg{X: 20, Y: hits.search.bottom + 1, Button: tea.MouseLeft})
	assert.False(t, model.selection.visible, "the transcript gesture stays with the transcript")
}
