//nolint:wsl_v5 // Projection transactions keep identity checks and cursor commits adjacent.
package tui

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/internal/coding"
)

const (
	maximumTeamProjectionViews = 64
	genericTeamWorkerLabel     = "Team Worker"
	genericTeamTaskLabel       = "Team task"
)

type teamAttemptKey struct {
	teamID    team.ID
	memberID  team.MemberID
	taskID    team.TaskID
	attemptID team.AttemptID
}

type teamProjectionState struct {
	sessionID        string
	views            map[team.ID]coding.TeamView
	viewOrder        []team.ID
	refreshing       map[team.ID]uint64
	refreshPending   map[team.ID]bool
	refreshSequence  uint64
	terminalOrder    map[teamAttemptKey]uint64
	terminalSequence uint64
}

type teamProjectionRefreshMsg struct {
	sessionID  string
	teamID     team.ID
	generation uint64
	view       coding.TeamView
	err        error
}

func (m *Model) resetTeamProjection(sessionID string) {
	m.teamProjection = teamProjectionState{sessionID: sessionID}
	m.teamPanel = teamPanelState{}
	m.resetTeamInteractions(sessionID)
	m.scrollback.teamAttempts = nil
}

func (m *Model) ensureTeamProjection() {
	projection := &m.teamProjection
	if projection.sessionID != m.state.SessionID {
		m.resetTeamProjection(m.state.SessionID)
		projection = &m.teamProjection
	}
	if projection.views == nil {
		projection.views = make(map[team.ID]coding.TeamView)
	}
	if projection.refreshing == nil {
		projection.refreshing = make(map[team.ID]uint64)
	}
	if projection.refreshPending == nil {
		projection.refreshPending = make(map[team.ID]bool)
	}
	if projection.terminalOrder == nil {
		projection.terminalOrder = make(map[teamAttemptKey]uint64)
	}
}

func (m *Model) trackTeamLifecycleEvent(event coding.Event) {
	value, ok := event.Payload.(coding.TeamLifecycle)
	if !ok || !terminalTeamAttempt(value) {
		return
	}

	key, ok := exactTeamAttemptKey(value)
	if !ok {
		return
	}

	m.ensureTeamProjection()
	if _, exists := m.teamProjection.terminalOrder[key]; exists {
		return
	}

	m.teamProjection.terminalSequence++
	m.teamProjection.terminalOrder[key] = m.teamProjection.terminalSequence
}

func (m *Model) ensureTeamTerminalOrder() {
	m.ensureTeamProjection()
	for _, state := range m.state.Teams {
		value := state.TeamLifecycle
		if !terminalTeamAttempt(value) {
			continue
		}
		key, ok := exactTeamAttemptKey(value)
		if !ok {
			continue
		}
		if _, exists := m.teamProjection.terminalOrder[key]; exists {
			continue
		}

		m.teamProjection.terminalSequence++
		m.teamProjection.terminalOrder[key] = m.teamProjection.terminalSequence
	}
}

func exactTeamAttemptKey(value coding.TeamLifecycle) (teamAttemptKey, bool) {
	key := teamAttemptKey{
		teamID: value.TeamID, memberID: value.MemberID,
		taskID: value.TaskID, attemptID: value.AttemptID,
	}

	return key, key.teamID != "" && key.memberID != "" &&
		key.taskID != "" && key.attemptID != ""
}

func terminalTeamAttempt(value coding.TeamLifecycle) bool {
	if _, ok := exactTeamAttemptKey(value); !ok {
		return false
	}

	switch value.State {
	case coding.TeamLifecycleCompleted, coding.TeamLifecycleFailed,
		coding.TeamLifecycleCancelled, coding.TeamLifecycleInterrupted:
		return true
	default:
		return false
	}
}

func teamAttemptFingerprint(value coding.TeamLifecycle) projectionFingerprint {
	return sha256.Sum256([]byte(fmt.Sprintf("%#v", value)))
}

func (m *Model) takeStableTeamAttemptBlocks() []timelineBlock {
	if visibleDraftText(m.state.Draft) != "" {
		return nil
	}

	m.ensureTeamTerminalOrder()
	if m.scrollback.teamAttempts == nil {
		m.scrollback.teamAttempts = make(map[teamAttemptKey]projectionFingerprint)
	}

	type orderedAttempt struct {
		order uint64
		key   teamAttemptKey
		value coding.TeamLifecycle
	}
	values := make([]orderedAttempt, 0)
	for _, state := range m.state.Teams {
		value := state.TeamLifecycle
		if !terminalTeamAttempt(value) {
			continue
		}
		key, ok := exactTeamAttemptKey(value)
		if !ok {
			continue
		}
		if _, committed := m.scrollback.teamAttempts[key]; committed {
			continue
		}
		values = append(values, orderedAttempt{
			order: m.teamProjection.terminalOrder[key], key: key, value: value,
		})
	}
	sort.SliceStable(values, func(left, right int) bool {
		return values[left].order < values[right].order
	})

	blocks := make([]timelineBlock, 0, len(values))
	for _, value := range values {
		blocks = append(blocks, m.projectTeamAttemptBlock(value.value))
		m.scrollback.teamAttempts[value.key] = teamAttemptFingerprint(value.value)
	}

	return blocks
}

