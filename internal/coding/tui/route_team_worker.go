//nolint:wsl_v5 // Exact Worker route ownership keeps generation checks and cleanup adjacent.
package tui

import (
	"context"
	"errors"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin/pips/internal/coding"
)

type workerSubscriptionBridge struct {
	events <-chan coding.EventRecord
	err    func() error
	close  func()
	once   sync.Once
}

type teamWorkerRouteDataMsg struct {
	generation uint64
	target     coding.TeamWorkerTarget
	state      coding.State
	hasState   bool
	bridge     *workerSubscriptionBridge
	background bool
	err        error
}

type teamWorkerRouteEventMsg struct {
	generation uint64
	target     coding.TeamWorkerTarget
	bridge     *workerSubscriptionBridge
	record     coding.EventRecord
	err        error
	ok         bool
}

type teamWorkerControlResultMsg struct {
	generation uint64
	target     coding.TeamWorkerTarget
	err        error
}

func newWorkerSubscriptionBridge(
	subscription *coding.EventSubscription,
) *workerSubscriptionBridge {
	if subscription == nil {
		return nil
	}

	return &workerSubscriptionBridge{
		events: subscription.Events(), err: subscription.Err, close: subscription.Close,
	}
}

func (b *workerSubscriptionBridge) wait(
	generation uint64,
	target coding.TeamWorkerTarget,
) tea.Cmd {
	if b == nil {
		return nil
	}

	return func() tea.Msg {
		record, ok := <-b.events
		var err error
		if !ok && b.err != nil {
			err = b.err()
		}

		return teamWorkerRouteEventMsg{
			generation: generation, target: target, bridge: b,
			record: record, err: err, ok: ok,
		}
	}
}

func (b *workerSubscriptionBridge) stop() {
	if b == nil {
		return
	}

	b.once.Do(func() {
		if b.close != nil {
			b.close()
		}
	})
}

func (m *Model) stopTeamWorkerRouteSubscription() {
	if m == nil || m.route.workerBridge == nil {
		return
	}

	m.route.workerBridge.stop()
	m.route.workerBridge = nil
}

func (m *Model) activateTeamWorkerRoute(request routeOpenRequest) tea.Cmd {
	m.routeSeq++
	m.route = routeState{
		kind: routeChild, loading: true, generation: m.routeSeq,
		childKind: childTeamWorker, childSummary: request.child,
		childSessionID: request.child.childSessionID, children: request.children,
		query: request.query, cursor: request.cursor,
	}
	m.composer.Blur()

	return m.teamWorkerRouteLoadCommand(false)
}

func (m *Model) teamWorkerRouteLoadCommand(background bool) tea.Cmd {
	if m.route.kind != routeChild || m.route.childKind != childTeamWorker {
		return nil
	}

	generation := m.route.generation
	child := m.route.childSummary
	target := child.worker.Target
	controller := m.controller
	ctx := m.ctx

	return func() tea.Msg {
		if child.live() {
			observation, observeErr := controller.ObserveTeamWorker(ctx, target)
			if observeErr == nil && observation.Subscription != nil &&
				observation.State.SessionID != "" {
				return teamWorkerRouteDataMsg{
					generation: generation, target: target,
					state: observation.State, hasState: true,
					bridge:     newWorkerSubscriptionBridge(observation.Subscription),
					background: background,
				}
			}
		}

		state, err := controller.InspectTeamWorkerState(ctx, target)

		return teamWorkerRouteDataMsg{
			generation: generation, target: target,
			state: state, hasState: err == nil, background: background, err: err,
		}
	}
}

func (m *Model) applyTeamWorkerRouteData(message teamWorkerRouteDataMsg) (tea.Model, tea.Cmd) {
	if !m.acceptTeamWorkerRouteMessage(message.generation, message.target) {
		message.bridge.stop()

		return m, nil
	}
	if message.hasState && message.state.SessionID != m.route.childSessionID {
		message.bridge.stop()
		message.bridge = nil
		message.hasState = false
		message.err = errors.New("team worker snapshot identity changed")
	}

	wasAtBottom := m.route.offset >= m.subagentRouteMaximumOffset()
	previousOffset := m.route.offset
	if message.background {
		m.route.refreshing = false
	} else {
		m.route.loading = false
	}
	switch {
	case message.hasState:
		state := message.state.Clone()
		m.route.childState = &state
		m.route.err = nil
		m.route.refreshErr = nil
	case message.background && m.route.childState != nil:
		m.route.refreshErr = message.err
	default:
		m.route.err = message.err
	}

	if message.bridge != nil {
		m.stopTeamWorkerRouteSubscription()
		m.route.workerBridge = message.bridge
	}

	maximum := m.subagentRouteMaximumOffset()
	if wasAtBottom {
		m.route.offset = maximum
	} else {
		m.route.offset = min(previousOffset, maximum)
	}
	m.route.refreshPending = false

	if m.route.workerBridge != nil {
		return m, m.route.workerBridge.wait(m.route.generation, message.target)
	}

	return m, nil
}

func (m *Model) acceptTeamWorkerRouteMessage(
	generation uint64,
	target coding.TeamWorkerTarget,
) bool {
	return m.route.kind == routeChild && m.route.childKind == childTeamWorker &&
		generation == m.route.generation &&
		target == m.route.childSummary.worker.Target
}

