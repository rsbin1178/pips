package tui

import (
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