func (m *Model) activeTeamAttemptBlocks() []timelineBlock {
	m.ensureTeamProjection()
	blocks := make([]timelineBlock, 0, len(m.state.Teams))
	for _, state := range m.state.Teams {
		value := state.TeamLifecycle
		key, ok := exactTeamAttemptKey(value)
		if !ok {
			continue
		}
		if terminalTeamAttempt(value) {
			if _, committed := m.scrollback.teamAttempts[key]; committed {
				continue
			}
		}
		blocks = append(blocks, m.projectTeamAttemptBlock(value))
	}

	return blocks
}

func (m *Model) projectTeamAttemptBlock(value coding.TeamLifecycle) timelineBlock {
	worker, task := m.teamAttemptLabels(value)
	status := teamAttemptStatus(value)
	facts := make([]string, 0, 4)
	if value.DurationMillis > 0 {
		facts = append(facts, formatInteractionDuration(value.DurationMillis))
	}
	if value.Turns > 0 {
		facts = append(facts, fmt.Sprintf("%d turns", value.Turns))
	}
	if value.ToolCalls > 0 {
		facts = append(facts, fmt.Sprintf("%d tools", value.ToolCalls))
	}
	if tokens := value.Usage.InputTokens + value.Usage.OutputTokens; tokens > 0 {
		facts = append(facts, compactTokenCount(tokens)+" tokens")
	}
	if len(facts) > 0 {
		status += " · " + strings.Join(facts, " · ")
	}

	glyph := "✻"
	if m.teamLifecycleActivityVisible(value) {
		glyph = m.activity.Frame()
	} else if terminalTeamAttempt(value) {
		glyph = completionGlyph
	}

	return timelineBlock{
		kind:   blockTeam,
		title:  glyph + " " + worker + " · " + task,
		body:   status,
		status: string(value.State),
	}
}

func (m *Model) teamLifecycleActivityVisible(value coding.TeamLifecycle) bool {
	if _, ok := exactTeamAttemptKey(value); !ok {
		return false
	}

	view, ok := m.teamProjection.views[value.TeamID]
	if !ok {
		return false
	}
	for _, attempt := range view.Attempts {
		if attempt.Target.MemberID != value.MemberID ||
			attempt.Target.TaskID != value.TaskID ||
			attempt.Target.AttemptID != value.AttemptID {
			continue
		}

		return teamAttemptActivityVisible(attempt)
	}

	return false
}

func (m *Model) teamTimelineActivityVisible() bool {
	if !m.parentActivitySurfaceVisible() {
		return false
	}

	for _, state := range m.state.Teams {
		if m.teamLifecycleActivityVisible(state.TeamLifecycle) {
			return true
		}
	}

	return false
}

func (m *Model) teamAttemptLabels(value coding.TeamLifecycle) (string, string) {
	worker := genericTeamWorkerLabel
	task := genericTeamTaskLabel
	view, ok := m.teamProjection.views[value.TeamID]
	if !ok {
		return worker, task
	}
	for _, member := range view.Members {
		if member.ID == value.MemberID {
			if label := boundedTeamLabel(member.Name); label != "" {
				worker = label
			}
			break
		}
	}
	for _, candidate := range view.Tasks {
		if candidate.ID == value.TaskID {
			if label := boundedTeamLabel(candidate.Title); label != "" {
				task = label
			}
			break
		}
	}

	return worker, task
}

func boundedTeamLabel(value string) string {
	return truncateText(oneLineToolText(value), 96)
}

func teamAttemptStatus(value coding.TeamLifecycle) string {
	if value.Activity != "" && !terminalTeamAttempt(value) {
		return humanizeTeamProjectionState(string(value.Activity))
	}

	return humanizeTeamProjectionState(string(value.State))
}

func humanizeTeamProjectionState(value string) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "_", " ")
	if value == "" {
		return "Team activity"
	}

	return strings.ToUpper(value[:1]) + value[1:]
}

