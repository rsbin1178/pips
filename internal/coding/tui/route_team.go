//nolint:wsl_v5 // The Team route is one explicit keyboard-driven state machine.
package tui

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/runtimecontrol"
)

const (
	maximumTeamRouteInputBytes = 16 << 10
	maximumTeamRouteControls   = 64
	teamRouteCleanupTimeout    = 5 * time.Second
	keyPageUp                  = "pgup"
	keyPageDown                = "pgdown"
)

type teamRouteStage uint8

const (
	teamRouteObjective teamRouteStage = iota
	teamRouteProposal
	teamRouteRevision
	teamRouteConfirmation
	teamRouteRecovery
	teamRouteRecoveryConfirmation
	teamRouteActive
	teamRouteControlInput
	teamRouteControlConfirmation
	teamRouteIntegration
	teamRouteIntegrationPreview
	teamRouteIntegrationConfirmation
)

type teamRouteOperation uint8

const (
	teamRouteOperationNone teamRouteOperation = iota
	teamRouteOperationGenerate
	teamRouteOperationRevise
	teamRouteOperationConfirm
	teamRouteOperationRead
	teamRouteOperationControl
	teamRouteOperationDecline
	teamRouteOperationDiscoverRecovery
	teamRouteOperationResumeRecovery
	teamRouteOperationLoadIntegration
	teamRouteOperationPrepareIntegration
	teamRouteOperationApplyIntegration
	teamRouteOperationRejectIntegration
	teamRouteOperationRecoverIntegration
	teamRouteOperationCleanup
)

type teamRouteIntegrationAction uint8

const (
	teamRouteIntegrationActionNone teamRouteIntegrationAction = iota
	teamRouteIntegrationActionApply
	teamRouteIntegrationActionReject
	teamRouteIntegrationActionComplete
	teamRouteIntegrationActionRollback
	teamRouteIntegrationActionCleanup
)

type teamRouteState struct {
	stage             teamRouteStage
	objective         string
	proposal          *coding.TeamProposal
	view              *coding.TeamView
	teamID            team.ID
	operation         uint64
	pending           teamRouteOperation
	cancel            context.CancelFunc
	cancelRequested   bool
	refreshPending    bool
	closeAfter        bool
	declineToClose    bool
	control           coding.TeamControlRequest
	recovery          []coding.TeamRecoveryCandidate
	retryWork         bool
	integrationTasks  map[team.TaskID]bool
	integration       *coding.TeamIntegrationPreview
	recoveries        []coding.TeamIntegrationRecovery
	integrationAction teamRouteIntegrationAction
}

type teamRouteResultMsg struct {
	generation        uint64
	operation         uint64
	kind              teamRouteOperation
	proposal          coding.TeamProposal
	reference         coding.TeamReference
	control           coding.TeamControlReference
	view              coding.TeamView
	recovery          []coding.TeamRecoveryCandidate
	preview           coding.TeamIntegrationPreview
	recoveries        []coding.TeamIntegrationRecovery
	integrationResult coding.TeamIntegrationResult
	cleanupResult     coding.TeamCleanupResult
	err               error
}

type teamRoutePresentationError struct {
	message string
}

func (e *teamRoutePresentationError) Error() string {
	if e == nil {
		return "Team route error"
	}

	return e.message
}

func newTeamRoutePresentationError(message string) error {
	return &teamRoutePresentationError{message: message}
}

func (m *Model) openTeamRoute(objective string) tea.Cmd {
	return m.requestRouteOpen(routeOpenRequest{
		kind: routeTeam, teamObjective: strings.TrimSpace(objective),
	})
}

func (m *Model) activateTeamRoute(objective string) tea.Cmd {
	m.routeSeq++
	state := &teamRouteState{
		stage:     teamRouteObjective,
		objective: strings.TrimSpace(objective),
	}
	m.route = routeState{kind: routeTeam, generation: m.routeSeq, team: state}
	m.composer.SetValue(state.objective)
	m.setLayout()

	if teamID := currentTeamRouteID(m.state); teamID != "" {
		m.ensureTeamProjection()
		m.route = routeState{}
		m.composer.Reset()
		m.teamPanel.isFocused = true
		m.composer.Blur()

		return m.readTeamProjection(teamID)
	}
	if state.objective != "" {
		return m.generateTeamRouteProposal()
	}

	return m.discoverTeamRouteRecovery()
}

func currentTeamRouteID(state coding.State) team.ID {
	for _, value := range slices.Backward(state.Teams) {
		if value.TeamID != "" && value.MemberID == "" && value.TaskID == "" &&
			value.AttemptID == "" && value.State != coding.TeamLifecycleProposed &&
			value.State != coding.TeamLifecycleRecoverable {
			return value.TeamID
		}
	}

	return ""
}

func (m *Model) beginTeamRouteOperation(
	kind teamRouteOperation,
) (context.Context, uint64, uint64, bool) {
	if kind == teamRouteOperationNone || m.route.kind != routeTeam ||
		m.route.team == nil || m.route.loading {
		return nil, 0, 0, false
	}

	state := m.route.team
	state.operation++
	state.pending = kind
	state.cancelRequested = false
	m.route.loading = true
	m.route.err = nil
	ctx, cancel := context.WithCancel(m.ctx)
	state.cancel = cancel

	return ctx, m.route.generation, state.operation, true
}

func (m *Model) withTeamRouteActivity(command tea.Cmd) tea.Cmd {
	if command == nil {
		return nil
	}

	return tea.Batch(command, m.activity.Tick())
}

