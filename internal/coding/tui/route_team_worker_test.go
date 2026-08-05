//nolint:wsl_v5 // Worker route ownership assertions stay adjacent to bridge transitions.
package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/subagent"
	"github.com/rsbin/pips/internal/coding/teamstate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentsRouteMergesSubagentsAndTeamWorkers(t *testing.T) {
	t.Parallel()

	controller := newTeamWorkerRouteController()
	controller.agents = []subagent.Summary{{
		ChildSessionID: "subagent-session", Role: subagent.RoleReview,
		State: subagent.StateRunning, TaskPreview: "Review the selector",
		CreatedAt: time.Now().UTC(),
	}}
	model := readyModelWithController(t, controller, true)
	model.state.Teams = []coding.TeamLifecycleState{{TeamLifecycle: coding.TeamLifecycle{
		TeamID: "team-1", State: coding.TeamLifecycleRunning,
	}}}

	driveModelCommands(t, model, model.openAgentsRoute())

	assert.Equal(t, routeAgents, model.route.kind)
	require.Len(t, model.route.children, 2)
	content := model.View().Content
	assert.Contains(t, content, "Agents")
	assert.Contains(t, content, "Review the selector")
	assert.Contains(t, content, "Build Team route")
	assert.Contains(t, content, "Builder")
	for _, private := range []string{"team-1", "worker-1", "attempt-1", "worker-session-1"} {
		assert.NotContains(t, content, private)
	}

	model.Update(tea.KeyPressMsg{Text: "Builder"})
	filtered := model.filteredChildren()
	require.Len(t, filtered, 1)
	assert.Equal(t, childTeamWorker, filtered[0].kind)
}

func TestAgentsTeamWorkersSortLiveThenNewest(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	view := teamWorkerTestView()
	oldTerminal := view.Attempts[0]
	oldTerminal.StartedAt = now.Add(-3 * time.Minute)
	oldTerminal.DomainState = team.AttemptStatusCompleted
	oldTerminal.ResourceState = teamstate.AttemptTerminal
	oldTerminal.LifecycleState = coding.TeamLifecycleCompleted
	newTerminal := oldTerminal
	newTerminal.Target.AttemptID = "attempt-2"
	newTerminal.ChildSessionID = "worker-session-2"
	newTerminal.StartedAt = now.Add(-time.Minute)
	live := oldTerminal
	live.Target.AttemptID = "attempt-3"
	live.ChildSessionID = "worker-session-3"
	live.StartedAt = now.Add(-2 * time.Minute)
	live.DomainState = team.AttemptStatusRunning
	live.ResourceState = teamstate.AttemptRunning
	live.LifecycleState = coding.TeamLifecycleRunning
	view.Attempts = []coding.TeamAttemptView{oldTerminal, newTerminal, live}

	model := readyModel(t, true)
	model.route = routeState{kind: routeAgents, children: childSummaries(nil, []coding.TeamView{view})}
	children := model.filteredChildren()
	require.Len(t, children, 3)
	assert.Equal(t, team.AttemptID("attempt-3"), children[0].worker.Target.AttemptID)
	assert.Equal(t, team.AttemptID("attempt-2"), children[1].worker.Target.AttemptID)
	assert.Equal(t, team.AttemptID("attempt-1"), children[2].worker.Target.AttemptID)
}

func TestTeamWorkerActivityPredicateKeepsPausedStaticAndCapturingLive(t *testing.T) {
	t.Parallel()

	controller := newTeamWorkerRouteController()
	child := onlyTeamWorkerChild(t, controller.viewSnapshot())

	capturing := child
	capturing.worker.DomainState = team.AttemptStatusRunning
	capturing.worker.ResourceState = teamstate.AttemptCapturing
	capturing.worker.LifecycleState = coding.TeamLifecycleCapturing
	assert.True(t, capturing.live())

	paused := child
	paused.worker.DomainState = team.AttemptStatusRunning
	paused.worker.ResourceState = teamstate.AttemptRunning
	paused.worker.LifecycleState = coding.TeamLifecyclePaused
	paused.worker.Activity = coding.TeamActivityAwaitingQuestion
	assert.False(t, paused.live())

	recoverable := child
	recoverable.worker.ResourceState = teamstate.AttemptRecoverable
	recoverable.worker.LifecycleState = coding.TeamLifecycleRecoverable
	assert.False(t, recoverable.live())
}

