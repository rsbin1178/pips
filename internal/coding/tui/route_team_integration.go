//nolint:wsl_v5 // Recovery and Integration confirmation state stays beside exact dispatch.
package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/agent/team"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/teamintegration"
	"github.com/rsbin1178/pips/internal/coding/teamstate"
)

func (m *Model) activateTeamRecoveryReview(
	values []coding.TeamRecoveryCandidate,
) tea.Cmd {
	m.routeSeq++
	state := &teamRouteState{
		stage:    teamRouteRecovery,
		recovery: cloneTeamRouteRecovery(values),
	}
	m.route = routeState{kind: routeTeam, generation: m.routeSeq, team: state}
	m.composer.Blur()
	m.setLayout()

	return nil
}

func (m *Model) activateTeamIntegrationRoute(view coding.TeamView) tea.Cmd {
	m.routeSeq++
	cloned := view.Clone()
	m.route = routeState{
		kind: routeTeam, generation: m.routeSeq,
		team: &teamRouteState{
			stage: teamRouteActive, teamID: cloned.TeamID, view: &cloned,
		},
	}
	m.composer.Blur()
	m.setLayout()

	return m.openTeamIntegrationRoute()
}

func (m *Model) discoverTeamRouteRecovery() tea.Cmd {
	ctx, generation, operation, ok := m.beginTeamRouteOperation(
		teamRouteOperationDiscoverRecovery,
	)
	if !ok {
		return nil
	}
	controller := m.controller

	return m.withTeamRouteActivity(func() tea.Msg {
		values, err := controller.DiscoverTeamRecovery(ctx)

		return teamRouteResultMsg{
			generation: generation, operation: operation,
			kind: teamRouteOperationDiscoverRecovery, recovery: values, err: err,
		}
	})
}

func (m *Model) applyDiscoveredTeamRecovery(message teamRouteResultMsg) tea.Cmd {
	state := m.route.team
	if message.err != nil {
		state.stage = teamRouteObjective
		m.route.err = message.err

		return m.composer.Focus()
	}

	state.recovery = cloneTeamRouteRecovery(message.recovery)
	m.route.cursor = 0
	m.route.err = nil
	if len(state.recovery) > 0 {
		state.stage = teamRouteRecovery

		return nil
	}
	if state.objective != "" {
		return m.generateTeamRouteProposal()
	}

	state.stage = teamRouteObjective

	return m.composer.Focus()
}

func cloneTeamRouteRecovery(
	values []coding.TeamRecoveryCandidate,
) []coding.TeamRecoveryCandidate {
	cloned := slices.Clone(values)
	for index := range cloned {
		cloned[index].Attempts = slices.Clone(cloned[index].Attempts)
		cloned[index].Diagnostics = slices.Clone(cloned[index].Diagnostics)
	}

	return cloned
}

func (m *Model) updateTeamRecoveryKey(key string) (tea.Model, tea.Cmd) {
	state := m.route.team
	switch key {
	case keyEscape, keyCtrlC:
		return m, m.closeRouteToParent()
	case "up", "k":
		m.route.cursor = wrapIndex(m.route.cursor-1, len(state.recovery))
	case keyDown, "j", keyTab:
		m.route.cursor = wrapIndex(m.route.cursor+1, len(state.recovery))
	case "r":
		return m, m.discoverTeamRouteRecovery()
	case keyEnter:
		candidate, ok := selectedTeamRecovery(state.recovery, m.route.cursor)
		if !ok || candidate.Disposition != coding.TeamRecoveryResume {
			m.route.err = newTeamRoutePresentationError(
				"the selected Team is blocked and cannot be resumed automatically",
			)

			return m, nil
		}
		state.stage = teamRouteRecoveryConfirmation
		state.retryWork = false
		m.route.err = nil
	}

	return m, nil
}