func (m *Model) generateTeamRouteProposal() tea.Cmd {
	state := m.route.team
	snapshot := m.composer.Snapshot()
	objectivePreview := strings.TrimSpace(snapshot.display)
	if len(objectivePreview) == 0 || len(objectivePreview) > maximumTeamRouteInputBytes {
		m.route.err = newTeamRoutePresentationError("enter a Team objective within the supported limit")

		return m.composer.Focus()
	}
	if m.controller.Mode().Current != coding.ModeAgent {
		m.route.err = newTeamRoutePresentationError("team proposals require Agent Mode; switch mode before continuing")

		return m.composer.Focus()
	}

	ctx, generation, operation, ok := m.beginTeamRouteOperation(teamRouteOperationGenerate)
	if !ok {
		return nil
	}
	state.objective = objectivePreview
	m.composer.Blur()

	return m.withTeamRouteActivity(func() tea.Msg {
		objective, resolveErr := resolveTeamComposerText(
			ctx,
			snapshot,
			m.controller.ResolveWorkspaceFile,
		)
		if resolveErr != nil {
			return teamRouteResultMsg{
				generation: generation, operation: operation,
				kind: teamRouteOperationGenerate, err: resolveErr,
			}
		}
		proposal, err := m.controller.GenerateTeamProposal(ctx, coding.TeamProposalPrompt{
			Objective: objective,
		})

		return teamRouteResultMsg{
			generation: generation, operation: operation,
			kind: teamRouteOperationGenerate, proposal: proposal, err: err,
		}
	})
}

func (m *Model) reviseTeamRouteProposal() tea.Cmd {
	state := m.route.team
	if state.proposal == nil {
		return nil
	}

	snapshot := m.composer.Snapshot()
	feedbackPreview := strings.TrimSpace(snapshot.display)
	if len(feedbackPreview) == 0 || len(feedbackPreview) > maximumTeamRouteInputBytes {
		m.route.err = newTeamRoutePresentationError("enter revision feedback within the supported limit")

		return m.composer.Focus()
	}

	ctx, generation, operation, ok := m.beginTeamRouteOperation(teamRouteOperationRevise)
	if !ok {
		return nil
	}
	proposalID := state.proposal.ID
	m.composer.Blur()

	return m.withTeamRouteActivity(func() tea.Msg {
		feedback, resolveErr := resolveTeamComposerText(
			ctx,
			snapshot,
			m.controller.ResolveWorkspaceFile,
		)
		if resolveErr != nil {
			return teamRouteResultMsg{
				generation: generation, operation: operation,
				kind: teamRouteOperationRevise, err: resolveErr,
			}
		}
		proposal, err := m.controller.ReviseTeamProposal(ctx, proposalID, feedback)

		return teamRouteResultMsg{
			generation: generation, operation: operation,
			kind: teamRouteOperationRevise, proposal: proposal, err: err,
		}
	})
}

func (m *Model) confirmTeamRouteProposal(admission coding.TeamAdmissionMode) tea.Cmd {
	state := m.route.team
	if state.proposal == nil {
		return nil
	}

	ctx, generation, operation, ok := m.beginTeamRouteOperation(teamRouteOperationConfirm)
	if !ok {
		return nil
	}
	proposalID := state.proposal.ID

	return m.withTeamRouteActivity(func() tea.Msg {
		reference, err := m.controller.ConfirmTeam(ctx, coding.TeamConfirmation{
			ProposalID: proposalID,
			Admission:  admission,
		})
		if err != nil {
			return teamRouteResultMsg{
				generation: generation, operation: operation,
				kind: teamRouteOperationConfirm, err: err,
			}
		}

		view, readErr := m.controller.ReadTeam(ctx, coding.TeamReadRequest{TeamID: reference.TeamID})

		return teamRouteResultMsg{
			generation: generation, operation: operation,
			kind: teamRouteOperationConfirm, reference: reference,
			view: view, err: readErr,
		}
	})
}

func (m *Model) declineTeamRouteProposal(closeAfter bool) tea.Cmd {
	state := m.route.team
	if state.proposal == nil {
		if closeAfter {
			return m.closeRouteToParent()
		}

		state.stage = teamRouteObjective

		m.composer.SetValue(state.objective)

		return m.composer.Focus()
	}

	ctx, generation, operation, ok := m.beginTeamRouteOperation(teamRouteOperationDecline)
	if !ok {
		return nil
	}
	state.declineToClose = closeAfter
	proposalID := state.proposal.ID

	return m.withTeamRouteActivity(func() tea.Msg {
		err := m.controller.DeclineTeam(ctx, proposalID)

		return teamRouteResultMsg{
			generation: generation, operation: operation,
			kind: teamRouteOperationDecline, err: err,
		}
	})
}

func (m *Model) readTeamRoute() tea.Cmd {
	state := m.route.team
	if state == nil || state.teamID == "" {
		return nil
	}
	if m.route.loading {
		state.refreshPending = true

		return nil
	}

	ctx, generation, operation, ok := m.beginTeamRouteOperation(teamRouteOperationRead)
	if !ok {
		return nil
	}
	request := coding.TeamReadRequest{TeamID: state.teamID}
	if state.view != nil {
		request.AfterRevision = state.view.ChangeCursor
		request.AfterControlRevision = state.view.ControlCursor
	}

	return m.withTeamRouteActivity(func() tea.Msg {
		view, err := m.controller.ReadTeam(ctx, request)

		return teamRouteResultMsg{
			generation: generation, operation: operation,
			kind: teamRouteOperationRead, view: view, err: err,
		}
	})
}

