//nolint:wsl_v5 // Picker setup and observable assertions remain adjacent.
package tui

import (
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/runtimecontrol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommandPickerOffersPermissionsAndOmitsDiff(t *testing.T) {
	t.Parallel()

	model := readyModelWithController(t, newOverlayController(readyState()), true)
	model.openCommandPicker()
	content := ansi.Strip(model.View().Content)

	assert.Contains(t, content, "/permissions")
	assert.NotContains(t, content, "/diff")
}

func TestPermissionsPickerIsDraftOnlyAndRestoresComposer(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	model := readyModelWithController(t, controller, true)
	model.composer.SetValue("keep this draft")
	before := model.composer.Snapshot()
	model.openCommandPicker()
	model.picker.query = commandPermissions
	model.syncCommandInput()

	_, command := model.Update(key("enter"))
	assert.Nil(t, command)
	assert.Equal(t, pickerPermissions, model.picker.kind)
	assert.Equal(t, before, model.composer.Snapshot())
	assert.Contains(t, model.View().Content, "Permissions")
	assert.Contains(t, model.View().Content, "Sandbox")
	assert.Contains(t, model.View().Content, "workspace-write")
	assert.Empty(t, controller.permissionUpdates)

	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, pickerNone, model.picker.kind)
	assert.Equal(t, before, model.composer.Snapshot())
	assert.Empty(t, controller.permissionUpdates)
}

func TestPermissionsPickerShowsConfiguredValuesForOverrides(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.permissions = runtimecontrol.PermissionState{
		Sandbox:            config.SandboxWorkspaceWrite,
		ConfiguredSandbox:  config.SandboxFullAccess,
		SandboxOverridden:  true,
		Approval:           config.ApprovalNever,
		ConfiguredApproval: config.ApprovalOnRequest,
		ApprovalOverridden: true,
		Network:            config.SandboxNetworkDeny,
		ConfiguredNetwork:  config.SandboxNetworkAllow,
		NetworkOverridden:  true,
	}
	model := readyModelWithController(t, controller, true)
	model.openPermissionsPicker()
	content := ansi.Strip(model.View().Content)

	assert.Contains(t, content, "configured: on-request")
	assert.Contains(t, content, "configured: allow")
	assert.Contains(t, content, "configured: full-access")
}

func TestPermissionsPickerAppliesProcessLocalUpdate(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	model := readyModelWithController(t, controller, true)
	model.openPermissionsPicker()
	model.picker.permissions.approval = config.ApprovalNever
	model.picker.permissions.network = config.SandboxNetworkDeny

	command := model.runPermissionControl(model.permissionUpdate())
	message := commandMessage(t, command)
	_, next := model.Update(message)
	if next != nil {
		driveModelCommands(t, model, next)
	}

	require.Len(t, controller.permissionUpdates, 1)
	assert.Equal(t, config.ApprovalNever, *controller.permissionUpdates[0].Approval)
	require.NotNil(t, controller.permissionUpdates[0].SandboxProfile)
	assert.Equal(t, config.SandboxNetworkDeny, *controller.permissionUpdates[0].SandboxProfile.Network)
	assert.Equal(t, pickerNone, model.picker.kind)
}

func TestPermissionsPickerKeepsDraftOnFailure(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.permissionErr = errors.New("permission replacement failed")
	model := readyModelWithController(t, controller, true)
	model.openPermissionsPicker()
	model.picker.permissions.approval = config.ApprovalNever

	command := model.runPermissionControl(model.permissionUpdate())
	message := commandMessage(t, command)
	_, next := model.Update(message)
	if next != nil {
		driveModelCommands(t, model, next)
	}

	assert.Equal(t, pickerPermissions, model.picker.kind)
	assert.Equal(t, config.ApprovalNever, model.picker.permissions.approval)
	require.ErrorContains(t, model.picker.err, "permission replacement failed")
}

