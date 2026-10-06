package tui

import (
	"context"
	"errors"
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
	routeMCP
	routeStatus
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
	mcp                 coding.MCPSnapshot
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

	statusTab           statusPanelTab
	statusRange         statusStatsRange
	statusSessions      session.MetadataListing
	statusProjections   map[string]usageProjectionSnapshot
	statusMCP           coding.MCPSnapshot
	statusDataRequested bool
	statusDataLoading   bool
	statusDataErr       error
	// statusCancel stops the panel's in-flight Session-store read when the route
	// closes or starts a replacement read, so a pass nobody reads stops early.
	statusCancel context.CancelFunc
	statusHits   statusPanelHits
	// listHits and tabHits describe the rows and tab row the last painted frame
	// afforded, so a click selects against what the reader saw.
	listHits routeListHit
	tabHits  routeTabHits
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

// errRouteActionNeedsIdle explains a reading surface that stays open while a turn
// runs but whose action would change the runtime that turn owns.
var errRouteActionNeedsIdle = errors.New("available once the current turn finishes")

// nativeWrite is one accepted logical payload. Only the head is dispatched.
// The output sequence and queue deliberately survive session projection resets.
type nativeWrite struct {
	sequence uint64
	content  string
	after    []tea.Cmd
}

type presentationState struct {
	writeSequence uint64
	writes        []nativeWrite
	pendingRoute  routeOpenRequest
	// heldInspections carries inspection output that arrived while a full-area
	// route owned the screen. The route returns it to the parent once its own
	// deferred conversation output has been committed.
	heldInspections []string
}

func (p *presentationState) hasScrollbackWrites() bool {
	return len(p.writes) > 0
}

func (m *Model) enqueueScrollback(content string) tea.Cmd {
	m.presentation.writeSequence++

	m.presentation.writes = append(m.presentation.writes, nativeWrite{
		sequence: m.presentation.writeSequence, content: content,
	})
	if len(m.presentation.writes) != 1 {
		return nil
	}

	return m.dispatchScrollback()
}

func (m *Model) dispatchScrollback() tea.Cmd {
	write := m.presentation.writes[0]

	return tea.Sequence(tea.Println(write.content), func() tea.Msg {
		return scrollbackWriteDoneMsg{sequence: write.sequence}
	})
}

// afterScrollback attaches continuation commands to the last accepted write,
// not to its dispatch command (which is nil while another write is in flight).
// Commands capture their inputs now; none read mutable Model state off-loop.
func (m *Model) afterScrollback(dispatch tea.Cmd, after ...tea.Cmd) tea.Cmd {
	if !m.presentation.hasScrollbackWrites() {
		return tea.Sequence(dispatch, tea.Batch(after...))
	}

	last := len(m.presentation.writes) - 1
	m.presentation.writes[last].after = append(m.presentation.writes[last].after, after...)

	return dispatch
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
	case routeMCP:
		return m.activateMCPRouteSnapshot(request.previousComposer)
	case routeStatus:
		return m.activateStatusRoute(request.previousComposer)
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
	if !m.presentation.hasScrollbackWrites() || m.presentation.writes[0].sequence != sequence {
		return nil
	}

	after := m.presentation.writes[0].after
	m.presentation.writes[0] = nativeWrite{}

	m.presentation.writes = m.presentation.writes[1:]
	if m.presentation.hasScrollbackWrites() {
		return tea.Batch(m.dispatchScrollback(), tea.Batch(after...))
	}

	m.presentation.writes = nil
	if !m.presentation.pendingRoute.pending() {
		return tea.Batch(after...)
	}

	request := m.presentation.pendingRoute
	m.presentation.pendingRoute = routeOpenRequest{}

	return tea.Batch(m.activateRoute(request), tea.Batch(after...))
}

// closeRouteToParent restores the parent as presentation owner before it
// catches up the stable projection accumulated while the child route was
// visible. The returned sequence keeps the Composer focus restoration behind
// the native scrollback insertion.
func (m *Model) closeRouteToParent() tea.Cmd {
	activityWasVisible := m.activityClockVisible()
	m.stopTeamWorkerRouteSubscription()

	// Closing the panel stops its Session-store read before the route that owns
	// the cancel is reset, so a pass nobody will read does not run to the end.
	if m.route.statusCancel != nil {
		m.route.statusCancel()
	}
	previous := m.route.previousComposer
	hasPrevious := m.route.hasPreviousComposer
	m.route = routeState{}
	if hasPrevious {
		if err := m.composer.Restore(previous); err != nil {
			m.streamErr = err
		}
	}
	m.setLayout()

	// Held inspection output follows the catch-up, so the conversation keeps its
	// own blocks adjacent instead of splitting them with an inspection report. The
	// returned command is whichever of the two queued the first native write.
	commit := m.commitStableTimeline()
	flush := m.flushHeldInspections()
	dispatch := commit
	if dispatch == nil {
		dispatch = flush
	}

	return m.afterScrollback(
		dispatch,
		tea.Batch(m.composer.Focus(), m.startActivityClock(activityWasVisible)),
	)
}

// holdInspection keeps inspection output until the parent owns presentation
// again, and reports whether it was held.
func (m *Model) holdInspection(content string) bool {
	if m.route.kind == routeNone && !m.presentation.pendingRoute.pending() {
		return false
	}

	m.presentation.heldInspections = append(m.presentation.heldInspections, content)

	return true
}

// flushHeldInspections prints inspection output that a route held back. It runs
// behind the route's own catch-up command, so it enqueues its own native writes.
func (m *Model) flushHeldInspections() tea.Cmd {
	held := m.presentation.heldInspections
	m.presentation.heldInspections = nil
	if len(held) == 0 {
		return nil
	}

	commands := make([]tea.Cmd, 0, len(held))
	for _, content := range held {
		if command := m.printScrollback(content); command != nil {
			commands = append(commands, command)
		}
	}
	if len(commands) == 0 {
		return nil
	}

	return tea.Batch(commands...)
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
	case routeMCP:
		return m.updateMCPRouteKey(message)
	case routeStatus:
		return m.updateStatusRouteKey(message)
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

// routeUsesSearch reports whether the active route owns the shared search
// box, so paste, width, and style updates reach it.
func (m *Model) routeUsesSearch() bool {
	switch m.route.kind {
	case routeSessions, routeSkills, routeMCP, routeStatus:
		return true
	default:
		return false
	}
}

func (m *Model) routeView() tea.View {
	switch m.route.kind {
	case routeSessions:
		return m.sessionPickerView()
	case routeSkills:
		return m.skillsRouteView()
	case routeMCP:
		return m.mcpRouteView()
	case routeStatus:
		return m.statusRouteView()
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
