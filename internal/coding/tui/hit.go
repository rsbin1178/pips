package tui

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// A click is resolved against the frame the user actually saw. readyView records
// the transcript band of every composed frame, and the renderer dispatches clicks
// through the handler of the last painted view, so a row that scrolled, grew or
// reflowed since then cannot be addressed by a stale coordinate.

// frameHitMap describes where the transcript band sat in the last painted frame.
type frameHitMap struct {
	// painted is false until a ready frame has been composed, which keeps a click
	// that arrives before the first paint from resolving against nothing.
	painted bool
	// transcript is the number of visible transcript rows at the top of the frame.
	transcript int
	// topDropped is how many transcript rows the frame clamp removed from the top.
	topDropped int
	// band is the frame row the reserved band was painted at, or -1 when the
	// frame drew none. A left click there restores the newest transcript rows,
	// which is what the arrow the band draws promises.
	band int
	// turnIndicator is true when this frame painted the up indicator in its first
	// row, so a click there can be answered without re-deciding the question
	// against a scroll position the reader has already left.
	turnIndicator bool
}

// frameRowOf reports the frame row the first line of the band at index was
// painted at, or -1 when the height clamp pushed it off the frame. parts is the
// composed frame before the clamp, so the row is the height above the band minus
// the rows the clamp removed from the top.
//
// An empty prefix is zero rows, not one: lipgloss answers Height("") with 1, so
// the empty join has to be special-cased or a band at the top of the frame would
// record the row below it.
func frameRowOf(parts []string, index, dropped int) int {
	if index < 0 || index >= len(parts) {
		return -1
	}

	row := 0
	if index > 0 {
		row = lipgloss.Height(lipgloss.JoinVertical(lipgloss.Left, parts[:index]...))
	}

	row -= dropped
	if row < 0 {
		return -1
	}

	return row
}

// bandRowOf reports the frame row the reserved band afforded a click at, or -1
// when the frame drew no arrow there. affords is the answer the frame was painted
// with rather than a fresh one: the height this frame settles on decides whether
// the newest rows are off screen, so asking again after the transcript window has
// measured itself could record a target the frame does not show.
func (m *Model) bandRowOf(parts []string, index, dropped int, affords bool) int {
	if !affords {
		return -1
	}

	return frameRowOf(parts, index, dropped)
}

// bandMouse answers a left click on the reserved band by returning to the newest
// rows, which is the mouse form of End. It requires the ready frame to be the one
// on screen, because the recorded row belongs to that frame; a frame that drew no
// arrow affords nothing.
func (m *Model) bandMouse(message tea.MouseMsg) (tea.Cmd, bool) {
	if !m.readyViewOwnsPointer() || m.frameHit.band < 0 {
		return nil, false
	}

	click, ok := message.(tea.MouseClickMsg)
	if !ok || click.Button != tea.MouseLeft || click.Y != m.frameHit.band {
		return nil, false
	}

	m.transcriptScroll.gotoBottom()

	return redrawFrame(), true
}

// handleMouse is installed on every frame's View while mouse reporting is on. It
// resolves a pointer event against what the last painted frame afforded: the panel
// answers clicks on its tabs, search box and range row, and the transcript answers
// the selection gesture.
func (m *Model) handleMouse(message tea.MouseMsg) tea.Cmd {
	if !m.mouseReportingEnabled() {
		return nil
	}

	// A full-area panel owns the pointer while it is open, and it only affords the
	// controls it painted.
	if m.route.kind == routeStatus {
		return m.statusRouteMouse(message)
	}

	// A full-area list route owns the whole pointer gesture: it consumes motion,
	// drag and release so they never reach the transcript, and only a left press
	// moves its cursor.
	if command, handled := m.routeListMouse(message); handled {
		return command
	}

	if !m.frameHit.painted {
		return nil
	}

	// The turn indicator and the reserved band are pointer targets of their own and
	// need no hit test: the frame that painted each one recorded where. They are
	// answered before the selection gate because neither is a selection gesture —
	// the gesture that owns the transcript would otherwise drop a click on them.
	if command, handled := m.turnIndicatorMouse(message); handled {
		return command
	}

	if command, handled := m.bandMouse(message); handled {
		return command
	}

	// An overlay owns the keyboard, so it owns the pointer: a click that cannot be
	// aimed at a visible entry is dropped rather than forwarded to the transcript.
	if !m.viewportOwnsPointer() {
		return nil
	}

	// The find box is a reading surface: a click would open a route that hides the
	// box behind it, so entry activation waits until the box is closed.
	if m.search.active {
		return nil
	}

	// Press, drag and release are one gesture: the pointer selects transcript text,
	// and a press and release on the same cell activates the entry under it.
	return m.handleSelectionMouse(message)
}

// entryRowAt converts a frame row into a transcript row, reporting false for a row
// that belongs to a control band or to no band at all.
func (m *Model) entryRowAt(frameRow int) (int, bool) {
	if frameRow < 0 || frameRow >= m.frameHit.transcript {
		return 0, false
	}

	return m.transcriptScroll.offset + m.frameHit.topDropped + frameRow, true
}

// activateEntry runs the interaction one transcript entry affords. A Tool entry
// opens its own detail, or the child session behind a subagent call: the surfaces
// Ctrl+T opens for the newest entry, addressed by position instead.
func (m *Model) activateEntry(row int) tea.Cmd {
	block, ok := m.transcript.toolAt(row)
	if !ok {
		return nil
	}

	if len(block.tools) > 0 && block.tools[0].childSessionID != "" {
		return m.openSubagentRoute(block.tools[0].childSessionID)
	}

	return m.openToolDetailRoute(newToolDetailView(block))
}