func TestPermissionsPickerConfirmsFullAccessBeforeControllerCall(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	model := readyModelWithController(t, controller, true)
	model.openPermissionsPicker()
	model.picker.permissions.sandbox = config.SandboxFullAccess

	_, command := model.updatePermissionsPickerKey(key(keyEnter))
	assert.Nil(t, command)
	assert.True(t, model.picker.permissions.confirming)
	assert.Empty(t, controller.permissionUpdates)
	assert.Zero(t, controller.confirmationRequests)
	assert.Contains(t, ansi.Strip(model.View().Content), "Full Access confirmation")

	_, command = model.updatePermissionsPickerKey(key(keyEscape))
	assert.Nil(t, command)
	assert.False(t, model.picker.permissions.confirming)
	assert.Empty(t, controller.permissionUpdates)
	assert.Zero(t, controller.confirmationRequests)

	model.picker.permissions.sandbox = config.SandboxFullAccess
	_, command = model.updatePermissionsPickerKey(key(keyEnter))
	assert.Nil(t, command)
	assert.True(t, model.picker.permissions.confirming)
	_, command = model.updatePermissionsPickerKey(key(keyEnter))
	require.NotNil(t, command)
	message := commandMessage(t, command)
	_, next := model.Update(message)
	if next != nil {
		driveModelCommands(t, model, next)
	}
	assert.Equal(t, 1, controller.confirmationRequests)
	assert.Len(t, controller.permissionUpdates, 1)
}

func TestPermissionsPickerKeepsApprovalAndNetworkOnRequestDistinct(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	model := readyModelWithController(t, controller, true)
	model.width = 240
	model.openPermissionsPicker()
	pickerContent := ansi.Strip(model.permissionsPickerView(20))
	assert.Contains(t, pickerContent, "Network: on-request")
	assert.Contains(t, pickerContent, "Approval: on-request")
	assert.Contains(t, pickerContent, "independent confirmation policy")

	statusContent := model.statusContent()
	assert.Contains(t, statusContent, "Approval: on-request")
	assert.Contains(t, statusContent, "Network: on-request")
}

func TestPermissionsPickerKeepsNetworkActiveForReadOnlyAndInactiveForFullAccess(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	model := readyModelWithController(t, controller, true)
	model.openPermissionsPicker()
	model.picker.permissions.sandbox = config.SandboxReadOnly
	content := ansi.Strip(model.permissionsPickerView(20))
	assert.Contains(t, content, "active")
	assert.Contains(t, content, "enforced")

	model.picker.permissions.sandbox = config.SandboxFullAccess
	content = ansi.Strip(model.permissionsPickerView(20))
	assert.Contains(t, content, "inactive")
	assert.Contains(t, content, "not enforced")
}

func TestStatusShowsPermissionSourcesWithoutSourceDetails(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.permissions = runtimecontrol.PermissionState{
		Sandbox:            config.SandboxWorkspaceWrite,
		ConfiguredSandbox:  config.SandboxWorkspaceWrite,
		SandboxSource:      config.SourceConfigFile,
		Approval:           config.ApprovalNever,
		ConfiguredApproval: config.ApprovalOnRequest,
		ApprovalSource:     config.SourceEnvironment,
		ApprovalOverridden: true,
		Network:            config.SandboxNetworkDeny,
		ConfiguredNetwork:  config.SandboxNetworkOnRequest,
		NetworkSource:      config.SourceFlag,
		NetworkOverridden:  true,
		SandboxProfile: runtimecontrol.SandboxPermissionState{
			Filesystem: runtimecontrol.PermissionFilesystemState{
				Effective:        config.SandboxWorkspaceWrite,
				Configured:       config.SandboxWorkspaceWrite,
				EffectiveSource:  config.SourceConfigFile,
				ConfiguredSource: config.SourceDefault,
				Overridden:       true,
			},
			Network: runtimecontrol.PermissionNetworkState{
				Effective:        config.SandboxNetworkDeny,
				Configured:       config.SandboxNetworkOnRequest,
				EffectiveSource:  config.SourceFlag,
				ConfiguredSource: config.SourceConfigFile,
				Overridden:       true,
			},
			NetworkEnforced: true,
		},
		ApprovalPolicy: runtimecontrol.PermissionApprovalState{
			Effective:        config.ApprovalNever,
			Configured:       config.ApprovalOnRequest,
			EffectiveSource:  config.SourceEnvironment,
			ConfiguredSource: config.SourceConfigFile,
			Overridden:       true,
		},
	}
	model := readyModelWithController(t, controller, true)
	content := model.statusContent()

	assert.Contains(t, content, "source=config_file")
	assert.Contains(t, content, "source=environment")
	assert.Contains(t, content, "source=flag")
	assert.Contains(t, content, "configured source=config_file")
	assert.Contains(t, content, "process override")
	assert.NotContains(t, content, "/Users/")
	assert.NotContains(t, content, "PIPS_")

	controller.permissions.Sandbox = config.SandboxFullAccess
	controller.permissions.SandboxProfile.Filesystem.Effective = config.SandboxFullAccess
	controller.permissions.SandboxProfile.NetworkEnforced = false
	content = model.statusContent()
	assert.Contains(t, content, "inactive under full-access")
}

