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
	profile    runtimecontrol.PermissionState
	sandbox    config.SandboxMode
	approval   config.ApprovalMode
	network    config.SandboxNetworkMode
	confirming bool
}

const (
	permissionModeRow = iota
	permissionNetworkRow
	permissionApprovalRow
	permissionRowCount
)

func (m *Model) openPermissionsPicker() {
	profile := m.permissionState()
	m.picker = pickerState{
		kind: pickerPermissions,
		permissions: permissionPickerState{
			profile:  profile,
			sandbox:  profile.SandboxProfile.Filesystem.Effective,
			approval: profile.ApprovalPolicy.Effective,
			network:  profile.SandboxProfile.Network.Effective,
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

	if m.picker.permissions.confirming {
		return m.updateFullAccessConfirmationKey(message)
	}

	switch message.String() {
	case keyEscape, keyCtrlC:
		return m, m.closePicker()
	case "up", "k":
		m.picker.cursor = permissionCursorStep(
			m.picker.cursor,
			-1,
			m.picker.permissions.sandbox,
		)
	case keyDown, "j", keyTab:
		m.picker.cursor = permissionCursorStep(
			m.picker.cursor,
			1,
			m.picker.permissions.sandbox,
		)
	case keyLeft, "h":
		m.cyclePermissionValue(-1)
	case keyRight, "l":
		m.cyclePermissionValue(1)
	case keyEnter:
		if m.permissionDraftUnchanged() {
			return m, m.closePicker()
		}
		if m.permissionNeedsFullAccessConfirmation() {
			m.picker.permissions.confirming = true
			m.picker.err = nil

			return m, nil
		}

		return m, m.runPermissionControl(m.permissionUpdate())
	}

	return m, nil
}

func (m *Model) updateFullAccessConfirmationKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch message.String() {
	case keyEscape, keyCtrlC, "n", "N":
		m.picker.permissions.confirming = false
		m.picker.err = nil
	case keyEnter, "y", "Y":
		m.picker.permissions.confirming = false
		return m, m.runPermissionControl(m.permissionUpdate(), true)
	}

	return m, nil
}

func (m *Model) cyclePermissionValue(direction int) {
	if direction == 0 {
		return
	}

	switch m.picker.cursor {
	case permissionModeRow:
		values := []config.SandboxMode{
			config.SandboxReadOnly,
			config.SandboxWorkspaceWrite,
			config.SandboxFullAccess,
		}
		index := slices.Index(values, m.picker.permissions.sandbox)
		if index < 0 {
			if direction < 0 {
				index = len(values) - 1
			} else {
				index = 0
			}
		} else {
			index = wrapIndex(index+direction, len(values))
		}
		m.picker.permissions.sandbox = values[index]
	case permissionNetworkRow:
		// Full Access retains the selected Network value for the next sandboxed
		// profile, but does not enforce or edit it while Full Access is drafted.
		if m.picker.permissions.sandbox == config.SandboxFullAccess {
			return
		}
		values := []config.SandboxNetworkMode{
			config.SandboxNetworkDeny,
			config.SandboxNetworkOnRequest,
			config.SandboxNetworkAllow,
		}
		index := slices.Index(values, m.picker.permissions.network)
		if index < 0 {
			if direction < 0 {
				index = len(values) - 1
			} else {
				index = 0
			}
		} else {
			index = wrapIndex(index+direction, len(values))
		}
		m.picker.permissions.network = values[index]
	case permissionApprovalRow:
		values := []config.ApprovalMode{config.ApprovalOnRequest, config.ApprovalNever}
		index := slices.Index(values, m.picker.permissions.approval)
		if index < 0 {
			if direction < 0 {
				index = len(values) - 1
			} else {
				index = 0
			}
		} else {
			index = wrapIndex(index+direction, len(values))
		}
		m.picker.permissions.approval = values[index]
	}
	m.picker.err = nil
}

func (m *Model) permissionUpdate() runtimecontrol.PermissionUpdate {
	permissions := m.picker.permissions

	return runtimecontrol.PermissionUpdate{
		SandboxProfile: &runtimecontrol.SandboxPermissionUpdate{
			Filesystem: &permissions.sandbox,
			Network:    &permissions.network,
		},
		Approval: &permissions.approval,
	}
}

func (m *Model) permissionDraftUnchanged() bool {
	permissions := m.picker.permissions
	profile := permissions.profile

	return permissions.sandbox == profile.SandboxProfile.Filesystem.Effective &&
		permissions.network == profile.SandboxProfile.Network.Effective &&
		permissions.approval == profile.ApprovalPolicy.Effective
}

func (m *Model) permissionNeedsFullAccessConfirmation() bool {
	permissions := m.picker.permissions

	return permissions.sandbox == config.SandboxFullAccess &&
		permissions.profile.SandboxProfile.Filesystem.Effective != config.SandboxFullAccess
}

func (m *Model) permissionsPickerView(maxHeight int) string {
	if m.picker.kind != pickerPermissions {
		return ""
	}

	permissions := m.picker.permissions
	if permissions.confirming {
		return m.fullAccessConfirmationView(maxHeight)
	}

	lines := []string{
		"Permissions · current process only",
		"",
		"Execution",
	}
	lines = append(lines, m.permissionPickerRow(
		permissionModeRow,
		"Mode",
		permissionModeText(permissions.sandbox),
		true,
	))
	lines = append(lines, m.permissionPickerDetail(permissionModeDescription(permissions.sandbox)))

	networkFocusable := permissions.sandbox != config.SandboxFullAccess
	lines = append(lines, m.permissionPickerRow(
		permissionNetworkRow,
		"Network",
		permissionNetworkText(permissions.network, networkFocusable),
		networkFocusable,
	))
	if networkFocusable {
		lines = append(lines, m.permissionPickerDetail("active inside Sandbox"))
	} else {
		lines = append(lines, m.permissionPickerDetail(
			"Select Read only or Workspace write to configure Network.",
		))
	}

	lines = append(lines, "", "Approval")
	lines = append(lines, m.permissionPickerRow(
		permissionApprovalRow,
		"Policy",
		permissionApprovalText(permissions.approval),
		true,
	))
	lines = append(lines, m.permissionPickerDetail("controls when risky actions need review"))
	if m.picker.loading {
		lines = append(lines, m.activityNotice("Working…"))
	}
	if m.picker.err != nil {
		lines = append(lines, "Error: "+safeError(m.picker.err))
	}
	lines = append(lines, "", "Changes apply to this Pips process only and reset on restart.")
	lines = append(lines, "↑/↓ choose · ←/→ change · Enter apply · Esc cancel")
	for index := range lines {
		lines[index] = ansi.Truncate(lines[index], max(1, m.width), "…")
	}

	return truncateHeight(strings.Join(lines, "\n"), max(1, maxHeight))
}

func (m *Model) fullAccessConfirmationView(maxHeight int) string {
	lines := []string{
		"Full access confirmation",
		"",
		"This removes the OS Sandbox boundary for subsequent commands.",
		"Commands may read and modify files outside the Workspace and use the network.",
		"This applies only to the current Pips process and resets on restart.",
		"",
		"Enter/y confirm · Esc/n cancel",
	}
	if !m.options.NoColor {
		lines[0] = lipgloss.NewStyle().Bold(true).Foreground(paletteFor(m.theme).warning).Render(lines[0])
	}
	if m.picker.err != nil {
		lines = append(lines, "Error: "+safeError(m.picker.err))
	}
	for index := range lines {
		lines[index] = ansi.Truncate(lines[index], max(1, m.width), "…")
	}

	return truncateHeight(strings.Join(lines, "\n"), max(1, maxHeight))
}

func (m *Model) permissionPickerRow(
	index int,
	name string,
	value string,
	focusable bool,
) string {
	prefix := "    "
	if focusable && index == m.picker.cursor {
		prefix = "  › "
	}
	line := prefix + name + ": " + value
	if !m.options.NoColor && focusable && index == m.picker.cursor {
		line = lipgloss.NewStyle().Foreground(paletteFor(m.theme).session).Render(line)
	}

	return line
}

func (m *Model) permissionPickerDetail(value string) string {
	return "            " + value
}

func permissionCursorStep(cursor, direction int, sandbox config.SandboxMode) int {
	if direction == 0 {
		return cursor
	}

	for range permissionRowCount {
		cursor = wrapIndex(cursor+direction, permissionRowCount)
		if cursor != permissionNetworkRow || sandbox != config.SandboxFullAccess {
			return cursor
		}
	}

	return permissionModeRow
}

func permissionModeText(mode config.SandboxMode) string {
	switch mode {
	case config.SandboxReadOnly:
		return "Read only"
	case config.SandboxWorkspaceWrite:
		return "Workspace write"
	case config.SandboxFullAccess:
		return "Full access"
	default:
		return "Unknown"
	}
}

func permissionModeDescription(mode config.SandboxMode) string {
	switch mode {
	case config.SandboxReadOnly:
		return "Inspect files and answer; cannot write files."
	case config.SandboxWorkspaceWrite:
		return "Modify files inside the active Workspace."
	case config.SandboxFullAccess:
		return "Run without the OS Sandbox boundary; requires confirmation."
	default:
		return "Unknown execution mode."
	}
}

func permissionNetworkText(mode config.SandboxNetworkMode, active bool) string {
	if !active {
		return "Unrestricted under Full access"
	}

	switch mode {
	case config.SandboxNetworkDeny:
		return "Off"
	case config.SandboxNetworkOnRequest:
		return "Ask when needed"
	case config.SandboxNetworkAllow:
		return "On"
	default:
		return "Unknown"
	}
}

func permissionApprovalText(mode config.ApprovalMode) string {
	switch mode {
	case config.ApprovalOnRequest:
		return "Ask before risky actions"
	case config.ApprovalNever:
		return "Block actions that need approval"
	default:
		return "Unknown"
	}
}
