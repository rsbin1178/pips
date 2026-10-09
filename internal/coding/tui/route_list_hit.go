package tui

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// paintedContentRows reports how many content lines a fitScrollableContentWindow
// frame paints: the whole content when it fits, or the window's content rows
// without the trailing "… lines x-y/z …" indicator, which is a chrome row rather
// than a content line and must never resolve to an item.
func paintedContentRows(content string, height int) int {
	rows := lipgloss.Height(content)
	if rows <= height {
		return rows
	}

	return max(1, height-1)
}

// routeListHit describes the list a full-area route painted in its last frame. A
// click is resolved against it rather than against re-derived layout, so a row
// that scrolled or reflowed since the frame was painted cannot be addressed by a
// stale coordinate. It mirrors the panel's statusPanelHits: the route records the
// rectangles while it composes the frame it is about to paint, and the pointer
// handler resolves against that record.
type routeListHit struct {
	// kind is the route that recorded the hit, so a map left behind by another
	// route cannot answer.
	kind    routeKind
	painted bool
	// columns is the frame width; a click outside it resolves nothing.
	columns int
	// frameTop is the frame row showing content line windowStart, and
	// windowStart/windowRows the content-line window the frame painted.
	frameTop    int
	windowStart int
	windowRows  int
	// firstLine is the content line where item `first` starts, heights are the
	// per-item painted row heights, and gap is the blank content lines between
	// consecutive items.
	firstLine int
	first     int
	heights   []int
	gap       int
}

// itemAt resolves a frame cell to the list item the painted frame put there. A
// click outside the frame width or the painted window resolves nothing, and so
// does a line past the last item; a line inside the blank gap between two items
// resolves to the item above it.
func (hit routeListHit) itemAt(row, column int) (int, bool) {
	if !hit.painted || len(hit.heights) == 0 ||
		column < 0 || column >= hit.columns ||
		row < hit.frameTop || row >= hit.frameTop+hit.windowRows {
		return 0, false
	}
	offset := hit.windowStart + (row - hit.frameTop) - hit.firstLine
	if offset < 0 {
		return 0, false
	}
	index := hit.first
	for index < len(hit.heights) {
		span := max(1, hit.heights[index]) + hit.gap
		if offset < span {
			return index, true
		}
		offset -= span
		index++
	}

	return 0, false
}

// routeTabHit is one addressable column range on a route's tab row.
type routeTabHit struct {
	start int
	end   int
	tab   agentsRouteTab
}

// routeTabHits records the tab row a full-area route painted in its last frame. A
// tab is a column span on one content line rather than a row window, so it is
// recorded separately from the list.
type routeTabHits struct {
	kind        routeKind
	painted     bool
	columns     int
	line        int
	windowStart int
	windowRows  int
	tabs        []routeTabHit
}

// tabAt resolves a frame cell to the tab painted there. It converts the frame row
// to a content line through the same window the item map uses, so a bar scrolled
// out of the route's window answers nothing.
func (hits routeTabHits) tabAt(row, column int) (agentsRouteTab, bool) {
	if !hits.painted || column < 0 || column >= hits.columns {
		return agentsTabRuns, false
	}
	line := hits.windowStart + row
	if line != hits.line || line < hits.windowStart || line >= hits.windowStart+hits.windowRows {
		return agentsTabRuns, false
	}
	for _, hit := range hits.tabs {
		if column >= hit.start && column < hit.end {
			return hit.tab, true
		}
	}

	return agentsTabRuns, false
}

// recordRouteListHit stores the list the current frame affords. The recorder
// stamps the route kind and the frame width, and clamps the painted window to the
// frame, so a band pushed off the bottom never answers.
func (m *Model) recordRouteListHit(hit routeListHit) {
	hit.kind = m.route.kind
	hit.columns = max(1, m.width)
	hit.painted = true
	hit.windowRows = min(hit.windowRows, max(0, m.height-hit.frameTop))
	m.route.listHits = hit
}

// routeListMouse resolves a pointer gesture on a full-area list route. The route
// owns the whole gesture — motion, drag and release are consumed so they never
// reach the transcript — while only a left press changes the selection. A click
// selects; it never activates, so no key action can fire from a stale pointer.
func (m *Model) routeListMouse(message tea.MouseMsg) (tea.Cmd, bool) {
	switch m.route.kind {
	case routeSessions, routeSkills, routeMCP, routeTree, routeAgents:
	default:
		return nil, false
	}

	click, ok := message.(tea.MouseClickMsg)
	if !ok || click.Button != tea.MouseLeft {
		return nil, true
	}

	if m.route.kind == routeAgents {
		if tab, ok := m.agentsRouteTabAt(click.Y, click.X); ok {
			m.selectAgentsRouteTab(tab)

			return redrawFrame(), true
		}
	}
	if index, ok := m.routeListRowAt(click.Y, click.X); ok {
		m.selectRouteListRow(index)

		return redrawFrame(), true
	}

	return nil, true
}

// routeListRowAt resolves a frame cell through the list map the current route
// painted. A map recorded by another route is stale and answers nothing.
func (m *Model) routeListRowAt(row, column int) (int, bool) {
	if m.route.listHits.kind != m.route.kind {
		return 0, false
	}

	return m.route.listHits.itemAt(row, column)
}

// agentsRouteTabAt resolves a frame cell through the tab map the current route
// painted.
func (m *Model) agentsRouteTabAt(row, column int) (agentsRouteTab, bool) {
	if m.route.tabHits.kind != m.route.kind {
		return agentsTabRuns, false
	}

	return m.route.tabHits.tabAt(row, column)
}

// selectRouteListRow moves the route's cursor to the clicked row of the same
// filtered list the renderer painted, clamped so a stale index cannot land out of
// range. The tree also re-derives its scroll offset, keeping the cursor invariant
// its own keys hold.
func (m *Model) selectRouteListRow(index int) {
	switch m.route.kind {
	case routeSessions:
		m.route.cursor = clampIndex(index, len(m.filteredSessionPickerValues()))
	case routeSkills:
		m.route.cursor = clampIndex(index, len(m.filteredSkillsRouteValues()))
	case routeMCP:
		m.route.cursor = clampIndex(index, len(m.filteredMCPRouteValues()))
	case routeTree:
		values := m.filteredTreeNodes()
		m.route.cursor = clampIndex(index, len(values))
		m.route.offset = m.treeRouteScrollOffset(values)
	case routeAgents:
		if m.route.agentsTab == agentsTabLibrary {
			m.route.cursor = clampIndex(index, len(m.filteredAgentLibrary()))
		} else {
			m.route.cursor = clampIndex(index, len(m.filteredChildren()))
		}
	}
}