func TestPermissionsPickerDraftFullAccessElevationUsesSessionOverrideSource(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.permissions = runtimecontrol.PermissionState{
		Sandbox:           config.SandboxWorkspaceWrite,
		ConfiguredSandbox: config.SandboxFullAccess,
		SandboxOverridden: true,
		SandboxProfile: runtimecontrol.SandboxPermissionState{
			Filesystem: runtimecontrol.PermissionFilesystemState{
				Effective:        config.SandboxWorkspaceWrite,
				Configured:       config.SandboxFullAccess,
				EffectiveSource:  config.SourceSessionOverride,
				ConfiguredSource: config.SourceConfigFile,
				Overridden:       true,
			},
			Network: runtimecontrol.PermissionNetworkState{
				Effective:        config.SandboxNetworkOnRequest,
				Configured:       config.SandboxNetworkOnRequest,
				EffectiveSource:  config.SourceConfigFile,
				ConfiguredSource: config.SourceConfigFile,
			},
			NetworkEnforced: true,
		},
	}
	model := readyModelWithController(t, controller, true)
	model.width = 240
	model.openPermissionsPicker()
	model.picker.permissions.sandbox = config.SandboxFullAccess

	content := ansi.Strip(model.permissionsPickerView(20))

	assert.Contains(t, content, "Filesystem: full-access")
	assert.Contains(t, content, "source=session_override")
	assert.Contains(t, content, "configured source=config_file")
	assert.Contains(t, content, "draft (not applied)")
}

func TestPermissionsPickerDraftUsesProposedSourceAndHandlesUnknownValues(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	controller.permissions = runtimecontrol.PermissionState{
		Sandbox:           config.SandboxWorkspaceWrite,
		ConfiguredSandbox: config.SandboxWorkspaceWrite,
		SandboxProfile: runtimecontrol.SandboxPermissionState{
			Filesystem: runtimecontrol.PermissionFilesystemState{
				Effective:        config.SandboxWorkspaceWrite,
				Configured:       config.SandboxWorkspaceWrite,
				EffectiveSource:  config.SourceEnvironment,
				ConfiguredSource: config.SourceConfigFile,
			},
			Network: runtimecontrol.PermissionNetworkState{
				Effective:        config.SandboxNetworkOnRequest,
				Configured:       config.SandboxNetworkOnRequest,
				EffectiveSource:  config.SourceConfigFile,
				ConfiguredSource: config.SourceConfigFile,
			},
			NetworkEnforced: true,
		},
		ApprovalPolicy: runtimecontrol.PermissionApprovalState{
			Effective:        config.ApprovalOnRequest,
			Configured:       config.ApprovalOnRequest,
			EffectiveSource:  config.SourceConfigFile,
			ConfiguredSource: config.SourceConfigFile,
		},
	}
	model := readyModelWithController(t, controller, true)
	model.width = 120
	model.openPermissionsPicker()
	model.picker.permissions.sandbox = config.SandboxFullAccess
	content := ansi.Strip(model.permissionsPickerView(20))

	assert.Contains(t, content, "source=session_override")
	assert.Contains(t, content, "configured source=config_file")
	assert.NotContains(t, content, "source=environment")

	model.picker.cursor = permissionFilesystemRow
	model.picker.permissions.sandbox = config.SandboxMode("")
	model.cyclePermissionValue(1)
	assert.Equal(t, config.SandboxReadOnly, model.picker.permissions.sandbox)

	model.picker.permissions.sandbox = config.SandboxMode("")
	model.cyclePermissionValue(-1)
	assert.Equal(t, config.SandboxFullAccess, model.picker.permissions.sandbox)
}
