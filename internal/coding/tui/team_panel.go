//nolint:wsl_v5 // Panel projection, key handling, and rendering form one local UI state machine.
package tui

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/teamstate"
)

const maximumTeamPanelTasks = 6

type teamPanelState struct {
	isFocused       bool
	isPeeking       bool
	showTasks       bool
	isTaskFocused   bool
	isConfirming    bool
	cursor          int
	taskCursor      int
	isControlling   bool
	controlSequence uint64
	controlAction   coding.TeamControlAction
	controlErr      error
}

type teamPanelControlResultMsg struct {
	sessionID  string
	generation uint64
	teamID     team.ID
	target     coding.TeamWorkerTarget
	action     coding.TeamControlAction
	err        error
}

type teamPanelWorker struct {
	member        coding.TeamMemberView
	attempt       coding.TeamAttemptView
	child         childSummary
	hasAttempt    bool
	hasChild      bool
	taskTitle     string
	originalIndex int
	priority      int
}

func (m *Model) currentTeamPanelView() (coding.TeamView, bool) {
	m.ensureTeamProjection()
	if teamID := currentTeamRouteID(m.state); teamID != "" {
		if view, ok := m.teamProjection.views[teamID]; ok {
			return view.Clone(), true
		}
	}

	for _, teamID := range slices.Backward(m.teamProjection.viewOrder) {
		if view, ok := m.teamProjection.views[teamID]; ok {
			return view.Clone(), true
		}
	}

	return coding.TeamView{}, false
}

func teamPanelWorkers(view coding.TeamView) []teamPanelWorker {
	children := mergeTeamWorkerChildren(nil, view)
	workers := make([]teamPanelWorker, 0, len(view.Members))
	for index, member := range view.Members {
		if member.ID == view.LeadMemberID {
			continue
		}

		workers = append(workers, teamPanelWorkerForMember(view, children, member, index))
	}

	sort.SliceStable(workers, func(left, right int) bool {
		if workers[left].priority != workers[right].priority {
			return workers[left].priority < workers[right].priority
		}

		return workers[left].originalIndex < workers[right].originalIndex
	})

	return workers
}

func teamPanelWorkerForMember(
	view coding.TeamView,
	children []childSummary,
	member coding.TeamMemberView,
	index int,
) teamPanelWorker {
	worker := teamPanelWorker{member: member, originalIndex: index, priority: 4}
	worker.attempt, worker.hasAttempt = latestTeamPanelAttempt(view.Attempts, member.ID)
	worker.child, worker.hasChild = latestTeamPanelChild(children, member.ID)
	if worker.hasAttempt {
		if worker.hasChild && worker.child.worker.Target != worker.attempt.Target {
			worker.child = childSummary{}
			worker.hasChild = false
		}
		worker.taskTitle = teamPanelTaskTitle(view, worker.attempt.Target.TaskID)
		worker.priority = teamPanelWorkerPriority(worker.attempt)

		return worker
	}
	if worker.hasChild {
		worker.attempt = worker.child.worker
		worker.hasAttempt = true
		worker.taskTitle = worker.child.taskTitle
		worker.priority = teamPanelWorkerPriority(worker.attempt)
	}

	return worker
}

func latestTeamPanelAttempt(
	attempts []coding.TeamAttemptView,
	memberID team.MemberID,
) (coding.TeamAttemptView, bool) {
	var latest coding.TeamAttemptView
	found := false
	for _, attempt := range attempts {
		if attempt.Target.MemberID == memberID &&
			(!found || newerTeamPanelAttempt(attempt, latest)) {
			latest = attempt
			found = true
		}
	}

	return latest, found
}

func latestTeamPanelChild(
	children []childSummary,
	memberID team.MemberID,
) (childSummary, bool) {
	var latest childSummary
	found := false
	for _, child := range children {
		if child.worker.Target.MemberID == memberID &&
			(!found || newerTeamPanelChild(child, latest)) {
			latest = child
			found = true
		}
	}

	return latest, found
}

func newerTeamPanelAttempt(candidate, current coding.TeamAttemptView) bool {
	if candidate.Number != current.Number {
		return candidate.Number > current.Number
	}

	return candidate.StartedAt.After(current.StartedAt)
}