func TestTeamWorkerRouteBootstrapsTerminalOrdinaryTimeline(t *testing.T) {
	t.Parallel()

	controller := newTeamWorkerRouteController()
	view := controller.viewSnapshot()
	view.Attempts[0].DomainState = team.AttemptStatusCompleted
	view.Attempts[0].ResourceState = teamstate.AttemptTerminal
	view.Attempts[0].LifecycleState = coding.TeamLifecycleCompleted
	view.Attempts[0].DurationMillis = 2_000
	controller.setView(view)
	controller.workerState.Phase = coding.PhaseIdle
	controller.workerState.Transcript = []ai.Message{
		ai.UserText("Implement the Worker route."),
		ai.AssistantText("The Worker route is complete."),
	}
	model := readyModelWithController(t, controller, true)
	child := onlyTeamWorkerChild(t, view)
	model.route = routeState{kind: routeAgents, children: []childSummary{child}}

	driveModelCommands(t, model, model.openChildRoute(child))

	assert.Equal(t, routeChild, model.route.kind)
	assert.Equal(t, childTeamWorker, model.route.childKind)
	require.NotNil(t, model.route.childState)
	content := model.View().Content
	assert.Contains(t, content, "❯ Implement the Worker route.")
	assert.Contains(t, content, "The Worker route is complete.")
	assert.Contains(t, content, "▣ openai/worker-model · 2s")
	assert.Contains(t, content, "Builder · Team Worker")
	assert.Equal(t, []coding.TeamWorkerTarget{child.worker.Target}, controller.inspected)
	assert.Empty(t, controller.observed)
	assert.Nil(t, model.route.workerBridge)
}

func TestTeamWorkerRouteFallbackUsesSharedActivityFrame(t *testing.T) {
	t.Parallel()

	controller := newTeamWorkerRouteController()
	model := readyModelWithController(t, controller, true)
	child := onlyTeamWorkerChild(t, controller.viewSnapshot())
	state := coding.State{}
	model.route = routeState{
		kind: routeChild, childKind: childTeamWorker,
		children: []childSummary{child}, childSummary: child,
		childState: &state,
	}
	model.activity.Reset()

	before := model.teamWorkerRouteContent(state, child)
	assert.True(t, model.activityClockVisible())
	assert.Contains(t, before, "✻ Working…")

	_, command := model.Update(model.activity.Tick()())
	require.NotNil(t, command)

	after := model.teamWorkerRouteContent(state, child)
	assert.NotEqual(t, before, after)
	assert.Contains(t, after, "✢ Working…")

	child.worker.DomainState = team.AttemptStatusCompleted
	child.worker.ResourceState = teamstate.AttemptTerminal
	child.worker.LifecycleState = coding.TeamLifecycleCompleted
	model.route.children[0] = child
	model.route.childSummary = child
	assert.False(t, model.activityClockVisible())
	static := model.teamWorkerRouteContent(state, child)
	_, command = model.Update(activityTickMsg{})
	assert.Nil(t, command)
	assert.Equal(t, static, model.teamWorkerRouteContent(state, child))

	paused := child
	paused.worker.DomainState = team.AttemptStatusRunning
	paused.worker.ResourceState = teamstate.AttemptRunning
	paused.worker.LifecycleState = coding.TeamLifecyclePaused
	paused.worker.Activity = coding.TeamActivityAwaitingQuestion
	assert.Equal(t, "Needs input · question", model.teamWorkerRouteContent(state, paused))

	recoverable := child
	recoverable.worker.ResourceState = teamstate.AttemptRecoverable
	recoverable.worker.LifecycleState = coding.TeamLifecycleRecoverable
	recoverable.worker.Activity = ""
	assert.Equal(t, "Recoverable", model.teamWorkerRouteContent(state, recoverable))
}