func (m *Model) submitTeamRouteControl() tea.Cmd {
	state := m.route.team
	request := state.control
	if request.TeamID == "" {
		return nil
	}

	ctx, generation, operation, ok := m.beginTeamRouteOperation(teamRouteOperationControl)
	if !ok {
		return nil
	}
	m.composer.Blur()

	return m.withTeamRouteActivity(func() tea.Msg {
		reference, err := m.controller.SubmitTeamControl(ctx, request)
		if err != nil {
			return teamRouteResultMsg{
				generation: generation, operation: operation,
				kind: teamRouteOperationControl, err: err,
			}
		}

		view, readErr := m.controller.ReadTeam(ctx, coding.TeamReadRequest{TeamID: request.TeamID})

		return teamRouteResultMsg{
			generation: generation, operation: operation,
			kind: teamRouteOperationControl, control: reference,
			view: view, err: readErr,
		}
	})
}

func (m *Model) applyTeamRouteResult(message teamRouteResultMsg) (tea.Model, tea.Cmd) {
	if !m.acceptTeamRouteResult(message) {
		return m, m.cleanupStaleTeamRouteProposal(message)
	}

	state := m.route.team
	if state.cancel != nil {
		state.cancel()
		state.cancel = nil
	}
	m.route.loading = false
	state.pending = teamRouteOperationNone
	cancelled := state.cancelRequested
	state.cancelRequested = false

	if cancelled && message.proposal.ID != "" {
		cleanup := m.cleanupStaleTeamRouteProposal(message)
		state.proposal = nil
		state.stage = teamRouteObjective
		m.route.err = nil

		return m, tea.Batch(m.composer.Focus(), cleanup)
	}

	command := m.applyTeamRouteOperation(message, cancelled)

	return m.finishTeamRouteResult(command)
}

func (m *Model) acceptTeamRouteResult(message teamRouteResultMsg) bool {
	return m.route.kind == routeTeam && m.route.team != nil &&
		message.generation == m.route.generation &&
		message.operation == m.route.team.operation &&
		message.kind == m.route.team.pending
}

func (m *Model) applyTeamRouteOperation(
	message teamRouteResultMsg,
	cancelled bool,
) tea.Cmd {
	switch message.kind {
	case teamRouteOperationGenerate:
		return m.applyGeneratedTeamProposal(message, cancelled)
	case teamRouteOperationRevise:
		return m.applyRevisedTeamProposal(message, cancelled)
	case teamRouteOperationConfirm:
		return m.applyConfirmedTeamProposal(message)
	case teamRouteOperationRead:
		return m.applyReadTeamRoute(message)
	case teamRouteOperationControl:
		return m.applyTeamRouteControl(message)
	case teamRouteOperationDecline:
		return m.applyDeclinedTeamProposal(message)
	case teamRouteOperationDiscoverRecovery:
		return m.applyDiscoveredTeamRecovery(message)
	case teamRouteOperationResumeRecovery:
		return m.applyResumedTeamRecovery(message)
	case teamRouteOperationLoadIntegration:
		return m.applyLoadedTeamIntegration(message)
	case teamRouteOperationPrepareIntegration:
		return m.applyPreparedTeamIntegration(message)
	case teamRouteOperationApplyIntegration, teamRouteOperationRejectIntegration,
		teamRouteOperationRecoverIntegration, teamRouteOperationCleanup:
		return m.applyTeamRouteIntegrationOperation(message)
	case teamRouteOperationNone:
		return nil
	default:
		return nil
	}
}

func (m *Model) applyTeamRouteIntegrationOperation(message teamRouteResultMsg) tea.Cmd {
	if message.kind == teamRouteOperationCleanup {
		return m.applyCompletedTeamCleanup(message)
	}

	return m.applyCompletedTeamIntegrationOperation(message)
}

func (m *Model) finishTeamRouteResult(command tea.Cmd) (tea.Model, tea.Cmd) {
	if m.route.kind == routeTeam && m.route.team != nil &&
		m.route.team.refreshPending && !m.route.loading {
		m.route.team.refreshPending = false

		return m, tea.Batch(command, m.refreshTeamRouteForStage())
	}

	return m, command
}

func (m *Model) applyGeneratedTeamProposal(
	message teamRouteResultMsg,
	cancelled bool,
) tea.Cmd {
	state := m.route.team
	if cancelled {
		m.route.err = nil
		state.stage = teamRouteObjective

		return m.composer.Focus()
	}
	if message.err != nil {
		m.route.err = message.err
		state.stage = teamRouteObjective

		return m.composer.Focus()
	}

	proposal := message.proposal.Clone()
	state.proposal = &proposal
	state.objective = proposal.Request.Objective
	state.stage = teamRouteProposal
	m.composer.Reset()
	m.composer.Blur()
	m.route.err = nil

	return nil
}

func (m *Model) applyRevisedTeamProposal(
	message teamRouteResultMsg,
	cancelled bool,
) tea.Cmd {
	state := m.route.team
	if cancelled {
		m.route.err = nil
		state.stage = teamRouteProposal
		m.composer.Reset()
		m.composer.Blur()

		return nil
	}
	if message.err != nil {
		m.route.err = message.err
		state.stage = teamRouteRevision

		return m.composer.Focus()
	}

	proposal := message.proposal.Clone()
	state.proposal = &proposal
	state.objective = proposal.Request.Objective
	state.stage = teamRouteProposal
	m.composer.Reset()
	m.composer.Blur()
	m.route.err = nil

	return nil
}

func (m *Model) applyConfirmedTeamProposal(message teamRouteResultMsg) tea.Cmd {
	state := m.route.team
	if message.reference.TeamID == "" {
		m.route.err = message.err
		state.stage = teamRouteProposal

		return nil
	}

	state.proposal = nil
	state.teamID = message.reference.TeamID
	state.stage = teamRouteActive
	m.composer.Reset()
	if message.view.TeamID != "" {
		view := m.mergeTeamRouteView(message.view)
		state.view = &view
	}
	m.route.err = message.err
	m.route.cursor = 0
	m.route.hasPreviousComposer = false
	m.teamPanel = teamPanelState{}

	return m.closeRouteToParent()
}