func teamPanelTaskTitle(view coding.TeamView, taskID team.TaskID) string {
	for _, taskValue := range view.Tasks {
		if taskValue.ID == taskID {
			return taskValue.Title
		}
	}

	return ""
}

func newerTeamPanelChild(candidate, current childSummary) bool {
	if candidate.worker.Number != current.worker.Number {
		return candidate.worker.Number > current.worker.Number
	}

	return candidate.createdAt.After(current.createdAt)
}

func teamPanelWorkerPriority(attempt coding.TeamAttemptView) int {
	switch {
	case teamAttemptNeedsInteraction(attempt):
		return 0
	case attempt.LifecycleState == coding.TeamLifecycleFailed ||
		attempt.DomainState == team.AttemptStatusFailed:
		return 1
	case attempt.LifecycleState == coding.TeamLifecycleRunning ||
		attempt.LifecycleState == coding.TeamLifecycleCapturing ||
		attempt.DomainState == team.AttemptStatusRunning:
		return 2
	default:
		return 3
	}
}

func (m *Model) updateTeamPanelKey(message tea.KeyPressMsg) (tea.Cmd, bool) {
	view, visible := m.currentTeamPanelView()
	if !visible || len(teamPanelWorkers(view)) == 0 {
		m.teamPanel = teamPanelState{}

		return nil, false
	}

	key := message.String()
	if key == keyCtrlT {
		return m.toggleTeamPanelTasks(), true
	}
	if !m.teamPanel.isFocused {
		return m.focusTeamPanel(key)
	}

	workers := teamPanelWorkers(view)
	m.teamPanel.cursor = min(m.teamPanel.cursor, len(workers)-1)
	if m.teamPanel.isConfirming {
		return m.updateTeamPanelConfirmationKey(view, key), true
	}
	if m.teamPanel.isTaskFocused {
		return m.updateTeamPanelTaskKey(view, key)
	}

	return m.updateTeamPanelWorkerKey(view, workers, key), true
}

func (m *Model) toggleTeamPanelTasks() tea.Cmd {
	m.teamPanel.showTasks = !m.teamPanel.showTasks
	m.teamPanel.isTaskFocused = m.teamPanel.showTasks
	m.teamPanel.isPeeking = false
	m.teamPanel.isConfirming = false
	m.teamPanel.controlErr = nil
	if m.teamPanel.showTasks {
		m.teamPanel.isFocused = true
		m.composer.Blur()
	}

	return nil
}

func (m *Model) focusTeamPanel(key string) (tea.Cmd, bool) {
	if key != keyTab || m.composer.Value() != "" {
		return nil, false
	}

	m.teamPanel.isFocused = true
	m.teamPanel.isPeeking = false
	m.teamPanel.controlErr = nil
	m.composer.Blur()

	return nil, true
}

func (m *Model) updateTeamPanelConfirmationKey(
	view coding.TeamView,
	key string,
) tea.Cmd {
	switch key {
	case keyEscape:
		m.teamPanel.isConfirming = false
		m.teamPanel.controlAction = ""
	case keyEnter:
		return m.submitTeamPanelControl(view)
	}

	return nil
}

func (m *Model) updateTeamPanelWorkerKey(
	view coding.TeamView,
	workers []teamPanelWorker,
	key string,
) tea.Cmd {
	if command, handled := m.updateTeamPanelNavigationKey(view, workers, key); handled {
		return command
	}

	worker := workers[m.teamPanel.cursor]
	switch key {
	case keyEnter:
		if !worker.hasChild {
			return nil
		}
		m.teamPanel.isFocused = false
		m.teamPanel.isPeeking = false

		return m.openChildRoute(worker.child)
	case "x":
		if !worker.hasAttempt || !teamPanelAttemptLive(worker.attempt) ||
			m.teamPanel.isControlling {
			return nil
		}

		return m.interruptTeamPanelWorker(worker.attempt.Target)
	case "g":
		m.teamPanel.isFocused = false
		m.teamPanel.isPeeking = false

		return m.openTeamPanelIntegration(view)
	case "C":
		return m.confirmTeamPanelControl(view, coding.TeamControlCancelTeam)
	}

	return nil
}

