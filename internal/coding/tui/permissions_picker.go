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
	permissionFilesystemRow = iota
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

	switch message.String() {
	case keyEscape, keyCtrlC:
		return m, m.closePicker()
	case "up", "k":
		m.picker.cursor = wrapIndex(m.picker.cursor-1, permissionRowCount)
	case keyDown, "j", keyTab:
		m.picker.cursor = wrapIndex(m.picker.cursor+1, permissionRowCount)
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

func (m *Model) cyclePermissionValue(direction int) {
	if direction == 0 {
		return
	}

	switch m.picker.cursor {
	case permissionFilesystemRow:
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

	profile := permissions.profile
	lines := []string{"Permissions · current process only", "Sandbox"}
	lines = append(lines, m.permissionPickerRow(
		permissionFilesystemRow,
		"Filesystem",
		permissionValueText(
			string(permissions.sandbox), string(profile.SandboxProfile.Filesystem.Configured),
			permissions.sandbox != profile.SandboxProfile.Filesystem.Effective ||
				profile.SandboxProfile.Filesystem.Overridden,
		),
		permissionDraftSourceText(
			string(permissions.sandbox),
			string(profile.SandboxProfile.Filesystem.Effective),
			string(profile.SandboxProfile.Filesystem.Configured),
			profile.SandboxProfile.Filesystem.EffectiveSource,
			profile.SandboxProfile.Filesystem.ConfiguredSource,
		),
		"read-only · workspace-write · full-access",
	))

	networkNote := "active · enforced for this Sandbox profile"
	if permissions.sandbox == config.SandboxFullAccess {
		networkNote = "inactive · not enforced under full-access"
	}
	lines = append(lines, m.permissionPickerRow(
		permissionNetworkRow,
		"Network",
		permissionValueText(
			string(permissions.network), string(profile.SandboxProfile.Network.Configured),
			permissions.network != profile.SandboxProfile.Network.Effective ||
				profile.SandboxProfile.Network.Overridden,
		),
		permissionDraftSourceText(
			string(permissions.network),
			string(profile.SandboxProfile.Network.Effective),
			string(profile.SandboxProfile.Network.Configured),
			profile.SandboxProfile.Network.EffectiveSource,
			profile.SandboxProfile.Network.ConfiguredSource,
		),
		networkNote,
	))
	lines = append(lines, "Approval")
	lines = append(lines, m.permissionPickerRow(
		permissionApprovalRow,
		"Approval",
		permissionValueText(
			string(permissions.approval), string(profile.ApprovalPolicy.Configured),
			permissions.approval != profile.ApprovalPolicy.Effective ||
				profile.ApprovalPolicy.Overridden,
		),
		permissionDraftSourceText(
			string(permissions.approval),
			string(profile.ApprovalPolicy.Effective),
			string(profile.ApprovalPolicy.Configured),
			profile.ApprovalPolicy.EffectiveSource,
			profile.ApprovalPolicy.ConfiguredSource,
		),
		"independent confirmation policy",
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

func (m *Model) fullAccessConfirmationView(maxHeight int) string {
	lines := []string{
		"Full Access confirmation",
		"",
		"Full Access disables the Sandbox boundary for subsequent commands.",
		"This changes only the current Pips process; it does not edit config or Session history.",
		"",
		"Press Enter or y to confirm · Esc or n to decline",
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
	source string,
	note string,
) string {
	prefix := "    "
	if index == m.picker.cursor {
		prefix = "  › "
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

func permissionSourceText(source, configuredSource config.SourceKind, overridden bool) string {
	return permissionSourceTextWithDraft(source, configuredSource, overridden, false)
}

func permissionDraftSourceText(
	draftValue, effectiveValue, configuredValue string,
	effectiveSource, configuredSource config.SourceKind,
) string {
	source := effectiveSource
	draft := draftValue != effectiveValue
	if draft {
		// Full Access is always a user-confirmed process-local elevation when
		// the effective profile is not already Full Access. This remains true
		// even when the configured value is also Full Access: the draft is
		// proposing the Controller's session override, not restoring a field.
		switch {
		case draftValue == string(config.SandboxFullAccess) &&
			effectiveValue != string(config.SandboxFullAccess):
			source = config.SourceSessionOverride
		case configuredValue != "" && draftValue == configuredValue:
			source = configuredSource
		default:
			source = config.SourceSessionOverride
		}
	}

	return permissionSourceTextWithDraft(
		source,
		configuredSource,
		source == config.SourceSessionOverride,
		draft,
	)
}

func permissionSourceTextWithDraft(
	source, configuredSource config.SourceKind,
	overridden bool,
	draft bool,
) string {
	label := "source=" + string(source)
	if source == "" {
		label = "source=unknown"
	}
	if configuredSource != "" && source != configuredSource {
		label += " · configured source=" + string(configuredSource)
	}
	if overridden {
		label += " · process override"
	}
	if draft {
		label += " · draft (not applied)"
	}

	return label
}
