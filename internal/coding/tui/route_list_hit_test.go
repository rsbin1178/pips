//nolint:wsl_v5 // Hit-map walks and their bounds checks stay adjacent.
package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/rsbin1178/pips/internal/coding/subagent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRouteListHitWalksItemsAndGaps pins the row walk: a multi-line item owns
// every line of its span, a line inside the blank gap resolves to the item above,
// and a line past the last item resolves nothing.
func TestRouteListHitWalksItemsAndGaps(t *testing.T) {
	t.Parallel()

	hit := routeListHit{
		painted: true, columns: 80,
		frameTop: 2, windowStart: 0, windowRows: 6,
		firstLine: 0, first: 0, heights: []int{3, 2}, gap: 1,
	}

	for _, row := range []int{2, 3, 4} {
		index, ok := hit.itemAt(row, 0)
		require.True(t, ok, "row %d is inside the first item", row)
		assert.Equal(t, 0, index)
	}
	// Row 5 is the blank separator, which belongs to the item above it.
	index, ok := hit.itemAt(5, 0)
	require.True(t, ok, "the gap line resolves to the item above")
	assert.Equal(t, 0, index)
	for _, row := range []int{6, 7} {
		index, ok := hit.itemAt(row, 0)
		require.True(t, ok, "row %d is inside the second item", row)
		assert.Equal(t, 1, index)
	}
	_, ok = hit.itemAt(8, 0)
	assert.False(t, ok, "a row past the painted window resolves nothing")
}

// TestRouteListHitRejectsColumnsBeyondTheFrame pins the width guard: a coordinate
// outside the frame width can never address a row.
func TestRouteListHitRejectsColumnsBeyondTheFrame(t *testing.T) {
	t.Parallel()

	hit := routeListHit{
		painted: true, columns: 10,
		frameTop: 0, windowStart: 0, windowRows: 1,
		firstLine: 0, first: 0, heights: []int{1}, gap: 0,
	}

	_, ok := hit.itemAt(0, 10)
	assert.False(t, ok)
	_, ok = hit.itemAt(0, -1)
	assert.False(t, ok)
	index, ok := hit.itemAt(0, 9)
	require.True(t, ok)
	assert.Equal(t, 0, index)
}

// TestRouteListHitEmptyListResolvesNothing pins the empty branch: a route that
// painted a notice records no items, so a click resolves to no row.
func TestRouteListHitEmptyListResolvesNothing(t *testing.T) {
	t.Parallel()

	hit := routeListHit{
		painted: true, columns: 80,
		frameTop: 0, windowStart: 0, windowRows: 3,
		firstLine: 0, first: 0, gap: 1,
	}

	_, ok := hit.itemAt(0, 0)
	assert.False(t, ok)
}

// TestRouteListHitScrolledWindowOffsetsTheWalk pins the content window: the
// painted line maps through windowStart, and item firstLine offsets the walk so a
// preamble above the first item resolves to nothing.
func TestRouteListHitScrolledWindowOffsetsTheWalk(t *testing.T) {
	t.Parallel()

	hit := routeListHit{
		painted: true, columns: 80,
		frameTop: 0, windowStart: 1, windowRows: 3,
		firstLine: 0, first: 2, heights: []int{1, 1, 1, 1, 1, 1}, gap: 0,
	}
	for row, expected := range map[int]int{0: 3, 1: 4, 2: 5} {
		index, ok := hit.itemAt(row, 0)
		require.True(t, ok, "row %d is inside the painted window", row)
		assert.Equal(t, expected, index)
	}

	preamble := hit
	preamble.windowStart = 0
	preamble.windowRows = 6
	preamble.firstLine = 4
	_, ok := preamble.itemAt(1, 0)
	assert.False(t, ok, "a line above the first item resolves nothing")
	index, ok := preamble.itemAt(4, 0)
	require.True(t, ok)
	assert.Equal(t, 2, index)
}

