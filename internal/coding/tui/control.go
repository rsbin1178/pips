//nolint:wsl_v5 // Control operations and their route/picker bookkeeping stay adjacent.
package tui

import (
	"errors"

	tea "charm.land/bubbletea/v2"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/modelcatalog"
	"github.com/rsbin1178/pips/internal/coding/runtimecontrol"
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
	saved     bool
	saveErr   error
}

var errSelectionPersistenceUnavailable = errors.New("model-selection persistence is unavailable")

type teamRecoveryProbeMsg struct {
	sessionID string
	values    []coding.TeamRecoveryCandidate
	err       error
}

func (m *Model) runModeControl(mode coding.OperatingMode) tea.Cmd {
	activityWasVisible := m.activityClockVisible()
	if m.picker.kind != pickerNone {
		m.picker.loading = true
		m.picker.controlling = true
		m.picker.err = nil
	}

	apply := func() tea.Msg {
		return controlResultMsg{operation: operationMode, err: m.controller.SetMode(m.ctx, mode)}
	}

	return tea.Batch(apply, m.startActivityClock(activityWasVisible))
}

func (m *Model) runPermissionControl(
	update runtimecontrol.PermissionUpdate,
	confirmed ...bool,
) tea.Cmd {
	activityWasVisible := m.activityClockVisible()
	m.picker.loading = true
	m.picker.controlling = true
	m.picker.err = nil
	needsConfirmation := len(confirmed) > 0 && confirmed[0]

	apply := func() tea.Msg {
		if needsConfirmation {
			confirmation, err := m.controller.NewFullAccessConfirmation(m.ctx, update)
			if err != nil {
				return controlResultMsg{operation: operationPermissions, err: err}
			}

			return controlResultMsg{
				operation: operationPermissions,
				err:       m.controller.SetPermissions(m.ctx, update, confirmation),
			}
		}

		return controlResultMsg{
			operation: operationPermissions,
			err:       m.controller.SetPermissions(m.ctx, update),
		}
	}

	return tea.Batch(apply, m.startActivityClock(activityWasVisible))
}

//nolint:gocyclo // The control operation table and its automatic selection save stay adjacent.
func (m *Model) runControl(
	operation controlOperation,
	sessionID string,
	selected modelcatalog.Selection,
) tea.Cmd {
	activityWasVisible := m.activityClockVisible()
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

	apply := func() tea.Msg {
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
		case operationPermissions:
			err = errors.New("coding tui: permission changes require runPermissionControl")
		}

		saved := false
		var saveErr error
		if err == nil && operation == operationModel {
			if m.options.SaveSelection == nil {
				saveErr = errSelectionPersistenceUnavailable
			} else if saveErr = m.options.SaveSelection(m.ctx, selected); saveErr == nil {
				saved = true
			}
		}

		return controlResultMsg{
			operation: operation, err: err, saved: saved, saveErr: saveErr,
		}
	}

	return tea.Batch(apply, m.startActivityClock(activityWasVisible))
}

// selectionSaveNotice reports the durable half of apply-and-save. The model
// selection is already effective, so a save failure is a warning rather than a
// failed control result.
func (m *Model) selectionSaveNotice(message controlResultMsg) tea.Cmd {
	switch {
	case message.saveErr != nil:
		return m.setStatusError("Model applied; save failed: " + safeSelectionSaveError(message.saveErr))
	case message.saved:
		notice := "Model selection saved as default"
		if m.selectionOverriddenAtStartup() {
			notice += " (startup flags/environment still override it)"
		}

		return m.setStatusNotice(notice)
	default:
		return nil
	}
}

func (m *Model) selectionOverriddenAtStartup() bool {
	if m.controller == nil {
		return false
	}
	value := m.controller.Config()
	for _, field := range []config.Field{config.FieldModel, config.FieldVariant, config.FieldReasoning} {
		source, ok := value.Source(field)
		if ok && (source.Kind == config.SourceEnvironment || source.Kind == config.SourceFlag) {
			return true
		}
	}

	return false
}

//nolint:dupl // Selection wording mirrors the status-line picker's error matrix.
func safeSelectionSaveError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errSelectionPersistenceUnavailable):
		return "selection persistence is unavailable"
	case errors.Is(err, config.ErrConflict):
		return "configuration changed; reopen /model and retry"
	case errors.Is(err, config.ErrUnsafe):
		return "configuration file is not safe to update"
	case errors.Is(err, config.ErrSelectionEditUnsupported):
		return "configuration shape is unsupported"
	case errors.Is(err, config.ErrSelectionDurability):
		return "saved; disk durability is uncertain"
	case errors.Is(err, config.ErrDecode):
		return "configuration is invalid"
	case errors.Is(err, config.ErrFile):
		return "configuration cannot be read or written"
	case errors.Is(err, config.ErrInvalid):
		return "model selection is not supported"
	default:
		return "model selection could not be saved"
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