func (m *Model) applyReadTeamRoute(message teamRouteResultMsg) tea.Cmd {
	state := m.route.team
	if errors.Is(message.err, coding.ErrTeamReadUnavailable) {
		state.view = nil
		state.teamID = ""
		state.stage = teamRouteObjective
		m.route.err = nil

		return m.discoverTeamRouteRecovery()
	}
	if message.err == nil {
		view := m.mergeTeamRouteView(message.view)
		state.view = &view
		state.teamID = view.TeamID
	}
	m.route.err = message.err
	state.stage = teamRouteActive
	if state.view != nil {
		m.route.cursor = min(m.route.cursor, max(0, len(state.view.Tasks)-1))
	}

	if state.closeAfter {
		state.closeAfter = false

		return m.closeRouteToParent()
	}

	return nil
}

func (m *Model) applyTeamRouteControl(message teamRouteResultMsg) tea.Cmd {
	state := m.route.team
	state.stage = teamRouteActive
	state.control = coding.TeamControlRequest{}
	if message.control.CommandID != "" && message.view.TeamID != "" {
		view := m.mergeTeamRouteView(message.view)
		state.view = &view
	}
	m.route.err = message.err

	return nil
}

func (m *Model) applyDeclinedTeamProposal(message teamRouteResultMsg) tea.Cmd {
	state := m.route.team
	if message.err != nil {
		m.route.err = message.err
		state.stage = teamRouteProposal

		return nil
	}

	state.proposal = nil
	m.route.err = nil
	if state.declineToClose {
		return m.closeRouteToParent()
	}

	state.stage = teamRouteObjective
	m.composer.SetValue(state.objective)

	return m.composer.Focus()
}

func (m *Model) cleanupStaleTeamRouteProposal(message teamRouteResultMsg) tea.Cmd {
	if message.err != nil || message.proposal.ID == "" ||
		(message.kind != teamRouteOperationGenerate && message.kind != teamRouteOperationRevise) {
		return nil
	}
	proposalID := message.proposal.ID

	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(m.ctx), teamRouteCleanupTimeout)
		defer cancel()
		_ = m.controller.DeclineTeam(ctx, proposalID)

		return nil
	}
}

func (m *Model) updateTeamRouteKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	state := m.route.team
	if state == nil {
		return m, nil
	}

	key := message.String()
	if key == keyPageUp || key == keyPageDown {
		delta := max(1, m.height/2)
		if key == keyPageUp {
			delta = -delta
		}
		m.route.offset = max(0, m.route.offset+delta)

		return m, nil
	}
	if m.route.loading {
		return m, m.updateBusyTeamRouteKey(key)
	}

	return m.updateTeamRouteStageKey(message)
}

func (m *Model) updateTeamRouteStageKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := message.String()
	switch m.route.team.stage {
	case teamRouteObjective:
		return m.updateTeamObjectiveKey(message)
	case teamRouteProposal:
		return m.updateTeamProposalKey(key)
	case teamRouteRevision:
		return m.updateTeamRevisionKey(message)
	case teamRouteConfirmation:
		return m.updateTeamConfirmationKey(key)
	default:
		return m.updateTeamRouteOperationalStageKey(message)
	}
}

func (m *Model) updateTeamRouteOperationalStageKey(
	message tea.KeyPressMsg,
) (tea.Model, tea.Cmd) {
	key := message.String()
	switch m.route.team.stage {
	case teamRouteRecovery:
		return m.updateTeamRecoveryKey(key)
	case teamRouteRecoveryConfirmation:
		return m.updateTeamRecoveryConfirmationKey(key)
	case teamRouteActive:
		return m.updateActiveTeamKey(key)
	case teamRouteControlInput:
		return m.updateTeamControlInputKey(message)
	case teamRouteControlConfirmation:
		return m.updateTeamControlConfirmationKey(key)
	default:
		return m.updateTeamRouteIntegrationStageKey(key)
	}
}

func (m *Model) updateTeamRouteIntegrationStageKey(key string) (tea.Model, tea.Cmd) {
	switch m.route.team.stage {
	case teamRouteIntegration:
		return m.updateTeamIntegrationKey(key)
	case teamRouteIntegrationPreview:
		return m.updateTeamIntegrationPreviewKey(key)
	case teamRouteIntegrationConfirmation:
		return m.updateTeamIntegrationConfirmationKey(key)
	default:
		return m, nil
	}
}

func (m *Model) updateBusyTeamRouteKey(key string) tea.Cmd {
	if key != keyEscape && key != keyCtrlC {
		return nil
	}

	state := m.route.team
	state.cancelRequested = true
	if state.stage == teamRouteActive {
		state.closeAfter = true
	}
	if state.cancel != nil {
		state.cancel()
	}
	if state.stage == teamRouteObjective &&
		state.pending == teamRouteOperationDiscoverRecovery {
		return m.closeRouteToParent()
	}

	return nil
}

func (m *Model) updateTeamObjectiveKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch message.String() {
	case keyEscape, keyCtrlC:
		return m, m.closeRouteToParent()
	case keyEnter:
		return m, m.generateTeamRouteProposal()
	}

	return m.updateTeamRouteInput(message)
}

func (m *Model) updateTeamProposalKey(key string) (tea.Model, tea.Cmd) {
	state := m.route.team
	switch key {
	case keyEscape:
		return m, m.declineTeamRouteProposal(false)
	case keyCtrlC, "c":
		return m, m.declineTeamRouteProposal(true)
	case "r":
		state.stage = teamRouteRevision
		m.composer.Reset()

		return m, m.composer.Focus()
	case keyEnter:
		state.stage = teamRouteConfirmation
		m.route.cursor = 0
		m.route.err = nil
	}

	return m, nil
}