func (m *Model) applyTeamWorkerRouteEvent(
	message teamWorkerRouteEventMsg,
) (tea.Model, tea.Cmd) {
	if !m.acceptTeamWorkerRouteMessage(message.generation, message.target) ||
		message.bridge != m.route.workerBridge {
		return m, nil
	}
	if !message.ok {
		m.stopTeamWorkerRouteSubscription()
		m.route.refreshErr = message.err

		return m, m.refreshTeamWorkerRoute()
	}
	if m.route.childState == nil ||
		message.record.Event.SessionID != m.route.childState.SessionID {
		m.stopTeamWorkerRouteSubscription()
		m.route.refreshErr = errors.New("team worker event identity changed")

		return m, m.refreshTeamWorkerRoute()
	}

	wasAtBottom := m.route.offset >= m.subagentRouteMaximumOffset()
	next, err := coding.Reduce(*m.route.childState, message.record.Event)
	if err != nil {
		m.stopTeamWorkerRouteSubscription()
		m.route.refreshErr = err

		return m, m.refreshTeamWorkerRoute()
	}
	m.route.childState = &next
	if wasAtBottom {
		m.route.offset = m.subagentRouteMaximumOffset()
	} else {
		m.route.offset = min(m.route.offset, m.subagentRouteMaximumOffset())
	}

	return m, message.bridge.wait(message.generation, message.target)
}

func (m *Model) refreshTeamWorkerRoute() tea.Cmd {
	if m.route.kind != routeChild || m.route.childKind != childTeamWorker {
		return nil
	}
	if m.route.loading || m.route.refreshing {
		m.route.refreshPending = true

		return nil
	}

	m.stopTeamWorkerRouteSubscription()
	m.route.refreshing = true

	return m.teamWorkerRouteLoadCommand(true)
}

func (m *Model) interruptTeamWorker(target coding.TeamWorkerTarget) tea.Cmd {
	if target == (coding.TeamWorkerTarget{}) || m.route.controlling {
		return nil
	}

	m.route.controlling = true
	generation := m.route.generation
	controller := m.controller
	ctx := m.ctx

	return func() tea.Msg {
		_, err := controller.SubmitTeamControl(ctx, coding.TeamControlRequest{
			TeamID: target.TeamID, Action: coding.TeamControlInterruptAttempt,
			MemberID: target.MemberID, TaskID: target.TaskID,
			ExpectedAttemptID: target.AttemptID,
			OwnerGeneration:   target.OwnerGeneration,
		})

		return teamWorkerControlResultMsg{
			generation: generation, target: target, err: err,
		}
	}
}

func (m *Model) applyTeamWorkerControl(message teamWorkerControlResultMsg) {
	if (m.route.kind != routeAgents && m.route.kind != routeChild) ||
		message.generation != m.route.generation {
		return
	}
	if m.route.kind == routeChild &&
		message.target != m.route.childSummary.worker.Target {
		return
	}

	m.route.controlling = false
	if m.route.kind == routeAgents && message.err != nil {
		m.route.err = newTeamRoutePresentationError("unable to interrupt the selected Team Worker")

		return
	}
	m.route.err = message.err
}

func (m *Model) teamWorkerRouteContent(state coding.State, child childSummary) string {
	blocks := projectTimeline(state)
	if marker, ok := teamWorkerCompletionMarker(state, child); ok {
		blocks = append(blocks, marker)
	}

	flow := m.renderTimelineBlocksWithOptions(
		blocks,
		timelineRenderOptions{expandToolResults: true},
	)
	if flow != "" {
		return flow
	}
	if child.live() {
		return "✻ Working…"
	}

	return humanizeTeamProjectionState(string(child.worker.LifecycleState))
}

func teamWorkerCompletionMarker(
	state coding.State,
	child childSummary,
) (timelineBlock, bool) {
	var outcome coding.InteractionOutcome
	switch child.worker.LifecycleState {
	case coding.TeamLifecycleCompleted:
		outcome = coding.InteractionSucceeded
	case coding.TeamLifecycleFailed:
		outcome = coding.InteractionFailed
	case coding.TeamLifecycleCancelled, coding.TeamLifecycleInterrupted:
		outcome = coding.InteractionCanceled
	case coding.TeamLifecycleProposed, coding.TeamLifecycleAdmitted,
		coding.TeamLifecycleWaiting, coding.TeamLifecycleRunning,
		coding.TeamLifecyclePaused, coding.TeamLifecycleCapturing,
		coding.TeamLifecycleRecoverable:
		return timelineBlock{}, false
	}

	model := strings.Trim(strings.Join([]string{
		string(state.Provider), state.ModelID,
	}, "/"), "/")

	return projectCompletionMarker(completionMarker{
		interactionID: string(child.worker.Target.AttemptID),
		afterMessages: len(state.Transcript), outcome: outcome,
		durationMillis: child.worker.DurationMillis, model: model,
	})
}

func (m *Model) safeChildRouteError(err error) string {
	if m.route.childKind != childTeamWorker {
		return safeError(err)
	}

	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "Team Worker inspection was canceled."
	case errors.Is(err, context.DeadlineExceeded):
		return "Team Worker inspection timed out."
	case errors.Is(err, coding.ErrTeamWorkerStale):
		return "Team Worker ownership changed; return to Agents and reopen it."
	case errors.Is(err, coding.ErrRuntimeBusy), errors.Is(err, coding.ErrEventGap):
		return "Team Worker state changed; refreshing the exact Worker."
	default:
		return "Team Worker activity is temporarily unavailable."
	}
}