func (m *Model) updateTeamPanelNavigationKey(
	view coding.TeamView,
	workers []teamPanelWorker,
	key string,
) (tea.Cmd, bool) {
	switch key {
	case "up", "k":
		m.teamPanel.cursor = wrapIndex(m.teamPanel.cursor-1, len(workers))
	case keyDown, "j":
		m.teamPanel.cursor = wrapIndex(m.teamPanel.cursor+1, len(workers))
	case keyTab:
		if m.teamPanel.showTasks && len(view.Tasks) > 0 {
			m.teamPanel.isTaskFocused = true
		} else {
			m.teamPanel.cursor = wrapIndex(m.teamPanel.cursor+1, len(workers))
		}
	case " ", keySpace:
		m.teamPanel.isPeeking = !m.teamPanel.isPeeking
	case keyEscape:
		if m.teamPanel.isPeeking {
			m.teamPanel.isPeeking = false
		} else {
			m.teamPanel.isFocused = false

			return m.composer.Focus(), true
		}
	default:
		return nil, false
	}

	return nil, true
}

func (m *Model) updateTeamPanelTaskKey(
	view coding.TeamView,
	key string,
) (tea.Cmd, bool) {
	if len(view.Tasks) == 0 {
		m.teamPanel.isTaskFocused = false

		return nil, true
	}

	m.teamPanel.taskCursor = min(m.teamPanel.taskCursor, len(view.Tasks)-1)
	switch key {
	case "up", "k":
		m.teamPanel.taskCursor = wrapIndex(m.teamPanel.taskCursor-1, len(view.Tasks))
	case keyDown, "j", keyTab:
		m.teamPanel.taskCursor = wrapIndex(m.teamPanel.taskCursor+1, len(view.Tasks))
	case keyEscape:
		m.teamPanel.showTasks = false
		m.teamPanel.isTaskFocused = false
	case "x":
		return m.confirmTeamPanelControl(view, coding.TeamControlCancelTask), true
	case "r":
		return m.confirmTeamPanelControl(view, coding.TeamControlRetryTask), true
	case "C":
		return m.confirmTeamPanelControl(view, coding.TeamControlCancelTeam), true
	case "g":
		m.teamPanel.isFocused = false
		m.teamPanel.isTaskFocused = false

		return m.openTeamPanelIntegration(view), true
	}

	return nil, true
}

func (m *Model) confirmTeamPanelControl(
	view coding.TeamView,
	action coding.TeamControlAction,
) tea.Cmd {
	if _, err := m.teamPanelControlRequest(view, action); err != nil {
		m.teamPanel.controlErr = err
		m.teamPanel.isConfirming = false

		return nil
	}

	m.teamPanel.controlAction = action
	m.teamPanel.isConfirming = true
	m.teamPanel.controlErr = nil

	return nil
}

func (m *Model) teamPanelControlRequest(
	view coding.TeamView,
	action coding.TeamControlAction,
) (coding.TeamControlRequest, error) {
	if m.controller.Mode().Current != coding.ModeAgent {
		return coding.TeamControlRequest{}, newTeamRoutePresentationError(
			"team controls are read-only in Plan Mode",
		)
	}
	if view.TeamID == "" {
		return coding.TeamControlRequest{}, newTeamRoutePresentationError(
			"team state is not available",
		)
	}

	request := coding.TeamControlRequest{TeamID: view.TeamID, Action: action}
	if action == coding.TeamControlCancelTeam {
		if view.Status != team.StatusActive {
			return coding.TeamControlRequest{}, newTeamRoutePresentationError(
				"the Team is not cancellable",
			)
		}

		return request, nil
	}
	if m.teamPanel.taskCursor < 0 || m.teamPanel.taskCursor >= len(view.Tasks) {
		return coding.TeamControlRequest{}, newTeamRoutePresentationError(
			"select a Team task first",
		)
	}

	taskValue := view.Tasks[m.teamPanel.taskCursor]
	request.TaskID = taskValue.ID

	return validateTeamRouteTaskControl(request, taskValue)
}