func (m *Model) updateTeamRecoveryConfirmationKey(key string) (tea.Model, tea.Cmd) {
	state := m.route.team
	if key == keyEscape || key == keyCtrlC {
		state.stage = teamRouteRecovery
		m.route.cursor = 0
		m.route.err = nil

		return m, nil
	}

	candidate, ok := selectedTeamRecovery(state.recovery, m.route.cursor)
	if !ok {
		state.stage = teamRouteRecovery

		return m, nil
	}
	choiceCount := 1
	if teamRecoveryMayRetry(candidate) {
		choiceCount = 2
	}
	switch key {
	case "up", "k":
		state.retryWork = wrapIndex(boolIndex(state.retryWork)-1, choiceCount) == 1
	case keyDown, "j", keyTab:
		state.retryWork = wrapIndex(boolIndex(state.retryWork)+1, choiceCount) == 1
	case keyEnter:
		return m, m.resumeTeamRouteRecovery(candidate, state.retryWork)
	}

	return m, nil
}

func boolIndex(value bool) int {
	if value {
		return 1
	}

	return 0
}

func selectedTeamRecovery(
	values []coding.TeamRecoveryCandidate,
	cursor int,
) (coding.TeamRecoveryCandidate, bool) {
	if cursor < 0 || cursor >= len(values) {
		return coding.TeamRecoveryCandidate{}, false
	}

	return values[cursor], true
}

func teamRecoveryMayRetry(value coding.TeamRecoveryCandidate) bool {
	for _, attempt := range value.Attempts {
		if attempt.RetryWork {
			return true
		}
	}

	return false
}

func (m *Model) resumeTeamRouteRecovery(
	candidate coding.TeamRecoveryCandidate,
	retryWork bool,
) tea.Cmd {
	ctx, generation, operation, ok := m.beginTeamRouteOperation(
		teamRouteOperationResumeRecovery,
	)
	if !ok {
		return nil
	}
	controller := m.controller

	return m.withTeamRouteActivity(func() tea.Msg {
		reference, err := controller.ResumeTeam(ctx, candidate.TeamID, coding.TeamResumeDecision{
			ExpectedResourceRevision: candidate.ResourceRevision,
			RetryInterruptedWork:     retryWork,
		})
		if err != nil {
			return teamRouteResultMsg{
				generation: generation, operation: operation,
				kind: teamRouteOperationResumeRecovery, err: err,
			}
		}
		view, readErr := controller.ReadTeam(ctx, coding.TeamReadRequest{TeamID: reference.TeamID})

		return teamRouteResultMsg{
			generation: generation, operation: operation,
			kind: teamRouteOperationResumeRecovery, reference: reference,
			view: view, err: readErr,
		}
	})
}

func (m *Model) applyResumedTeamRecovery(message teamRouteResultMsg) tea.Cmd {
	state := m.route.team
	if message.reference.TeamID == "" {
		state.stage = teamRouteRecovery
		m.route.err = message.err

		return nil
	}

	state.recovery = nil
	state.teamID = message.reference.TeamID
	state.stage = teamRouteActive
	state.retryWork = false
	if message.view.TeamID != "" {
		view := m.mergeTeamRouteView(message.view)
		state.view = &view
	}
	m.route.cursor = 0
	m.route.err = message.err

	return m.closeRouteToParent()
}

func (m *Model) refreshTeamRouteForStage() tea.Cmd {
	if m.route.kind != routeTeam || m.route.team == nil {
		return nil
	}
	if m.route.loading {
		m.route.team.refreshPending = true

		return nil
	}

	switch m.route.team.stage {
	case teamRouteRecovery, teamRouteRecoveryConfirmation:
		return m.discoverTeamRouteRecovery()
	case teamRouteIntegration, teamRouteIntegrationPreview,
		teamRouteIntegrationConfirmation:
		m.route.team.integration = nil

		return m.loadTeamRouteIntegration()
	default:
		return m.readTeamRoute()
	}
}

