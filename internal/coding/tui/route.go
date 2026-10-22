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
	routeHelp
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
	// watchFollowPending marks a re-read of the open watch page that has not
	// answered yet, so the activity clock cannot queue a second one.
	watchFollowPending bool

	statusTab           statusPanelTab
	helpTab             helpTab
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
	// queuedBytes is the rendered content waiting for the terminal. It is the
	// sum of every queued write's content, and gates stable projection commits
	// once it reaches scrollbackQueueBytes.
	queuedBytes int
	// pendingSources hold content that did not fit the queue, oldest first.
	// They are ordered before anything enqueued later, so new output must wait
	// behind them rather than overtake them. Each source renders its own pages
	// on demand, so the deferred content is never a second rendered copy.
	pendingSources []scrollbackSource
	pendingRoute   routeOpenRequest
	// heldInspections carries inspection output that arrived while a full-area
	// route owned the screen. The route returns it to the parent once its own
	// deferred conversation output has been committed.
	heldInspections []string
}

func (p *presentationState) hasScrollbackWrites() bool {
	return len(p.writes) > 0
}

func (p *presentationState) hasPendingScrollback() bool {
	return len(p.pendingSources) > 0
}

// queueFull reports whether the rendered queue has reached its byte cap.
func (p *presentationState) queueFull() bool {
	return p.queuedBytes >= scrollbackQueueBytes
}

// backpressured reports whether rendered output must wait for the terminal
// before the stable projection advances again.
func (p *presentationState) backpressured() bool {
	return p.queueFull() || p.hasPendingScrollback()
}

func (m *Model) enqueueScrollback(content string) tea.Cmd {
	var commands []tea.Cmd

	for index, page := range splitScrollbackPages(content, scrollbackPageBytes) {
		if command := m.enqueueScrollbackPage(page, index > 0); command != nil {
			commands = append(commands, command)
		}
	}

	return tea.Batch(commands...)
}

// enqueueScrollbackPage adds one already bounded native write. The terminal's
// newline after the previous write supplies the line break between pages, so
// only a new logical block needs a leading gap.
func (m *Model) enqueueScrollbackPage(content string, continuation bool) tea.Cmd {
	if content == "" {
		return nil
	}

	if m.scrollbackOutput && !continuation {
		content = "\n" + content
	}

	m.presentation.writeSequence++
	m.presentation.writes = append(m.presentation.writes, nativeWrite{
		sequence: m.presentation.writeSequence, content: content,
	})
	m.presentation.queuedBytes += len(content)
	m.scrollbackOutput = true
	if len(m.presentation.writes) != 1 {
		return nil
	}

	return m.dispatchScrollback()
}

// pumpPendingScrollback moves deferred pages into the write queue while it is
// below the byte cap. The first page it enqueues into an empty queue owns the
// dispatch command; later pages only extend the queue.
func (m *Model) pumpPendingScrollback() tea.Cmd {
	var dispatch tea.Cmd

	for !m.presentation.queueFull() {
		if len(m.presentation.pendingSources) == 0 {
			break
		}

		source := m.presentation.pendingSources[0]
		write, ok := source.nextPage(m, scrollbackPageBytes)
		if !ok {
			// Release the exhausted source: its backing array would otherwise
			// keep the content it paged alive behind the new slice header.
			m.presentation.pendingSources[0] = nil
			m.presentation.pendingSources = m.presentation.pendingSources[1:]
			if len(m.presentation.pendingSources) == 0 {
				m.presentation.pendingSources = nil
			}

			continue
		}

		if command := m.enqueueScrollbackPage(write.content, write.continuation); command != nil &&
			dispatch == nil {
			dispatch = command
		}
	}

	return dispatch
}