func (m *Model) submitTeamPanelControl(view coding.TeamView) tea.Cmd {
	request, err := m.teamPanelControlRequest(view, m.teamPanel.controlAction)
	if err != nil {
		m.teamPanel.controlErr = err
		m.teamPanel.isConfirming = false

		return nil
	}
	if m.teamPanel.isControlling {
		return nil
	}

	m.teamPanel.isControlling = true
	m.teamPanel.isConfirming = false
	m.teamPanel.controlErr = nil
	m.teamPanel.controlSequence++
	generation := m.teamPanel.controlSequence
	sessionID := m.state.SessionID
	controller := m.controller
	ctx := m.ctx

	command := func() tea.Msg {
		_, controlErr := controller.SubmitTeamControl(ctx, request)

		return teamPanelControlResultMsg{
			sessionID: sessionID, generation: generation,
			teamID: request.TeamID, action: request.Action, err: controlErr,
		}
	}
	if request.Action == coding.TeamControlRetryTask {
		return tea.Batch(command, m.activity.Tick())
	}

	return command
}

func (m *Model) interruptTeamPanelWorker(target coding.TeamWorkerTarget) tea.Cmd {
	if target == (coding.TeamWorkerTarget{}) || m.teamPanel.isControlling {
		return nil
	}
	if m.controller.Mode().Current != coding.ModeAgent {
		m.teamPanel.controlErr = newTeamRoutePresentationError("team controls are read-only in Plan Mode")

		return nil
	}

	m.teamPanel.isControlling = true
	m.teamPanel.controlErr = nil
	m.teamPanel.controlSequence++
	generation := m.teamPanel.controlSequence
	sessionID := m.state.SessionID
	controller := m.controller
	ctx := m.ctx

	return func() tea.Msg {
		_, err := controller.SubmitTeamControl(ctx, coding.TeamControlRequest{
			TeamID: target.TeamID, Action: coding.TeamControlInterruptAttempt,
			MemberID: target.MemberID, TaskID: target.TaskID,
			ExpectedAttemptID: target.AttemptID,
			OwnerGeneration:   target.OwnerGeneration,
		})

		return teamPanelControlResultMsg{
			sessionID: sessionID, generation: generation,
			teamID: target.TeamID, target: target,
			action: coding.TeamControlInterruptAttempt, err: err,
		}
	}
}

func (m *Model) applyTeamPanelControl(message teamPanelControlResultMsg) tea.Cmd {
	if message.sessionID != m.state.SessionID ||
		message.generation != m.teamPanel.controlSequence {
		return nil
	}

	m.teamPanel.isControlling = false
	m.teamPanel.controlErr = message.err
	m.teamPanel.controlAction = ""
	if message.err != nil || message.teamID == "" {
		return nil
	}

	return m.readTeamProjection(message.teamID)
}

func (m *Model) teamPanelRetryInFlight() bool {
	return m.teamPanel.isControlling &&
		m.teamPanel.controlAction == coding.TeamControlRetryTask
}

func (m *Model) teamPanelRetryActivityLine() string {
	if !m.teamPanelRetryInFlight() {
		return ""
	}

	return m.activity.View(activityStatus{
		kind: activityWorking, label: "Retrying Task…",
	}, m.theme, m.options.NoColor)
}

func (m *Model) teamPanelView() string {
	view, ok := m.currentTeamPanelView()
	if !ok {
		return ""
	}

	workers := teamPanelWorkers(view)
	if len(workers) == 0 {
		return ""
	}
	cursor := min(m.teamPanel.cursor, len(workers)-1)
	completed, total := teamPanelTaskProgress(view.Tasks)
	lines := []string{m.teamRouteStyle(
		fmt.Sprintf("  Team · %d Workers · Tasks %d/%d", len(workers), completed, total),
		teamRouteTonePrimary,
		true,
	)}
	for index, worker := range workers {
		lines = append(lines, m.teamPanelWorkerLines(worker, index == cursor)...)
	}
	if m.teamPanel.isPeeking {
		lines = append(lines, m.teamPanelPeekLines(workers[cursor])...)
	}
	if m.teamPanel.showTasks {
		lines = append(lines, m.teamPanelTaskLines(view)...)
	}
	if m.teamPanel.isConfirming {
		lines = append(lines,
			"",
			m.teamRouteStyle(
				"  Confirm · "+teamPanelControlLabel(m.teamPanel.controlAction),
				teamRouteToneWarning,
				true,
			),
			m.teamRouteFooter("Enter confirm durable intent · Esc back"),
		)
	}
	if activity := m.teamPanelRetryActivityLine(); activity != "" {
		lines = append(lines, "", "  "+activity)
	}
	if m.teamPanel.controlErr != nil {
		message := "Error: unable to apply the selected Team control"
		var presentation *teamRoutePresentationError
		if errors.As(m.teamPanel.controlErr, &presentation) {
			message = "Error: " + presentation.Error()
		}
		lines = append(lines, m.teamRouteStyle(
			"  "+message,
			teamRouteToneError,
			false,
		))
	}

	hint := "Tab focus Workers · Ctrl+T tasks"
	if m.teamPanel.isFocused {
		hint = "↑/↓ Worker · Space peek · Enter open · x interrupt · g integrate · Esc Composer · Ctrl+T tasks"
	}
	if m.teamPanel.isTaskFocused {
		hint = "↑/↓ Task · x cancel · r retry · C cancel Team · g integrate · Esc close tasks"
	}
	lines = append(lines, m.teamRouteFooter(hint))

	return fitScrollableContent(
		strings.Join(lines, "\n"),
		max(1, m.width),
		max(1, len(lines)),
		0,
	)
}