func teamIDFromProjectionEvent(event coding.Event) team.ID {
	switch value := event.Payload.(type) {
	case coding.TeamLifecycle:
		return value.TeamID
	case coding.TeamControlLifecycle:
		return value.TeamID
	case coding.TeamIntegrationLifecycle:
		return value.TeamID
	default:
		return ""
	}
}

func (m *Model) invalidateTeamProjection(event coding.Event) tea.Cmd {
	teamID := teamIDFromProjectionEvent(event)
	if teamID == "" || event.SessionID != m.state.SessionID {
		return nil
	}

	m.ensureTeamProjection()
	if value, ok := event.Payload.(coding.TeamControlLifecycle); ok {
		m.mergeTeamRouteControl(value, event.Time)
	}

	return m.readTeamProjection(teamID)
}

func (m *Model) refreshTeamProjectionSnapshot() tea.Cmd {
	seen := make(map[team.ID]struct{})
	commands := make([]tea.Cmd, 0)
	appendTeam := func(teamID team.ID) {
		if teamID == "" {
			return
		}
		if _, exists := seen[teamID]; exists {
			return
		}
		seen[teamID] = struct{}{}
		commands = append(commands, m.readTeamProjection(teamID))
	}

	for _, value := range m.state.Teams {
		appendTeam(value.TeamID)
	}
	for _, value := range m.state.TeamControls {
		appendTeam(value.TeamID)
	}
	for _, value := range m.state.TeamIntegrations {
		appendTeam(value.TeamID)
	}

	return tea.Batch(commands...)
}

func (m *Model) readTeamProjection(teamID team.ID) tea.Cmd {
	if m.controller == nil || teamID == "" {
		return nil
	}
	m.ensureTeamProjection()
	if m.teamProjection.refreshing[teamID] != 0 {
		m.teamProjection.refreshPending[teamID] = true

		return nil
	}

	m.teamProjection.refreshSequence++
	generation := m.teamProjection.refreshSequence
	m.teamProjection.refreshing[teamID] = generation
	request := coding.TeamReadRequest{TeamID: teamID}
	if view, ok := m.teamProjection.views[teamID]; ok {
		request.AfterRevision = view.ChangeCursor
		request.AfterControlRevision = view.ControlCursor
	}
	sessionID := m.state.SessionID
	controller := m.controller
	ctx := m.ctx

	return func() tea.Msg {
		view, err := controller.ReadTeam(ctx, request)

		return teamProjectionRefreshMsg{
			sessionID: sessionID, teamID: teamID, generation: generation,
			view: view, err: err,
		}
	}
}

func (m *Model) applyTeamProjectionRefresh(message teamProjectionRefreshMsg) tea.Cmd {
	m.ensureTeamProjection()
	if message.sessionID != m.state.SessionID ||
		m.teamProjection.refreshing[message.teamID] != message.generation {
		return nil
	}

	delete(m.teamProjection.refreshing, message.teamID)
	var interactions tea.Cmd
	if message.err == nil && message.view.TeamID == message.teamID {
		interactions = m.applyAcceptedTeamProjectionView(message)
	}

	if m.teamProjection.refreshPending[message.teamID] {
		delete(m.teamProjection.refreshPending, message.teamID)

		return tea.Batch(
			m.requestRender(), m.readTeamProjection(message.teamID), interactions,
		)
	}

	return tea.Batch(m.requestRender(), interactions)
}

func (m *Model) applyAcceptedTeamProjectionView(message teamProjectionRefreshMsg) tea.Cmd {
	activityWasVisible := m.activityClockVisible()
	view := m.mergeTeamProjectionView(message.view)
	m.storeTeamProjectionView(view)
	if m.route.kind == routeAgents {
		m.route.children = mergeTeamWorkerChildren(m.route.children, view)
	}
	if m.route.kind == routeChild && m.route.childKind == childTeamWorker {
		m.refreshSelectedTeamWorkerSummary(view)
	}
	if m.route.kind == routeTeam && m.route.team != nil &&
		m.route.team.stage == teamRouteActive && m.route.team.teamID == message.teamID &&
		!m.route.loading {
		cloned := view.Clone()
		m.route.team.view = &cloned
	}

	return tea.Batch(
		m.reconcileTeamInteractionView(view),
		m.startActivityClock(activityWasVisible),
	)
}

func (m *Model) refreshSelectedTeamWorkerSummary(view coding.TeamView) {
	for _, child := range mergeTeamWorkerChildren(nil, view) {
		if m.route.childSummary.sameIdentity(child) {
			m.route.childSummary = child

			return
		}
	}
}