// TestRouteListHitClampsToTheFrameBottom pins the recorder: a band pushed off the
// bottom of the frame never answers.
func TestRouteListHitClampsToTheFrameBottom(t *testing.T) {
	t.Parallel()

	model := readyModelWithController(t, newOverlayController(readyState()), true)
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	model.route.kind = routeSessions
	model.recordRouteListHit(routeListHit{
		frameTop: 8, windowStart: 0, windowRows: 5,
		firstLine: 0, first: 0, heights: []int{1, 1, 1, 1, 1}, gap: 0,
	})

	assert.Equal(t, 2, model.route.listHits.windowRows, "the band is clamped to the frame")
	_, ok := model.route.listHits.itemAt(10, 0)
	assert.False(t, ok)
	index, ok := model.route.listHits.itemAt(9, 0)
	require.True(t, ok)
	assert.Equal(t, 1, index)
}

// TestRouteListHitExcludesTheScrollIndicator pins the painted window: when the
// route's content overflows the frame, the trailing "… lines x-y/z …" indicator
// is not a content line, so a click on it resolves to no row.
func TestRouteListHitExcludesTheScrollIndicator(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	for index := range 6 {
		controller.agents = append(controller.agents, subagent.Summary{
			ChildSessionID: fmt.Sprintf("child-%d", index),
			State:          subagent.StateSucceeded,
			TaskPreview:    fmt.Sprintf("AGENT-%d", index),
		})
	}
	model := fullscreenModel(t, controller, true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 12})
	model.route = routeState{
		kind: routeAgents, generation: 1,
		children: childSummaries(controller.agents, nil),
	}
	content := ansi.Strip(model.View().Content)
	require.Contains(t, content, "… lines", "the fixture overflows the frame")

	hits := model.route.listHits
	require.True(t, hits.painted)
	// The indicator row is painted but is not a content line, so a click on it
	// resolves to no row and the window counts only the content rows above it.
	indicatorRow := -1
	for index, line := range strings.Split(content, "\n") {
		if strings.Contains(line, "… lines") {
			indicatorRow = index
		}
	}
	require.GreaterOrEqual(t, indicatorRow, 0, "the overflow paints an indicator:\n%s", content)
	_, ok := hits.itemAt(indicatorRow, 0)
	assert.False(t, ok, "the scroll indicator is not a row")
	assert.Equal(t, indicatorRow-hits.frameTop, hits.windowRows,
		"the painted window counts only the content rows")
	assert.NotContains(t, content, "AGENT-5", "the frame drops the rows below the window")
}

// TestTreeRouteHitExcludesTheBoundedNotice pins the tree's window: the trailing
// "… lines x-y/z …" indicator the fit window adds is not a node row.
func TestTreeRouteHitExcludesTheBoundedNotice(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	nodes := make([]coding.SessionNode, 0, 40)
	for index := range 40 {
		nodes = append(nodes, coding.SessionNode{
			ID:    fmt.Sprintf("node-%d", index),
			Kind:  coding.SessionNodeMessage,
			Label: fmt.Sprintf("NODE-%d", index),
		})
	}
	controller.tree = coding.SessionTree{Nodes: nodes, TotalNodes: 80, Truncated: true}
	model := fullscreenModel(t, controller, true)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 12})
	model.route = routeState{kind: routeTree, generation: 1, tree: controller.tree}
	content := ansi.Strip(model.View().Content)
	require.Contains(t, content, "… lines", "the fixture overflows the frame")

	hits := model.route.listHits
	require.True(t, hits.painted)
	indicatorRow := -1
	for index, line := range strings.Split(content, "\n") {
		if strings.Contains(line, "… lines") {
			indicatorRow = index
		}
	}
	require.GreaterOrEqual(t, indicatorRow, 0)
	_, ok := hits.itemAt(indicatorRow, 0)
	assert.False(t, ok, "the scroll indicator is not a node row")
	assert.Equal(t, indicatorRow-hits.frameTop, hits.windowRows,
		"the painted window counts only the content rows")
}

// TestRouteListHitStaleKindResolvesNothing pins the route guard: a map left behind
// by another route cannot answer a click on the current one.
func TestRouteListHitStaleKindResolvesNothing(t *testing.T) {
	t.Parallel()

	model := readyModelWithController(t, newOverlayController(readyState()), true)
	model.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	model.route.kind = routeSessions
	model.route.listHits = routeListHit{
		painted: true, kind: routeTree, columns: 40,
		frameTop: 0, windowStart: 0, windowRows: 1,
		firstLine: 0, first: 0, heights: []int{1}, gap: 0,
	}

	_, ok := model.routeListRowAt(0, 0)
	assert.False(t, ok)
}

