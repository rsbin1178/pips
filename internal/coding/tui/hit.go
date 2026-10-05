package tui

import (
	tea "charm.land/bubbletea/v2"
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
}

// handleMouse is installed on the ready frame's View while mouse reporting is on.
// It is the only place a pointer can act on an entry, and it acts on exactly one
// surface: the transcript.
func (m *Model) handleMouse(message tea.MouseMsg) tea.Cmd {
	if !m.frameHit.painted || !m.mouseReportingEnabled() {
		return nil
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