func TestTeamWorkerRouteAppliesLiveDeltasAndRecoversGapWithLastGoodState(t *testing.T) {
	t.Parallel()

	controller := newTeamWorkerRouteController()
	model := readyModelWithController(t, controller, true)
	child := onlyTeamWorkerChild(t, controller.viewSnapshot())
	model.route = routeState{
		kind: routeChild, generation: 7, childKind: childTeamWorker,
		childSummary: child, childSessionID: child.childSessionID,
	}

	events := make(chan coding.EventRecord, 1)
	closed := false
	bridgeErr := error(nil)
	bridge := &workerSubscriptionBridge{
		events: events,
		err:    func() error { return bridgeErr },
		close:  func() { closed = true },
	}
	_, wait := model.applyTeamWorkerRouteData(teamWorkerRouteDataMsg{
		generation: 7, target: child.worker.Target,
		state: controller.workerState, hasState: true, bridge: bridge,
	})
	require.NotNil(t, wait)

	event := coding.Event{
		Schema: coding.EventSchema, Sequence: controller.workerState.Sequence + 1,
		Time: time.Now().UTC(), SessionID: child.childSessionID,
		Type:    coding.EventStatusChanged,
		Payload: coding.StatusChanged{Phase: coding.PhasePaused},
	}
	events <- coding.EventRecord{Cursor: 1, Event: event}
	message, ok := wait().(teamWorkerRouteEventMsg)
	require.True(t, ok)
	_, next := model.Update(message)
	require.NotNil(t, next)
	require.NotNil(t, model.route.childState)
	assert.Equal(t, coding.PhasePaused, model.route.childState.Phase)

	bridgeErr = coding.ErrEventGap
	close(events)
	closedMessage, ok := next().(teamWorkerRouteEventMsg)
	require.True(t, ok)
	_, refresh := model.Update(closedMessage)
	require.NotNil(t, refresh)
	assert.True(t, closed)
	assert.True(t, model.route.refreshing)
	assert.Equal(t, coding.PhasePaused, model.route.childState.Phase)
	assert.ErrorIs(t, model.route.refreshErr, coding.ErrEventGap)
}

func TestTeamWorkerRouteRejectsLateTargetAndClosesSubscription(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	view := teamWorkerTestView()
	child := onlyTeamWorkerChild(t, view)
	model.route = routeState{
		kind: routeChild, generation: 9, childKind: childTeamWorker,
		childSummary: child, childSessionID: child.childSessionID,
	}
	closed := false
	bridge := &workerSubscriptionBridge{close: func() { closed = true }}
	stale := child.worker.Target
	stale.OwnerGeneration++

	model.Update(teamWorkerRouteDataMsg{
		generation: 9, target: stale, bridge: bridge,
		state: teamWorkerState(), hasState: true,
	})

	assert.True(t, closed)
	assert.Nil(t, model.route.childState)
	assert.Nil(t, model.route.workerBridge)
}

func TestTeamWorkerRouteRapidSwitchClosesOldBridgeAndRejectsLateLoad(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	first := onlyTeamWorkerChild(t, teamWorkerTestView())
	second := first
	second.worker.Target.AttemptID = "attempt-2"
	second.worker.ChildSessionID = "worker-session-2"
	second.childSessionID = "worker-session-2"
	closed := false
	oldBridge := &workerSubscriptionBridge{close: func() { closed = true }}
	model.route = routeState{
		kind: routeChild, generation: 11, childKind: childTeamWorker,
		childSummary: first, childSessionID: first.childSessionID,
		workerBridge: oldBridge,
	}

	command := model.activateRoute(routeOpenRequest{kind: routeChild, child: second})
	require.NotNil(t, command)
	assert.True(t, closed)
	assert.Equal(t, second.worker.Target, model.route.childSummary.worker.Target)
	assert.Equal(t, "worker-session-2", model.route.childSessionID)

	lateClosed := false
	lateBridge := &workerSubscriptionBridge{close: func() { lateClosed = true }}
	model.Update(teamWorkerRouteDataMsg{
		generation: 11, target: first.worker.Target,
		state: teamWorkerState(), hasState: true, bridge: lateBridge,
	})
	assert.True(t, lateClosed)
	assert.Equal(t, second.worker.Target, model.route.childSummary.worker.Target)
	assert.Nil(t, model.route.childState)
}