func (m *Model) openTeamIntegrationRoute() tea.Cmd {
	state := m.route.team
	if state == nil || state.view == nil {
		m.route.err = newTeamRoutePresentationError("team state is not available")

		return nil
	}

	state.stage = teamRouteIntegration
	state.integrationAction = teamRouteIntegrationActionNone
	state.integrationTasks = make(map[team.TaskID]bool)
	for _, taskValue := range completedTeamRouteTasks(state.view) {
		state.integrationTasks[taskValue.ID] = true
	}
	m.route.cursor = 0
	m.route.err = nil

	return m.loadTeamRouteIntegration()
}

func (m *Model) loadTeamRouteIntegration() tea.Cmd {
	state := m.route.team
	if state == nil || state.teamID == "" {
		return nil
	}
	ctx, generation, operation, ok := m.beginTeamRouteOperation(
		teamRouteOperationLoadIntegration,
	)
	if !ok {
		return nil
	}
	controller := m.controller
	teamID := state.teamID

	return m.withTeamRouteActivity(func() tea.Msg {
		recoveries, err := controller.TeamIntegrationRecoveries(ctx)
		if err != nil {
			return teamRouteResultMsg{
				generation: generation, operation: operation,
				kind: teamRouteOperationLoadIntegration, err: err,
			}
		}
		view, readErr := controller.ReadTeam(ctx, coding.TeamReadRequest{TeamID: teamID})

		return teamRouteResultMsg{
			generation: generation, operation: operation,
			kind: teamRouteOperationLoadIntegration,
			view: view, recoveries: recoveries, err: readErr,
		}
	})
}

func (m *Model) applyLoadedTeamIntegration(message teamRouteResultMsg) tea.Cmd {
	state := m.route.team
	if message.view.TeamID != "" {
		view := m.mergeTeamRouteView(message.view)
		state.view = &view
		state.teamID = view.TeamID
	}
	state.recoveries = slices.Clone(message.recoveries)
	state.stage = teamRouteIntegration
	state.integrationAction = teamRouteIntegrationActionNone
	m.route.cursor = min(m.route.cursor, max(0, m.teamIntegrationRowCount()-1))
	m.route.err = message.err

	return nil
}

func completedTeamRouteTasks(view *coding.TeamView) []coding.TeamTaskView {
	if view == nil {
		return nil
	}
	values := make([]coding.TeamTaskView, 0, len(view.Tasks))
	for _, taskValue := range view.Tasks {
		if taskValue.Status == team.TaskStatusCompleted {
			values = append(values, taskValue)
		}
	}

	return values
}

func (m *Model) teamIntegrationRowCount() int {
	state := m.route.team
	if len(state.recoveries) > 0 {
		return len(state.recoveries)
	}

	return len(completedTeamRouteTasks(state.view))
}

func (m *Model) updateTeamIntegrationKey(key string) (tea.Model, tea.Cmd) {
	state := m.route.team
	if len(state.recoveries) > 0 {
		return m.updateTeamIntegrationRecoveryKey(key)
	}

	return m.updateTeamIntegrationSelectionKey(key)
}

func (m *Model) updateTeamIntegrationRecoveryKey(key string) (tea.Model, tea.Cmd) {
	state := m.route.team
	rows := m.teamIntegrationRowCount()
	switch key {
	case keyEscape, keyCtrlC:
		return m, m.closeRouteToParent()
	case "up", "k":
		m.route.cursor = wrapIndex(m.route.cursor-1, rows)
	case keyDown, "j", keyTab:
		m.route.cursor = wrapIndex(m.route.cursor+1, rows)
	case "r":
		return m, m.loadTeamRouteIntegration()
	case "v":
		if state.integration != nil {
			state.stage = teamRouteIntegrationPreview
		}
	case "c":
		state.integrationAction = teamRouteIntegrationActionComplete
		state.stage = teamRouteIntegrationConfirmation
	case "b":
		state.integrationAction = teamRouteIntegrationActionRollback
		state.stage = teamRouteIntegrationConfirmation
	}

	return m, nil
}