func (m *Model) updateTeamRevisionKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch message.String() {
	case keyEscape, keyCtrlC:
		m.route.team.stage = teamRouteProposal
		m.composer.Reset()
		m.composer.Blur()
		m.route.err = nil

		return m, nil
	case keyEnter:
		return m, m.reviseTeamRouteProposal()
	}

	return m.updateTeamRouteInput(message)
}

func (m *Model) updateTeamConfirmationKey(key string) (tea.Model, tea.Cmd) {
	state := m.route.team
	if state.proposal == nil {
		return m, nil
	}
	if key == keyEscape || key == keyCtrlC {
		state.stage = teamRouteProposal
		m.route.err = nil

		return m, nil
	}

	if state.proposal.Dirty {
		switch key {
		case "up", "k":
			m.route.cursor = wrapIndex(m.route.cursor-1, 2)
		case keyDown, "j", keyTab:
			m.route.cursor = wrapIndex(m.route.cursor+1, 2)
		case keyEnter:
			if m.route.cursor == 0 {
				state.stage = teamRouteProposal
				m.route.err = newTeamRoutePresentationError("HEAD-only admission was not selected; no Team was created")

				return m, nil
			}

			return m, m.confirmTeamRouteProposal(coding.TeamAdmissionHEADOnly)
		}

		return m, nil
	}

	if key == keyEnter {
		return m, m.confirmTeamRouteProposal(coding.TeamAdmissionClean)
	}

	return m, nil
}

func (m *Model) updateActiveTeamKey(key string) (tea.Model, tea.Cmd) {
	state := m.route.team
	if key == keyEscape || key == keyCtrlC {
		return m, m.closeRouteToParent()
	}
	if state.view == nil {
		if key == "r" {
			return m, m.readTeamRoute()
		}

		return m, nil
	}

	switch key {
	case "up", "k":
		m.route.cursor = wrapIndex(m.route.cursor-1, len(state.view.Tasks))
	case keyDown, "j", keyTab:
		m.route.cursor = wrapIndex(m.route.cursor+1, len(state.view.Tasks))
	default:
		return m.updateActiveTeamActionKey(key)
	}

	return m, nil
}

func (m *Model) updateActiveTeamActionKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "m":
		return m, m.openTeamControlInput(coding.TeamControlMessage)
	case "f":
		return m, m.openTeamControlInput(coding.TeamControlFollowUp)
	case "i":
		return m, m.openTeamControlConfirmation(coding.TeamControlInterruptAttempt)
	case "x":
		return m, m.openTeamControlConfirmation(coding.TeamControlCancelTask)
	case "r":
		return m, m.openTeamControlConfirmation(coding.TeamControlRetryTask)
	case "C":
		return m, m.openTeamControlConfirmation(coding.TeamControlCancelTeam)
	case "g":
		return m, m.openTeamIntegrationRoute()
	}

	return m, nil
}

func (m *Model) openTeamControlInput(action coding.TeamControlAction) tea.Cmd {
	request, err := m.teamRouteControlRequest(action)
	if err != nil {
		m.route.err = err

		return nil
	}

	state := m.route.team
	state.control = request
	state.stage = teamRouteControlInput
	m.composer.Reset()
	m.route.err = nil

	return m.composer.Focus()
}

func (m *Model) openTeamControlConfirmation(action coding.TeamControlAction) tea.Cmd {
	request, err := m.teamRouteControlRequest(action)
	if err != nil {
		m.route.err = err

		return nil
	}

	m.route.team.control = request
	m.route.team.stage = teamRouteControlConfirmation
	m.route.err = nil

	return nil
}

func (m *Model) updateTeamControlInputKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	state := m.route.team
	switch message.String() {
	case keyEscape, keyCtrlC:
		state.stage = teamRouteActive
		state.control = coding.TeamControlRequest{}
		m.composer.Reset()
		m.composer.Blur()
		m.route.err = nil

		return m, nil
	case keyEnter:
		text := strings.TrimSpace(m.composer.Value())
		if text == "" || len(text) > maximumTeamRouteInputBytes {
			m.route.err = newTeamRoutePresentationError("enter a message within the supported limit")

			return m, nil
		}
		state.control.Text = text

		return m, m.submitTeamRouteControl()
	}

	return m.updateTeamRouteInput(message)
}

func (m *Model) updateTeamControlConfirmationKey(key string) (tea.Model, tea.Cmd) {
	if key == keyEscape || key == keyCtrlC {
		m.route.team.stage = teamRouteActive
		m.route.team.control = coding.TeamControlRequest{}
		m.route.err = nil

		return m, nil
	}
	if key == keyEnter {
		return m, m.submitTeamRouteControl()
	}

	return m, nil
}

func (m *Model) updateTeamRouteInput(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if message.String() == keyCtrlV {
		return m, m.readClipboardImage()
	}

	before := m.composer.Snapshot()
	var command tea.Cmd
	m.composer, command = m.composer.Update(message)
	if len(m.composer.Value()) > maximumTeamRouteInputBytes {
		if err := m.composer.Restore(before); err != nil {
			m.streamErr = err
		}
		m.route.err = newTeamRoutePresentationError("input is too long")
	} else {
		m.route.err = nil
	}
	m.setLayout()
	if message.Key().Text == "$" && m.canOpenSkillPickerAtCursor() {
		return m, tea.Batch(command, m.openSkillPickerAfterDollar())
	}
	if message.Key().Text == "@" && m.canOpenFilePickerAtCursor() {
		return m, tea.Batch(command, m.openFilePickerAfterAt())
	}

	return m, command
}