func teamPanelControlLabel(action coding.TeamControlAction) string {
	switch action {
	case coding.TeamControlCancelTask:
		return "cancel the selected Task"
	case coding.TeamControlRetryTask:
		return "retry the selected failed Task"
	case coding.TeamControlCancelTeam:
		return "cancel the entire Team"
	default:
		return "apply the selected Team control"
	}
}

func (m *Model) openTeamPanelIntegration(view coding.TeamView) tea.Cmd {
	cloned := view.Clone()

	return m.requestRouteOpen(routeOpenRequest{
		kind: routeTeam, teamView: &cloned, teamIntegration: true,
	})
}

func (m *Model) teamPanelWorkerLines(worker teamPanelWorker, selected bool) []string {
	marker := "  "
	if selected && m.teamPanel.isFocused {
		marker = "› "
	}
	glyph, label, tone := teamPanelWorkerState(worker)
	name := boundedTeamRouteText(worker.member.Name, teamRoutePrimaryTextBytes)
	primary := fmt.Sprintf("%s%s %s  %s", marker, glyph, name, label)
	lines := []string{m.teamRouteStyle(primary, tone, selected && m.teamPanel.isFocused)}
	if worker.taskTitle != "" && m.width < teamRouteWideWidth {
		lines = append(lines, m.teamRouteMetadata(
			boundedTeamRouteText(worker.taskTitle, teamRoutePrimaryTextBytes),
		))
	}

	return lines
}

func teamPanelWorkerState(worker teamPanelWorker) (string, string, teamRouteTone) {
	if !worker.hasAttempt {
		return "○", "Waiting for assignment", teamRouteToneMuted
	}

	if teamAttemptNeedsInteraction(worker.attempt) {
		if worker.attempt.Activity == coding.TeamActivityAwaitingApproval {
			return "!", "Needs input · approval", teamRouteToneWarning
		}

		return "!", "Needs input · question", teamRouteToneWarning
	}

	return teamPanelAttemptState(worker.attempt)
}

func teamPanelAttemptState(attempt coding.TeamAttemptView) (string, string, teamRouteTone) {
	switch {
	case attempt.LifecycleState == coding.TeamLifecycleFailed ||
		attempt.DomainState == team.AttemptStatusFailed:
		return "✗", "Failed", teamRouteToneError
	case attempt.LifecycleState == coding.TeamLifecycleCapturing:
		return "✻", "Capturing result", teamRouteToneActive
	case attempt.LifecycleState == coding.TeamLifecycleRunning ||
		attempt.DomainState == team.AttemptStatusRunning:
		return "✻", teamPanelActivityLabel(attempt), teamRouteToneActive
	case attempt.LifecycleState == coding.TeamLifecycleCompleted ||
		attempt.DomainState == team.AttemptStatusCompleted:
		return "✓", "Completed", teamRouteToneSuccess
	case attempt.LifecycleState == coding.TeamLifecycleCancelled ||
		attempt.LifecycleState == coding.TeamLifecycleInterrupted ||
		attempt.DomainState == team.AttemptStatusCancelled:
		return "⊘", humanizeTeamProjectionState(string(attempt.LifecycleState)), teamRouteToneMuted
	default:
		return "○", humanizeTeamProjectionState(string(attempt.LifecycleState)), teamRouteToneMuted
	}
}