func (m *Model) updateTeamIntegrationSelectionKey(key string) (tea.Model, tea.Cmd) {
	state := m.route.team
	tasks := completedTeamRouteTasks(state.view)
	switch key {
	case keyEscape, keyCtrlC:
		return m, m.closeRouteToParent()
	case "up", "k":
		m.route.cursor = wrapIndex(m.route.cursor-1, len(tasks))
	case keyDown, "j", keyTab:
		m.route.cursor = wrapIndex(m.route.cursor+1, len(tasks))
	case "r":
		return m, m.loadTeamRouteIntegration()
	case "v":
		if state.integration != nil {
			state.stage = teamRouteIntegrationPreview
		}
	case "d":
		return m.openTeamRouteCleanupConfirmation()
	case " ", keySpace:
		if m.route.cursor >= 0 && m.route.cursor < len(tasks) {
			id := tasks[m.route.cursor].ID
			state.integrationTasks[id] = !state.integrationTasks[id]
		}
	case "p", keyEnter:
		if state.integration != nil {
			state.stage = teamRouteIntegrationPreview

			return m, nil
		}

		return m, m.prepareTeamRouteIntegration()
	}

	return m, nil
}

func (m *Model) openTeamRouteCleanupConfirmation() (tea.Model, tea.Cmd) {
	state := m.route.team
	if state.integration != nil {
		m.route.err = newTeamRoutePresentationError(
			"reject the pending Integration preview before cleanup",
		)

		return m, nil
	}
	state.integrationAction = teamRouteIntegrationActionCleanup
	state.stage = teamRouteIntegrationConfirmation
	m.route.err = nil

	return m, nil
}

func (m *Model) prepareTeamRouteIntegration() tea.Cmd {
	state := m.route.team
	if m.controller.Mode().Current != coding.ModeAgent {
		m.route.err = newTeamRoutePresentationError(
			"integration preparation requires Agent Mode; switch mode before continuing",
		)

		return nil
	}
	tasks := completedTeamRouteTasks(state.view)
	selected := make([]team.TaskID, 0, len(tasks))
	for _, taskValue := range tasks {
		if state.integrationTasks[taskValue.ID] {
			selected = append(selected, taskValue.ID)
		}
	}
	if len(selected) == 0 {
		m.route.err = newTeamRoutePresentationError("select at least one completed Task")

		return nil
	}
	if len(selected) == len(tasks) {
		selected = nil
	}

	ctx, generation, operation, ok := m.beginTeamRouteOperation(
		teamRouteOperationPrepareIntegration,
	)
	if !ok {
		return nil
	}
	controller := m.controller

	return m.withTeamRouteActivity(func() tea.Msg {
		preview, err := controller.PrepareTeamIntegration(ctx, coding.TeamIntegrationRequest{
			TaskIDs: selected,
		})

		return teamRouteResultMsg{
			generation: generation, operation: operation,
			kind: teamRouteOperationPrepareIntegration, preview: preview, err: err,
		}
	})
}

func (m *Model) applyPreparedTeamIntegration(message teamRouteResultMsg) tea.Cmd {
	state := m.route.team
	if message.err != nil || message.preview.ID == "" || message.preview.ApprovalToken == "" {
		state.integration = nil
		state.stage = teamRouteIntegration
		m.route.err = message.err
		if m.route.err == nil {
			m.route.err = newTeamRoutePresentationError("integration preview is not approvable")
		}

		return nil
	}

	preview := cloneTeamRouteIntegrationPreview(message.preview)
	state.integration = &preview
	state.stage = teamRouteIntegrationPreview
	m.route.err = nil

	return nil
}

func cloneTeamRouteIntegrationPreview(
	value coding.TeamIntegrationPreview,
) coding.TeamIntegrationPreview {
	value.AttemptIDs = slices.Clone(value.AttemptIDs)
	value.Manifest.Entries = slices.Clone(value.Manifest.Entries)

	return value
}