func (m *Model) teamRouteControlRequest(
	action coding.TeamControlAction,
) (coding.TeamControlRequest, error) {
	state := m.route.team
	if m.controller.Mode().Current != coding.ModeAgent {
		return coding.TeamControlRequest{}, newTeamRoutePresentationError("team controls are read-only in Plan Mode")
	}
	if state == nil || state.view == nil || state.view.TeamID == "" {
		return coding.TeamControlRequest{}, newTeamRoutePresentationError("team state is not available")
	}

	request := coding.TeamControlRequest{TeamID: state.view.TeamID, Action: action}
	if action == coding.TeamControlCancelTeam {
		if state.view.Status != team.StatusActive {
			return coding.TeamControlRequest{}, newTeamRoutePresentationError("the Team is not cancellable")
		}

		return request, nil
	}

	task, ok := selectedTeamRouteTask(state.view, m.route.cursor)
	if !ok {
		return coding.TeamControlRequest{}, newTeamRoutePresentationError("select a Team task first")
	}
	request.TaskID = task.ID

	switch action {
	case coding.TeamControlCancelTask, coding.TeamControlRetryTask:
		return validateTeamRouteTaskControl(request, task)
	case coding.TeamControlMessage, coding.TeamControlFollowUp,
		coding.TeamControlInterruptAttempt:
		return bindTeamRouteAttemptControl(request, state.view, task.ID)
	case coding.TeamControlCancelTeam:
		return request, nil
	case coding.TeamControlResolveApproval, coding.TeamControlResolveQuestion,
		coding.TeamControlRejectQuestion:
		return coding.TeamControlRequest{}, newTeamRoutePresentationError("unsupported Team interaction control")
	default:
		return coding.TeamControlRequest{}, newTeamRoutePresentationError("unsupported Team control")
	}
}

func validateTeamRouteTaskControl(
	request coding.TeamControlRequest,
	task coding.TeamTaskView,
) (coding.TeamControlRequest, error) {
	if request.Action == coding.TeamControlRetryTask {
		if task.Status != team.TaskStatusFailed {
			return coding.TeamControlRequest{}, newTeamRoutePresentationError("the selected task is not retryable")
		}

		return request, nil
	}

	switch task.Status {
	case team.TaskStatusPending, team.TaskStatusReady,
		team.TaskStatusClaimed, team.TaskStatusRunning:
		return request, nil
	default:
		return coding.TeamControlRequest{}, newTeamRoutePresentationError("the selected task is not cancellable")
	}
}

func bindTeamRouteAttemptControl(
	request coding.TeamControlRequest,
	view *coding.TeamView,
	taskID team.TaskID,
) (coding.TeamControlRequest, error) {
	attempt, found := runningTeamRouteAttempt(view, taskID)
	if !found {
		return coding.TeamControlRequest{}, newTeamRoutePresentationError("the selected task has no live Worker Attempt")
	}
	request.MemberID = attempt.Target.MemberID
	request.ExpectedAttemptID = attempt.Target.AttemptID
	request.OwnerGeneration = attempt.Target.OwnerGeneration

	return request, nil
}

func selectedTeamRouteTask(view *coding.TeamView, cursor int) (coding.TeamTaskView, bool) {
	if view == nil || cursor < 0 || cursor >= len(view.Tasks) {
		return coding.TeamTaskView{}, false
	}

	return view.Tasks[cursor], true
}

func runningTeamRouteAttempt(
	view *coding.TeamView,
	taskID team.TaskID,
) (coding.TeamAttemptView, bool) {
	var selected coding.TeamAttemptView
	found := false
	for _, attempt := range view.Attempts {
		if attempt.Target.TaskID != taskID ||
			attempt.DomainState != team.AttemptStatusRunning ||
			attempt.Target.OwnerGeneration == 0 {
			continue
		}
		if !found || attempt.Number > selected.Number {
			selected = attempt
			found = true
		}
	}

	return selected, found
}

func (m *Model) invalidateTeamRoute(event coding.Event) tea.Cmd {
	if m.route.kind != routeTeam || m.route.team == nil {
		return nil
	}

	var teamID team.ID
	switch payload := event.Payload.(type) {
	case coding.TeamLifecycle:
		teamID = payload.TeamID
	case coding.TeamControlLifecycle:
		teamID = payload.TeamID
		m.mergeTeamRouteControl(payload, event.Time)
	case coding.TeamIntegrationLifecycle:
		teamID = payload.TeamID
	default:
		return nil
	}
	if teamID == "" || teamID != m.route.team.teamID {
		return nil
	}

	return m.refreshTeamRouteForStage()
}

func (m *Model) mergeTeamRouteView(next coding.TeamView) coding.TeamView {
	merged := m.mergeTeamProjectionView(next)
	if m.route.team != nil && m.route.team.view != nil {
		previous := m.route.team.view
		for _, control := range previous.Controls {
			merged.Controls = upsertTeamRouteControl(merged.Controls, control)
		}
		merged.ControlRevision = max(merged.ControlRevision, previous.ControlRevision)
		merged.ControlCursor = max(merged.ControlCursor, previous.ControlCursor)
	}
	if len(merged.Controls) > maximumTeamRouteControls {
		merged.Controls = slices.Clone(
			merged.Controls[len(merged.Controls)-maximumTeamRouteControls:],
		)
	}
	m.storeTeamProjectionView(merged)

	return merged
}

func (m *Model) mergeTeamRouteControl(value coding.TeamControlLifecycle, at time.Time) {
	m.ensureTeamProjection()
	m.mergeTeamProjectionControl(value, at)
	if m.route.team == nil || m.route.team.view == nil ||
		value.TeamID != m.route.team.view.TeamID {
		return
	}

	view := m.route.team.view
	view.Controls = upsertTeamRouteControl(
		view.Controls,
		teamRouteControlView(value, at),
	)
	if len(view.Controls) > maximumTeamRouteControls {
		view.Controls = slices.Clone(view.Controls[len(view.Controls)-maximumTeamRouteControls:])
	}
	view.ControlRevision = max(view.ControlRevision, value.Revision)
	view.ControlCursor = max(view.ControlCursor, value.Revision)
}