// dropPendingScrollback releases deferred content that belongs to a projection
// being replaced. Accepted writes stay: they are already on their way to the
// terminal and cannot be taken back.
func (m *Model) dropPendingScrollback() {
	m.presentation.pendingSources = nil
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
	// The route owns the keyboard from the request onward, including the window
	// where a native scrollback write delays activation.
	m.tasksPane.focused = false
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
	// A full-area route owns the keyboard. Leaving tasks-pane focus behind would
	// make the pane reclaim keys when the route closes, including after the work
	// that opened it has already stopped.
	m.tasksPane.focused = false

	switch request.kind {
	case routeSessions:
		return m.activateSessionPickerSnapshot(request.previousComposer)
	case routeSkills:
		return m.activateSkillsRouteSnapshot(request.previousComposer)
	case routeMCP:
		return m.activateMCPRouteSnapshot(request.previousComposer)
	case routeStatus:
		return m.activateStatusRoute(request.previousComposer)
	case routeHelp:
		return m.activateHelpRoute(request.previousComposer)
	case routeAgents, routeChild, routeTeam, routeTree:
		return m.activateSnapshotRoute(request)
	case routeToolDetail:
		return m.activateToolDetailRoute(request)
	case routeNone:
		return nil
	default:
		return nil
	}
}

// activateSnapshotRoute opens a route that carries the Composer's draft as its own
// snapshot, so closing it hands the draft back untouched.
func (m *Model) activateSnapshotRoute(request routeOpenRequest) tea.Cmd {
	var command tea.Cmd

	switch request.kind {
	case routeAgents:
		command = m.activateAgentsRoute()
	case routeChild:
		command = m.activateChildRoute(request)
	case routeTeam:
		if request.teamIntegration && request.teamView != nil {
			command = m.activateTeamIntegrationRoute(*request.teamView)

			break
		}

		command = m.activateTeamRoute(request.teamObjective)
	case routeTree:
		command = m.activateTreeRoute(request.forkMode)
	default:
		return nil
	}

	m.setRouteComposerSnapshot(request)

	return command
}

// activateToolDetailRoute opens the Ctrl+T detail panel, which owns the screen
// without taking the Composer's draft. A page that shows a running watch starts
// the activity clock, because that clock is what re-reads its output and keeps
// its spinner moving.
func (m *Model) activateToolDetailRoute(request routeOpenRequest) tea.Cmd {
	if request.toolDetail == nil {
		return nil
	}

	activityWasVisible := m.activityClockVisible()
	detail := *request.toolDetail
	m.route = routeState{
		kind: routeToolDetail, toolDetail: &detail,
		previousInput:       request.previousInput,
		previousComposer:    request.previousComposer.clone(),
		hasPreviousComposer: request.hasPreviousComposer,
	}
	m.composer.Blur()
	// A running watch opens on the tail of its output, which is where a reader
	// who just asked to watch the task is looking. Scrolling up releases the
	// tail; scrolling back to it follows again.
	if detail.watchRunning {
		m.route.offset = m.toolDetailMaximumOffset()
	}

	return m.startActivityClock(activityWasVisible)
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
	m.presentation.queuedBytes = max(0, m.presentation.queuedBytes-len(m.presentation.writes[0].content))
	m.presentation.writes[0] = nativeWrite{}

	m.presentation.writes = m.presentation.writes[1:]

	// The queue has room again: move deferred pages and the stable projection
	// that backpressure held back. Pages enqueued into an empty queue own the
	// dispatch command; an existing head is dispatched here.
	pump := m.pumpPendingScrollback()
	dispatch := pump
	if m.presentation.hasScrollbackWrites() && dispatch == nil {
		dispatch = m.dispatchScrollback()
	}
	catchUp := m.commitStableTimeline()

	if m.presentation.hasScrollbackWrites() {
		return tea.Batch(dispatch, catchUp, tea.Batch(after...))
	}

	m.presentation.writes = nil
	if m.presentation.hasPendingScrollback() || !m.presentation.pendingRoute.pending() {
		return tea.Batch(dispatch, catchUp, tea.Batch(after...))
	}

	request := m.presentation.pendingRoute
	m.presentation.pendingRoute = routeOpenRequest{}

	return tea.Batch(dispatch, catchUp, m.activateRoute(request), tea.Batch(after...))
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

	m.syncTasksPane(time.Now())
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
	case routeHelp:
		return m.updateHelpRouteKey(message)
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
	case routeHelp:
		return m.helpRouteView()
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
		// An unrecognized route still draws an empty frame through the
		// presentation boundary, so it cannot leave the alternate screen by
		// omission either.
		return m.presentationView("")
	}
}