func TestTeamWorkerInterruptUsesExactControlTarget(t *testing.T) {
	t.Parallel()

	controller := newTeamWorkerRouteController()
	model := readyModelWithController(t, controller, true)
	child := onlyTeamWorkerChild(t, controller.viewSnapshot())
	model.route = routeState{
		kind: routeAgents, generation: 4, children: []childSummary{child},
	}

	_, command := model.Update(key("c"))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)

	controls := controller.controlsSnapshot()
	require.Len(t, controls, 1)
	assert.Equal(t, coding.TeamControlInterruptAttempt, controls[0].Action)
	assert.Equal(t, child.worker.Target.TeamID, controls[0].TeamID)
	assert.Equal(t, child.worker.Target.MemberID, controls[0].MemberID)
	assert.Equal(t, child.worker.Target.TaskID, controls[0].TaskID)
	assert.Equal(t, child.worker.Target.AttemptID, controls[0].ExpectedAttemptID)
	assert.Equal(t, child.worker.Target.OwnerGeneration, controls[0].OwnerGeneration)
}

func TestTeamWorkerRouteUsesComposerForExactDirectMessageAndRestoresParentDraft(t *testing.T) {
	t.Parallel()

	controller := newTeamWorkerRouteController()
	model := readyModelWithController(t, controller, true)
	child := onlyTeamWorkerChild(t, controller.viewSnapshot())
	model.composer.SetValue("parent draft")

	driveModelCommands(t, model, model.openChildRoute(child))
	assert.Equal(t, routeChild, model.route.kind)
	assert.Empty(t, model.composer.Value())
	require.NotNil(t, model.View().Cursor)
	assert.Contains(t, model.View().Content, "Enter me")

	model.Update(tea.KeyPressMsg{Text: "check the exact ownership edge"})
	_, command := model.Update(key(keyEnter))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)
	assert.Empty(t, model.composer.Value())

	controls := controller.controlsSnapshot()
	require.Len(t, controls, 1)
	request := controls[0]
	assert.Equal(t, coding.TeamControlMessage, request.Action)
	assert.Equal(t, child.worker.Target.TeamID, request.TeamID)
	assert.Equal(t, child.worker.Target.MemberID, request.MemberID)
	assert.Equal(t, child.worker.Target.TaskID, request.TaskID)
	assert.Equal(t, child.worker.Target.AttemptID, request.ExpectedAttemptID)
	assert.Equal(t, child.worker.Target.OwnerGeneration, request.OwnerGeneration)
	assert.Equal(t, "check the exact ownership edge", request.Text)

	_, command = model.Update(key(keyEscape))
	driveModelCommands(t, model, command)
	assert.Equal(t, routeNone, model.route.kind)
	assert.Equal(t, "parent draft", model.composer.Value())
}

func TestTeamWorkerMessageResolvesPasteAndRetainsDraftOnFailure(t *testing.T) {
	t.Parallel()

	controller := newTeamWorkerRouteController()
	controller.controlErr = errors.New("control unavailable")
	model := readyModelWithController(t, controller, true)
	child := onlyTeamWorkerChild(t, controller.viewSnapshot())
	driveModelCommands(t, model, model.openChildRoute(child))

	pasted := strings.Repeat("review this boundary\n", 9)
	placeholder, err := model.composer.InsertPaste(pasted)
	require.NoError(t, err)
	assert.True(t, placeholder)
	draft := model.composer.Snapshot()

	_, command := model.Update(key(keyEnter))
	require.NotNil(t, command)
	driveModelCommands(t, model, command)
	assert.Equal(t, draft, model.composer.Snapshot())
	require.Error(t, model.route.err)
	require.NotNil(t, model.View().Cursor)
	assert.Equal(t, strings.TrimSpace(pasted), controller.controlsSnapshot()[0].Text)
}

func TestTeamWorkerMessageIsReadOnlyInPlanMode(t *testing.T) {
	t.Parallel()

	state := readyState()
	state.Mode = coding.ModePlan
	controller := newTeamWorkerRouteController()
	controller.state = state
	model := readyModelWithController(t, controller, true)
	child := onlyTeamWorkerChild(t, controller.viewSnapshot())
	driveModelCommands(t, model, model.openChildRoute(child))
	model.composer.SetValue("do not send")

	_, command := model.Update(key(keyEnter))
	assert.Nil(t, command)
	assert.Equal(t, "do not send", model.composer.Value())
	assert.Contains(t, safeTeamRouteError(model.route.err), "read-only in Plan Mode")
	assert.Empty(t, controller.controlsSnapshot())
}