func (m *Model) mergeTeamProjectionView(next coding.TeamView) coding.TeamView {
	m.ensureTeamProjection()
	merged := next.Clone()
	controls := make([]coding.TeamControlView, 0, maximumTeamRouteControls)
	if previous, ok := m.teamProjection.views[next.TeamID]; ok {
		controls = append(controls, previous.Controls...)
		merged.ControlRevision = max(merged.ControlRevision, previous.ControlRevision)
		merged.ControlCursor = max(merged.ControlCursor, previous.ControlCursor)
	}
	for _, control := range next.Controls {
		controls = upsertTeamRouteControl(controls, control)
	}
	for _, state := range m.state.TeamControls {
		if state.TeamID != next.TeamID {
			continue
		}
		controls = upsertTeamRouteControl(controls, teamRouteControlView(
			state.TeamControlLifecycle,
			time.Time{},
		))
		merged.ControlRevision = max(merged.ControlRevision, state.Revision)
		merged.ControlCursor = max(merged.ControlCursor, state.Revision)
	}
	if len(controls) > maximumTeamRouteControls {
		controls = slices.Clone(controls[len(controls)-maximumTeamRouteControls:])
	}
	merged.Controls = controls

	return merged
}

func (m *Model) storeTeamProjectionView(view coding.TeamView) {
	if view.TeamID == "" {
		return
	}
	m.ensureTeamProjection()
	if _, exists := m.teamProjection.views[view.TeamID]; !exists {
		m.teamProjection.viewOrder = append(m.teamProjection.viewOrder, view.TeamID)
	}
	m.teamProjection.views[view.TeamID] = view.Clone()
	for len(m.teamProjection.viewOrder) > maximumTeamProjectionViews {
		oldest := m.teamProjection.viewOrder[0]
		m.teamProjection.viewOrder = slices.Clone(m.teamProjection.viewOrder[1:])
		delete(m.teamProjection.views, oldest)
		delete(m.teamProjection.refreshing, oldest)
		delete(m.teamProjection.refreshPending, oldest)
	}
}

func (m *Model) mergeTeamProjectionControl(value coding.TeamControlLifecycle, at time.Time) {
	view, ok := m.teamProjection.views[value.TeamID]
	if !ok {
		return
	}
	view.Controls = upsertTeamRouteControl(view.Controls, teamRouteControlView(value, at))
	if len(view.Controls) > maximumTeamRouteControls {
		view.Controls = slices.Clone(view.Controls[len(view.Controls)-maximumTeamRouteControls:])
	}
	view.ControlRevision = max(view.ControlRevision, value.Revision)
	view.ControlCursor = max(view.ControlCursor, value.Revision)
	m.teamProjection.views[value.TeamID] = view
}

func (m *Model) teamStatusLabel(width int) string {
	if integration := latestPendingTeamIntegration(m.state.TeamIntegrations); integration != "" {
		switch {
		case width >= 64:
			return "Integration · " + humanizeTeamProjectionState(string(integration))
		case width >= 24:
			return "Integration pending"
		default:
			return "integration"
		}
	}
	if state := latestPendingTeamState(m.state.Teams); state != "" {
		switch {
		case width >= 64:
			return "Team · " + humanizeTeamProjectionState(string(state))
		case width >= 24:
			return "Team " + strings.ReplaceAll(string(state), "_", " ")
		default:
			return "team"
		}
	}

	return ""
}

func latestPendingTeamIntegration(
	values []coding.TeamIntegrationLifecycleState,
) coding.TeamIntegrationStatus {
	for _, value := range slices.Backward(values) {
		switch value.State {
		case coding.TeamIntegrationApplied, coding.TeamIntegrationRejected,
			coding.TeamIntegrationRolledBack:
			continue
		default:
			return value.State
		}
	}

	return ""
}

func latestPendingTeamState(values []coding.TeamLifecycleState) coding.TeamLifecycleStatus {
	for _, value := range slices.Backward(values) {
		switch value.State {
		case coding.TeamLifecycleCompleted, coding.TeamLifecycleFailed,
			coding.TeamLifecycleCancelled, coding.TeamLifecycleInterrupted:
			continue
		default:
			return value.State
		}
	}

	return ""
}

func combinedStatusRight(mode coding.OperatingMode, teamStatus string, width int) string {
	modeStatus := statusModeLabel(mode, width)
	if teamStatus == "" {
		return modeStatus
	}
	if modeStatus == "" {
		return teamStatus
	}

	switch {
	case width >= 80:
		return modeStatus + " · " + teamStatus
	case width >= 40:
		return "Plan mode · " + compactTeamStatus(teamStatus)
	case width >= 12:
		return "plan · team"
	default:
		return "team"
	}
}

func compactTeamStatus(value string) string {
	if strings.HasPrefix(value, "Integration") || value == "integration" {
		return "Integration"
	}

	return "Team"
}