func (m *Model) updateTeamIntegrationPreviewKey(key string) (tea.Model, tea.Cmd) {
	state := m.route.team
	switch key {
	case keyEscape, keyCtrlC:
		state.stage = teamRouteIntegration
		m.route.err = nil
	case keyEnter:
		state.integrationAction = teamRouteIntegrationActionApply
		state.stage = teamRouteIntegrationConfirmation
	case "x":
		state.integrationAction = teamRouteIntegrationActionReject
		state.stage = teamRouteIntegrationConfirmation
	}

	return m, nil
}

func (m *Model) updateTeamIntegrationConfirmationKey(key string) (tea.Model, tea.Cmd) {
	state := m.route.team
	if key == keyEscape || key == keyCtrlC {
		if state.integrationAction == teamRouteIntegrationActionApply ||
			state.integrationAction == teamRouteIntegrationActionReject {
			state.stage = teamRouteIntegrationPreview
		} else {
			state.stage = teamRouteIntegration
		}
		state.integrationAction = teamRouteIntegrationActionNone
		m.route.err = nil

		return m, nil
	}
	if key != keyEnter {
		return m, nil
	}

	return m, m.submitTeamIntegrationAction()
}

func (m *Model) submitTeamIntegrationAction() tea.Cmd {
	if m.controller.Mode().Current != coding.ModeAgent {
		m.route.err = newTeamRoutePresentationError(
			"integration mutation requires Agent Mode; switch mode before continuing",
		)

		return nil
	}
	dispatch, ok := m.teamIntegrationDispatch()
	if !ok {
		return nil
	}

	ctx, generation, operation, ok := m.beginTeamRouteOperation(dispatch.kind)
	if !ok {
		return nil
	}
	controller := m.controller

	return m.withTeamRouteActivity(func() tea.Msg {
		return dispatchTeamIntegration(ctx, controller, generation, operation, dispatch)
	})
}

type teamIntegrationDispatch struct {
	approval coding.TeamIntegrationApproval
	recovery coding.TeamIntegrationRecoveryRequest
	cleanup  coding.TeamCleanupRequest
	kind     teamRouteOperation
}

func (m *Model) teamIntegrationDispatch() (teamIntegrationDispatch, bool) {
	state := m.route.team
	dispatch := teamIntegrationDispatch{}
	switch state.integrationAction {
	case teamRouteIntegrationActionApply, teamRouteIntegrationActionReject:
		if state.integration == nil {
			return dispatch, false
		}
		dispatch.approval = coding.TeamIntegrationApproval{
			ID: state.integration.ID, Token: state.integration.ApprovalToken,
		}
		dispatch.kind = teamRouteOperationApplyIntegration
		if state.integrationAction == teamRouteIntegrationActionReject {
			dispatch.kind = teamRouteOperationRejectIntegration
		}
	case teamRouteIntegrationActionComplete, teamRouteIntegrationActionRollback:
		candidate, exists := selectedTeamIntegrationRecovery(state.recoveries, m.route.cursor)
		if !exists {
			return dispatch, false
		}
		recoveryAction := teamintegration.RecoveryComplete
		if state.integrationAction == teamRouteIntegrationActionRollback {
			recoveryAction = teamintegration.RecoveryRollback
		}
		dispatch.recovery = coding.TeamIntegrationRecoveryRequest{
			ID: candidate.ID, Action: recoveryAction,
		}
		dispatch.kind = teamRouteOperationRecoverIntegration
	case teamRouteIntegrationActionCleanup:
		if state.view == nil || state.view.TeamID == "" || state.integration != nil {
			return dispatch, false
		}
		dispatch.cleanup = coding.TeamCleanupRequest{
			TeamID:                   state.view.TeamID,
			ExpectedResourceRevision: state.view.ResourceRevision,
			CloseWithoutIntegration:  state.view.ResourceState != teamstate.StateIntegrated,
		}
		dispatch.kind = teamRouteOperationCleanup
	default:
		return dispatch, false
	}

	return dispatch, true
}