func upsertTeamRouteControl(
	values []coding.TeamControlView,
	value coding.TeamControlView,
) []coding.TeamControlView {
	for index := range values {
		if values[index].CommandID != value.CommandID {
			continue
		}
		if value.CreatedAt.IsZero() {
			value.CreatedAt = values[index].CreatedAt
		}
		values[index] = value

		return values
	}

	return append(values, value)
}

func teamRouteControlView(value coding.TeamControlLifecycle, at time.Time) coding.TeamControlView {
	return coding.TeamControlView{
		Revision: value.Revision, CommandID: value.CommandID, Action: value.Action,
		MemberID: value.MemberID, TaskID: value.TaskID, AttemptID: value.AttemptID,
		OwnerGeneration: value.OwnerGeneration, State: value.State, Code: value.Code,
		CreatedAt: at, UpdatedAt: at,
	}
}

func (m *Model) teamRouteView() tea.View {
	content, _ := m.teamRouteContent()
	content, _ = fitScrollableContentWindow(
		content,
		max(1, m.width),
		max(1, m.height),
		m.route.offset,
	)
	view := tea.NewView(content)
	view.AltScreen = false
	view.MouseMode = tea.MouseModeNone
	view.WindowTitle = appTitle

	return view
}

func (m *Model) teamRouteIsInline() bool {
	if m.route.kind != routeTeam || m.route.team == nil {
		return false
	}

	switch m.route.team.stage {
	case teamRouteObjective, teamRouteProposal, teamRouteRevision,
		teamRouteConfirmation, teamRouteActive, teamRouteControlInput,
		teamRouteControlConfirmation:
		return true
	case teamRouteRecovery, teamRouteRecoveryConfirmation,
		teamRouteIntegration, teamRouteIntegrationPreview,
		teamRouteIntegrationConfirmation:
		return false
	default:
		return false
	}
}

func (m *Model) inlineTeamRouteView() string {
	if !m.teamRouteIsInline() {
		return ""
	}

	content, _ := m.teamRouteContent()
	reserved := lipgloss.Height(m.composerBox()) +
		lipgloss.Height(m.statusLineView()) + conversationGapHeight
	available := max(1, m.height-reserved)

	return fitScrollableContent(content, max(1, m.width), available, m.route.offset)
}

func teamRouteInputStage(stage teamRouteStage) bool {
	return stage == teamRouteObjective || stage == teamRouteRevision ||
		stage == teamRouteControlInput
}

func (m *Model) teamRouteContent() (string, int) {
	state := m.route.team
	if state == nil {
		return "Team route unavailable.", -1
	}

	content, inputY := m.teamRouteStageContent(state)
	if m.route.loading {
		content = m.teamRouteLoadingContent(content, state)
	}
	if m.route.err != nil {
		errorLines := m.appendTeamRouteWrapped(
			nil,
			"Error: "+safeTeamRouteError(m.route.err),
			"",
			teamRouteToneError,
		)
		content += "\n\n" + strings.Join(errorLines, "\n")
	}

	return content, inputY
}

func (m *Model) teamRouteStageContent(state *teamRouteState) (string, int) {
	switch state.stage {
	case teamRouteObjective:
		return m.teamObjectiveContent()
	case teamRouteProposal:
		return m.teamProposalContent(false), -1
	case teamRouteRevision:
		return m.teamRevisionContent()
	case teamRouteConfirmation:
		return m.teamConfirmationContent(), -1
	default:
		return m.teamRouteOperationalStageContent(state)
	}
}

func (m *Model) teamRouteOperationalStageContent(state *teamRouteState) (string, int) {
	switch state.stage {
	case teamRouteRecovery:
		return m.teamRecoveryContent(), -1
	case teamRouteRecoveryConfirmation:
		return m.teamRecoveryConfirmationContent(), -1
	case teamRouteActive:
		return m.activeTeamContent(), -1
	case teamRouteControlInput:
		return m.teamControlInputContent()
	case teamRouteControlConfirmation:
		return m.teamControlConfirmationContent(), -1
	default:
		return m.teamRouteIntegrationStageContent(state), -1
	}
}

func (m *Model) teamRouteIntegrationStageContent(state *teamRouteState) string {
	switch state.stage {
	case teamRouteIntegration:
		return m.teamIntegrationContent()
	case teamRouteIntegrationPreview:
		return m.teamIntegrationPreviewContent()
	case teamRouteIntegrationConfirmation:
		return m.teamIntegrationConfirmationContent()
	default:
		return "Team route unavailable."
	}
}

func (m *Model) teamObjectiveContent() (string, int) {
	lines := []string{"Coding Team", ""}
	if m.controller.Mode().Current == coding.ModePlan {
		lines = append(lines, "Plan Mode is read-only. Switch to Agent Mode before generating a proposal.", "")
	}
	lines = append(
		lines,
		"Describe one outcome. A constrained Lead will propose up to three Workers and a task DAG.",
		"",
		"Enter generate proposal · Esc close",
	)

	return strings.Join(lines, "\n"), -1
}

func (m *Model) teamRevisionContent() (string, int) {
	lines := []string{"Revise Team proposal", "", "Describe the change in natural language.", ""}
	lines = append(
		lines,
		"Enter regenerate · Esc keep current proposal",
		"",
	)
	lines = append(lines, m.teamProposalLines()...)

	return strings.Join(lines, "\n"), -1
}