// TestRouteTabHitResolvesTitlesAndScrolledOutBar pins the tab row: a column inside
// a title answers, a column between titles answers nothing, and a bar scrolled out
// of the window answers nothing.
func TestRouteTabHitResolvesTitlesAndScrolledOutBar(t *testing.T) {
	t.Parallel()

	hits := routeTabHits{
		painted: true, columns: 20,
		line: 1, windowStart: 0, windowRows: 3,
		tabs: []routeTabHit{{start: 0, end: 6, tab: agentsTabRuns}, {start: 8, end: 15, tab: agentsTabLibrary}},
	}

	tab, ok := hits.tabAt(1, 0)
	require.True(t, ok)
	assert.Equal(t, agentsTabRuns, tab)
	tab, ok = hits.tabAt(1, 9)
	require.True(t, ok)
	assert.Equal(t, agentsTabLibrary, tab)
	_, ok = hits.tabAt(1, 7)
	assert.False(t, ok, "the gap between titles answers nothing")
	_, ok = hits.tabAt(2, 0)
	assert.False(t, ok, "another content line is not the tab row")

	scrolled := hits
	scrolled.windowStart = 2
	scrolled.windowRows = 2
	_, ok = scrolled.tabAt(0, 0)
	assert.False(t, ok, "a bar scrolled out of the window answers nothing")
}

// TestRouteListMouseLeavesTheTeamRouteAlone pins the exclusion: the inline Team
// stage draws in the composer band rather than a full-area list, so the list
// dispatch must not answer for it and a click there changes nothing.
func TestRouteListMouseLeavesTheTeamRouteAlone(t *testing.T) {
	t.Parallel()

	model := fullscreenModel(t, newOverlayController(readyState()), true)
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model.route = routeState{kind: routeTeam, generation: 1, cursor: 1}
	model.route.listHits = routeListHit{
		painted: true, kind: routeTeam, columns: 80,
		frameTop: 3, windowStart: 0, windowRows: 2,
		firstLine: 0, first: 0, heights: []int{1, 1}, gap: 0,
	}

	_, handled := model.routeListMouse(tea.MouseClickMsg{Button: tea.MouseLeft, X: 1, Y: 4})
	assert.False(t, handled, "a Team route is not a list route")
	assert.Nil(t, model.handleMouse(tea.MouseClickMsg{Button: tea.MouseLeft, X: 1, Y: 4}))
	assert.Equal(t, 1, model.route.cursor, "a click on the Team stage moves no list cursor")
}

// TestRouteListMouseSelectsOnlyAndOwnsTheGesture pins the pointer contract: only a
// left press moves the cursor, motion and release are consumed without changing
// state, and a released capture resolves nothing.
func TestRouteListMouseSelectsOnlyAndOwnsTheGesture(t *testing.T) {
	t.Parallel()

	model := fullscreenModel(t, newOverlayController(readyState()), true)
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model.route.kind = routeSessions
	model.route.sessions = []session.Metadata{{ID: "s-1"}, {ID: "s-2"}}
	model.route.cursor = 0
	model.recordRouteListHit(routeListHit{
		frameTop: 3, windowStart: 0, windowRows: 2,
		firstLine: 0, first: 0, heights: []int{1, 1}, gap: 0,
	})

	motion := tea.MouseMotionMsg{Button: tea.MouseLeft, X: 1, Y: 4}
	assert.Nil(t, model.handleMouse(motion))
	assert.Equal(t, 0, model.route.cursor, "motion never moves the selection")

	release := tea.MouseReleaseMsg{Button: tea.MouseLeft, X: 1, Y: 4}
	assert.Nil(t, model.handleMouse(release))
	assert.Equal(t, 0, model.route.cursor)

	click := tea.MouseClickMsg{Button: tea.MouseLeft, X: 1, Y: 4}
	assert.NotNil(t, model.handleMouse(click), "a press repaints the frame it changed")
	assert.Equal(t, 1, model.route.cursor, "a press selects the painted row")

	model.route.cursor = 0
	model.mouseCaptureOff = true
	assert.Nil(t, model.handleMouse(click), "a released capture resolves nothing")
	assert.Equal(t, 0, model.route.cursor)
}
