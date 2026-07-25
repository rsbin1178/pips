package tui

import (
	"slices"
	"time"

	"charm.land/bubbles/v2/textinput"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/session"
	"github.com/rsbin/pips/internal/coding/subagent"

	tea "charm.land/bubbletea/v2"
)

type routeKind uint8

const (
	routeNone routeKind = iota
	routeSessions
	routeAgents
	routeSubagent
	routeTree
	routeToolDetail
)

// routeState holds the full-area surface currently owning view and keyboard
// input. Each route uses only its relevant payload fields.
type routeState struct {
	kind        routeKind
	generation  uint64
	cursor      int
	query       string
	offset      int
	loading     bool
	controlling bool
	err         error

	search        textinput.Model
	sessions      []session.Metadata
	previousInput string
	openedAt      time.Time

	agents []subagent.Summary

	childSessionID string
	detail         *subagent.Detail
	childState     *coding.State
	refreshing     bool
	refreshPending bool
	refreshErr     error

	tree       coding.SessionTree
	forkMode   bool
	toolDetail *toolDetailView
}

// routeOpenRequest is a typed route transition intent. Keeping the payload as
// data lets the presentation coordinator delay a full-area route until every
// already-issued native scrollback write has crossed Bubble Tea's renderer.
type routeOpenRequest struct {
	kind routeKind

	previousInput  string
	forkMode       bool
	childSessionID string
	agents         []subagent.Summary
	query          string
	cursor         int
	toolDetail     *toolDetailView
}

func (r routeOpenRequest) pending() bool {
	return r.kind != routeNone
}

type presentationState struct {
	writeSequence uint64
	writes        map[uint64]struct{}
	pendingRoute  routeOpenRequest
}

func (p *presentationState) beginScrollbackWrite() uint64 {
	p.writeSequence++
	if p.writes == nil {
		p.writes = make(map[uint64]struct{})
	}

	p.writes[p.writeSequence] = struct{}{}

	return p.writeSequence
}

func (p *presentationState) finishScrollbackWrite(sequence uint64) bool {
	delete(p.writes, sequence)

	return len(p.writes) == 0
}

func (p *presentationState) hasScrollbackWrites() bool {
	return len(p.writes) > 0
}

// requestRouteOpen transfers full-area presentation ownership only after the
// parent's stable output is terminal-native. Parent events may keep reducing
// State while a transition waits, but commitStableTimeline will not advance
// its projection cursor again until the route returns.
func (m *Model) requestRouteOpen(request routeOpenRequest) tea.Cmd {
	if !request.pending() {
		return nil
	}

	if m.route.kind != routeNone {
		return m.activateRoute(request)
	}

	var commit tea.Cmd
	if !m.presentation.hasScrollbackWrites() {
		commit = m.commitStableTimeline()
	}

	m.presentation.pendingRoute = request
	if m.presentation.hasScrollbackWrites() {
		return commit
	}

	m.presentation.pendingRoute = routeOpenRequest{}

	return m.activateRoute(request)
}

func (m *Model) activateRoute(request routeOpenRequest) tea.Cmd {
	switch request.kind {
	case routeSessions:
		return m.activateSessionPicker(request.previousInput)
	case routeAgents:
		return m.activateAgentsRoute()
	case routeSubagent:
		return m.activateSubagentRoute(request)
	case routeTree:
		return m.activateTreeRoute(request.forkMode)
	case routeToolDetail:
		if request.toolDetail == nil {
			return nil
		}

		detail := *request.toolDetail
		m.route = routeState{kind: routeToolDetail, toolDetail: &detail}
		m.composer.Blur()

		return nil
	case routeNone:
		return nil
	default:
		return nil
	}
}

func (m *Model) finishScrollbackWrite(sequence uint64) tea.Cmd {
	if !m.presentation.finishScrollbackWrite(sequence) ||
		!m.presentation.pendingRoute.pending() {
		return nil
	}

	request := m.presentation.pendingRoute
	m.presentation.pendingRoute = routeOpenRequest{}

	return m.activateRoute(request)
}

// closeRouteToParent restores the parent as presentation owner before it
// catches up the stable projection accumulated while the child route was
// visible. The returned sequence keeps the Composer focus restoration behind
// the native scrollback insertion.
func (m *Model) closeRouteToParent() tea.Cmd {
	m.route = routeState{}
	m.setLayout()

	return tea.Sequence(m.commitStableTimeline(), m.composer.Focus())
}

func newSubagentRouteRequest(previous routeState, childSessionID string) routeOpenRequest {
	return routeOpenRequest{
		kind: routeSubagent, childSessionID: childSessionID,
		agents: slices.Clone(previous.agents), query: previous.query, cursor: previous.cursor,
	}
}

func (m *Model) updateRouteKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch m.route.kind {
	case routeSessions:
		return m.updateSessionPickerKey(message)
	case routeAgents:
		return m.updateAgentsRouteKey(message)
	case routeSubagent:
		return m.updateSubagentRouteKey(message)
	case routeTree:
		return m.updateTreeRouteKey(message)
	case routeToolDetail:
		return m.updateToolDetailRouteKey(message)
	case routeNone:
		return m, nil
	default:
		return m, nil
	}
}

func (m *Model) routeView() tea.View {
	switch m.route.kind {
	case routeSessions:
		return m.sessionPickerView()
	case routeAgents:
		return m.agentsRouteView()
	case routeSubagent:
		return m.subagentRouteView()
	case routeTree:
		return m.treeRouteView()
	case routeToolDetail:
		return m.toolDetailRouteView()
	default:
		return tea.NewView("")
	}
}
