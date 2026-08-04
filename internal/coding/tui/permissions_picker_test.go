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
	assert.Contains(t, model.View().Content, "full-access elevation")
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
	assert.Equal(t, config.SandboxNetworkDeny, *controller.permissionUpdates[0].Network)
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
	}
	model := readyModelWithController(t, controller, true)
	content := model.statusContent()

	assert.Contains(t, content, "source=config_file")
	assert.Contains(t, content, "source=environment")
	assert.Contains(t, content, "source=flag")
	assert.Contains(t, content, "process override")
	assert.NotContains(t, content, "/Users/")
	assert.NotContains(t, content, "PIPS_")

	controller.permissions.Sandbox = config.SandboxFullAccess
	content = model.statusContent()
	assert.Contains(t, content, "inactive under full-access")
}