func (m *Model) teamProposalContent(confirmation bool) string {
	lines := m.teamProposalLines()
	if confirmation {
		return strings.Join(lines, "\n")
	}

	lines = append(lines,
		"",
		m.teamRouteFooter("Enter confirm · r revise · c cancel · Esc back"),
		"",
	)

	return strings.Join(lines, "\n")
}

func (m *Model) teamConfirmationContent() string {
	proposal := m.route.team.proposal
	if proposal == nil {
		return "Team proposal unavailable."
	}

	lines := []string{"Confirm Team admission", "", "No Worker, Worktree, branch, or lease exists yet.", ""}
	if !proposal.Dirty {
		lines = append(lines,
			"> Create the Team from the reviewed clean Workspace state",
			"", "Enter confirm clean admission · Esc back",
		)

		return strings.Join(lines, "\n")
	}

	choices := []string{
		"Stop and return to the proposal (recommended)",
		"Create from committed HEAD only; exclude all uncommitted changes",
	}
	for index, choice := range choices {
		marker := "  "
		if index == m.route.cursor {
			marker = "> "
		}
		lines = append(lines, marker+choice)
	}
	lines = append(lines, "", "↑/↓ choose · Enter apply exact choice · Esc back")

	return strings.Join(lines, "\n")
}

func latestTeamRouteAttempt(
	view *coding.TeamView,
	taskID team.TaskID,
) (coding.TeamAttemptView, bool) {
	var selected coding.TeamAttemptView
	found := false
	for _, attempt := range view.Attempts {
		if attempt.Target.TaskID == taskID && (!found || attempt.Number > selected.Number) {
			selected = attempt
			found = true
		}
	}

	return selected, found
}

func teamRouteMemberName(view *coding.TeamView, memberID team.MemberID) string {
	for _, member := range view.Members {
		if member.ID == memberID {
			return safeDetailText(member.Name)
		}
	}
	if memberID == "" {
		return "unassigned"
	}

	return "Worker"
}

func (m *Model) teamControlInputContent() (string, int) {
	request := m.route.team.control
	label := "Message"
	if request.Action == coding.TeamControlFollowUp {
		label = "Follow-up"
	}
	lines := []string{
		label + " selected Worker", "",
		"The command is bound to the exact live Attempt reviewed in the Team view.", "",
	}
	lines = append(
		lines,
		"Enter submit · Esc cancel",
	)

	return strings.Join(lines, "\n"), -1
}

func (m *Model) teamControlConfirmationContent() string {
	request := m.route.team.control
	label := "Apply Team control"
	switch request.Action {
	case coding.TeamControlMessage:
		label = "Send a message to the exact selected Attempt"
	case coding.TeamControlFollowUp:
		label = "Queue a follow-up for the exact selected Attempt"
	case coding.TeamControlInterruptAttempt:
		label = "Interrupt the exact selected Attempt"
	case coding.TeamControlCancelTask:
		label = "Cancel the selected Task"
	case coding.TeamControlRetryTask:
		label = "Retry the selected failed Task"
	case coding.TeamControlCancelTeam:
		label = "Cancel the entire Team"
	case coding.TeamControlResolveApproval, coding.TeamControlResolveQuestion,
		coding.TeamControlRejectQuestion:
		label = "Resolve the exact selected Worker interaction"
	}

	return strings.Join([]string{
		"Confirm Team control", "", "> " + label, "",
		"Enter submit durable intent · Esc back",
	}, "\n")
}

func safeTeamRouteError(err error) string {
	var presentation *teamRoutePresentationError
	if errors.As(err, &presentation) {
		return truncateText(sanitizeInspectionText(presentation.message), 512)
	}

	switch {
	case err == nil:
		return "Team operation failed."
	case errors.Is(err, context.Canceled):
		return "Team operation canceled."
	case errors.Is(err, context.DeadlineExceeded):
		return "Team operation timed out."
	}
	if message := safeTeamAdmissionError(err); message != "" {
		return message
	}
	if message := safeTeamRuntimeError(err); message != "" {
		return message
	}
	if message := safeTeamIntegrationError(err); message != "" {
		return message
	}

	return "The Team operation failed; review Runtime diagnostics."
}

func safeTeamAdmissionError(err error) string {
	switch {
	case errors.Is(err, coding.ErrTeamRepositoryRequired):
		return "Coding Team requires the Workspace to be a Git repository root with at least one commit. " +
			"Create the initial commit, then retry."
	case errors.Is(err, coding.ErrTeamActive):
		return "A Team proposal or admitted Team is already active."
	case errors.Is(err, coding.ErrTeamProposalNotFound):
		return "The Team proposal is no longer available."
	case errors.Is(err, coding.ErrTeamProposalStale):
		return "The Team proposal is stale; generate a fresh proposal."
	case errors.Is(err, coding.ErrTeamDirty):
		return "The Workspace changed; review admission evidence again."
	case errors.Is(err, coding.ErrTeamInteractionRequired):
		return "Explicit Team confirmation is required."
	case errors.Is(err, coding.ErrTeamReadStale):
		return "The Team view cursor is stale; reopen the route."
	case errors.Is(err, coding.ErrTeamReadUnavailable):
		return "The selected Team is no longer available."
	case errors.Is(err, coding.ErrTeamAdmission):
		return "The Team request could not be completed; review Runtime diagnostics."
	default:
		return ""
	}
}

func safeTeamRuntimeError(err error) string {
	switch {
	case errors.Is(err, coding.ErrRuntimeBusy), errors.Is(err, runtimecontrol.ErrBusy):
		return "Another Runtime operation is still active."
	case errors.Is(err, coding.ErrRuntimeClosed), errors.Is(err, runtimecontrol.ErrClosed):
		return "The Runtime is closed."
	default:
		return ""
	}
}
