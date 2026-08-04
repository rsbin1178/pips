//nolint:wsl_v5 // Control operations and their route/picker bookkeeping stay adjacent.
package tui

import (
	"errors"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/modelcatalog"
	"github.com/rsbin/pips/internal/coding/runtimecontrol"
)

type controlOperation uint8

const (
	operationNew controlOperation = iota
	operationResume
	operationModel
	operationReload
	operationFork
	operationMode
	operationPermissions
)

type controlResultMsg struct {
	operation controlOperation
	err       error
}

type teamRecoveryProbeMsg struct {
	sessionID string
	values    []coding.TeamRecoveryCandidate
	err       error
}

func (m *Model) runModeControl(mode coding.OperatingMode) tea.Cmd {
	if m.picker.kind != pickerNone {
		m.picker.loading = true
		m.picker.controlling = true
		m.picker.err = nil
	}

	return func() tea.Msg {
		return controlResultMsg{operation: operationMode, err: m.controller.SetMode(m.ctx, mode)}
	}
}

func (m *Model) runPermissionControl(update runtimecontrol.PermissionUpdate) tea.Cmd {
	activityWasVisible := m.activityClockVisible()
	m.picker.loading = true
	m.picker.controlling = true
	m.picker.err = nil

	apply := func() tea.Msg {
		return controlResultMsg{
			operation: operationPermissions,
			err:       m.controller.SetPermissions(m.ctx, update),
		}
	}

	return tea.Batch(apply, m.startActivityClock(activityWasVisible))
}

func (m *Model) runControl(
	operation controlOperation,
	sessionID string,
	selected modelcatalog.Selection,
) tea.Cmd {
	switch {
	case m.picker.kind != pickerNone:
		m.picker.loading = true
		m.picker.controlling = true
		m.picker.err = nil
	case m.route.kind != routeNone:
		m.route.loading = true
		m.route.controlling = true
		m.route.err = nil
	}

	return func() tea.Msg {
		var err error
		switch operation {
		case operationNew:
			err = m.controller.NewSession(m.ctx)
		case operationResume:
			err = m.controller.ResumeSession(m.ctx, sessionID)
		case operationModel:
			err = m.controller.SwitchModel(m.ctx, selected)
		case operationReload:
			err = m.controller.Reload(m.ctx)
		case operationFork:
			err = m.controller.ForkSession(m.ctx, sessionID)
		case operationMode:
			err = errors.New("coding tui: mode changes require runModeControl")
		}

		return controlResultMsg{operation: operation, err: err}
	}
}

func (m *Model) probeTeamRecoveryAfterResume() tea.Cmd {
	controller := m.controller
	sessionID := m.state.SessionID
	ctx := m.ctx

	return func() tea.Msg {
		values, err := controller.DiscoverTeamRecovery(ctx)

		return teamRecoveryProbeMsg{
			sessionID: sessionID, values: cloneTeamRouteRecovery(values), err: err,
		}
	}
}