func TestTeamWorkerRouteClosesBridgeOnBackAndParentCatchUp(t *testing.T) {
	t.Parallel()

	model := readyModel(t, true)
	child := onlyTeamWorkerChild(t, teamWorkerTestView())
	closed := false
	model.route = routeState{
		kind: routeChild, generation: 1, childKind: childTeamWorker,
		childSummary: child, childSessionID: child.childSessionID,
		children:     []childSummary{child},
		workerBridge: &workerSubscriptionBridge{close: func() { closed = true }},
	}
	model.state.Transcript = []ai.Message{ai.UserText("parent output while Worker is open")}
	assert.Nil(t, model.commitStableTimeline())

	_, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Nil(t, command)
	assert.True(t, closed)
	assert.Equal(t, routeAgents, model.route.kind)
	assert.Zero(t, model.scrollback.messages)

	_, command = model.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	printed := driveModelCommandsCapture(t, model, command)
	assert.Equal(t, routeNone, model.route.kind)
	assert.Contains(t, printed, "parent output while Worker is open")
	assert.Equal(t, 1, model.scrollback.messages)
}

func TestLatestTeamWorkerIsDirectOnlyWhenNoOrdinaryToolDetailExists(t *testing.T) {
	t.Parallel()

	controller := newTeamWorkerRouteController()
	model := readyModelWithController(t, controller, true)
	view := controller.viewSnapshot()
	newer := view.Attempts[0]
	newer.Target.AttemptID = "attempt-2"
	newer.ChildSessionID = "worker-session-2"
	newer.StartedAt = view.Attempts[0].StartedAt.Add(time.Second)
	view.Attempts = append(view.Attempts, newer)
	model.storeTeamProjectionView(view)

	command := model.toggleLatestTool()
	require.NotNil(t, command)
	assert.Equal(t, routeChild, model.route.kind)
	assert.Equal(t, childTeamWorker, model.route.childKind)
	assert.Equal(t, team.AttemptID("attempt-2"), model.route.childSummary.worker.Target.AttemptID)

	model.stopTeamWorkerRouteSubscription()
	model.route = routeState{}
	model.state.Tools = []coding.ToolState{{
		Call:   coding.ToolCall{ID: "read-1", Name: toolNameRead},
		Status: coding.ToolStatusCompleted,
		Result: codingToolResultFor("read-1", toolNameRead, "ordinary tool result"),
	}}
	command = model.toggleLatestTool()
	require.NotNil(t, command)
	driveModelCommands(t, model, command)
	assert.Equal(t, routeToolDetail, model.route.kind)
	assert.Contains(t, model.toolDetailRouteContent(), "ordinary tool result")
}

type teamWorkerRouteController struct {
	*teamRouteTestController
	workerState coding.State
	observed    []coding.TeamWorkerTarget
	inspected   []coding.TeamWorkerTarget
	observeErr  error
}

func newTeamWorkerRouteController() *teamWorkerRouteController {
	controller := newTeamRouteTestController(readyState())
	view := teamWorkerTestView()
	controller.setView(view)

	return &teamWorkerRouteController{
		teamRouteTestController: controller,
		workerState:             teamWorkerState(),
		observeErr:              coding.ErrRuntimeBusy,
	}
}

func (c *teamWorkerRouteController) ObserveTeamWorker(
	_ context.Context,
	target coding.TeamWorkerTarget,
) (coding.EventObservation, error) {
	c.observed = append(c.observed, target)

	return coding.EventObservation{}, c.observeErr
}

func (c *teamWorkerRouteController) InspectTeamWorkerState(
	_ context.Context,
	target coding.TeamWorkerTarget,
) (coding.State, error) {
	c.inspected = append(c.inspected, target)
	if c.workerState.SessionID == "" {
		return coding.State{}, errors.New("worker state unavailable")
	}

	return c.workerState.Clone(), nil
}

func teamWorkerTestView() coding.TeamView {
	view := testTeamRouteView()
	view.Attempts[0].ChildSessionID = "worker-session-1"

	return view
}

func teamWorkerState() coding.State {
	return coding.State{
		Sequence: 1, SessionID: "worker-session-1", SessionOpen: true,
		Provider: ai.ProviderOpenAI, ModelID: "worker-model",
		Mode: coding.ModeAgent, Phase: coding.PhaseRunning,
	}
}

func onlyTeamWorkerChild(t *testing.T, view coding.TeamView) childSummary {
	t.Helper()

	values := mergeTeamWorkerChildren(nil, view)
	require.Len(t, values, 1)

	return values[0]
}