func dispatchTeamIntegration(
	ctx context.Context,
	controller Controller,
	generation, operation uint64,
	dispatch teamIntegrationDispatch,
) teamRouteResultMsg {
	var (
		result coding.TeamIntegrationResult
		err    error
	)
	switch dispatch.kind {
	case teamRouteOperationApplyIntegration:
		result, err = controller.ApplyTeamIntegration(ctx, dispatch.approval)
	case teamRouteOperationRejectIntegration:
		err = controller.RejectTeamIntegration(ctx, dispatch.approval)
	case teamRouteOperationRecoverIntegration:
		result, err = controller.RecoverTeamIntegration(ctx, dispatch.recovery)
	case teamRouteOperationCleanup:
		cleanupResult, cleanupErr := controller.CleanupTeam(ctx, dispatch.cleanup)

		return teamRouteResultMsg{
			generation: generation, operation: operation, kind: dispatch.kind,
			cleanupResult: cleanupResult, err: cleanupErr,
		}
	case teamRouteOperationNone, teamRouteOperationGenerate,
		teamRouteOperationRevise, teamRouteOperationConfirm,
		teamRouteOperationRead, teamRouteOperationControl,
		teamRouteOperationDecline, teamRouteOperationDiscoverRecovery,
		teamRouteOperationResumeRecovery, teamRouteOperationLoadIntegration,
		teamRouteOperationPrepareIntegration:
	}

	return teamRouteResultMsg{
		generation: generation, operation: operation, kind: dispatch.kind,
		integrationResult: result, err: err,
	}
}

func (m *Model) applyCompletedTeamCleanup(message teamRouteResultMsg) tea.Cmd {
	state := m.route.team
	state.integrationAction = teamRouteIntegrationActionNone
	if message.err != nil {
		state.stage = teamRouteIntegration
		m.route.err = message.err

		return nil
	}

	state.integration = nil
	state.recoveries = nil
	state.view = nil
	state.teamID = ""
	m.route.err = nil

	return m.closeRouteToParent()
}

func selectedTeamIntegrationRecovery(
	values []coding.TeamIntegrationRecovery,
	cursor int,
) (coding.TeamIntegrationRecovery, bool) {
	if cursor < 0 || cursor >= len(values) {
		return coding.TeamIntegrationRecovery{}, false
	}

	return values[cursor], true
}

func (m *Model) applyCompletedTeamIntegrationOperation(
	message teamRouteResultMsg,
) tea.Cmd {
	state := m.route.team
	if message.err != nil {
		m.route.err = message.err
		switch {
		case errors.Is(message.err, teamintegration.ErrStale) ||
			errors.Is(message.err, teamintegration.ErrConsumed):
			state.integration = nil
			state.stage = teamRouteIntegration
		case message.kind == teamRouteOperationRecoverIntegration:
			state.stage = teamRouteIntegration
		default:
			state.stage = teamRouteIntegrationPreview
		}
		state.integrationAction = teamRouteIntegrationActionNone

		return nil
	}

	state.integration = nil
	state.integrationAction = teamRouteIntegrationActionNone
	state.stage = teamRouteIntegration
	m.route.err = nil

	return m.loadTeamRouteIntegration()
}

func (m *Model) teamRecoveryContent() string {
	state := m.route.team
	lines := []string{
		"Team recovery review", "",
		"Discovery is read-only. No Worker, scheduler, model, or Tool starts until confirmation.", "",
	}
	for index, candidate := range state.recovery {
		marker := "  "
		if index == m.route.cursor {
			marker = "> "
		}
		lines = append(lines, fmt.Sprintf(
			"%sRetained Team · %s · %s · %d Attempts",
			marker, candidate.ResourceState, candidate.Disposition, len(candidate.Attempts),
		))
		if candidate.PendingControls > 0 || candidate.InterruptedControl > 0 {
			lines = append(lines, fmt.Sprintf(
				"    controls: %d pending · %d interrupted",
				candidate.PendingControls, candidate.InterruptedControl,
			))
		}
		if len(candidate.Diagnostics) > 0 {
			codes := make([]string, len(candidate.Diagnostics))
			for codeIndex, diagnostic := range candidate.Diagnostics {
				codes[codeIndex] = safeDetailText(diagnostic.Code)
			}
			lines = append(lines, "    review: "+strings.Join(codes, ", "))
		}
	}
	lines = append(lines, "", "↑/↓ choose · Enter review resume · r refresh · Esc close")

	return strings.Join(lines, "\n")
}

