package tui

import (
	"time"

	"charm.land/bubbles/v2/textinput"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/runtimecontrol"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/rsbin1178/pips/internal/coding/subagent"

	tea "charm.land/bubbletea/v2"
)

type routeKind uint8

const (
	routeNone routeKind = iota
	routeSessions
	routeSkills
	routeAgents
	routeChild
	routeTeam
	routeTree
	routeToolDetail
)

const routeSubagent = routeChild

// routeState holds the full-area surface currently owning view and keyboard
// input. Each route uses only its relevant payload fields.
type routeState struct {
	kind                 routeKind
	generation           uint64
	cursor               int
	query                string
	offset               int
	loading              bool
	controlling          bool
	controllingChildKind childKind
	controllingChildSet  bool
	err                  error

	search              textinput.Model
	sessions            []session.Metadata
	sessionRecovery     map[string]runtimecontrol.TeamRecoveryHint
	skills              []coding.SkillSummary
	diagnostics         []coding.SkillDiagnostic
	agentLibrary        []coding.AgentLibraryEntry
	agentsTab           agentsRouteTab
	showDetails         bool
	previousInput       string
	previousComposer    composerSnapshot
	hasPreviousComposer bool
	openedAt            time.Time

	children []childSummary

	childKind      childKind
	childSummary   childSummary
	childSessionID string
	detail         *subagent.Detail
	childState     *coding.State
	workerBridge   *workerSubscriptionBridge
	refreshing     bool
	refreshPending bool
	refreshErr     error
	team           *teamRouteState

	tree       coding.SessionTree
	forkMode   bool
	toolDetail *toolDetailView
}

// routeOpenRequest is a typed route transition intent. Keeping the payload as
// data lets the presentation coordinator delay a full-area route until every
// already-issued native scrollback write has crossed Bubble Tea's renderer.
type routeOpenRequest struct {
	kind routeKind

	previousInput       string
	previousComposer    composerSnapshot
	hasPreviousComposer bool
	forkMode            bool
	childSessionID      string
	teamObjective       string
	teamView            *coding.TeamView
	teamIntegration     bool
	children            []childSummary
	child               childSummary
	query               string
	cursor              int
	toolDetail          *toolDetailView
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
	if !request.hasPreviousComposer {
		if m.route.hasPreviousComposer {
			request.previousComposer = m.route.previousComposer.clone()
		} else {
			request.previousComposer = m.composer.Snapshot()
		}
		request.previousInput = request.previousComposer.display
		request.hasPreviousComposer = true
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
	m.stopTeamWorkerRouteSubscription()

	switch request.kind {
	case routeSessions:
		return m.activateSessionPickerSnapshot(request.previousComposer)
	case routeSkills:
		return m.activateSkillsRouteSnapshot(request.previousComposer)
	case routeAgents:
		command := m.activateAgentsRoute()
		m.setRouteComposerSnapshot(request)

		return command
	case routeChild:
		command := m.activateChildRoute(request)
		m.setRouteComposerSnapshot(request)

		return command
	case routeTeam:
		if request.teamIntegration && request.teamView != nil {
			command := m.activateTeamIntegrationRoute(*request.teamView)
			m.setRouteComposerSnapshot(request)

			return command
		}

		command := m.activateTeamRoute(request.teamObjective)
		m.setRouteComposerSnapshot(request)

		return command
	case routeTree:
		command := m.activateTreeRoute(request.forkMode)
		m.setRouteComposerSnapshot(request)

		return command
	case routeToolDetail:
		if request.toolDetail == nil {
			return nil
		}

		detail := *request.toolDetail
		m.route = routeState{
			kind: routeToolDetail, toolDetail: &detail,
			previousInput:       request.previousInput,
			previousComposer:    request.previousComposer.clone(),
			hasPreviousComposer: request.hasPreviousComposer,
		}
		m.composer.Blur()

		return nil
	case routeNone:
		return nil
	default:
		return nil
	}
}

func (m *Model) setRouteComposerSnapshot(request routeOpenRequest) {
	if m.route.kind == routeNone || !request.hasPreviousComposer {
		return
	}

	m.route.previousInput = request.previousInput
	m.route.previousComposer = request.previousComposer.clone()
	m.route.hasPreviousComposer = true
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
	activityWasVisible := m.activityClockVisible()
	m.stopTeamWorkerRouteSubscription()
	previous := m.route.previousComposer
	hasPrevious := m.route.hasPreviousComposer
	m.route = routeState{}
	if hasPrevious {
		if err := m.composer.Restore(previous); err != nil {
			m.streamErr = err
		}
	}
	m.setLayout()

	return tea.Sequence(
		m.commitStableTimeline(),
		tea.Batch(m.composer.Focus(), m.startActivityClock(activityWasVisible)),
	)
}

func newChildRouteRequest(previous routeState, child childSummary) routeOpenRequest {
	return routeOpenRequest{
		kind: routeChild, childSessionID: child.childSessionID, child: child,
		children: append([]childSummary(nil), previous.children...),
		query:    previous.query, cursor: previous.cursor,
		previousInput:       previous.previousInput,
		previousComposer:    previous.previousComposer.clone(),
		hasPreviousComposer: previous.hasPreviousComposer,
	}
}

func (m *Model) updateRouteKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch m.route.kind {
	case routeSessions:
		return m.updateSessionPickerKey(message)
	case routeSkills:
		return m.updateSkillsRouteKey(message)
	case routeAgents:
		return m.updateAgentsRouteKey(message)
	case routeChild:
		return m.updateSubagentRouteKey(message)
	case routeTeam:
		return m.updateTeamRouteKey(message)
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
	case routeSkills:
		return m.skillsRouteView()
	case routeAgents:
		return m.agentsRouteView()
	case routeChild:
		return m.subagentRouteView()
	case routeTeam:
		return m.teamRouteView()
	case routeTree:
		return m.treeRouteView()
	case routeToolDetail:
		return m.toolDetailRouteView()
	default:
		return tea.NewView("")
	}
}
