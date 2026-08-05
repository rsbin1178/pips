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
	assert.Contains(t, ansi.Strip(model.View().Content), "Workspace write")
	assert.Empty(t, controller.permissionUpdates)

	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, pickerNone, model.picker.kind)
	assert.Equal(t, before, model.composer.Snapshot())
	assert.Empty(t, controller.permissionUpdates)
}

func TestPermissionsPickerHidesConfiguredValuesAndProvenance(t *testing.T) {
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

	assert.Contains(t, content, "Mode: Workspace write")
	assert.Contains(t, content, "Policy: Block actions that need approval")
	assert.Contains(t, content, "Network: Off")
	assert.NotContains(t, content, "configured")
	assert.NotContains(t, content, "source=")
	assert.NotContains(t, content, "process override")
	assert.NotContains(t, content, "workspace-write")
	assert.NotContains(t, content, "on-request")
	assert.NotContains(t, content, "full-access")
}

func TestPermissionsPickerChangesNetworkWithDownThenRight(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	model := readyModelWithController(t, controller, true)
	model.openPermissionsPicker()

	assert.Equal(t, permissionModeRow, model.picker.cursor)
	_, command := model.updatePermissionsPickerKey(tea.KeyPressMsg{Code: tea.KeyDown})
	assert.Nil(t, command)
	assert.Equal(t, permissionNetworkRow, model.picker.cursor)

	_, command = model.updatePermissionsPickerKey(tea.KeyPressMsg{Code: tea.KeyRight})
	assert.Nil(t, command)
	assert.Equal(t, config.SandboxNetworkAllow, model.picker.permissions.network)

	_, command = model.updatePermissionsPickerKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, command)
	message := commandMessage(t, command)
	_, next := model.Update(message)
	if next != nil {
		driveModelCommands(t, model, next)
	}

	require.Len(t, controller.permissionUpdates, 1)
	require.NotNil(t, controller.permissionUpdates[0].SandboxProfile)
	assert.Equal(t, config.SandboxNetworkAllow, *controller.permissionUpdates[0].SandboxProfile.Network)
}

func TestPermissionsPickerSkipsNetworkUnderFullAccess(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	model := readyModelWithController(t, controller, true)
	model.openPermissionsPicker()
	model.picker.permissions.sandbox = config.SandboxFullAccess

	_, command := model.updatePermissionsPickerKey(tea.KeyPressMsg{Code: tea.KeyDown})
	assert.Nil(t, command)
	assert.Equal(t, permissionApprovalRow, model.picker.cursor)

	_, command = model.updatePermissionsPickerKey(tea.KeyPressMsg{Code: tea.KeyUp})
	assert.Nil(t, command)
	assert.Equal(t, permissionModeRow, model.picker.cursor)
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
	assert.Contains(t, ansi.Strip(model.View().Content), "Full access confirmation")

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
	assert.Contains(t, pickerContent, "Network: Ask when needed")
	assert.Contains(t, pickerContent, "Policy: Ask before risky actions")
	assert.Contains(t, pickerContent, "active inside Sandbox")

	statusContent := model.statusContent()
	assert.Contains(t, statusContent, "Approval: Ask before risky actions")
	assert.Contains(t, statusContent, "Network: Ask when needed (active)")
}

func TestPermissionsPickerKeepsNetworkActiveForReadOnlyAndInactiveForFullAccess(t *testing.T) {
	t.Parallel()

	controller := newOverlayController(readyState())
	model := readyModelWithController(t, controller, true)
	model.openPermissionsPicker()
	model.picker.permissions.sandbox = config.SandboxReadOnly
	content := ansi.Strip(model.permissionsPickerView(20))
	assert.Contains(t, content, "Network: Ask when needed")
	assert.Contains(t, content, "active inside Sandbox")

	model.picker.permissions.sandbox = config.SandboxFullAccess
	content = ansi.Strip(model.permissionsPickerView(20))
	assert.Contains(t, content, "Network: Unrestricted under Full access")
	assert.Contains(t, content, "Select Read only or Workspace write")
	assert.NotContains(t, content, "› Network")
}

func TestStatusHidesPermissionProvenance(t *testing.T) {
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

	assert.Contains(t, content, "Sandbox: Workspace write")
	assert.Contains(t, content, "Approval: Block actions that need approval")
	assert.Contains(t, content, "Network: Off (active)")
	assert.NotContains(t, content, "source=")
	assert.NotContains(t, content, "configured")
	assert.NotContains(t, content, "process override")
	assert.NotContains(t, content, "workspace-write")
	assert.NotContains(t, content, "on-request")
	assert.NotContains(t, content, "full-access")
	assert.NotContains(t, content, "/Users/")
	assert.NotContains(t, content, "PIPS_")

	controller.permissions.Sandbox = config.SandboxFullAccess
	controller.permissions.SandboxProfile.Filesystem.Effective = config.SandboxFullAccess
	controller.permissions.SandboxProfile.NetworkEnforced = false
	content = model.statusContent()
	assert.Contains(t, content, "Sandbox: Full access")
	assert.Contains(t, content, "Network: Unrestricted under Full access")
}

func TestPermissionsPickerDraftFullAccessUsesHumanLabels(t *testing.T) {
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

	assert.Contains(t, content, "Mode: Full access")
	assert.Contains(t, content, "Network: Unrestricted under Full access")
	assert.NotContains(t, content, "source=")
	assert.NotContains(t, content, "configured")
	assert.NotContains(t, content, "draft (not applied)")
}

func TestPermissionsPickerHandlesUnknownValuesWithoutProvenance(t *testing.T) {
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

	assert.Contains(t, content, "Mode: Full access")
	assert.NotContains(t, content, "source=")
	assert.NotContains(t, content, "configured")

	model.picker.cursor = permissionModeRow
	model.picker.permissions.sandbox = config.SandboxMode("")
	model.cyclePermissionValue(1)
	assert.Equal(t, config.SandboxReadOnly, model.picker.permissions.sandbox)

	model.picker.permissions.sandbox = config.SandboxMode("")
	model.cyclePermissionValue(-1)
	assert.Equal(t, config.SandboxFullAccess, model.picker.permissions.sandbox)
}