func (m *Model) teamRecoveryConfirmationContent() string {
	candidate, ok := selectedTeamRecovery(m.route.team.recovery, m.route.cursor)
	if !ok {
		return "Team recovery candidate unavailable."
	}
	lines := []string{
		"Confirm Team recovery", "",
		"The reviewed resource revision will be checked again before any owner starts.", "",
	}
	choices := []string{"Resume without replaying interrupted Work"}
	if teamRecoveryMayRetry(candidate) {
		choices = append(choices, "Resume and explicitly retry interrupted Work")
	}
	for index, choice := range choices {
		marker := "  "
		if index == boolIndex(m.route.team.retryWork) {
			marker = "> "
		}
		lines = append(lines, marker+choice)
	}
	lines = append(lines, "", "↑/↓ choose · Enter apply exact choice · Esc back")

	return strings.Join(lines, "\n")
}

func (m *Model) teamIntegrationContent() string {
	state := m.route.team
	lines := []string{"Team Integration", ""}
	if len(state.recoveries) > 0 {
		lines = append(lines,
			"Interrupted apply journals require an explicit manager-provided direction.", "",
		)
		for index, candidate := range state.recoveries {
			marker := "  "
			if index == m.route.cursor {
				marker = "> "
			}
			lines = append(lines, fmt.Sprintf(
				"%sRecovery · %s · base %d · target %d · unknown %d · unrelated %d",
				marker, safeDetailText(candidate.State), candidate.Base,
				candidate.Target, candidate.Unknown, candidate.Unrelated,
			))
		}
		lines = append(lines, "", "↑/↓ choose · c complete · b rollback · r refresh · Esc Team")

		return strings.Join(lines, "\n")
	}

	lines = append(lines,
		"Select completed Tasks. The preview uses captured results and no ad-hoc shell command.", "",
	)
	tasks := completedTeamRouteTasks(state.view)
	if len(tasks) == 0 {
		lines = append(lines, "No completed Tasks are available.")
	}
	for index, taskValue := range tasks {
		marker := "  "
		if index == m.route.cursor {
			marker = "> "
		}
		selected := "[ ]"
		if state.integrationTasks[taskValue.ID] {
			selected = "[x]"
		}
		lines = append(lines, fmt.Sprintf(
			"%s%s %s", marker, selected, safeDetailText(taskValue.Title),
		))
	}
	if state.integration != nil {
		lines = append(lines, "", "A reviewed process-local preview is still pending; press v to return to it.")
	}
	if m.controller.Mode().Current == coding.ModePlan {
		lines = append(lines, "", "Plan Mode is read-only; preview preparation and mutation are disabled.")
	}
	if state.view != nil {
		lines = append(lines, "", fmt.Sprintf(
			"Cleanup: %d eligible · %d retained · %d complete",
			state.view.Cleanup.Eligible, state.view.Cleanup.Retained, state.view.Cleanup.Complete,
		))
	}
	lines = append(lines, "", "↑/↓ choose · Space toggle · p preview · v pending preview · d cleanup/close · r refresh · Esc Team")

	return strings.Join(lines, "\n")
}

func (m *Model) teamIntegrationPreviewContent() string {
	preview := m.route.team.integration
	if preview == nil {
		return "Team Integration preview unavailable."
	}
	verification := string(preview.Verification.Status)
	if verification == "" {
		verification = "not_run"
	}
	lines := []string{
		"Review Team Integration", "",
		fmt.Sprintf("Tasks: %d · files: %d", len(preview.AttemptIDs), len(preview.Manifest.Entries)),
		fmt.Sprintf(
			"Changes: %d added · %d changed · %d deleted · %d binary",
			preview.Manifest.Added, preview.Manifest.Changed,
			preview.Manifest.Deleted, preview.Manifest.Binary,
		),
		"Verification: " + safeDetailText(verification),
		"Parent result: unstaged/untracked Workspace changes",
	}
	if !preview.ExpiresAt.IsZero() {
		lines = append(lines, "Approval expires: "+preview.ExpiresAt.Local().Format("2006-01-02 15:04:05"))
	}
	lines = append(lines, "", "Enter review Apply confirmation · x review Reject confirmation · Esc back")

	return strings.Join(lines, "\n")
}