func teamPanelAttemptLive(attempt coding.TeamAttemptView) bool {
	return attempt.DomainState == team.AttemptStatusRunning &&
		attempt.ResourceState == teamstate.AttemptRunning
}

func teamPanelActivityLabel(attempt coding.TeamAttemptView) string {
	if attempt.Activity == "" {
		return "Working"
	}

	return humanizeTeamProjectionState(string(attempt.Activity))
}

func (m *Model) teamPanelPeekLines(worker teamPanelWorker) []string {
	lines := []string{m.teamRouteStyle("    ┌ Worker peek", teamRouteToneAccent, true)}
	if worker.taskTitle != "" {
		lines = append(lines, m.teamRouteMetadata(
			"Task · "+boundedTeamRouteText(worker.taskTitle, teamRouteDetailTextBytes),
		))
	}
	if !worker.hasAttempt {
		return append(lines, m.teamRouteMetadata("State · waiting for assignment"))
	}

	attempt := worker.attempt
	_, label, _ := teamPanelWorkerState(worker)
	lines = append(lines, m.teamRouteMetadata("State · "+label))
	if attempt.Code != "" {
		lines = append(lines, m.teamRouteMetadata(
			"Code · "+boundedTeamRouteText(humanizeStatusCode(attempt.Code), teamRouteDetailTextBytes),
		))
	}
	if attempt.DurationMillis > 0 {
		lines = append(lines, m.teamRouteMetadata(
			"Elapsed · "+formatInteractionDuration(attempt.DurationMillis),
		))
	}

	return lines
}

func (m *Model) teamPanelTaskLines(view coding.TeamView) []string {
	lines := []string{"", m.teamRouteSection(fmt.Sprintf("Tasks · %d", len(view.Tasks)))}
	ordinals := make(map[team.TaskID]int, len(view.Tasks))
	for index, taskValue := range view.Tasks {
		ordinals[taskValue.ID] = index + 1
	}

	start, end := teamPanelTaskWindow(len(view.Tasks), m.teamPanel.taskCursor)
	if start > 0 {
		lines = append(lines, m.teamRouteMetadata(fmt.Sprintf("… %d earlier tasks", start)))
	}
	for index, taskValue := range view.Tasks[start:end] {
		absoluteIndex := start + index
		marker := ""
		if m.teamPanel.isTaskFocused && absoluteIndex == m.teamPanel.taskCursor {
			marker = "› "
		}
		metadata := teamRouteMemberName(&view, taskValue.AssignedMemberID)
		if len(taskValue.DependencyIDs) > 0 {
			dependencies := make([]string, 0, len(taskValue.DependencyIDs))
			for _, dependencyID := range taskValue.DependencyIDs {
				if ordinal := ordinals[dependencyID]; ordinal > 0 {
					dependencies = append(dependencies, fmt.Sprintf("T%d", ordinal))
				}
			}
			if len(dependencies) > 0 {
				metadata += " · after " + strings.Join(dependencies, ", ")
			}
		}
		lines = append(lines,
			m.teamRouteEntityRow(
				marker,
				fmt.Sprintf("T%d", absoluteIndex+1),
				boundedTeamRouteText(taskValue.Title, teamRoutePrimaryTextBytes)+
					"  "+humanizeTeamProjectionState(string(taskValue.Status)),
				string(taskValue.Status),
			),
			m.teamRouteMetadata(metadata),
		)
	}
	if omitted := len(view.Tasks) - end; omitted > 0 {
		lines = append(lines, m.teamRouteMetadata(fmt.Sprintf("… %d later tasks", omitted)))
	}

	return lines
}

func teamPanelTaskWindow(taskCount, cursor int) (int, int) {
	if taskCount <= maximumTeamPanelTasks {
		return 0, taskCount
	}

	boundedCursor := min(max(cursor, 0), taskCount-1)
	start := boundedCursor - maximumTeamPanelTasks/2
	start = min(max(start, 0), taskCount-maximumTeamPanelTasks)

	return start, start + maximumTeamPanelTasks
}

func teamPanelTaskProgress(tasks []coding.TeamTaskView) (int, int) {
	completed := 0
	for _, taskValue := range tasks {
		if taskValue.Status == team.TaskStatusCompleted {
			completed++
		}
	}

	return completed, len(tasks)
}
