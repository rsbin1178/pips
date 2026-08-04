//nolint:wsl_v5 // Permission picker rows and transitions stay locally auditable.
package tui

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/runtimecontrol"
)

type permissionPickerState struct {
	profile  runtimecontrol.PermissionState
	sandbox  config.SandboxMode
	approval config.ApprovalMode
	network  config.SandboxNetworkMode
}

func (m *Model) openPermissionsPicker() {
	profile := m.permissionState()
	m.picker = pickerState{
		kind: pickerPermissions,
		permissions: permissionPickerState{
			profile:  profile,
			sandbox:  profile.Sandbox,
			approval: profile.Approval,
			network:  profile.Network,
		},
	}
	m.setLayout()
}

func (m *Model) permissionState() runtimecontrol.PermissionState {
	return m.controller.Permissions()
}

func (m *Model) updatePermissionsPickerKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.picker.kind != pickerPermissions || m.picker.controlling {
		return m, nil
	}

	switch message.String() {
	case keyEscape, keyCtrlC:
		return m, m.closePicker()
	case "up", "k":
		m.picker.cursor = wrapIndex(m.picker.cursor-1, 3)
	case keyDown, "j", keyTab:
		m.picker.cursor = wrapIndex(m.picker.cursor+1, 3)
	case keyLeft, "h":
		m.cyclePermissionValue(-1)
	case keyRight, "l":
		m.cyclePermissionValue(1)
	case keyEnter:
		return m, m.runPermissionControl(m.permissionUpdate())
	}

	return m, nil
}

func (m *Model) cyclePermissionValue(direction int) {
	if direction == 0 {
		return
	}

	switch m.picker.cursor {
	case 0:
		values := []config.ApprovalMode{config.ApprovalOnRequest, config.ApprovalNever}
		index := slices.Index(values, m.picker.permissions.approval)
		m.picker.permissions.approval = values[wrapIndex(index+direction, len(values))]
	case 1:
		values := []config.SandboxNetworkMode{
			config.SandboxNetworkDeny,
			config.SandboxNetworkOnRequest,
			config.SandboxNetworkAllow,
		}
		index := slices.Index(values, m.picker.permissions.network)
		m.picker.permissions.network = values[wrapIndex(index+direction, len(values))]
	case 2:
		values := []config.SandboxMode{config.SandboxWorkspaceWrite}
		if m.picker.permissions.profile.Sandbox == config.SandboxFullAccess {
			values = append(values, config.SandboxFullAccess)
		}
		index := slices.Index(values, m.picker.permissions.sandbox)
		m.picker.permissions.sandbox = values[wrapIndex(index+direction, len(values))]
	}
	m.picker.err = nil
}

func (m *Model) permissionUpdate() runtimecontrol.PermissionUpdate {
	permissions := m.picker.permissions

	return runtimecontrol.PermissionUpdate{
		Sandbox:  &permissions.sandbox,
		Approval: &permissions.approval,
		Network:  &permissions.network,
	}
}

func (m *Model) permissionsPickerView(maxHeight int) string {
	if m.picker.kind != pickerPermissions {
		return ""
	}

	permissions := m.picker.permissions
	lines := []string{"Permissions · current process only"}
	lines = append(lines, m.permissionPickerRow(
		0, "Approval", permissionValueText(
			string(permissions.approval), string(permissions.profile.ConfiguredApproval),
			permissions.profile.ApprovalOverridden,
		),
		permissionSourceText(permissions.profile.ApprovalSource, permissions.profile.ApprovalOverridden),
		"on-request asks before gated operations · never fails closed",
	))
	networkNote := "workspace-write network authority"
	if permissions.profile.Sandbox == config.SandboxFullAccess {
		networkNote = "configured value; inactive under full-access"
	}
	lines = append(lines, m.permissionPickerRow(
		1, "Network", permissionValueText(
			string(permissions.network), string(permissions.profile.ConfiguredNetwork),
			permissions.profile.NetworkOverridden,
		),
		permissionSourceText(permissions.profile.NetworkSource, permissions.profile.NetworkOverridden),
		networkNote,
	))
	var sandboxNote string
	switch {
	case permissions.profile.Sandbox == config.SandboxFullAccess:
		sandboxNote = "user-configured full-access; picker can only narrow"
	case permissions.profile.ConfiguredSandbox == config.SandboxFullAccess:
		sandboxNote = "process narrowed from full-access; restore unavailable until restart"
	default:
		sandboxNote = "full-access elevation requires explicit user configuration"
	}
	lines = append(lines, m.permissionPickerRow(
		2, "Sandbox", permissionValueText(
			string(permissions.sandbox), string(permissions.profile.ConfiguredSandbox),
			permissions.profile.SandboxOverridden,
		),
		permissionSourceText(permissions.profile.SandboxSource, permissions.profile.SandboxOverridden),
		sandboxNote,
	))
	if m.picker.loading {
		lines = append(lines, m.activityNotice("Working…"))
	}
	if m.picker.err != nil {
		lines = append(lines, "Error: "+safeError(m.picker.err))
	}
	lines = append(lines, "↑/↓ choose · ←/→ change · Enter apply · Esc cancel")
	for index := range lines {
		lines[index] = ansi.Truncate(lines[index], max(1, m.width), "…")
	}

	return truncateHeight(strings.Join(lines, "\n"), max(1, maxHeight))
}

func (m *Model) permissionPickerRow(
	index int,
	name string,
	value string,
	source string,
	note string,
) string {
	prefix := "  "
	if index == m.picker.cursor {
		prefix = "› "
	}
	line := prefix + name + ": " + value + " · " + source + " · " + note
	if !m.options.NoColor && index == m.picker.cursor {
		line = lipgloss.NewStyle().Foreground(paletteFor(m.theme).session).Render(line)
	}

	return line
}

func permissionValueText(effective, configured string, overridden bool) string {
	if !overridden || effective == configured || configured == "" {
		return effective
	}

	return effective + " (configured: " + configured + ")"
}

func permissionSourceText(source config.SourceKind, overridden bool) string {
	label := "source=" + string(source)
	if source == "" {
		label = "source=unknown"
	}
	if overridden {
		label += " · process override"
	}

	return label
}