func (m *Model) teamIntegrationConfirmationContent() string {
	label := "Integration action unavailable"
	switch m.route.team.integrationAction {
	case teamRouteIntegrationActionApply:
		label = "Apply the exact reviewed manifest as unstaged/untracked changes"
	case teamRouteIntegrationActionReject:
		label = "Reject this exact preview and retain its evidence"
	case teamRouteIntegrationActionComplete:
		label = "Complete the selected interrupted apply using manager evidence"
	case teamRouteIntegrationActionRollback:
		label = "Roll back the selected interrupted apply using manager evidence"
	case teamRouteIntegrationActionCleanup:
		if m.route.team.view != nil &&
			m.route.team.view.ResourceState == teamstate.StateIntegrated {
			label = "Clean exact terminal artifacts after the applied Integration"
		} else {
			label = "Close without Integration and clean exact terminal artifacts"
		}
	case teamRouteIntegrationActionNone:
	}

	return strings.Join([]string{
		"Confirm Team Integration", "", "> " + label, "",
		"Enter apply exact choice · Esc back",
	}, "\n")
}

func safeTeamIntegrationError(err error) string {
	if message := safeTeamRecoveryCleanupError(err); message != "" {
		return message
	}

	return safeTeamIntegrationManagerError(err)
}

func safeTeamRecoveryCleanupError(err error) string {
	switch {
	case errors.Is(err, coding.ErrTeamRecoveryStale):
		return "Team recovery state changed; refresh and review it again."
	case errors.Is(err, coding.ErrTeamRecoveryBlocked):
		return "Team recovery is blocked; preserve the retained evidence for review."
	case errors.Is(err, coding.ErrTeamRecoveryDecisionRequired):
		return "Interrupted Work requires a separate explicit retry decision."
	case errors.Is(err, coding.ErrTeamRecoveryNotFound):
		return "The reviewed Team recovery candidate is no longer available."
	case errors.Is(err, coding.ErrTeamIntegrationUnavailable):
		return "Team Integration is no longer available."
	case errors.Is(err, coding.ErrTeamIntegrationSelection):
		return "The selected completed Tasks no longer have one exact captured result closure."
	case errors.Is(err, coding.ErrTeamCleanupStale):
		return "Team cleanup state changed; refresh and review it again."
	case errors.Is(err, coding.ErrTeamCleanupIntegrationRequired):
		return "Apply, recover, or reject the pending Integration before cleanup."
	case errors.Is(err, coding.ErrTeamCleanupRetained):
		return "Cleanup retained dirty or identity-changed resources for explicit review."
	case errors.Is(err, coding.ErrTeamCleanupUnavailable):
		return "Team cleanup is unavailable until all Worker owners are terminal."
	default:
		return ""
	}
}

func safeTeamIntegrationManagerError(err error) string {
	switch {
	case errors.Is(err, teamintegration.ErrStale):
		return "The Integration preview is stale; prepare and review a new preview."
	case errors.Is(err, teamintegration.ErrConsumed):
		return "The Integration preview was already used and cannot be replayed."
	case errors.Is(err, teamintegration.ErrConflict):
		return "Captured Task results conflict; retained evidence requires review."
	case errors.Is(err, teamintegration.ErrRecovery):
		return "Integration recovery cannot converge automatically; retain the journal for review."
	case errors.Is(err, teamintegration.ErrLimit):
		return "Integration evidence exceeds the configured safety limit."
	case errors.Is(err, teamintegration.ErrInvalid):
		return "Integration evidence failed validation."
	default:
		return ""
	}
}
